package memory

import (
	"errors"
	"sync"
	"testing"
)

func TestLimitsDefaults(t *testing.T) {
	d := DefaultLimits()
	if d.MaxFileBytes != 64<<10 || d.MaxJobBytes != 256<<10 || d.MaxReadBytes != 2<<20 {
		t.Fatalf("defaults = %+v", d)
	}
	if got := (Limits{MaxFileBytes: 10}).withDefaults(); got.MaxFileBytes != 10 || got.MaxJobBytes != d.MaxJobBytes || got.MaxReadBytes != d.MaxReadBytes {
		t.Fatalf("withDefaults = %+v", got)
	}
}

func TestBudget(t *testing.T) {
	b := NewBudget(10)
	if err := b.Reserve(6); err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve(5); !errors.Is(err, ErrBudget) {
		t.Fatalf("over the budget = %v", err)
	}
	if b.Used() != 6 {
		t.Fatalf("a refused reservation must not count, used = %d", b.Used())
	}
	b.Release(6)
	if err := b.Reserve(10); err != nil {
		t.Fatalf("after release: %v", err)
	}
	b.Release(100) // never goes negative
	if b.Used() != 0 {
		t.Fatalf("used = %d", b.Used())
	}
}

func TestNilBudgetMeansNoJobCap(t *testing.T) {
	var b *Budget
	if err := b.Reserve(1 << 40); err != nil {
		t.Fatal(err)
	}
	b.Release(5)
	if b.Used() != 0 {
		t.Fatal("nil budget must report 0")
	}
}

func TestBudgetIsSafeForConcurrentUse(t *testing.T) {
	b := NewBudget(1000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Reserve(100) == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 10 || b.Used() != 1000 {
		t.Fatalf("granted %d, used %d; want exactly 10 grants", granted, b.Used())
	}
}
