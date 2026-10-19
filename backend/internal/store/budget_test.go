package store

import "testing"

func TestDailyBudget(t *testing.T) {
	cases := []struct {
		stored, limit, want int64
		why                 string
	}{
		{0, 50000, 50000, "a stored 0 (unlimited in the store) is the operator's cap"},
		{200000, 50000, 50000, "a stored value above the cap is the cap"},
		{50000, 50000, 50000, "the cap itself"},
		{1000, 50000, 1000, "a user may lower the budget"},
		{-5, 50000, 50000, "a negative stored value is the cap"},
		{1000, 0, 1000, "no operator cap: the stored value"},
		{0, 0, 0, "no operator cap and 0 stored: unlimited"},
	}
	for _, c := range cases {
		if got := DailyBudget(c.stored, c.limit); got != c.want {
			t.Errorf("DailyBudget(%d, %d) = %d, want %d (%s)", c.stored, c.limit, got, c.want, c.why)
		}
	}
}
