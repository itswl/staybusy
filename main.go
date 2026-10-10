// staybusy places a controlled, predictable load on a machine.
//
// It can drive four resources independently:
//
//	CPU     a duty-cycled busy loop, expressed as a percentage of the whole machine
//	memory  anonymous pages, allocated and touched so they are really resident
//	network sustained transfers, up and/or down, throttled to a target rate
//	disk    sustained reads and/or writes, throttled to a target rate
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

	// diskBlock is the I/O granularity for the disk dimension. O_DIRECT wants
	// offsets and lengths that are a multiple of the device block, so this is a
	// whole MiB.
	diskBlock = 1 * miB

	// linuxO_DIRECT is O_DIRECT on linux/amd64 and linux/arm64 (both 040000).
	// Kept as a plain number so the file stays platform-independent; it is only
	// ever used when runtime.GOOS == "linux".
	linuxO_DIRECT = 0x4000
)

var (
	flagCPU        = flag.Int("cpu", 0, "percentage of the whole machine's CPU to use while busy (1-100); 0 disables.\nLoad is spread over every core, so the figure means the same on any core count")
	flagCores      = flag.Int("cores", 0, "how many cores to load; 0 means all of them")
	flagPerCore    = flag.Int("per-core", 0, "percentage of each loaded core (1-100). Takes precedence over -cpu:\nuse it when you want an exact number of cores at an exact load each")
	flagDuty       = flag.Int("duty", 100, "percentage of time spent busy. 100 = continuous; 10 means 6 minutes\nper hour, which raises the upper decile of samples while averaging a tenth of the load")
	flagCycle      = flag.Duration("cycle", time.Hour, "length of one duty cycle (when -duty < 100) or of one -schedule pass")
	flagRamp       = flag.Duration("ramp", 0, "ramp CPU from 0 up to its target over this period at startup; 0 disables")
	flagSchedule   = flag.String("schedule", "", "comma-separated CPU percentages of the machine to cycle through, each\nheld for an equal slice of -cycle (e.g. 80,20,0)")
	flagMem        = flag.String("mem", "0", "memory to hold: 512M / 1.5G / 1024K (a bare number means GiB); 0 disables")
	flagNetDown    = flag.Int("net-down", 0, "sustained download rate in Mbps; 0 disables")
	flagNetUp      = flag.Int("net-up", 0, "sustained upload rate in Mbps; 0 disables. Runs alongside -net-down, so\nboth directions can be driven at once")
	flagNetDownURL = flag.String("net-down-url", "https://speed.cloudflare.com/__down?bytes=10485760", "download source (this endpoint returns 403 above 10 MB)")
	flagNetUpURL   = flag.String("net-up-url", "https://speed.cloudflare.com/__up", "upload target")
	flagNetConns   = flag.Int("net-conns", 1, "concurrent connections per direction")
	flagNetRPS     = flag.Int("net-rps", 0, "requests per second per direction, on top of the byte rate; 0 disables")
	flagDiskRead   = flag.Int("disk-read", 0, "sustained disk read rate in MB/s; 0 disables")
	flagDiskWrite  = flag.Int("disk-write", 0, "sustained disk write rate in MB/s; 0 disables")
	flagDiskSize   = flag.String("disk-size", "512M", "size of the file used for disk load: 512M / 1G / ...; a bare number means GiB")
	flagDiskDir    = flag.String("disk-dir", "", "directory for the disk file (default the OS temp dir)")
	flagDiskFsync  = flag.Bool("disk-fsync", false, "fsync after each written pass so the writes are durable")
	flagDiskDirect = flag.Bool("disk-direct", false, "open the disk file with O_DIRECT on Linux to bypass the page cache (ignored elsewhere)")
	flagServe      = flag.String("serve", "", "listen address for the built-in load endpoint and metrics, e.g. :8080")
	flagDuration   = flag.Duration("duration", 0, "stop after this long; 0 runs until signalled")
	flagCheck      = flag.Bool("check", false, "with -duration, exit non-zero if a measured rate came in under its target")
	flagConfig     = flag.String("config", "", "JSON file of flag values; command line and environment take precedence")
	flagLogJSON    = flag.Bool("log-json", false, "write logs as JSON lines")
	flagVersion    = flag.Bool("version", false, "print the version and exit")
)

