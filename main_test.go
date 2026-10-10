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
