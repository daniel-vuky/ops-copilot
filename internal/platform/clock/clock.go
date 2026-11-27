// Package clock gives domain code the current time through an interface, so
// tests can substitute a fixed clock.
package clock

import "time"

// Clock reports the current time, for timestamps. Measure elapsed time with
// time.Now and time.Since instead: System.Now returns Now().UTC(), which drops
// the monotonic clock reading, so durations between two of its values can
// jump when the wall clock is changed.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}

// System is the Clock backed by the operating system's wall clock.
type System struct{}

// Now returns the current time in UTC.
func (System) Now() time.Time {
	return time.Now().UTC()
}
