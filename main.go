// staybusy places a controlled, predictable load on a machine.
//
// It can drive several kinds of load independently:
//
//	CPU     a duty-cycled busy loop, expressed as a percentage of the whole machine
//	memory  anonymous pages, allocated and touched so they are really resident
//	         -mem-bw adds sustained memory bandwidth instead
//	network sustained transfers, up and/or down, throttled to a target rate
//	disk    sustained reads and/or writes, throttled to a target rate or an IOPS
//	process -procs forks children, -threads holds OS threads, -fds holds handles
//
// Load can be changed at runtime over HTTP (-serve exposes /control) or scripted
// over time (-profile), and the whole thing can run for a fixed -duration.
//
// Useful for exercising autoscaling rules, validating monitoring thresholds and
// alerts, reproducing resource contention, and smoke-testing capacity limits.
//
// Single file, standard library only, builds to a static binary.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// version is stamped at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

// childToken marks a re-exec of ourselves as an inert child for -procs. It is not
// a flag, so it stays out of the help and the docs.
const childToken = "--staybusy-child"

const (
	kiB = 1024
	miB = 1024 * kiB
	giB = 1024 * miB

	// memReserve is the most that is ever left to the system. Allocation is
	// clamped to "available - reserve"; better to hold less than to wedge the
	// machine. The reserve scales down inside a small cgroup - see reserveFor.
	memReserve = 512 * miB
	// minMem: below this there is no point allocating, so the dimension is skipped.
	minMem = 64 * miB

	// linuxO_DIRECT is O_DIRECT on linux/amd64 and linux/arm64 (both 040000).
	// Kept as a plain number so the file stays platform-independent; it is only
	// ever used when runtime.GOOS == "linux".
	linuxO_DIRECT = 0x4000
)

var (
	flagCPU           = flag.Int("cpu", 0, "percentage of the whole machine's CPU to use while busy (1-100); 0 disables.\nLoad is spread over every core, so the figure means the same on any core count")
	flagCores         = flag.Int("cores", 0, "how many cores to load; 0 means all of them")
	flagPerCore       = flag.Int("per-core", 0, "percentage of each loaded core (1-100). Takes precedence over -cpu:\nuse it when you want an exact number of cores at an exact load each")
	flagDuty          = flag.Int("duty", 100, "percentage of time spent busy, across every dimension.\n100 = continuous; 10 means 6 minutes per hour, raising the upper decile of samples")
	flagCycle         = flag.Duration("cycle", time.Hour, "length of one duty cycle, or of one -schedule pass")
	flagRamp          = flag.Duration("ramp", 0, "ramp CPU from 0 up to its target over this period at startup; 0 disables")
	flagSchedule      = flag.String("schedule", "", "comma-separated CPU percentages of the machine to cycle through, each\nheld for an equal slice of -cycle (e.g. 80,20,0)")
	flagProfile       = flag.String("profile", "", "JSON file of timed steps that change the load over time")
	flagMem           = flag.String("mem", "0", "memory to hold: 512M / 1.5G / 1024K (a bare number means GiB); 0 disables")
	flagMemBW         = flag.Int("mem-bw", 0, "sustained memory bandwidth in MB/s; 0 disables")
	flagMemBWSize     = flag.String("mem-bw-size", "256M", "buffer size for -mem-bw")
	flagNetDown       = flag.Int("net-down", 0, "sustained download rate in Mbps; 0 disables")
	flagNetUp         = flag.Int("net-up", 0, "sustained upload rate in Mbps; 0 disables. Runs alongside -net-down, so\nboth directions can be driven at once")
	flagNetDownURL    = flag.String("net-down-url", "https://speed.cloudflare.com/__down?bytes=10485760", "download source (this endpoint returns 403 above 10 MB)")
	flagNetUpURL      = flag.String("net-up-url", "https://speed.cloudflare.com/__up", "upload target")
	flagNetConns      = flag.Int("net-conns", 1, "concurrent connections per direction")
	flagNetRPS        = flag.Int("net-rps", 0, "requests per second per direction, on top of the byte rate; 0 disables")
	flagNetChurn      = flag.Bool("net-churn", false, "do not reuse connections; every request opens a new one")
	flagDiskRead      = flag.Int("disk-read", 0, "sustained disk read rate in MB/s; 0 disables")
	flagDiskWrite     = flag.Int("disk-write", 0, "sustained disk write rate in MB/s; 0 disables")
	flagDiskReadIOPS  = flag.Int("disk-read-iops", 0, "disk read rate in operations per second; takes precedence over -disk-read")
	flagDiskWriteIOPS = flag.Int("disk-write-iops", 0, "disk write rate in operations per second; takes precedence over -disk-write")
	flagDiskSize      = flag.String("disk-size", "512M", "size of the file used for disk load: 512M / 1G / ...; a bare number means GiB")
	flagDiskBlock     = flag.String("disk-block", "1M", "I/O block size for disk load")
	flagDiskPattern   = flag.String("disk-pattern", "seq", "access pattern for disk load: seq or rand")
	flagDiskConns     = flag.Int("disk-conns", 1, "concurrent workers per disk direction")
	flagDiskDir       = flag.String("disk-dir", "", "directory for the disk file (default the OS temp dir)")
	flagDiskFsync     = flag.Bool("disk-fsync", false, "fsync after each written pass so the writes are durable")
	flagDiskDirect    = flag.Bool("disk-direct", false, "open the disk file with O_DIRECT on Linux to bypass the page cache (ignored elsewhere)")
	flagProcs         = flag.Int("procs", 0, "spawn this many inert child processes")
	flagThreads       = flag.Int("threads", 0, "hold this many OS threads")
	flagFds           = flag.Int("fds", 0, "hold this many open file descriptors")
	flagServe         = flag.String("serve", "", "listen address for the built-in load endpoint, metrics and control, e.g. :8080")
	flagDuration      = flag.Duration("duration", 0, "stop after this long; 0 runs until signalled")
	flagCheck         = flag.Bool("check", false, "with -duration, exit non-zero if a measured rate came in under its target")
	flagReport        = flag.String("report", "", "write a JSON summary of the run to this file on exit")
	flagConfig        = flag.String("config", "", "JSON file of flag values; command line and environment take precedence")
	flagLogJSON       = flag.Bool("log-json", false, "write logs as JSON lines")
	flagVersion       = flag.Bool("version", false, "print the version and exit")
)

// knobSet is the live load target. Every generator reads it each pass and
// /control or -profile writes it, so load can change without a restart.
type knobSet struct {
	cpuPct    atomic.Int64 // machine percent
	netDown   atomic.Int64 // bytes per second
	netUp     atomic.Int64
	diskRead  atomic.Int64 // bytes per second
	diskWrite atomic.Int64
	memBW     atomic.Int64 // bytes per second
	duty      atomic.Int64 // percent
	cycleNs   atomic.Int64 // nanoseconds
}

var knobs knobSet

var (
	startTime = time.Now()

	statNetDown   atomic.Int64
	statNetUp     atomic.Int64
	statDiskRead  atomic.Int64
	statDiskWrite atomic.Int64
	statMemBW     atomic.Int64
	statServeUp   atomic.Int64

	// heldBytes is memory this process has already committed (-mem plus a
	// -mem-bw buffer), so a second allocation is measured against what is left.
	heldBytes atomic.Int64

	// Expected byte totals, accumulated once a second from the live knobs, so
	// -check can compare against a target that moved during the run.
	expNetDown, expNetUp, expDiskRead, expDiskWrite, expMemBW atomic.Int64

	// tMem is the memory actually held, reported by /metrics and -report.
	tMem int64

	diskCleanup []string
	diskFiles   []*os.File
	fdHold      []*os.File
	childProcs  []*os.Process
)

