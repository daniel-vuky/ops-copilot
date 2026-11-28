package jobs

// SetRunningForTest sets the running count of kind, as if that many of its
// jobs had been claimed, so tests can check what Enqueue counts as free
// slots without running the dispatcher.
func (s *Scheduler) SetRunningForTest(kind string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kinds[kind].running = n
}
