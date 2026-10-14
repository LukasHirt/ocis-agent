package store

import "testing"

func TestApprovalTransitionAllowedMatrix(t *testing.T) {
	all := []ApprovalState{ApprovalPending, ApprovalApproved, ApprovalRejected, ApprovalExpired,
		ApprovalExecuting, ApprovalExecuted, ApprovalFailed, ApprovalUnknown, "", "weird"}
	allowed := map[[2]ApprovalState]bool{
		{ApprovalApproved, ApprovalExecuting}: true,
		{ApprovalApproved, ApprovalExpired}:   true,
		{ApprovalApproved, ApprovalFailed}:    true,
		{ApprovalExecuting, ApprovalExecuted}: true,
		{ApprovalExecuting, ApprovalFailed}:   true,
		{ApprovalExecuting, ApprovalUnknown}:  true,
	}
	for _, from := range all {
		for _, to := range all {
			if got, want := ApprovalTransitionAllowed(from, to), allowed[[2]ApprovalState{from, to}]; got != want {
				t.Errorf("ApprovalTransitionAllowed(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestApprovalStateValidAndTerminal(t *testing.T) {
	cases := []struct {
		s               ApprovalState
		valid, terminal bool
	}{
		{ApprovalPending, true, false},
		{ApprovalApproved, true, false},
		{ApprovalExecuting, true, false},
		{ApprovalRejected, true, true},
		{ApprovalExpired, true, true},
		{ApprovalExecuted, true, true},
		{ApprovalFailed, true, true},
		{ApprovalUnknown, true, true},
		{"", false, false},
		{"weird", false, false},
	}
	for _, c := range cases {
		if c.s.Valid() != c.valid || c.s.Terminal() != c.terminal {
			t.Errorf("%q: Valid=%v Terminal=%v, want %v %v", c.s, c.s.Valid(), c.s.Terminal(), c.valid, c.terminal)
		}
	}
}

func TestIdemStateValid(t *testing.T) {
	for s, want := range map[IdemState]bool{IdemStarted: true, IdemDone: true, IdemFailed: true, "": false, "weird": false} {
		if s.Valid() != want {
			t.Errorf("%q.Valid() = %v, want %v", s, s.Valid(), want)
		}
	}
}
