package clock

import (
	"testing"
	"time"
)

// Clock contract:
//   - System.Now is monotonic with the wall clock; System.After fires
//     after the requested duration.
//   - Fake.Now returns the start time until Advance is called; After
//     returns a channel that fires when the fake has advanced past
//     the scheduled time.
//   - Zero-duration After fires immediately.

func TestSystem_NowAdvances(t *testing.T) {
	c := System{}
	t1 := c.Now()
	time.Sleep(2 * time.Millisecond)
	t2 := c.Now()
	if !t2.After(t1) {
		t.Errorf("System.Now did not advance: t1=%v t2=%v", t1, t2)
	}
}

func TestSystem_AfterFires(t *testing.T) {
	c := System{}
	select {
	case <-c.After(5 * time.Millisecond):
		// fine
	case <-time.After(time.Second):
		t.Fatal("System.After did not fire within 1s")
	}
}

func TestFake_StartsAtConfiguredTime(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := NewFake(start)
	if !f.Now().Equal(start) {
		t.Errorf("Now() = %v, want %v", f.Now(), start)
	}
}

func TestFake_AdvanceMovesTime(t *testing.T) {
	f := NewFake(time.Time{})
	f.Advance(5 * time.Second)
	if got := f.Now().Sub(time.Time{}); got != 5*time.Second {
		t.Errorf("after Advance(5s), elapsed = %v, want 5s", got)
	}
}

func TestFake_AfterFiresAfterAdvance(t *testing.T) {
	f := NewFake(time.Time{})
	ch := f.After(2 * time.Second)
	select {
	case <-ch:
		t.Fatal("After channel fired before Advance")
	default:
	}
	f.Advance(1 * time.Second)
	select {
	case <-ch:
		t.Fatal("After channel fired before full duration elapsed")
	default:
	}
	f.Advance(1 * time.Second)
	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("After channel did not fire after total duration elapsed")
	}
}

func TestFake_AfterZeroDuration_FiresImmediately(t *testing.T) {
	f := NewFake(time.Time{})
	ch := f.After(0)
	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("zero-duration After did not fire immediately")
	}
}

func TestFake_PendingReportsScheduledTimers(t *testing.T) {
	f := NewFake(time.Time{})
	if got := f.Pending(); got != 0 {
		t.Errorf("Pending() = %d before any timers, want 0", got)
	}
	_ = f.After(time.Second)
	_ = f.After(2 * time.Second)
	if got := f.Pending(); got != 2 {
		t.Errorf("Pending() = %d, want 2", got)
	}
	f.Advance(time.Second + time.Millisecond)
	if got := f.Pending(); got != 1 {
		t.Errorf("Pending() = %d after firing one, want 1", got)
	}
}

func TestFake_AdvanceFiresMultipleEligibleTimers(t *testing.T) {
	f := NewFake(time.Time{})
	c1 := f.After(time.Second)
	c2 := f.After(2 * time.Second)
	c3 := f.After(5 * time.Second)
	f.Advance(3 * time.Second)
	for _, ch := range []<-chan time.Time{c1, c2} {
		select {
		case <-ch:
		case <-time.After(100 * time.Millisecond):
			t.Error("eligible timer did not fire after Advance")
		}
	}
	select {
	case <-c3:
		t.Error("c3 fired prematurely")
	default:
	}
}
