package main

import (
	"runtime"
	"strings"
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

// A read larger than the burst must not wait for tokens that can never accrue.
func TestLimiterHugeReadDoesNotHang(t *testing.T) {
	l := newLimiter(1e6)
	done := make(chan struct{})
	go func() {
		l.wait(50 * miB)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait on a read larger than the burst blocked forever")
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

func TestCPUShapeSchedule(t *testing.T) {
	s := &cpuShape{ncpu: 4, workers: 4, target: 50, cycle: 3 * time.Minute,
		schedule: []int{80, 20, 0}, duty: 100, start: time.Now()}
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
	s := &cpuShape{ncpu: 1, workers: 1, target: 100, ramp: 10 * time.Second,
		cycle: time.Hour, duty: 100, start: time.Now().Add(-5 * time.Second)}
	if got := s.level(); got < 40 || got > 60 {
		t.Errorf("mid-ramp = %d, want about 50", got)
	}
	s.start = time.Now().Add(-20 * time.Second)
	if got := s.level(); got != 100 {
		t.Errorf("ramp done = %d, want 100", got)
	}
}

func TestCPUShapeDuty(t *testing.T) {
	s := &cpuShape{ncpu: 1, workers: 1, target: 80, duty: 50,
		cycle: time.Minute, start: time.Now().Add(-10 * time.Second)}
	if got := s.level(); got != 80 {
		t.Errorf("within burn = %d, want 80", got)
	}
	s.start = time.Now().Add(-40 * time.Second)
	if got := s.level(); got != 0 {
		t.Errorf("after burn = %d, want 0", got)
	}
}

func TestCPUShapeWorkerScaling(t *testing.T) {
	s := &cpuShape{ncpu: 4, workers: 2, target: 50, duty: 100, cycle: time.Hour, start: time.Now()}
	if got := s.workerPct(); got != 100 {
		t.Errorf("workerPct = %d, want 100 (50%% of 4 cores over 2 workers)", got)
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

func TestDarwinAvail(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	if got := darwinAvail(); got <= 0 {
		t.Errorf("darwinAvail() = %d, want > 0", got)
	}
}