// hold keeps every allocated block reachable so the garbage collector leaves it
// alone. The kernel reclaims it when the process exits.
var hold [][]byte

// dutyCycle returns the current duty window: whether load should be running, and
// how long each cycle is.
func pulseActive() bool {
	duty := int(knobs.duty.Load())
	if duty >= 100 {
		return true
	}
	cycle := time.Duration(knobs.cycleNs.Load())
	if cycle <= 0 {
		return true
	}
	burn := cycle * time.Duration(duty) / 100
	if burn <= 0 {
		burn = time.Millisecond
	}
	return time.Since(startTime)%cycle < burn
}

// accountLoop integrates the live targets into the expected-byte counters.
//
// It samples whether the duty window is open right now rather than scaling by
// the duty fraction: -duty is a square wave (busy for the first duty% of each
// cycle), so a short run can sit entirely inside the busy leg. Sampling at 100ms
// tracks that, where a duty-scaled average would be off by the whole cycle.
func accountLoop() {
	const dt = 100 * time.Millisecond
	for range time.NewTicker(dt).C {
		if !pulseActive() {
			continue
		}
		acc := func(k, e *atomic.Int64) {
			if v := k.Load(); v > 0 {
				e.Add(int64(float64(v) * dt.Seconds()))
			}
		}
		acc(&knobs.netDown, &expNetDown)
		acc(&knobs.netUp, &expNetUp)
		acc(&knobs.diskRead, &expDiskRead)
		acc(&knobs.diskWrite, &expDiskWrite)
		acc(&knobs.memBW, &expMemBW)
	}
}

// checkRates compares what was moved against what was asked for. The bar is
// deliberately loose: a short run spends its first seconds warming up.
func checkRates() int {
	ok := true
	chk := func(name string, got, exp *atomic.Int64) {
		e := exp.Load()
		if e <= 0 {
			return
		}
		g := got.Load()
		logf("check: %s %.0f%% of target (%s measured, %s expected)",
			name, float64(g)/float64(e)*100, humanSize(g), humanSize(e))
		if g < 6*e/10 {
			ok = false
		}
	}
	chk("net down", &statNetDown, &expNetDown)
	chk("net up", &statNetUp, &expNetUp)
	chk("disk read", &statDiskRead, &expDiskRead)
	chk("disk write", &statDiskWrite, &expDiskWrite)
	chk("mem bw", &statMemBW, &expMemBW)
	if ok {
		return 0
	}
	return 1
}

// cpuPlan works out how many workers to run and how hard each one spins.
//
// Two ways to ask for load:
//
//	-cpu 25              25% of the whole machine, spread over every core.
//	                     Means the same thing regardless of core count.
//	-cores 2 -per-core 50   exactly 2 cores at 50% each, whatever the machine has.
//
// The first is what you want when the target is a machine-wide figure; the
// second when you want a specific number of busy cores. -per-core wins if both
// are given.
//
// Utilisation is averaged across cores, which is why -cpu has to be spread: one
// core at 25% on a 4-core box is 6.25% machine-wide, a quarter of what was asked.
func cpuPlan(pct, cores, perCore int) (workers, duty int, desc string) {
	ncpu := runtime.NumCPU()
	if perCore > 0 {
		workers = cores
		if workers <= 0 || workers > ncpu {
			workers = ncpu
		}
		return workers, perCore, fmt.Sprintf("%d of %d cores at %d%% each (%d%% of the machine)",
			workers, ncpu, perCore, perCore*workers/ncpu)
	}
	workers = cores
	if workers <= 0 || workers > ncpu {
		// Capped at the core count, as the docs promise. Without the upper clamp
		// `-cpu 5 -cores 200` on an 8-core box would spread 5% over 200 workers,
		// integer-divide each down to 0%, and run no load at all.
		workers = ncpu
	}
	// Spreading pct over fewer cores means each works proportionally harder.
	each := pct * ncpu / workers
	if each > 100 {
		// Fewer cores than the target needs. Say so rather than report a figure
		// that cannot be reached: N cores flat out is only N/ncpu of the machine.
		return workers, 100, fmt.Sprintf(
			"%d%% of the machine asked for, but %d of %d cores can only reach %d%% - running them flat out",
			pct, workers, ncpu, 100*workers/ncpu)
	}
	return workers, each, fmt.Sprintf("%d%% of the machine, over %d of %d cores at %d%% each",
		pct, workers, ncpu, each)
}

// cpuShape is the CPU plan evaluated over time. Every worker asks it how hard to
// spin right now, so -duty, -ramp, -schedule and runtime control share one
// mechanism instead of starting and stopping workers as the plan changes.
type cpuShape struct {
	ncpu, workers int
	ramp          time.Duration
	start         time.Time

	mu       sync.Mutex
	schedule []int
}

func (s *cpuShape) setSchedule(v []int) {
	s.mu.Lock()
	s.schedule = v
	s.mu.Unlock()
}

func (s *cpuShape) currentSchedule() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.schedule
}

// activeCPU is the running shape, so an explicit CPU target (from /control or a
// -profile step) can cancel a schedule that would otherwise ignore it.
var activeCPU *cpuShape

func clearSchedule() {
	if activeCPU != nil {
		activeCPU.setSchedule(nil)
	}
}

// level reports the instantaneous target as a machine-wide percentage.
func (s *cpuShape) level() int {
	target := int(knobs.cpuPct.Load())
	schedule := s.currentSchedule()
	el := time.Since(s.start)
	duty := int(knobs.duty.Load())
	cycle := time.Duration(knobs.cycleNs.Load())
	switch {
	case len(schedule) > 0 && cycle > 0:
		slot := cycle / time.Duration(len(schedule))
		if slot <= 0 {
			return schedule[0]
		}
		return schedule[int((el%cycle)/slot)]
	case s.ramp > 0 && el < s.ramp:
		return target * int(el) / int(s.ramp)
	case duty < 100 && cycle > 0:
		burn := cycle * time.Duration(duty) / 100
		if burn <= 0 {
			burn = time.Millisecond
		}
		if el%cycle >= burn {
			return 0
		}
	}
	return target
}

// workerPct turns a machine-wide level into what a single worker should spin at.
func (s *cpuShape) workerPct() int {
	p := s.level() * s.ncpu / s.workers
	if p > 100 {
		p = 100
	}
	if p < 0 {
		p = 0
	}
	return p
}

// startCPU builds the plan and launches one worker per core it wants to load.
// The workers run for the life of the process; the shape and the knobs decide
// how hard they spin, so runtime control needs nothing respawned.
func startCPU(pct, cores, perCore, duty int, cycle, ramp time.Duration, schedule []int) {
	workers, _, desc := cpuPlan(pct, cores, perCore)
	target := pct
	if perCore > 0 {
		// Keep control uniform by working in machine-wide percent throughout.
		target = perCore * workers / runtime.NumCPU()
	}
	knobs.cpuPct.Store(int64(target))
	knobs.duty.Store(int64(duty))
	knobs.cycleNs.Store(int64(cycle))

	s := &cpuShape{ncpu: runtime.NumCPU(), workers: workers, ramp: ramp, schedule: schedule, start: time.Now()}
	activeCPU = s
	suffix := "continuously"
	switch {
	case len(schedule) > 0:
		suffix = fmt.Sprintf("cycling through %s%% of the machine every %s", joinInts(schedule), cycle)
	case ramp > 0:
		suffix = "ramping up over " + ramp.String()
	case duty < 100:
		suffix = fmt.Sprintf("%s busy every %s (duty %d%%)", (cycle * time.Duration(duty) / 100).Round(time.Second), cycle, duty)
	}
	logf("cpu: %s, %s", desc, suffix)
	for i := 0; i < workers; i++ {
		go burnCPU(s)
	}
}