var (
	startTime     = time.Now()
	statNetDown   atomic.Int64
	statNetUp     atomic.Int64
	statDiskRead  atomic.Int64
	statDiskWrite atomic.Int64
	statServeUp   atomic.Int64

	// Targets reported by /metrics, set in main once the flags are validated.
	tCPU, tMem, tNetDown, tNetUp, tDiskRead, tDiskWrite int64

	// Files the disk dimension created, removed on a clean exit.
	diskCleanup []string
)

// hold keeps every allocated block reachable so the garbage collector leaves it
// alone. The kernel reclaims it when the process exits.
var hold [][]byte

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
// spin right now, so -duty, -ramp and -schedule share one mechanism instead of
// starting and stopping workers as the plan changes.
type cpuShape struct {
	ncpu, workers int
	target        int // machine percent, or per-core percent when perCore is set
	perCore       bool
	ramp          time.Duration
	schedule      []int
	cycle         time.Duration
	duty          int
	start         time.Time
}

// level reports the instantaneous target: machine-wide normally, per-core when
// perCore is set.
func (s *cpuShape) level() int {
	el := time.Since(s.start)
	switch {
	case len(s.schedule) > 0 && s.cycle > 0:
		slot := s.cycle / time.Duration(len(s.schedule))
		if slot <= 0 {
			return s.schedule[0]
		}
		return s.schedule[int((el%s.cycle)/slot)]
	case s.ramp > 0 && el < s.ramp:
		return s.target * int(el) / int(s.ramp)
	case s.duty < 100:
		burn := s.cycle * time.Duration(s.duty) / 100
		if burn <= 0 {
			burn = time.Millisecond
		}
		if el%s.cycle >= burn {
			return 0
		}
	}
	return s.target
}

// workerPct turns a level into what a single worker should spin at.
func (s *cpuShape) workerPct() int {
	p := s.level()
	if s.perCore {
		if p > 100 {
			p = 100
		}
		return p
	}
	p = p * s.ncpu / s.workers
	if p > 100 {
		p = 100
	}
	return p
}

// startCPU builds the plan and launches one worker per core it wants to load.
// The workers run for the life of the process; the shape they read decides how
// hard they spin, so there is no churn when it changes.
func startCPU(pct, cores, perCore, duty int, cycle, ramp time.Duration, schedule []int) {
	workers, _, desc := cpuPlan(pct, cores, perCore)
	target := pct
	if perCore > 0 {
		target = perCore
	}
	s := &cpuShape{
		ncpu: runtime.NumCPU(), workers: workers, target: target,
		// A schedule names machine-wide percentages, so it never uses per-core scaling.
		perCore: perCore > 0 && len(schedule) == 0,
		ramp:    ramp, schedule: schedule, cycle: cycle, duty: duty, start: time.Now(),
	}
	switch {
	case len(schedule) > 0:
		logf("cpu: cycling through %s%% of the machine every %s, over %d of %d cores",
			joinInts(schedule), cycle, workers, s.ncpu)
	case ramp > 0:
		logf("cpu: %s, ramping up over %s", desc, ramp)
	case duty < 100:
		burn := cycle * time.Duration(duty) / 100
		logf("cpu: %s, %s busy every %s (duty %d%%)", desc, burn.Round(time.Second), cycle, duty)
	default:
		logf("cpu: %s, continuously", desc)
	}
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
		n -= size
	}
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
	burst := bytesPerSec / 5 // 200 ms of headroom
	if burst < 64*kiB {
		burst = 64 * kiB
	}
	if burst > 4*miB {
		burst = 4 * miB
	}
	return &limiter{rate: bytesPerSec, burst: burst, tokens: burst, last: time.Now()}
}

// wait blocks until n bytes may go out. n is charged at most a full burst so a
// caller asking for more than the burst cannot wait forever for tokens that
// will never accumulate.
func (l *limiter) wait(n int) {
	want := math.Min(float64(n), l.burst)
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens = math.Min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
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

// throttled paces a reader through a limiter: every read is charged before the
// next one is served, which back-pressures the socket via TCP. A nil limiter
// passes through untouched.
type throttled struct {
	r io.Reader
	l *limiter
}

func (t *throttled) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && t.l != nil {
		t.l.wait(n)
	}
	return n, err
}

// countingReader adds every byte read to a counter, so throughput shows up while
// a long transfer is still in flight rather than only when it completes. A
// streaming source like the built-in /down endpoint never completes.
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

type xferFunc func(client *http.Client, url string, lim *limiter, ctr *atomic.Int64) error

