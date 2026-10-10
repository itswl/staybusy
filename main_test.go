package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{"0", 0, false},
		{"512M", 512 * miB, false},
		{"512MB", 512 * miB, false},
		{"1024K", 1024 * kiB, false},
		{"1.5G", 3 * giB / 2, false},
		{"1g", 1 * giB, false},
		{"2", 2 * giB, false}, // a bare number means GiB
		{" 256 M ", 256 * miB, false},
		{"-1", 0, true},
		{"abc", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("parseSize(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{miB, "1 MiB"},
		{2 * miB, "2 MiB"},
		{giB, "1.00 GiB"},
		{3 * giB / 2, "1.50 GiB"},
	}
	for _, c := range cases {
		if got := humanSize(c.in); got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCPUPlan(t *testing.T) {
	ncpu := runtime.NumCPU()

	t.Run("spreads over every core", func(t *testing.T) {
		w, d, _ := cpuPlan(25, 0, 0)
		if w != ncpu {
			t.Errorf("workers = %d, want %d", w, ncpu)
		}
		if d != 25 {
			t.Errorf("duty = %d, want 25", d)
		}
	})

	t.Run("per-core wins and caps cores", func(t *testing.T) {
		w, d, _ := cpuPlan(0, 999, 50)
		if w != ncpu || d != 50 {
			t.Errorf("got workers=%d duty=%d, want %d/50", w, d, ncpu)
		}
	})

	// Regression: -cpu 5 -cores 200 used to spread 5% over 200 workers, divide
	// each down to 0%, and quietly run no load.
	t.Run("cores over the count do not zero the load", func(t *testing.T) {
		w, d, _ := cpuPlan(5, 999, 0)
		if w != ncpu {
			t.Errorf("workers = %d, want %d (capped)", w, ncpu)
		}
		if d < 1 {
			t.Fatalf("duty = %d: asking for load produced none", d)
		}
	})

	t.Run("concentrating raises each core", func(t *testing.T) {
		if ncpu < 4 {
			t.Skip("needs at least 4 cores")
		}
		w, d, _ := cpuPlan(10, 2, 0)
		if w != 2 || d != 10*ncpu/2 {
			t.Errorf("got workers=%d duty=%d, want 2/%d", w, d, 10*ncpu/2)
		}
	})

	t.Run("unreachable target runs flat out", func(t *testing.T) {
		if ncpu < 2 {
			t.Skip("needs at least 2 cores")
		}
		w, d, desc := cpuPlan(100, 1, 0)
		if w != 1 || d != 100 {
			t.Errorf("got workers=%d duty=%d, want 1/100", w, d)
		}
		if !strings.Contains(desc, "flat out") {
			t.Errorf("description should warn, got %q", desc)
		}
	})
}

func setKnobs(cpuPct, duty int, cycle time.Duration) {
	knobs.cpuPct.Store(int64(cpuPct))
	knobs.duty.Store(int64(duty))
	knobs.cycleNs.Store(int64(cycle))
}

func TestCPUShapeSchedule(t *testing.T) {
	setKnobs(50, 100, 3*time.Minute)
	s := &cpuShape{ncpu: 4, workers: 4, schedule: []int{80, 20, 0}, start: time.Now()}
	if got := s.level(); got != 80 {
		t.Errorf("slot 0 = %d, want 80", got)
	}
	s.start = time.Now().Add(-90 * time.Second)
	if got := s.level(); got != 20 {
		t.Errorf("slot 1 = %d, want 20", got)
	}
	s.start = time.Now().Add(-150 * time.Second)
	if got := s.level(); got != 0 {
		t.Errorf("slot 2 = %d, want 0", got)
	}
}

func TestCPUShapeRamp(t *testing.T) {
	setKnobs(100, 100, time.Hour)
	s := &cpuShape{ncpu: 1, workers: 1, ramp: 10 * time.Second, start: time.Now().Add(-5 * time.Second)}
	if got := s.level(); got < 40 || got > 60 {
		t.Errorf("mid-ramp = %d, want about 50", got)
	}
	s.start = time.Now().Add(-20 * time.Second)
	if got := s.level(); got != 100 {
		t.Errorf("ramp done = %d, want 100", got)
	}
}

func TestCPUShapeDuty(t *testing.T) {
	setKnobs(80, 50, time.Minute)
	s := &cpuShape{ncpu: 1, workers: 1, start: time.Now().Add(-10 * time.Second)}
	if got := s.level(); got != 80 {
		t.Errorf("within burn = %d, want 80", got)
	}
	s.start = time.Now().Add(-40 * time.Second)
	if got := s.level(); got != 0 {
		t.Errorf("after burn = %d, want 0", got)
	}
}

func TestCPUShapeWorkerScaling(t *testing.T) {
	setKnobs(50, 100, time.Hour)
	s := &cpuShape{ncpu: 4, workers: 2, start: time.Now()}
	if got := s.workerPct(); got != 100 {
		t.Errorf("workerPct = %d, want 100 (50%% of 4 cores over 2 workers)", got)
	}
}

func TestReserveFromBudget(t *testing.T) {
	cases := []struct {
		budget int64
		want   int64
	}{
		{0, memReserve},         // nothing to read - keep the full reserve
		{64 * miB, 16 * miB},    // quarter of a small cgroup
		{1 * giB, 256 * miB},    // quarter of a 1 GiB cgroup
		{100 * giB, memReserve}, // never more than the cap
	}
	for _, c := range cases {
		if got := reserveFromBudget(c.budget); got != c.want {
			t.Errorf("reserveFromBudget(%d) = %d, want %d", c.budget, got, c.want)
		}
	}
}

func TestDiskRate(t *testing.T) {
	cases := []struct {
		mbs, iops int
		block     int64
		want      int64
	}{
		{0, 0, 1 * miB, 0},
		{50, 0, 1 * miB, 50_000_000},
		{0, 1000, 4096, 1000 * 4096},
		{50, 1000, 4096, 1000 * 4096}, // iops wins
	}
	for _, c := range cases {
		if got := diskRate(c.mbs, c.iops, c.block); got != c.want {
			t.Errorf("diskRate(%d,%d,%d) = %d, want %d", c.mbs, c.iops, c.block, got, c.want)
		}
	}
}

func TestParseSchedule(t *testing.T) {
	if s, err := parseSchedule(""); err != nil || s != nil {
		t.Fatalf("parseSchedule(\"\") = %v, %v", s, err)
	}
	s, err := parseSchedule(" 80, 20 ,0 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 3 || s[0] != 80 || s[1] != 20 || s[2] != 0 {
		t.Errorf("got %v, want [80 20 0]", s)
	}
	for _, bad := range []string{"80,x", "101", "-1", "80,,0"} {
		if _, err := parseSchedule(bad); err == nil {
			t.Errorf("parseSchedule(%q) should fail", bad)
		}
	}
}

func TestApplyProfileStep(t *testing.T) {
	setKnobs(0, 100, time.Hour)
	step := profileStep{CPU: intp(80), NetDown: intp(20), Duty: intp(25)}
	applyStep(step)
	if got := knobs.cpuPct.Load(); got != 80 {
		t.Errorf("cpuPct = %d, want 80", got)
	}
	// 20 Mbps in bytes per second
	if got, want := knobs.netDown.Load(), int64(20*1e6/8); got != want {
		t.Errorf("netDown = %d, want %d", got, want)
	}
	if got := knobs.duty.Load(); got != 25 {
		t.Errorf("duty = %d, want 25", got)
	}
	// An absent field is left alone.
	applyStep(profileStep{CPU: intp(10)})
	if got := knobs.netDown.Load(); got != 20*1e6/8 {
		t.Errorf("netDown changed to %d, want it untouched", got)
	}
}

func intp(v int) *int { return &v }

func TestKnobState(t *testing.T) {
	knobs.netDown.Store(20 * 1e6 / 8)
	knobs.diskRead.Store(50 * 1e6)
	st := knobState()
	if st["net_down_mbps"].(float64) != 20 {
		t.Errorf("net_down_mbps = %v, want 20", st["net_down_mbps"])
	}
	if st["disk_read_mbs"].(float64) != 50 {
		t.Errorf("disk_read_mbs = %v, want 50", st["disk_read_mbs"])
	}
}

func TestPulseActiveAlwaysOnAtFullDuty(t *testing.T) {
	knobs.duty.Store(100)
	knobs.cycleNs.Store(int64(time.Second))
	if !pulseActive() {
		t.Error("duty 100 should always be active")
	}
}

func TestConfigValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"x", "x"},
		{true, "true"},
		{false, "false"},
		{float64(20), "20"},
		{float64(1.5), "1.5"},
	}
	for _, c := range cases {
		if got := configValue(c.in); got != c.want {
			t.Errorf("configValue(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLimiterPaces(t *testing.T) {
	l := newLimiter(8e6) // 8 MB/s; burst is 1.6 MB
	if l.burst < 64*kiB {
		t.Fatalf("burst floor not applied: %v", l.burst)
	}
	l.wait(int(l.burst)) // spend the initial burst so timing reflects the rate

	start := time.Now()
	const chunk = 256 * kiB
	for i := 0; i < 8; i++ { // 2 MiB at 8 MB/s -> about 0.26 s
		l.wait(chunk)
	}
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Fatalf("limiter did not pace: 2 MiB went out in %s", el)
	}
}

// A read larger than the burst is charged in full, so it waits its true
// duration and then returns - it must not block forever.
func TestLimiterHugeReadDoesNotHang(t *testing.T) {
	l := newLimiter(1e6) // 1 MB/s, burst 64 KiB
	done := make(chan struct{})
	go func() {
		l.wait(500_000) // 0.5 MB -> about 0.5 s after the initial burst
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait on a read larger than the burst blocked forever")
	}
}

func TestDarwinAvail(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	if got := darwinAvail(); got <= 0 {
		t.Errorf("darwinAvail() = %d, want > 0", got)
	}
}

func TestWaitExit(t *testing.T) {
	stop := make(chan os.Signal, 1)
	tick := make(chan time.Time, 1)
	dur := make(chan time.Time, 1)
	done := make(chan string, 1)
	go func() { done <- waitExit(stop, dur, tick) }()

	// A liveness tick must not end the wait.
	tick <- time.Now()
	select {
	case r := <-done:
		t.Fatalf("liveness tick ended the wait: %q", r)
	case <-time.After(100 * time.Millisecond):
	}

	dur <- time.Now()
	if r := <-done; r != "duration" {
		t.Fatalf("got %q, want duration", r)
	}

	// A signal ends it too.
	stop2 := make(chan os.Signal, 1)
	done2 := make(chan string, 1)
	go func() { done2 <- waitExit(stop2, nil, tick) }()
	stop2 <- syscall.SIGTERM
	if r := <-done2; r != "signal" {
		t.Fatalf("got %q, want signal", r)
	}
}

func TestServeDown(t *testing.T) {
	rec := httptest.NewRecorder()
	serveDown(rec, httptest.NewRequest("GET", "/down?bytes=1000", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 1000 {
		t.Errorf("body = %d bytes, want 1000", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != "1000" {
		t.Errorf("Content-Length = %q, want 1000", got)
	}
}

func TestServeStatusClamps(t *testing.T) {
	for _, c := range []struct {
		q    string
		want int
	}{
		{"?code=418", 418},
		{"?code=0", http.StatusBadRequest},   // would panic WriteHeader
		{"?code=999", http.StatusBadRequest}, // out of range
		{"", http.StatusInternalServerError}, // default
	} {
		rec := httptest.NewRecorder()
		serveStatus(rec, httptest.NewRequest("GET", "/status"+c.q, nil))
		if rec.Code != c.want {
			t.Errorf("status%s code = %d, want %d", c.q, rec.Code, c.want)
		}
	}
}

func TestServeFlaky(t *testing.T) {
	rec := httptest.NewRecorder()
	serveFlaky(rec, httptest.NewRequest("GET", "/flaky?p=1&code=503", nil))
	if rec.Code != 503 {
		t.Errorf("p=1 code = %d, want 503", rec.Code)
	}
	rec = httptest.NewRecorder()
	serveFlaky(rec, httptest.NewRequest("GET", "/flaky?p=0", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("p=0 code = %d, want 200", rec.Code)
	}
	// code=0 is clamped rather than panicking WriteHeader
	rec = httptest.NewRecorder()
	serveFlaky(rec, httptest.NewRequest("GET", "/flaky?p=1&code=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("p=1&code=0 code = %d, want 400", rec.Code)
	}
}

func TestServeUp(t *testing.T) {
	rec := httptest.NewRecorder()
	serveUp(rec, httptest.NewRequest("POST", "/up", strings.NewReader("hello")))
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
}

func TestServeControl(t *testing.T) {
	knobs.cpuPct.Store(0)
	knobs.netDown.Store(0)
	knobs.duty.Store(100)

	rec := httptest.NewRecorder()
	serveControl(rec, httptest.NewRequest("POST", "/control?cpu=50&net-down=8&duty=25", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := knobs.cpuPct.Load(); got != 50 {
		t.Errorf("cpuPct = %d, want 50", got)
	}
	if got := knobs.netDown.Load(); got != 1e6 { // 8 Mbps
		t.Errorf("netDown = %d, want 1000000", got)
	}
	if got := knobs.duty.Load(); got != 25 {
		t.Errorf("duty = %d, want 25", got)
	}

	// A bad field must not apply the good ones that came before it.
	before := knobs.cpuPct.Load()
	rec = httptest.NewRecorder()
	serveControl(rec, httptest.NewRequest("POST", "/control?cpu=90&net-down=abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
	if got := knobs.cpuPct.Load(); got != before {
		t.Errorf("cpuPct changed to %d despite a bad field; want %d", got, before)
	}

	// out-of-range values are rejected
	for _, q := range []string{"?duty=0", "?cpu=101", "?cycle=0s"} {
		rec = httptest.NewRecorder()
		serveControl(rec, httptest.NewRequest("POST", "/control"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST /control%s code = %d, want 400", q, rec.Code)
		}
	}

	// GET reports without changing anything
	rec = httptest.NewRecorder()
	serveControl(rec, httptest.NewRequest("GET", "/control?cpu=1", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cpu_percent") {
		t.Errorf("GET /control = %d %q, want the state", rec.Code, rec.Body.String())
	}
}

func TestControlClearsSchedule(t *testing.T) {
	setKnobs(50, 100, time.Minute)
	s := &cpuShape{ncpu: 2, workers: 2, schedule: []int{80, 20}, start: time.Now()}
	activeCPU = s
	defer func() { activeCPU = nil }()
	if got := s.level(); got != 80 {
		t.Fatalf("schedule not active: level = %d", got)
	}
	rec := httptest.NewRecorder()
	serveControl(rec, httptest.NewRequest("POST", "/control?cpu=10", nil))
	if got := s.level(); got != 10 {
		t.Errorf("after control cpu=10, level = %d, want the schedule cancelled", got)
	}
}

func TestLoadProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, []byte(`[{"duration":"1s","cpu":80},{"cpu":0}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	steps, err := loadProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(steps))
	}
	if steps[0].CPU == nil || *steps[0].CPU != 80 || steps[0].Duration == nil || *steps[0].Duration != "1s" {
		t.Errorf("step 0 = %+v", steps[0])
	}
	if steps[1].Duration != nil {
		t.Errorf("step 1 should have no duration, got %v", *steps[1].Duration)
	}
}
