package scheduler

// trackedSweeps is how many users the sweep remembers.
func (s *Scheduler) trackedSweeps() int { return len(s.sweep.last) }

// trackedSkips is how many (user, reason) skip events the proactive task remembers.
func (s *Scheduler) trackedSkips() int { return len(s.pro.skipped) }
