// staybusy places a controlled, predictable load on a machine.
//
// It can drive three resources independently:
//
//	CPU     a duty-cycled busy loop, expressed as a percentage of the whole machine
//	memory  anonymous pages, allocated and touched so they are really resident
//	network sustained downloads throttled to a target rate
//
// Useful for exercising autoscaling rules, validating monitoring thresholds and
// alerts, reproducing resource contention, and smoke-testing capacity limits.
//
// Single file, standard library only, builds to a static binary.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	kiB = 1024
	miB = 1024 * kiB
	giB = 1024 * miB

	// memReserve is always left to the system. Allocation is clamped to
	// "available - memReserve"; better to hold less than to wedge the machine.
	memReserve = 512 * miB
	// minMem: below this there is no point allocating, so the dimension is skipped.
	minMem = 64 * miB
)

var (
	flagCPU        = flag.Int("cpu", 0, "percentage of the whole machine's CPU to use while busy (1-100); 0 disables.\nLoad is spread over every core, so the figure means the same on any core count")
	flagCores      = flag.Int("cores", 0, "how many cores to load; 0 means all of them")
	flagPerCore    = flag.Int("per-core", 0, "percentage of each loaded core (1-100). Takes precedence over -cpu:\nuse it when you want an exact number of cores at an exact load each")
	flagDuty       = flag.Int("duty", 100, "percentage of time spent busy. 100 = continuous; 10 means 6 minutes\nper hour, which raises the upper decile of samples while averaging a tenth of the load")
	flagCycle      = flag.Duration("cycle", time.Hour, "length of one duty cycle; applies when -duty < 100")
	flagMem        = flag.String("mem", "0", "memory to hold: 512M / 1.5G / 1024K (a bare number means GiB); 0 disables")
	flagNetDown    = flag.Int("net-down", 0, "sustained download rate in Mbps; 0 disables")
	flagNetUp      = flag.Int("net-up", 0, "sustained upload rate in Mbps; 0 disables. Runs alongside -net-down, so\nboth directions can be driven at once")
	flagNetDownURL = flag.String("net-down-url", "https://speed.cloudflare.com/__down?bytes=10485760", "download source (this endpoint returns 403 above 10 MB)")
	flagNetUpURL   = flag.String("net-up-url", "https://speed.cloudflare.com/__up", "upload target")
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
	if workers <= 0 {
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

// startCPU runs the plan, continuously or pulsed.
//
// With duty < 100 the load is pulsed: busy for duty% of each cycle, idle for the
// rest. Averaged over the cycle the cost is scaled by duty, while samples taken
// during a burst still read the full figure. That distinction matters whenever
// the metric you care about is a high percentile rather than a mean.
func startCPU(pct, cores, perCore, duty int, cycle time.Duration) {
	workers, each, desc := cpuPlan(pct, cores, perCore)
	if duty >= 100 {
		logf("cpu: %s, continuously", desc)
		for i := 0; i < workers; i++ {
			go burnCPU(each, nil)
		}
		return
	}
	burnFor := cycle * time.Duration(duty) / 100
	logf("cpu: %s, %s busy every %s (duty %d%%)", desc, burnFor.Round(time.Second), cycle, duty)
	go func() {
		for {
			stop := make(chan struct{})
			for i := 0; i < workers; i++ {
				go burnCPU(each, stop)
			}
			time.Sleep(burnFor)
			close(stop)
			time.Sleep(cycle - burnFor)
		}
	}()
}

// burnCPU spins for pct% of each 100 ms period and sleeps for the rest.
// LockOSThread pins the goroutine to one OS thread so the scheduler cannot move
// it around and skew the ratio. A nil stop channel means run forever.
func burnCPU(pct int, stop <-chan struct{}) {
	const period = 100 * time.Millisecond
	busy := period * time.Duration(pct) / 100
	runtime.LockOSThread()
	for {
		select {
		case <-stop:
			return
		default:
		}
		begin := time.Now()
		for time.Since(begin) < busy {
		}
		time.Sleep(period - busy)
	}
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

// pushNet drives traffic in one direction, throttled to mbps: after each block
// it works out how long that transfer should have taken at the target rate and
// sleeps off the difference. Download and upload run as independent instances,
// so both directions can be driven at the same time.
func pushNet(dir string, mbps int, url string, xfer func(*http.Client, string) (int64, error)) {
	client := &http.Client{Timeout: 5 * time.Minute}
	var total int64
	report := time.Now()
	for {
		start := time.Now()
		n, err := xfer(client, url)
		if err != nil {
			logf("net %s: %v (retrying in 30s)", dir, err)
			time.Sleep(30 * time.Second)
			continue
		}
		total += n
		want := time.Duration(float64(n) * 8 / float64(mbps) / 1e6 * float64(time.Second))
		if d := want - time.Since(start); d > 0 {
			time.Sleep(d)
		}
		if el := time.Since(report); el >= 5*time.Minute { // summarise rather than log every block
			logf("net %s: %s over the last %s, about %.1f Mbps (target %d)",
				dir, humanSize(total), el.Round(time.Second), float64(total)*8/el.Seconds()/1e6, mbps)
			total, report = 0, time.Now()
		}
	}
}

// download fetches once and returns the byte count. The status check matters:
// an error page would otherwise count as a successful transfer and the dimension
// would quietly do nothing (the default endpoint answers 403 with a 1-byte body
// above 10 MB).
func download(client *http.Client, url string) (int64, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "staybusy/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download source answered HTTP %d, try another -net-down-url", resp.StatusCode)
	}
	return io.Copy(io.Discard, resp.Body)
}

// upBlock is posted over and over. Allocated on first use rather than at
// startup, so a run without -net-up does not carry 8 MiB it will never touch.
// Filled with random data: an all-zero body would compress away on the wire and
// the measured rate would be a fiction.
var upBlock []byte

// upload posts one block and returns the byte count.
func upload(client *http.Client, url string) (int64, error) {
	if upBlock == nil { // only ever called from the single upload goroutine
		upBlock = make([]byte, 8*miB)
		rand.Read(upBlock)
	}
	req, _ := http.NewRequest("POST", url, bytes.NewReader(upBlock))
	req.Header.Set("User-Agent", "staybusy/1.0")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(upBlock))
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("upload target answered HTTP %d, try another -net-up-url", resp.StatusCode)
	}
	return int64(len(upBlock)), nil
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
// the host has free and what this cgroup has left. Returns 0 (check skipped) off
// Linux.
func availBytes() int64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	host := hostAvail()
	cg := cgroupAvail()
	if cg > 0 && (host == 0 || cg < host) {
		return cg
	}
	return host
}

