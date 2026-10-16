package scheduler

// trackedSweeps is how many users the sweep remembers.
func (s *Scheduler) trackedSweeps() int { return len(s.sweep.last) }
