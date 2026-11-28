package store

import (
	"context"
	"sync"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/clock"
)

// LastUsed throttles "last used" writes, such as a session's or an API
// key's last_used_at, to at most one per key per period, so a busy client
// does not take the writer on every request. It is safe for concurrent use.
//
// Key it by kind and row id, for example "session:<id>" or "apikey:<id>",
// never by the raw token, and call Touch only after the credential has been
// authenticated, so unauthenticated requests cannot fill the map.
//
// Call Touch outside any Transactor.Write. Its write func runs its own
// Write with a short timeout (2 s); inside another Write that would be a
// nested Write, and a failed stamp would roll back the request's
// transaction. The throttle is in memory and per process; the write keeps
// the bound across restarts with a guard in the SQL:
//
//	UPDATE ... SET last_used_at = ? WHERE id = ?
//	  AND (last_used_at IS NULL OR last_used_at <= ?)
//
// binding at and at minus the period. On the request path, a Touch error is
// logged and ignored: a missed last-used stamp must not fail the request.
type LastUsed struct {
	clock clock.Clock
	every time.Duration

	mu        sync.Mutex
	stamps    map[string]time.Time // key -> time of the last write started
	lastSweep time.Time
}

// NewLastUsed returns a LastUsed that lets one write per key through every
// period, timed by c. A period of zero or less lets every call write.
func NewLastUsed(c clock.Clock, every time.Duration) *LastUsed {
	return &LastUsed{
		clock:  c,
		every:  every,
		stamps: make(map[string]time.Time),
	}
}

// Touch calls write with the current time if key is due, and returns
// write's error. A key is due when it has no stamp, when its stamp is at
// least one period old, or when its stamp is in the future (the wall clock
// was stepped back). Touch records the stamp before calling write, so
// concurrent calls for one key write once. If write fails, panics or exits
// its goroutine, the key becomes due again unless a newer call has stamped
// it since; a panic still propagates. The lock is never held while write
// runs.
func (l *LastUsed) Touch(ctx context.Context, key string, write func(ctx context.Context, at time.Time) error) error {
	// The clock is read under the lock so stamps are taken in lock order
	// and the sweep runs on schedule.
	l.mu.Lock()
	now := l.clock.Now()
	l.sweepLocked(now)
	if stamp, ok := l.stamps[key]; ok && !l.due(now, stamp) {
		l.mu.Unlock()
		return nil
	}
	l.stamps[key] = now
	l.mu.Unlock()

	succeeded := false
	defer func() {
		if !succeeded {
			l.forget(key, now)
		}
	}()

	if err := write(ctx, now); err != nil {
		return err
	}
	succeeded = true
	return nil
}

// forget removes key's stamp if it is still the one taken at stamp. Any
// stamp the failed call replaced was due, so removing the entry leaves the
// key due, as restoring that stamp would.
func (l *LastUsed) forget(key string, stamp time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.stamps[key]; ok && cur.Equal(stamp) {
		delete(l.stamps, key)
	}
}

// due reports whether a key stamped at stamp may write again at now.
func (l *LastUsed) due(now, stamp time.Time) bool {
	return now.Sub(stamp) >= l.every || now.Before(stamp)
}

// sweepLocked drops due stamps, at most once per period, so keys that stop
// being used do not stay in memory. l.mu must be held.
func (l *LastUsed) sweepLocked(now time.Time) {
	if !l.lastSweep.IsZero() && !l.due(now, l.lastSweep) {
		return
	}
	l.lastSweep = now
	for k, stamp := range l.stamps {
		if l.due(now, stamp) {
			delete(l.stamps, k)
		}
	}
}
