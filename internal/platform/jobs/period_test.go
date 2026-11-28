package jobs

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPeriodStart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		now           string
		every, offset time.Duration
		want          string
	}{
		{now: "2026-11-28T19:59:59Z", every: 24 * time.Hour, offset: 20 * time.Hour, want: "2026-11-27T20:00:00Z"},
		{now: "2026-11-28T20:00:00Z", every: 24 * time.Hour, offset: 20 * time.Hour, want: "2026-11-28T20:00:00Z"},
		{now: "2026-11-28T10:30:00Z", every: time.Hour, offset: 0, want: "2026-11-28T10:00:00Z"},
		{now: "1969-12-31T23:30:00Z", every: time.Hour, offset: 0, want: "1969-12-31T23:00:00Z"},
	}
	for _, tt := range tests {
		got := periodStart(mustTime(t, tt.now), tt.every, tt.offset)
		if want := mustTime(t, tt.want); !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("periodStart(%s, %v, %v) = %v, want %v UTC", tt.now, tt.every, tt.offset, got, want)
		}
	}
}

func TestNextRun(t *testing.T) {
	t.Parallel()
	s := newTestScheduler()
	if err := s.Register(recurringKind()); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(baseKind()); err != nil {
		t.Fatal(err)
	}
	now := mustTime(t, "2026-11-28T19:59:59Z")
	got, ok := s.NextRun("cve.scan", now)
	if want := mustTime(t, "2026-11-28T20:00:00Z"); !ok || !got.Equal(want) {
		t.Errorf("NextRun(cve.scan) = %v, %v; want %v, true", got, ok, want)
	}
	// At a period start the next run is the following one, strictly after now.
	at := mustTime(t, "2026-11-28T20:00:00Z")
	got, ok = s.NextRun("cve.scan", at)
	if want := mustTime(t, "2026-11-29T20:00:00Z"); !ok || !got.Equal(want) {
		t.Errorf("NextRun(cve.scan) at a period start = %v, %v; want %v, true", got, ok, want)
	}
	if _, ok := s.NextRun("missing", now); ok {
		t.Error("NextRun(unknown kind) ok = true, want false")
	}
	if _, ok := s.NextRun("doc.extract", now); ok {
		t.Error("NextRun(non-recurring kind) ok = true, want false")
	}
}