// burnCPU spins for pct% of each 100 ms period and sleeps for the rest, taking
// pct from the shape each pass. LockOSThread pins the goroutine to one OS thread
// so the scheduler cannot move it around and skew the ratio.
func burnCPU(s *cpuShape) {
	const period = 100 * time.Millisecond
	runtime.LockOSThread()
	for {
		busy := period * time.Duration(s.workerPct()) / 100
		begin := time.Now()
		for time.Since(begin) < busy {
		}
		time.Sleep(period - busy)
	}
}

// parseSchedule reads "80,20,0" into levels.
func parseSchedule(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v < 0 || v > 100 {
			return nil, fmt.Errorf("entries must be 0-100, got %q", strings.TrimSpace(p))
		}
		out = append(out, v)
	}
	return out, nil
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// eatMem allocates n bytes and writes to every block.
//
// The write is not optional: an untouched make() only reserves address space,
// so the kernel never backs it with physical pages and the allocation is
// invisible in /proc/meminfo. Allocating in chunks is what allows an arbitrary
// byte count rather than whole-GiB steps.
func eatMem(n int64) {
	const chunk = 64 * miB
	for n > 0 {
		size := int64(chunk)
		if n < size {
			size = n
		}
		b := make([]byte, size)
		rand.Read(b) // random data also defeats hypervisor page dedup
		hold = append(hold, b)
		heldBytes.Add(size)
		n -= size
	}
}

// memBuilder allocates the bandwidth buffer lazily, the first time -mem-bw (or a
// /control or -profile step) asks for a non-zero rate. The allocation is clamped
// through the same budget as -mem, so it cannot walk a small cgroup into an OOM.
type memBuilder struct {
	size int64
	once sync.Once
	buf  []byte
}

func (m *memBuilder) buffer() []byte {
	m.once.Do(func() {
		size := memAllowance(m.size, "mem-bw", heldBytes.Load())
		if size <= 0 {
			return
		}
		m.buf = make([]byte, size)
		rand.Read(m.buf) // touch every page so it is resident
		heldBytes.Add(size)
	})
	return m.buf
}

// startMemBW runs a STREAM-like read-modify-write loop over a buffer, paced to
// the memory-bandwidth target. It is distinct from -mem, which holds pages.
// A size of 0 means the budget could not fit it, so the loop stays idle.
func startMemBW(size int64) {
	mb := &memBuilder{size: size}
	lim := newLimiter(1)
	go func() {
		const blk = 64 * kiB
		for {
			if !pulseActive() {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			bps := knobs.memBW.Load()
			if bps <= 0 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			lim.setRate(float64(bps))
			buf := mb.buffer()
			if len(buf) == 0 {
				time.Sleep(time.Second)
				continue
			}
			for off := 0; off+blk <= len(buf); off += blk {
				if !pulseActive() {
					break
				}
				lim.wait(blk)
				seg := buf[off : off+blk]
				for i := range seg {
					seg[i] ^= 0x5a // read and write, so it is bandwidth not a no-op
				}
				statMemBW.Add(blk)
			}
		}
	}()
}

// limiter is a byte-rate token bucket. Reads are paced through it so the rate
// stays near the target continuously, rather than bursting each block out at
// line speed and sleeping off the difference afterwards. Still stdlib only.
type limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64 // tokens held back to absorb one burst
	tokens float64
	last   time.Time
}

func newLimiter(bytesPerSec float64) *limiter {
	b := burstFor(bytesPerSec)
	return &limiter{rate: bytesPerSec, burst: b, tokens: b, last: time.Now()}
}

// setRate changes the target and rescales the burst with it, so a block sized
// for a fast rate is still charged correctly after the rate drops.
func (l *limiter) setRate(bytesPerSec float64) {
	l.mu.Lock()
	l.rate = bytesPerSec
	l.burst = burstFor(bytesPerSec)
	l.mu.Unlock()
}

func burstFor(bytesPerSec float64) float64 {
	b := bytesPerSec / 5 // 200 ms of headroom
	if b < 64*kiB {
		b = 64 * kiB
	}
	if b > 4*miB {
		b = 4 * miB
	}
	return b
}

// wait blocks until n bytes may go out. Tokens are capped at one burst so idle
// time cannot bank an unbounded transfer, but a single read larger than the
// burst is still charged in full: the cap lifts to the request size so it waits
// its true duration instead of either racing through or blocking forever.
func (l *limiter) wait(n int) {
	want := float64(n)
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens = math.Min(math.Max(l.burst, want), l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
		if l.tokens >= want {
			l.tokens -= want
			l.mu.Unlock()
			return
		}
		need := (want - l.tokens) / l.rate
		l.mu.Unlock()
		time.Sleep(time.Duration(need * float64(time.Second)))
	}
}

// countingReader adds every byte read to a counter, so throughput shows up while
// a long transfer is still in flight rather than only when it completes.
type countingReader struct {
	r   io.Reader
	ctr *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.ctr.Add(int64(n))
	}
	return n, err
}

// throttled paces a reader through a limiter: every read is charged before the
// next one is served, which back-pressures the socket via TCP. A nil limiter
// passes through untouched.
type throttled struct {
	r io.Reader
	l *limiter
}

func (t *throttled) Read(p []byte) (int, error) {
	// Honour -duty in the middle of a long transfer: a streaming download never
	// returns to the worker loop, so the pause has to live in the read path.
	for !pulseActive() {
		time.Sleep(20 * time.Millisecond)
	}
	n, err := t.r.Read(p)
	if n > 0 && t.l != nil {
		t.l.wait(n)
	}
	return n, err
}

type xferFunc func(client *http.Client, url string, lim *limiter, ctr *atomic.Int64) error

// pushNet drives traffic in one direction, reading the target from knob each
// pass so it can be changed at runtime. conns workers run in parallel and share
// one limiter, so the target is the total across them.
func pushNet(dir string, knob *atomic.Int64, rps, conns int, url string, ctr *atomic.Int64, churn bool) {
	if conns < 1 {
		conns = 1
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if churn {
		transport.DisableKeepAlives = true
	}
	client := &http.Client{Timeout: 5 * time.Minute, Transport: transport}
	lim := newLimiter(1)
	var gap time.Duration
	if rps > 0 {
		gap = time.Duration(conns) * time.Second / time.Duration(rps)
	}
	xfer := download
	if dir == "up" {
		xfer = upload
	}
	for i := 0; i < conns; i++ {
		go func() {
			for {
				if !pulseActive() {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				bps := knob.Load()
				if bps <= 0 {
					time.Sleep(200 * time.Millisecond)
					continue
				}
				lim.setRate(float64(bps))
				if err := xfer(client, url, lim, ctr); err != nil {
					logf("net %s: %v (retrying in 30s)", dir, err)
					time.Sleep(30 * time.Second)
					continue
				}
				if gap > 0 {
					time.Sleep(gap)
				}
			}
		}()
	}
	go rateReport("net "+dir, "Mbps", ctr, knob, 8)
}

// rateReport logs achieved throughput every five minutes. scale is 8 for bits
// (bytes to megabits) and 1 for bytes (already MB/s).
func rateReport(label, unit string, ctr, knob *atomic.Int64, scale float64) {
	last := time.Now()
	prev := int64(0)
	for range time.NewTicker(5 * time.Minute).C {
		cur := ctr.Load()
		el := time.Since(last)
		logf("%s: %s over the last %s, about %.1f %s (target %d)", label,
			humanSize(cur-prev), el.Round(time.Second),
			float64(cur-prev)*scale/el.Seconds()/1e6, unit,
			int64(float64(knob.Load())*scale/1e6))
		prev, last = cur, time.Now()
	}
}

// download fetches once and returns an error if it did not succeed. The status
// check matters: an error page would otherwise count as a successful transfer
// and the dimension would quietly do nothing (the default endpoint answers 403
// with a 1-byte body above 10 MB).
func download(client *http.Client, url string, lim *limiter, ctr *atomic.Int64) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "staybusy/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download source answered HTTP %d, try another -net-down-url", resp.StatusCode)
	}
	_, err = io.Copy(io.Discard, &throttled{r: &countingReader{r: resp.Body, ctr: ctr}, l: lim})
	return err
}

