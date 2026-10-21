package scheduler

import "time"

// trackedSweeps is how many users the sweep remembers.
func (s *Scheduler) trackedSweeps() int { return len(s.sweep.last) }

// trackedNotBefore is how many skipped users the proactive task makes wait.
func (s *Scheduler) trackedNotBefore() int { return len(s.pro.notBefore) }

// notBeforeOf is when the proactive task checks the user again.
func (s *Scheduler) notBeforeOf(user string) time.Time { return s.pro.notBefore[user] }
