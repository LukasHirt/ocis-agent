package store

import "testing"

func TestPauseSupersedes(t *testing.T) {
	order := []string{PauseDormant, PauseCredentialExpired, PauseCredentialInvalid, PauseCredentialRevoked, PauseTokenAnomaly}
	cases := []struct {
		current, next string
		want          bool
		why           string
	}{
		{"", PauseDormant, true, "not paused"},
		{"", PauseUser, true, "not paused"},
		{PauseUser, PauseUser, false, "the same reason changes nothing"},
		{PauseUser, PauseTokenAnomaly, false, "the user's pause is sticky"},
		{PauseUser, PauseDormant, false, "the user's pause is sticky"},
		{PauseDormant, PauseUser, true, "a user pause overrides an automatic one"},
		{PauseTokenAnomaly, PauseUser, true, "a user pause overrides an automatic one"},
		{PauseDormant, PauseDormant, false, "equal rank"},
		{PauseTokenAnomaly, PauseDormant, false, "lower rank"},
	}
	for _, c := range cases {
		if got := PauseSupersedes(c.current, c.next); got != c.want {
			t.Errorf("PauseSupersedes(%q, %q) = %v, want %v (%s)", c.current, c.next, got, c.want, c.why)
		}
	}
	for i := range order {
		for j := range order {
			want := j > i
			if got := PauseSupersedes(order[i], order[j]); got != want {
				t.Errorf("PauseSupersedes(%q, %q) = %v, want %v (rank order)", order[i], order[j], got, want)
			}
		}
	}
}