// upBlock is posted over and over. Allocated on first use rather than at
// startup, so a run without -net-up does not carry 8 MiB it will never touch.
// Filled with random data: an all-zero body would compress away on the wire and
// the measured rate would be a fiction.
var (
	upBlock []byte
	upOnce  sync.Once
)

// upload posts one block, counting bytes as they are read off the buffer.
func upload(client *http.Client, url string, lim *limiter, ctr *atomic.Int64) error {
	upOnce.Do(func() {
		upBlock = make([]byte, 8*miB)
		rand.Read(upBlock)
	})
	body := &throttled{r: &countingReader{r: bytes.NewReader(upBlock), ctr: ctr}, l: lim}
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("User-Agent", "staybusy/1.0")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(upBlock))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload target answered HTTP %d, try another -net-up-url", resp.StatusCode)
	}
	return nil
}

// diskDir is one disk direction. The file is opened lazily, the first time the
// target goes non-zero, so an idle instance never creates it. All file state is
// behind mu: with -disk-conns > 1 several workers share the one handle, and a
// direct-IO failure has to swap the handle for a plain one without racing.
type diskDir struct {
	name    string
	path    string
	size    int64
	block   int64
	pattern string
	fsync   bool
	write   bool
	knob    *atomic.Int64
	ctr     *atomic.Int64
	lim     *limiter

	mu     sync.Mutex
	direct bool
	opened bool
	f      *os.File
}

// file returns the shared handle, opening it on first use. The bool reports
// whether it was opened with O_DIRECT, so the caller knows if a failure is worth
// retrying without it.
func (d *diskDir) file() (*os.File, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.opened {
		return d.f, d.direct
	}
	d.opened = true
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		logf("disk %s: %v", d.name, err)
		return nil, d.direct
	}
	var f *os.File
	var err error
	if d.write {
		if f, err = openDisk(d.path, os.O_RDWR|os.O_CREATE, d.direct); err != nil {
			logf("disk %s: open: %v", d.name, err)
			return nil, d.direct
		}
		if err := f.Truncate(d.size); err != nil {
			logf("disk %s: truncate: %v", d.name, err)
		}
	} else {
		if err = ensureDiskFile(d.path, d.size, d.block); err != nil {
			logf("disk %s: %v", d.name, err)
			return nil, d.direct
		}
		if f, err = openDisk(d.path, os.O_RDONLY, d.direct); err != nil {
			logf("disk %s: open: %v", d.name, err)
			return nil, d.direct
		}
	}
	logf("disk %s: about %s/s over a %s file at %s", d.name,
		humanSize(int64(d.knob.Load())), humanSize(d.size), d.path)
	d.f = f
	diskFiles = append(diskFiles, f)
	return d.f, d.direct
}

// dropDirect closes the shared handle and reopens without O_DIRECT next time.
// It is idempotent, so several workers hitting the same failure is fine.
func (d *diskDir) dropDirect() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.direct {
		return
	}
	d.direct = false
	if d.f != nil {
		d.f.Close()
	}
	d.f, d.opened = nil, false
}

func (d *diskDir) run(seed int64) {
	buf := make([]byte, d.block)
	if d.write {
		rand.Read(buf)
	}
	off := seed
	ops := int64(0)
	for {
		if !pulseActive() {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		bps := d.knob.Load()
		if bps <= 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		f, wasDirect := d.file()
		if f == nil {
			time.Sleep(time.Second)
			continue
		}
		d.lim.setRate(float64(bps))
		if d.pattern == "rand" {
			off = rand.Int63n(d.size/d.block) * d.block
		} else if off+d.block > d.size {
			off = 0
		}
		d.lim.wait(int(d.block))
		var err error
		if d.write {
			_, err = f.WriteAt(buf, off)
		} else {
			_, err = f.ReadAt(buf, off)
		}
		if err != nil && err != io.EOF {
			if wasDirect {
				logf("disk %s: %v - retrying without O_DIRECT", d.name, err)
				d.dropDirect()
				continue
			}
			logf("disk %s: %v (retrying in 30s)", d.name, err)
			time.Sleep(30 * time.Second)
			continue
		}
		d.ctr.Add(d.block)
		off += d.block
		if ops++; d.fsync && ops%(d.size/d.block) == 0 && d.write {
			f.Sync()
		}
	}
}

// startDisk prepares the disk dimension. Files are created lazily by each
// direction, so an instance that never asks for disk I/O never touches the disk.
func startDisk(p *diskParams) {
	if p.size <= 0 {
		logf("disk: -disk-size must be positive, skipping")
		return
	}
	if p.block <= 0 {
		p.block = miB
	}
	if p.pattern != "rand" {
		p.pattern = "seq"
	}
	if p.direct && runtime.GOOS != "linux" {
		logf("disk: -disk-direct is Linux-only, ignoring it on %s", runtime.GOOS)
		p.direct = false
	}
	if p.block > p.size {
		p.block = p.size
	}
	if rem := p.size % p.block; rem != 0 {
		p.size += p.block - rem // keep offsets block-aligned for O_DIRECT
	}
	for _, dir := range []*diskDir{&p.read, &p.write} {
		if dir == nil {
			continue
		}
		d := dir
		d.size, d.block, d.pattern, d.fsync, d.direct = p.size, p.block, p.pattern, p.fsync, p.direct
		if d.name == "" {
			continue
		}
		for i := 0; i < p.conns; i++ {
			go d.run(int64(i) * d.block)
		}
		go rateReport("disk "+d.name, "MB/s", d.ctr, d.knob, 1)
	}
}

type diskParams struct {
	read, write diskDir
	size        int64
	block       int64
	pattern     string
	conns       int
	fsync       bool
	direct      bool
}

// openDisk opens the disk file, adding O_DIRECT on Linux when asked. If the
// direct open fails (a filesystem that does not support it) it retries plainly.
func openDisk(path string, flags int, direct bool) (*os.File, error) {
	f, err := os.OpenFile(path, flags|directFlag(direct), 0o644)
	if err != nil && direct {
		logf("disk: O_DIRECT open failed (%v), falling back", err)
		return os.OpenFile(path, flags, 0o644)
	}
	return f, err
}

func directFlag(direct bool) int {
	if direct && runtime.GOOS == "linux" {
		return linuxO_DIRECT
	}
	return 0
}

// ensureDiskFile makes sure the read file exists at the right size with real
// data on disk, so reads are not served from untouched sparse holes.
func ensureDiskFile(path string, size, block int64) error {
	if fi, err := os.Stat(path); err == nil && fi.Size() == size {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, block)
	rand.Read(buf)
	for off := int64(0); off < size; off += block {
		if _, err := f.Write(buf); err != nil {
			return err
		}
	}
	return f.Sync()
}

// startProcs forks inert copies of ourselves to occupy process slots.
func startProcs(n int) {
	exe, err := os.Executable()
	if err != nil {
		logf("procs: %v", err)
		return
	}
	for i := 0; i < n; i++ {
		cmd := exec.Command(exe, childToken)
		cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"} // stay inert
		if err := cmd.Start(); err != nil {
			logf("procs: started %d of %d: %v", i, n, err)
			break
		}
		childProcs = append(childProcs, cmd.Process)
	}
	logf("procs: spawned %d child processes", len(childProcs))
}

func stopProcs() {
	for _, p := range childProcs {
		p.Kill()
	}
	childProcs = nil
}

// startThreads holds n OS threads, one locked goroutine each, to occupy thread
// slots without burning CPU.
func startThreads(n int) {
	for i := 0; i < n; i++ {
		go func() {
			runtime.LockOSThread()
			for {
				time.Sleep(time.Millisecond)
			}
		}()
	}
	logf("threads: holding %d OS threads", n)
}

// startFDs opens and holds n file descriptors; it stops early if the limit is
// reached, which is itself the result worth seeing.
func startFDs(n int) {
	for i := 0; i < n; i++ {
		f, err := os.Open(os.DevNull)
		if err != nil {
			logf("fds: opened %d of %d: %v", i, n, err)
			return
		}
		fdHold = append(fdHold, f)
	}
	logf("fds: holding %d open file descriptors", len(fdHold))
}

// startServe runs the built-in HTTP endpoint: a load target and fault injector
// for other instances, a control plane, health, Prometheus metrics and pprof.
func startServe(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "staybusy endpoint\n\n"+
			"GET  /down[?bytes=N]   random bytes (unbounded without ?bytes)\n"+
			"POST /up               discard the request body\n"+
			"GET  /slow?ms=N        respond after N ms\n"+
			"GET  /status?code=N    respond with that HTTP status\n"+
			"GET  /flaky?p=0.1      fail with probability p (code=503)\n"+
			"GET  /hang             never respond (test client timeouts)\n"+
			"GET  /control          current load targets (POST to change)\n"+
			"GET  /healthz          liveness probe\n"+
			"GET  /metrics          Prometheus metrics\n"+
			"GET  /debug/pprof/     Go profiles\n")
	})
	mux.HandleFunc("/down", serveDown)
	mux.HandleFunc("/up", serveUp)
	mux.HandleFunc("/slow", serveSlow)
	mux.HandleFunc("/status", serveStatus)
	mux.HandleFunc("/flaky", serveFlaky)
	mux.HandleFunc("/hang", serveHang)
	mux.HandleFunc("/control", serveControl)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/metrics", serveMetrics)
	servePprof(mux)
	// WriteTimeout is deliberately unset: /down and /hang are long-lived by
	// design. Header and idle timeouts still bound slow-client abuse.
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			logf("serve: %v", err)
		}
	}()
	logf("serve: listening on %s (no auth: bind it to a trusted address)", addr)
}