func hostAvail() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb * kiB
		}
	}
	return 0
}

// cgroupAvail reports the headroom left in this cgroup, or 0 when unconstrained.
//
// Inside a container /proc/meminfo describes the host, not the cgroup limit.
// Consulting only that lets `docker run --memory 64m ... -mem 512M` sail past
// the safety check and get OOM-killed by the kernel (exit 137), with nothing in
// the log but a line claiming it allocated 512 MiB.
func cgroupAvail() int64 {
	// cgroup v2: the file reads "max" when unconstrained, so a parse failure
	// is the unconstrained case.
	if max, ok := readUint("/sys/fs/cgroup/memory.max"); ok {
		cur, _ := readUint("/sys/fs/cgroup/memory.current")
		return max - cur
	}
	// cgroup v1: unconstrained shows up as a value near the int64 ceiling.
	if max, ok := readUint("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok && max < 1<<62 {
		cur, _ := readUint("/sys/fs/cgroup/memory/memory.usage_in_bytes")
		return max - cur
	}
	return 0
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

func logf(format string, a ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
}

func main() {
	flag.Parse()

	mem, err := parseSize(*flagMem)
	if err != nil {
		fmt.Println("-mem:", err)
		os.Exit(2)
	}
	if *flagCPU < 0 || *flagCPU > 100 {
		fmt.Println("-cpu must be between 1 and 100")
		os.Exit(2)
	}
	if *flagPerCore < 0 || *flagPerCore > 100 {
		fmt.Println("-per-core must be between 1 and 100")
		os.Exit(2)
	}
	if *flagCores < 0 {
		fmt.Println("-cores must not be negative")
		os.Exit(2)
	}
	if *flagDuty < 1 || *flagDuty > 100 {
		fmt.Println("-duty must be between 1 and 100")
		os.Exit(2)
	}
	if *flagCPU == 0 && *flagPerCore == 0 && mem == 0 && *flagNetDown == 0 && *flagNetUp == 0 {
		fmt.Print("staybusy - put a controlled load on a machine\n\n")
		flag.PrintDefaults()
		return
	}

	if *flagCPU > 0 || *flagPerCore > 0 {
		startCPU(*flagCPU, *flagCores, *flagPerCore, *flagDuty, *flagCycle)
	}
	if mem > 0 {
		// Clamp rather than warn: a warning followed by the allocation anyway
		// is no protection at all.
		if avail := availBytes(); avail > 0 {
			safe := avail - memReserve
			switch {
			case safe < minMem:
				logf("mem: only %s available, less than %s once the %s reserve is kept - skipping",
					humanSize(avail), humanSize(minMem), humanSize(memReserve))
				mem = 0
			case mem > safe:
				src := "host"
				if cg := cgroupAvail(); cg > 0 && cg <= avail {
					src = "cgroup"
				}
				logf("mem: %s requested, holding %s instead (%s has %s available, less a %s reserve)",
					humanSize(mem), humanSize(safe), src, humanSize(avail), humanSize(memReserve))
				mem = safe
			}
		}
		if mem > 0 {
			logf("mem: holding %s", humanSize(mem))
			go eatMem(mem)
		}
	}
	if *flagNetDown > 0 {
		logf("net down: about %d Mbps (%.2f TB/day) from %s", *flagNetDown,
			float64(*flagNetDown)*86400/8/1e6, *flagNetDownURL)
		go pushNet("down", *flagNetDown, *flagNetDownURL, download)
	}
	if *flagNetUp > 0 {
		logf("net up: about %d Mbps (%.2f TB/day) to %s", *flagNetUp,
			float64(*flagNetUp)*86400/8/1e6, *flagNetUpURL)
		go pushNet("up", *flagNetUp, *flagNetUpURL, upload)
	}

	// Not `select {}`: with only -mem set, the allocating goroutine finishes and
	// leaves nothing runnable, which the runtime reports as a deadlock and panics
	// on. A sleep loop keeps a timer pending, so it is never considered deadlocked.
	for {
		time.Sleep(24 * time.Hour)
	}
}