// pushNet drives traffic in one direction, throttled to mbps by a shared limiter
// and to rps by a per-worker gap. conns workers run in parallel; the byte
// limiter is shared, so mbps is the total across them.
func pushNet(dir string, mbps, rps, conns int, url string, ctr *atomic.Int64, xfer xferFunc) {
	if conns < 1 {
		conns = 1
	}
	var lim *limiter
	if mbps > 0 {
		lim = newLimiter(float64(mbps) * 1e6 / 8)
	}
	var gap time.Duration
	if rps > 0 {
		gap = time.Duration(conns) * time.Second / time.Duration(rps)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	for i := 0; i < conns; i++ {
		go func() {
			for {
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
	go rateReport("net "+dir, "Mbps", ctr, mbps, 8)
}

// rateReport logs achieved throughput every five minutes. scale is 8 for bits
// (to turn bytes into megabits) and 1 for bytes (already MB/s).
func rateReport(label, unit string, ctr *atomic.Int64, target int, scale float64) {
	last := time.Now()
	prev := int64(0)
	for range time.NewTicker(5 * time.Minute).C {
		cur := ctr.Load()
		el := time.Since(last)
		logf("%s: %s over the last %s, about %.1f %s (target %d)", label,
			humanSize(cur-prev), el.Round(time.Second),
			float64(cur-prev)*scale/el.Seconds()/1e6, unit, target)
		prev, last = cur, time.Now()
	}
}

// download fetches once and returns the byte count. The status check matters:
// an error page would otherwise count as a successful transfer and the dimension
// would quietly do nothing (the default endpoint answers 403 with a 1-byte body
// above 10 MB).
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

// startDisk prepares and launches the disk dimension.
func startDisk(readMBs, writeMBs int, size int64, dir string, fsync, direct bool) {
	if size <= 0 {
		logf("disk: -disk-size must be positive, skipping")
		return
	}
	if dir == "" {
		dir = os.TempDir()
	}
	if direct && runtime.GOOS != "linux" {
		logf("disk: -disk-direct is Linux-only, ignoring it on %s", runtime.GOOS)
		direct = false
	}
	block := int64(diskBlock)
	if size < block {
		block = size
	}
	if rem := size % block; rem != 0 {
		size += block - rem // keep offsets block-aligned for O_DIRECT
	}
	if writeMBs > 0 {
		path := filepath.Join(dir, "staybusy-write.dat")
		diskCleanup = append(diskCleanup, path)
		logf("disk write: about %d MB/s to %s (%s file)", writeMBs, path, humanSize(size))
		go diskWrite(path, size, block, writeMBs, fsync, direct)
		go rateReport("disk write", "MB/s", &statDiskWrite, writeMBs, 1)
	}
	if readMBs > 0 {
		path := filepath.Join(dir, "staybusy-read.dat")
		diskCleanup = append(diskCleanup, path)
		if err := ensureDiskFile(path, size, block); err != nil {
			logf("disk: cannot prepare %s: %v", path, err)
			return
		}
		logf("disk read: about %d MB/s from %s (%s file)", readMBs, path, humanSize(size))
		go diskRead(path, size, block, readMBs, direct)
		go rateReport("disk read", "MB/s", &statDiskRead, readMBs, 1)
	}
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

func diskWrite(path string, size, block int64, mbps int, fsync, direct bool) {
	f, err := openDisk(path, os.O_RDWR|os.O_CREATE, direct)
	if err != nil {
		logf("disk write: open: %v", err)
		return
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		logf("disk write: truncate: %v", err)
	}
	buf := make([]byte, block)
	rand.Read(buf)
	lim := newLimiter(float64(mbps) * 1e6)
	off := int64(0)
	for {
		if off >= size {
			if fsync {
				f.Sync()
			}
			off = 0
		}
		lim.wait(int(block))
		if _, err := f.WriteAt(buf, off); err != nil {
			if direct {
				// Likely O_DIRECT rejected the buffer or the filesystem; drop it.
				logf("disk write: %v - retrying without O_DIRECT", err)
				f.Close()
				if f, err = openDisk(path, os.O_RDWR|os.O_CREATE, false); err != nil {
					logf("disk write: open: %v", err)
					return
				}
				direct = false
				continue
			}
			logf("disk write: %v (retrying in 30s)", err)
			time.Sleep(30 * time.Second)
			continue
		}
		statDiskWrite.Add(block)
		off += block
	}
}

func diskRead(path string, size, block int64, mbps int, direct bool) {
	f, err := openDisk(path, os.O_RDONLY, direct)
	if err != nil {
		logf("disk read: open: %v", err)
		return
	}
	defer f.Close()
	buf := make([]byte, block)
	lim := newLimiter(float64(mbps) * 1e6)
	off := int64(0)
	for {
		if off >= size {
			off = 0
		}
		lim.wait(int(block))
		if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
			if direct {
				logf("disk read: %v - retrying without O_DIRECT", err)
				f.Close()
				if f, err = openDisk(path, os.O_RDONLY, false); err != nil {
					logf("disk read: open: %v", err)
					return
				}
				direct = false
				continue
			}
			logf("disk read: %v (retrying in 30s)", err)
			time.Sleep(30 * time.Second)
			continue
		}
		statDiskRead.Add(block)
		off += block
	}
}

// startServe runs the built-in HTTP endpoint: a load target for other staybusy
// instances, plus health and Prometheus metrics.
func startServe(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "staybusy load endpoint\n\n"+
			"GET  /down[?bytes=N]  random bytes (unbounded without ?bytes)\n"+
			"POST /up              discard the request body\n"+
			"GET  /healthz         liveness probe\n"+
			"GET  /metrics         Prometheus metrics\n")
	})
	mux.HandleFunc("/down", serveDown)
	mux.HandleFunc("/up", serveUp)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/metrics", serveMetrics)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			logf("serve: %v", err)
		}
	}()
	logf("serve: listening on %s (/down, /up, /metrics, /healthz)", addr)
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

func serveMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE staybusy_info gauge\nstaybusy_info{version=%q} 1\n", version)
	metric := func(name string, v int64) { fmt.Fprintf(w, "staybusy_%s %d\n", name, v) }
	metric("uptime_seconds", int64(time.Since(startTime).Seconds()))
	metric("cpu_target_percent", tCPU)
	metric("mem_target_bytes", tMem)
	metric("net_down_target_bytes_per_second", tNetDown)
	metric("net_up_target_bytes_per_second", tNetUp)
	metric("disk_read_target_bytes_per_second", tDiskRead)
	metric("disk_write_target_bytes_per_second", tDiskWrite)
	metric("net_down_bytes_total", statNetDown.Load())
	metric("net_up_bytes_total", statNetUp.Load())
	metric("disk_read_bytes_total", statDiskRead.Load())
	metric("disk_write_bytes_total", statDiskWrite.Load())
	metric("serve_up_bytes_total", statServeUp.Load())
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
// the host has free and what this cgroup has left. On macOS it is the free plus
// speculative page pool; elsewhere the check is skipped (0).
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
	free, spec, page, ok := darwinVM()
	if !ok {
		return 0
	}
	return (free + spec) * page
}

// darwinVM reads the free and speculative page counts and the page size. There
// is no cgo and no stdlib binding for host_statistics on macOS, so this shells
// out to sysctl; it never runs on the Linux image.
func darwinVM() (free, speculative, pagesize int64, ok bool) {
	out, err := exec.Command("sysctl", "-n", "vm.page_free_count", "vm.page_speculative_count", "hw.pagesize").Output()
	if err != nil {
		return 0, 0, 0, false
	}
	f := strings.Fields(string(out))
	if len(f) < 3 {
		return 0, 0, 0, false
	}
	free, _ = strconv.ParseInt(f[0], 10, 64)
	speculative, _ = strconv.ParseInt(f[1], 10, 64)
	pagesize, _ = strconv.ParseInt(f[2], 10, 64)
	if pagesize <= 0 {
		return 0, 0, 0, false
	}
	return free, speculative, pagesize, true
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
				fmt.Printf("%s: %v\n", env, err)
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
			fmt.Printf("-config: %s: %v\n", k, err)
			os.Exit(2)
		}
	}
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

// checkRates compares what was actually moved against the targets and returns a
// non-zero exit code if any of them came in well under. The bar is deliberately
// loose: a short run spends its first seconds warming up, so a steady rate is
// below target across the whole window. CPU and memory are not checked, since
// they are held rather than measured.
func checkRates(el time.Duration) int {
	if el <= 0 {
		el = time.Second
	}
	ok := true
	mbps := func(v int64) float64 { return float64(v) * 8 / el.Seconds() / 1e6 }
	mbs := func(v int64) float64 { return float64(v) / el.Seconds() / 1e6 }
	if *flagNetDown > 0 {
		got := mbps(statNetDown.Load())
		logf("check: net down %.1f Mbps measured, target %d", got, *flagNetDown)
		ok = ok && got >= 0.6*float64(*flagNetDown)
	}
	if *flagNetUp > 0 {
		got := mbps(statNetUp.Load())
		logf("check: net up %.1f Mbps measured, target %d", got, *flagNetUp)
		ok = ok && got >= 0.6*float64(*flagNetUp)
	}
	if *flagDiskRead > 0 {
		got := mbs(statDiskRead.Load())
		logf("check: disk read %.1f MB/s measured, target %d", got, *flagDiskRead)
		ok = ok && got >= 0.6*float64(*flagDiskRead)
	}
	if *flagDiskWrite > 0 {
		got := mbs(statDiskWrite.Load())
		logf("check: disk write %.1f MB/s measured, target %d", got, *flagDiskWrite)
		ok = ok && got >= 0.6*float64(*flagDiskWrite)
	}
	if ok {
		return 0
	}
	return 1
}