// servePprof mounts the standard Go profilers on our own mux, so nothing is
// exposed unless -serve is set.
func servePprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

func serveDown(w http.ResponseWriter, r *http.Request) {
	limit := int64(-1)
	if q := r.URL.Query().Get("bytes"); q != "" {
		if v, err := strconv.ParseInt(q, 10, 64); err == nil && v >= 0 {
			limit = v
		}
	}
	buf := make([]byte, miB)
	rand.Read(buf)
	if limit >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(limit, 10))
	}
	var sent int64
	for {
		n := int64(len(buf))
		if limit >= 0 && sent+n > limit {
			n = limit - sent
		}
		if n <= 0 {
			return
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return
		}
		sent += n
	}
}

func serveUp(w http.ResponseWriter, r *http.Request) {
	n, _ := io.Copy(io.Discard, r.Body)
	statServeUp.Add(n)
	w.WriteHeader(http.StatusOK)
}

func serveSlow(w http.ResponseWriter, r *http.Request) {
	time.Sleep(time.Duration(intQuery(r, "ms", 1000)) * time.Millisecond)
	io.WriteString(w, "ok\n")
}

func serveStatus(w http.ResponseWriter, r *http.Request) {
	code := statusFromQuery(r, http.StatusInternalServerError)
	w.WriteHeader(code)
	fmt.Fprintf(w, "status %d\n", code)
}

// statusFromQuery reads a valid HTTP status from ?code=, clamping to 100-599.
// WriteHeader panics below 100, and the standard library would tear the
// connection down; a bad code is answered as a plain 400 instead.
func statusFromQuery(r *http.Request, def int) int {
	code := intQuery(r, "code", def)
	if code < 100 || code > 599 {
		return http.StatusBadRequest
	}
	return code
}

func serveFlaky(w http.ResponseWriter, r *http.Request) {
	p := 0.1
	if v := r.URL.Query().Get("p"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			p = f
		}
	}
	if rand.Float64() < p {
		code := statusFromQuery(r, http.StatusServiceUnavailable)
		w.WriteHeader(code)
		fmt.Fprintf(w, "injected %d\n", code)
		return
	}
	io.WriteString(w, "ok\n")
}

func serveHang(w http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
}

// serveControl applies the query string to the live knobs (POST) or reports them
// (GET). Rates are in the same units as the flags: Mbps for net, MB/s for disk
// and memory bandwidth, percent for cpu and duty.
func serveControl(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if r.Method != http.MethodPost && r.Method != http.MethodPut || len(q) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(knobState())
		return
	}
	// Parse and validate everything first, then commit: a bad field must not
	// leave the earlier ones applied.
	type edit struct {
		k   *atomic.Int64
		val int64
	}
	var pending []edit
	add := func(name string, k *atomic.Int64, mult, min, max int64) bool {
		v := q.Get(name)
		if v == "" {
			return true
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < min || n > max {
			http.Error(w, fmt.Sprintf("%s must be an integer between %d and %d", name, min, max), http.StatusBadRequest)
			return false
		}
		if name == "cpu" {
			// An explicit CPU target overrides a running -schedule.
			clearSchedule()
		}
		pending = append(pending, edit{k, n * mult})
		return true
	}
	const big = 1_000_000
	if !add("cpu", &knobs.cpuPct, 1, 0, 100) ||
		!add("duty", &knobs.duty, 1, 1, 100) ||
		!add("net-down", &knobs.netDown, 1e6/8, 0, big) ||
		!add("net-up", &knobs.netUp, 1e6/8, 0, big) ||
		!add("disk-read", &knobs.diskRead, 1e6, 0, big) ||
		!add("disk-write", &knobs.diskWrite, 1e6, 0, big) ||
		!add("mem-bw", &knobs.memBW, 1e6, 0, big) {
		return
	}
	var cycleNs int64 = -1
	if v := q.Get("cycle"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			http.Error(w, "cycle must be a positive duration, e.g. 30s", http.StatusBadRequest)
			return
		}
		cycleNs = int64(d)
	}
	for _, e := range pending {
		e.k.Store(e.val)
	}
	if cycleNs >= 0 {
		knobs.cycleNs.Store(cycleNs)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(knobState())
}

