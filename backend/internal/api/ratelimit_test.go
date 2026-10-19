package api

import (
	"fmt"
	"testing"
	"time"
)

func TestLimiterBurstAndRefill(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	l := newLimiter(2, 3, 100, c.Now)
	for i := range 3 {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("request %d denied", i)
		}
	}
	ok, wait := l.allow("k")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("over burst: ok=%v wait=%v", ok, wait)
	}
	c.Advance(250 * time.Millisecond)
	if ok, wait := l.allow("k"); ok || wait != 250*time.Millisecond {
		t.Errorf("half refilled: ok=%v wait=%v", ok, wait)
	}
	c.Advance(250 * time.Millisecond)
	if ok, _ := l.allow("k"); !ok {
		t.Error("refilled token denied")
	}
	// refill is capped at the burst
	c.Advance(time.Hour)
	for i := range 3 {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("after idle, request %d denied", i)
		}
	}
	if ok, _ := l.allow("k"); ok {
		t.Error("refill exceeded the burst")
	}
	// keys are independent
	if ok, _ := l.allow("other"); !ok {
		t.Error("other key denied")
	}
}

func TestLimiterClockGoingBackwards(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	l := newLimiter(1, 1, 10, c.Now)
	l.allow("k")
	c.Advance(-time.Hour)
	if ok, _ := l.allow("k"); ok {
		t.Error("a clock step back refilled the bucket")
	}
}

func TestLimiterIsBoundedAndEvictsIdleFirst(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	l := newLimiter(1, 2, 3, c.Now)
	l.allow("active")
	l.allow("active") // empty bucket
	c.Advance(time.Millisecond)
	l.allow("idle1")
	l.allow("idle2")
	c.Advance(10 * time.Second) // idle1 and idle2 are full again, active too
	l.allow("active")
	l.allow("active") // active is empty again
	l.allow("new")    // the map is full: idle (full) buckets go first
	if n := l.size(); n > 3 {
		t.Fatalf("size = %d", n)
	}
	if ok, _ := l.allow("active"); ok {
		t.Error("the active key was evicted (its empty bucket was reset)")
	}
	// many distinct keys never grow the map beyond its bound
	for i := range 50 {
		l.allow(fmt.Sprintf("flood-%d", i))
		if n := l.size(); n > 3 {
			t.Fatalf("size = %d after %d keys", n, i)
		}
	}
}

func TestLimiterEvictsLeastRecentlyUsedWhenNoneIsIdle(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	l := newLimiter(0.001, 1, 2, c.Now)
	l.allow("a")
	c.Advance(time.Second)
	l.allow("b")
	c.Advance(time.Second)
	l.allow("c") // nothing is full: the oldest ("a") goes
	if l.size() != 2 {
		t.Fatalf("size = %d", l.size())
	}
	if ok, _ := l.allow("b"); ok {
		t.Error("b was evicted instead of the oldest key")
	}
}

func TestDefaultLimiterBound(t *testing.T) {
	if maxLimiterKeys != 10000 {
		t.Errorf("maxLimiterKeys = %d", maxLimiterKeys)
	}
}
