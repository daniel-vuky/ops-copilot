package jobs

import "time"

// minEvery is the shortest period of a recurring kind (ADR 0006 §5).
const minEvery = time.Minute

// periodStart returns the start of the period that holds now: periods are
// every long, shifted by offset from the Unix epoch, in Unix milliseconds
// (ADR 0006 §5). The result is in UTC and never after now. every must be
// positive and offset in [0, every).
func periodStart(now time.Time, every, offset time.Duration) time.Time {
	e := every.Milliseconds()
	o := offset.Milliseconds()
	d := now.UnixMilli() - o
	n := d / e
	if d%e < 0 { // floor, not truncation, before the epoch
		n--
	}
	return time.UnixMilli(n*e + o).UTC()
}

// NextRun returns the start of the next period of a recurring kind,
// strictly after now. ok is false when the kind is not registered or not
// recurring.
func (s *Scheduler) NextRun(kind string, now time.Time) (next time.Time, ok bool) {
	s.mu.Lock()
	ks, found := s.kinds[kind]
	s.mu.Unlock()
	if !found || ks.kind.Every <= 0 {
		return time.Time{}, false
	}
	every := ks.kind.Every
	return periodStart(now, every, ks.kind.Offset).Add(every), true
}