func knobState() map[string]any {
	return map[string]any{
		"cpu_percent":    knobs.cpuPct.Load(),
		"net_down_mbps":  float64(knobs.netDown.Load()) * 8 / 1e6,
		"net_up_mbps":    float64(knobs.netUp.Load()) * 8 / 1e6,
		"disk_read_mbs":  float64(knobs.diskRead.Load()) / 1e6,
		"disk_write_mbs": float64(knobs.diskWrite.Load()) / 1e6,
		"mem_bw_mbs":     float64(knobs.memBW.Load()) / 1e6,
		"duty":           knobs.duty.Load(),
		"cycle":          time.Duration(knobs.cycleNs.Load()).String(),
	}
}

func serveMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE staybusy_info gauge\nstaybusy_info{version=%q} 1\n", version)
	metric := func(name string, v int64) { fmt.Fprintf(w, "staybusy_%s %d\n", name, v) }
	metric("uptime_seconds", int64(time.Since(startTime).Seconds()))
	metric("cpu_target_percent", knobs.cpuPct.Load())
	metric("mem_target_bytes", tMem)
	metric("net_down_target_bytes_per_second", knobs.netDown.Load())
	metric("net_up_target_bytes_per_second", knobs.netUp.Load())
	metric("disk_read_target_bytes_per_second", knobs.diskRead.Load())
	metric("disk_write_target_bytes_per_second", knobs.diskWrite.Load())
	metric("mem_bw_target_bytes_per_second", knobs.memBW.Load())
	metric("duty_percent", knobs.duty.Load())
	metric("net_down_bytes_total", statNetDown.Load())
	metric("net_up_bytes_total", statNetUp.Load())
	metric("disk_read_bytes_total", statDiskRead.Load())
	metric("disk_write_bytes_total", statDiskWrite.Load())
	metric("mem_bw_bytes_total", statMemBW.Load())
	metric("serve_up_bytes_total", statServeUp.Load())
}

func intQuery(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// profileStep is one entry of a -profile file. Absent fields are left unchanged.
type profileStep struct {
	Duration  *string `json:"duration"`
	CPU       *int    `json:"cpu"`
	NetDown   *int    `json:"net-down"`
	NetUp     *int    `json:"net-up"`
	DiskRead  *int    `json:"disk-read"`
	DiskWrite *int    `json:"disk-write"`
	MemBW     *int    `json:"mem-bw"`
	Duty      *int    `json:"duty"`
	Cycle     *string `json:"cycle"`
}

func loadProfile(path string) ([]profileStep, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var steps []profileStep
	if err := json.Unmarshal(b, &steps); err != nil {
		return nil, err
	}
	return steps, nil
}

func applyStep(s profileStep) {
	set := func(p *int, k *atomic.Int64, mult int64) {
		if p != nil {
			k.Store(int64(*p) * mult)
		}
	}
	if s.CPU != nil {
		clearSchedule() // an explicit CPU target overrides a running -schedule
	}
	set(s.CPU, &knobs.cpuPct, 1)
	set(s.NetDown, &knobs.netDown, 1e6/8)
	set(s.NetUp, &knobs.netUp, 1e6/8)
	set(s.DiskRead, &knobs.diskRead, 1e6)
	set(s.DiskWrite, &knobs.diskWrite, 1e6)
	set(s.MemBW, &knobs.memBW, 1e6)
	set(s.Duty, &knobs.duty, 1)
	if s.Cycle != nil {
		if d, err := time.ParseDuration(*s.Cycle); err == nil {
			knobs.cycleNs.Store(int64(d))
		}
	}
}

func runProfile(steps []profileStep) {
	go func() {
		for i, st := range steps {
			applyStep(st)
			logf("profile: step %d/%d cpu=%d net-down=%d net-up=%d", i+1, len(steps),
				knobs.cpuPct.Load(), int64(float64(knobs.netDown.Load())*8/1e6), int64(float64(knobs.netUp.Load())*8/1e6))
			if st.Duration == nil {
				return
			}
			d, err := time.ParseDuration(*st.Duration)
			if err != nil {
				logf("profile: bad duration %q", *st.Duration)
				return
			}
			time.Sleep(d)
		}
	}()
}

// writeReport dumps the achieved versus expected totals as JSON.
func writeReport(path string) {
	el := time.Since(startTime).Seconds()
	if el <= 0 {
		el = 1
	}
	type dim struct {
		TargetBytesPerSecond int64   `json:"target_bytes_per_second"`
		ExpectedBytes        int64   `json:"expected_bytes_total"`
		Bytes                int64   `json:"bytes_total"`
		MeasuredBytesPerSec  float64 `json:"measured_bytes_per_second"`
	}
	report := func(k, ctr, exp *atomic.Int64) dim {
		return dim{k.Load(), exp.Load(), ctr.Load(), float64(ctr.Load()) / el}
	}
	r := map[string]any{
		"version":            version,
		"uptime_seconds":     el,
		"cpu_target_percent": knobs.cpuPct.Load(),
		"mem_target_bytes":   tMem,
		"net_down":           report(&knobs.netDown, &statNetDown, &expNetDown),
		"net_up":             report(&knobs.netUp, &statNetUp, &expNetUp),
		"disk_read":          report(&knobs.diskRead, &statDiskRead, &expDiskRead),
		"disk_write":         report(&knobs.diskWrite, &statDiskWrite, &expDiskWrite),
		"mem_bw":             report(&knobs.memBW, &statMemBW, &expMemBW),
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		logf("report: %v", err)
		return
	}
	logf("report: wrote %s", path)
}

// parseSize reads "512M" / "1.5G" / "1024K". A bare number means GiB.
func parseSize(s string) (int64, error) {
	s = strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(s)), "B") // allow MB / GB
	mult := int64(giB)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult, s = kiB, s[:n-1]
		case 'M':
			mult, s = miB, s[:n-1]
		case 'G':
			mult, s = giB, s[:n-1]
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return int64(f * float64(mult)), nil
}