func cleanup() {
	for _, p := range diskCleanup {
		os.Remove(p)
	}
}

func main() {
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
	schedule, err := parseSchedule(*flagSchedule)
	if err != nil {
		fmt.Println("-schedule:", err)
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
	diskOn := *flagDiskRead > 0 || *flagDiskWrite > 0

	if !cpuOn && mem == 0 && !netDownOn && !netUpOn && !diskOn && *flagServe == "" {
		fmt.Print("staybusy - put a controlled load on a machine\n\n")
		flag.PrintDefaults()
		return
	}

	tCPU = int64(*flagCPU)
	tNetDown = int64(*flagNetDown) * 1e6 / 8
	tNetUp = int64(*flagNetUp) * 1e6 / 8
	tDiskRead = int64(*flagDiskRead) * 1e6
	tDiskWrite = int64(*flagDiskWrite) * 1e6

	if cpuOn {
		startCPU(*flagCPU, *flagCores, *flagPerCore, *flagDuty, *flagCycle, *flagRamp, schedule)
	}
	if mem > 0 {
		// Clamp rather than warn: a warning followed by the allocation anyway
		// is no protection at all.
		reserve := reserveFor()
		if avail := availBytes(); avail > 0 {
			safe := avail - reserve
			switch {
			case safe < minMem:
				logf("mem: only %s available, less than %s once the %s reserve is kept - skipping",
					humanSize(avail), humanSize(minMem), humanSize(reserve))
				mem = 0
			case mem > safe:
				src := "host"
				if cg := cgroupAvail(); cg > 0 && cg <= avail {
					src = "cgroup"
				}
				logf("mem: %s requested, holding %s instead (%s has %s available, less a %s reserve)",
					humanSize(mem), humanSize(safe), src, humanSize(avail), humanSize(reserve))
				mem = safe
			}
		}
		if mem > 0 {
			logf("mem: holding %s", humanSize(mem))
			go eatMem(mem)
		}
	}
	tMem = mem
	if netDownOn {
		logf("net down: about %d Mbps (%.2f TB/day) from %s", *flagNetDown,
			float64(*flagNetDown)*86400/8/1e6, *flagNetDownURL)
		pushNet("down", *flagNetDown, *flagNetRPS, *flagNetConns, *flagNetDownURL, &statNetDown, download)
	}
	if netUpOn {
		logf("net up: about %d Mbps (%.2f TB/day) to %s", *flagNetUp,
			float64(*flagNetUp)*86400/8/1e6, *flagNetUpURL)
		pushNet("up", *flagNetUp, *flagNetRPS, *flagNetConns, *flagNetUpURL, &statNetUp, upload)
	}
	if diskOn {
		startDisk(*flagDiskRead, *flagDiskWrite, diskSize, *flagDiskDir, *flagDiskFsync, *flagDiskDirect)
	}
	if *flagServe != "" {
		startServe(*flagServe)
	}

	// Not a bare `select {}`: with only -mem set, the allocating goroutine
	// finishes and leaves nothing runnable, which the runtime reports as a
	// deadlock and panics on. The pending timer keeps the process off that path,
	// and the same select turns Ctrl-C / SIGTERM into a clean exit - SIGTERM
	// being what `docker stop` sends.
	var duration <-chan time.Time
	if *flagDuration > 0 {
		duration = time.NewTimer(*flagDuration).C
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	exitCode := 0
	select {
	case s := <-stop:
		logf("%s received, exiting", s)
	case <-duration:
		logf("duration %s reached, exiting", flagDuration.Round(time.Second))
		if *flagCheck {
			exitCode = checkRates(time.Since(startTime))
		}
	case <-time.After(24 * time.Hour):
	}
	cleanup()
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}