func humanSize(n int64) string {
	switch {
	case n >= giB:
		return fmt.Sprintf("%.2f GiB", float64(n)/giB)
	case n >= miB:
		return fmt.Sprintf("%.0f MiB", float64(n)/miB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// availBytes reports how much can still be allocated safely: the lesser of what
// the host has free and what this cgroup has left. On macOS it is the free,
// speculative and file-backed page pools; elsewhere the check is skipped (0).
func availBytes() int64 {
	switch runtime.GOOS {
	case "linux":
		host := hostAvail()
		cg := cgroupAvail()
		if cg > 0 && (host == 0 || cg < host) {
			return cg
		}
		return host
	case "darwin":
		return darwinAvail()
	}
	return 0
}

func darwinAvail() int64 {
	free, speculative, external, page, ok := darwinVM()
	if !ok {
		return 0
	}
	// free + speculative + external. External pages are file-backed and clean,
	// so the kernel evicts them to satisfy an allocation; free alone is tiny on
	// a healthy Mac and would refuse almost every request. macOS exposes no
	// inactive count via sysctl, so this is the usable proxy.
	return (free + speculative + external) * page
}

// darwinVM reads the page counts and the page size. There is no cgo and no
// stdlib binding for host_statistics on macOS, so this shells out to sysctl; it
// never runs on the Linux image.
func darwinVM() (free, speculative, external, pagesize int64, ok bool) {
	out, err := exec.Command("sysctl", "-n",
		"vm.page_free_count", "vm.page_speculative_count", "vm.page_pageable_external_count", "hw.pagesize").Output()
	if err != nil {
		return 0, 0, 0, 0, false
	}
	f := strings.Fields(string(out))
	if len(f) < 4 {
		return 0, 0, 0, 0, false
	}
	free, _ = strconv.ParseInt(f[0], 10, 64)
	speculative, _ = strconv.ParseInt(f[1], 10, 64)
	external, _ = strconv.ParseInt(f[2], 10, 64)
	pagesize, _ = strconv.ParseInt(f[3], 10, 64)
	if pagesize <= 0 {
		return 0, 0, 0, 0, false
	}
	return free, speculative, external, pagesize, true
}

// meminfo reads one field out of /proc/meminfo, in bytes. Returns 0 if absent.
func meminfo(key string) int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == key {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb * kiB
		}
	}
	return 0
}

func hostAvail() int64 {
	return meminfo("MemAvailable:")
}

// cgroupLimit reports this cgroup's memory ceiling, or 0 when unconstrained.
func cgroupLimit() int64 {
	// cgroup v2 reads "max" when unconstrained, so a parse failure is that case.
	if max, ok := readUint("/sys/fs/cgroup/memory.max"); ok {
		return max
	}
	// cgroup v1 spells unconstrained as a value near the int64 ceiling.
	if max, ok := readUint("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok && max < 1<<62 {
		return max
	}
	return 0
}

// memBudget is the total this process may use: the cgroup ceiling if there is
// one, else the host's total. Off Linux/macOS there is nothing to read, so 0.
func memBudget() int64 {
	switch runtime.GOOS {
	case "linux":
		if lim := cgroupLimit(); lim > 0 {
			return lim
		}
		return meminfo("MemTotal:")
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		v, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		return v
	}
	return 0
}

// reserveFromBudget scales the reserve down with the budget: 512 MiB is a sane
// cushion on a host, but inside a 1 GiB container it would swallow half the
// allowance. A quarter of the budget, capped at memReserve.
func reserveFromBudget(budget int64) int64 {
	if budget <= 0 {
		return memReserve
	}
	r := budget / 4
	if r > memReserve {
		r = memReserve
	}
	return r
}

func reserveFor() int64 {
	return reserveFromBudget(memBudget())
}

// memAllowance clamps a requested memory size to what can safely be committed,
// after the reserve and whatever is already held. It logs what it did; a return
// of 0 means the request was refused. Where the platform gives no visibility
// (availBytes returns 0) the request is passed through unclamped.
func memAllowance(requested int64, label string, held int64) int64 {
	avail := availBytes()
	if avail <= 0 {
		logf("%s: cannot read available memory on this platform, holding %s unverified",
			label, humanSize(requested))
		return requested
	}
	src := "host"
	if runtime.GOOS == "linux" {
		if cg := cgroupAvail(); cg > 0 && cg <= avail {
			src, avail = "cgroup", cg
		}
	}
	reserve := reserveFor()
	safe := avail - reserve - held
	switch {
	case safe < minMem:
		logf("%s: only %s available, less than %s once the %s reserve is kept - skipping",
			label, humanSize(avail), humanSize(minMem), humanSize(reserve))
		return 0
	case requested > safe:
		extra := ""
		if held > 0 {
			extra = fmt.Sprintf(" and %s already held", humanSize(held))
		}
		logf("%s: %s requested, holding %s instead (%s has %s available, less a %s reserve%s)",
			label, humanSize(requested), humanSize(safe), src, humanSize(avail), humanSize(reserve), extra)
		return safe
	}
	return requested
}

// cgroupAvail reports the headroom left in this cgroup, or 0 when unconstrained.
//
// Inside a container /proc/meminfo describes the host, not the cgroup limit.
// Consulting only that lets `docker run --memory 64m ... -mem 512M` sail past
// the safety check and get OOM-killed by the kernel (exit 137), with nothing in
// the log but a line claiming it allocated 512 MiB.
func cgroupAvail() int64 {
	max := cgroupLimit()
	if max <= 0 {
		return 0
	}
	cur, _ := readUint("/sys/fs/cgroup/memory.current") // cgroup v2
	if cur == 0 {
		cur, _ = readUint("/sys/fs/cgroup/memory/memory.usage_in_bytes") // v1
	}
	return max - cur
}

func readUint(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// logf writes one timestamped line, or a JSON object with -log-json.
func logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if *flagLogJSON {
		b, _ := json.Marshal(map[string]string{"time": time.Now().Format(time.RFC3339), "msg": msg})
		fmt.Println(string(b))
		return
	}
	fmt.Printf("%s %s\n", time.Now().Format("15:04:05"), msg)
}

// applyConfig fills in unset flags from STAYBUSY_* environment variables, then
// from a -config JSON file. Explicit command-line flags win over both.
func applyConfig() {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	flag.VisitAll(func(f *flag.Flag) {
		if set[f.Name] {
			return
		}
		env := "STAYBUSY_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v, ok := os.LookupEnv(env); ok {
			if err := flag.Set(f.Name, v); err != nil {
				fmt.Printf("%s=%q: %v\n", env, v, hintDuration(f, err))
				os.Exit(2)
			}
			set[f.Name] = true
		}
	})
	if *flagConfig == "" {
		return
	}
	b, err := os.ReadFile(*flagConfig)
	if err != nil {
		fmt.Println("-config:", err)
		os.Exit(2)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		fmt.Println("-config:", err)
		os.Exit(2)
	}
	for k, v := range m {
		if set[k] {
			continue
		}
		if err := flag.Set(k, configValue(v)); err != nil {
			fmt.Printf("-config: %s=%v: %v\n", k, configValue(v), hintDuration(flag.Lookup(k), err))
			os.Exit(2)
		}
	}
}

// hintDuration adds a unit hint when a duration flag was given a bare number.
func hintDuration(f *flag.Flag, err error) error {
	if f == nil {
		return err
	}
	if f.Name == "duration" || f.Name == "cycle" || f.Name == "ramp" {
		return fmt.Errorf("%v (a duration needs a unit, e.g. 30m, 1h)", err)
	}
	return err
}

func configValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

func diskRate(mbs, iops int, block int64) int64 {
	if iops > 0 && block > 0 {
		return int64(iops) * block
	}
	return int64(mbs) * 1e6
}

func cleanup() {
	stopProcs()
	for _, f := range fdHold {
		f.Close()
	}
	// Close disk handles before removing: Windows refuses to unlink an open
	// file, which would leave the temporary behind.
	for _, f := range diskFiles {
		f.Close()
	}
	for _, p := range diskCleanup {
		os.Remove(p)
	}
}

func main() {
	// An inert child for -procs: no flags, no work, just hold a process slot.
	if len(os.Args) > 1 && os.Args[1] == childToken {
		for {
			time.Sleep(time.Hour)
		}
	}

	flag.Parse()
	if *flagVersion {
		fmt.Println("staybusy " + version)
		return
	}
	applyConfig()

	mem, err := parseSize(*flagMem)
	if err != nil {
		fmt.Println("-mem:", err)
		os.Exit(2)
	}
	diskSize, err := parseSize(*flagDiskSize)
	if err != nil {
		fmt.Println("-disk-size:", err)
		os.Exit(2)
	}
	diskBlock, err := parseSize(*flagDiskBlock)
	if err != nil {
		fmt.Println("-disk-block:", err)
		os.Exit(2)
	}
	memBWSize, err := parseSize(*flagMemBWSize)
	if err != nil {
		fmt.Println("-mem-bw-size:", err)
		os.Exit(2)
	}
	schedule, err := parseSchedule(*flagSchedule)
	if err != nil {
		fmt.Println("-schedule:", err)
		os.Exit(2)
	}
	if *flagDiskPattern != "seq" && *flagDiskPattern != "rand" {
		fmt.Println("-disk-pattern must be seq or rand")
		os.Exit(2)
	}
	for _, c := range []struct {
		bad bool
		msg string
	}{
		{*flagCPU < 0 || *flagCPU > 100, "-cpu must be between 0 and 100 (0 disables)"},
		{*flagPerCore < 0 || *flagPerCore > 100, "-per-core must be between 0 and 100 (0 disables)"},
		{*flagCores < 0, "-cores must not be negative"},
		{*flagDuty < 1 || *flagDuty > 100, "-duty must be between 1 and 100"},
		{*flagCycle <= 0, "-cycle must be positive"},
		{*flagRamp < 0, "-ramp must not be negative"},
		{*flagNetDown < 0 || *flagNetUp < 0, "-net-down and -net-up must not be negative"},
		{*flagNetConns < 1, "-net-conns must be at least 1"},
		{*flagNetRPS < 0, "-net-rps must not be negative"},
		{*flagDiskRead < 0 || *flagDiskWrite < 0, "-disk-read and -disk-write must not be negative"},
		{*flagDiskReadIOPS < 0 || *flagDiskWriteIOPS < 0, "-disk-read-iops and -disk-write-iops must not be negative"},
		{*flagDiskConns < 1, "-disk-conns must be at least 1"},
		{*flagMemBW < 0, "-mem-bw must not be negative"},
		{*flagProcs < 0 || *flagThreads < 0 || *flagFds < 0, "-procs, -threads and -fds must not be negative"},
		{*flagDuration < 0, "-duration must not be negative"},
	} {
		if c.bad {
			fmt.Println(c.msg)
			os.Exit(2)
		}
	}

	cpuOn := *flagCPU > 0 || *flagPerCore > 0 || len(schedule) > 0
	netDownOn := *flagNetDown > 0
	netUpOn := *flagNetUp > 0
	diskReadOn := *flagDiskRead > 0 || *flagDiskReadIOPS > 0
	diskWriteOn := *flagDiskWrite > 0 || *flagDiskWriteIOPS > 0
	memBWOn := *flagMemBW > 0
	dynamic := *flagServe != "" || *flagProfile != ""
	procOn := *flagProcs > 0 || *flagThreads > 0 || *flagFds > 0

	if !cpuOn && mem == 0 && !netDownOn && !netUpOn && !diskReadOn && !diskWriteOn &&
		!memBWOn && !procOn && !dynamic {
		fmt.Print("staybusy - put a controlled load on a machine\n\n")
		flag.PrintDefaults()
		return
	}

	var steps []profileStep
	if *flagProfile != "" {
		if steps, err = loadProfile(*flagProfile); err != nil {
			fmt.Println("-profile:", err)
			os.Exit(2)
		}
	}

	// Seed the live knobs from the flags; the generators read these.
	knobs.netDown.Store(int64(*flagNetDown) * 1e6 / 8)
	knobs.netUp.Store(int64(*flagNetUp) * 1e6 / 8)
	knobs.diskRead.Store(diskRate(*flagDiskRead, *flagDiskReadIOPS, diskBlock))
	knobs.diskWrite.Store(diskRate(*flagDiskWrite, *flagDiskWriteIOPS, diskBlock))
	knobs.memBW.Store(int64(*flagMemBW) * 1e6)
	knobs.duty.Store(int64(*flagDuty))
	knobs.cycleNs.Store(int64(*flagCycle))

	go accountLoop()

	if cpuOn || dynamic {
		startCPU(*flagCPU, *flagCores, *flagPerCore, *flagDuty, *flagCycle, *flagRamp, schedule)
	}
	if mem > 0 {
		mem = memAllowance(mem, "mem", 0)
		if mem > 0 {
			logf("mem: holding %s", humanSize(mem))
			go eatMem(mem)
		}
	}
	tMem = mem
	if netDownOn || dynamic {
		if netDownOn {
			logf("net down: about %d Mbps (%.2f TB/day) from %s", *flagNetDown,
				float64(*flagNetDown)*86400/8/1e6, *flagNetDownURL)
		}
		pushNet("down", &knobs.netDown, *flagNetRPS, *flagNetConns, *flagNetDownURL, &statNetDown, *flagNetChurn)
	}
	if netUpOn || dynamic {
		if netUpOn {
			logf("net up: about %d Mbps (%.2f TB/day) to %s", *flagNetUp,
				float64(*flagNetUp)*86400/8/1e6, *flagNetUpURL)
		}
		pushNet("up", &knobs.netUp, *flagNetRPS, *flagNetConns, *flagNetUpURL, &statNetUp, *flagNetChurn)
	}
	if memBWOn || dynamic {
		startMemBW(memBWSize)
	}
	if diskReadOn || diskWriteOn || dynamic {
		p := diskParams{size: diskSize, block: diskBlock, pattern: *flagDiskPattern,
			conns: *flagDiskConns, fsync: *flagDiskFsync, direct: *flagDiskDirect}
		if diskReadOn || dynamic {
			p.read = diskDir{name: "read", path: filepath.Join(tmpDir(), "staybusy-read.dat"),
				knob: &knobs.diskRead, ctr: &statDiskRead, lim: newLimiter(1)}
			diskCleanup = append(diskCleanup, p.read.path)
		}
		if diskWriteOn || dynamic {
			p.write = diskDir{name: "write", path: filepath.Join(tmpDir(), "staybusy-write.dat"),
				knob: &knobs.diskWrite, ctr: &statDiskWrite, lim: newLimiter(1)}
			diskCleanup = append(diskCleanup, p.write.path)
		}
		startDisk(&p)
	}
	if *flagProcs > 0 {
		startProcs(*flagProcs)
	}
	if *flagThreads > 0 {
		startThreads(*flagThreads)
	}
	if *flagFds > 0 {
		startFDs(*flagFds)
	}
	if *flagServe != "" {
		startServe(*flagServe)
	}
	if len(steps) > 0 {
		runProfile(steps)
	}

	// Wait for a signal or the duration. The liveness ticker is what keeps an
	// otherwise idle process off the runtime's deadlock detector: with only -mem
	// set, the allocating goroutine finishes and leaves nothing runnable, which
	// the runtime reports as a deadlock and panics on. A pending timer avoids
	// that, and unlike the old one-shot select it does not end the process.
	var duration <-chan time.Time
	if *flagDuration > 0 {
		duration = time.NewTimer(*flagDuration).C
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	exitCode := 0
	switch waitExit(stop, duration, time.NewTicker(time.Hour).C) {
	case "signal":
		// already logged
	case "duration":
		logf("duration %s reached, exiting", flagDuration.Round(time.Second))
		if *flagCheck {
			exitCode = checkRates()
		}
	}
	if *flagReport != "" {
		writeReport(*flagReport)
	}
	cleanup()
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// waitExit blocks until a signal arrives or the duration fires, ignoring liveness
// ticks. It returns "signal" or "duration". A nil duration channel never fires.
func waitExit(stop <-chan os.Signal, duration, tick <-chan time.Time) string {
	for {
		select {
		case s := <-stop:
			logf("%s received, exiting", s)
			return "signal"
		case <-duration:
			return "duration"
		case <-tick:
			// liveness only; keeps a timer pending so an idle process is not
			// mistaken for a deadlock
		}
	}
}

func tmpDir() string {
	if *flagDiskDir != "" {
		return *flagDiskDir
	}
	return os.TempDir()
}
