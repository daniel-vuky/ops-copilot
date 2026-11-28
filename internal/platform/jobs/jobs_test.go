package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/clock"
)

func noopHandler(context.Context, Job) error { return nil }

// baseKind is a valid non-recurring kind each Register case starts from.
func baseKind() Kind {
	return Kind{Name: "doc.extract", Handler: noopHandler}
}

// recurringKind is a valid recurring kind.
func recurringKind() Kind {
	return Kind{Name: "cve.scan", Handler: noopHandler, Every: 24 * time.Hour, Offset: 20 * time.Hour}
}

func newTestScheduler() *Scheduler {
	return New(nil, nil, Options{})
}

func TestRegisterAcceptsValidKinds(t *testing.T) {
	t.Parallel()
	s := newTestScheduler()
	for _, k := range []Kind{
		baseKind(),
		recurringKind(),
		{Name: "a", Handler: noopHandler, NoRetry: true, Concurrency: 2, MaxWaiting: 20, Timeout: time.Minute},
		{Name: "b", Handler: noopHandler, Retry: []time.Duration{0, time.Minute}},
		{Name: "c", Handler: noopHandler, Every: time.Minute},
		{Name: "a" + strings.Repeat("z", 63), Handler: noopHandler},
	} {
		if err := s.Register(k); err != nil {
			t.Errorf("Register(%q) = %v, want nil", k.Name, err)
		}
	}
}

func TestRegisterRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(k *Kind)
		setup  func(s *Scheduler)
	}{
		{name: "empty name", mutate: func(k *Kind) { k.Name = "" }},
		{name: "upper case name", mutate: func(k *Kind) { k.Name = "Bad" }},
		{name: "leading digit", mutate: func(k *Kind) { k.Name = "1abc" }},
		{name: "65-char name", mutate: func(k *Kind) { k.Name = "a" + strings.Repeat("b", 64) }},
		{name: "duplicate", setup: func(s *Scheduler) { _ = s.Register(baseKind()) }},
		{name: "nil handler", mutate: func(k *Kind) { k.Handler = nil }},
		{name: "negative concurrency", mutate: func(k *Kind) { k.Concurrency = -1 }},
		{name: "negative max waiting", mutate: func(k *Kind) { k.MaxWaiting = -1 }},
		{name: "negative timeout", mutate: func(k *Kind) { k.Timeout = -time.Second }},
		{name: "negative retry delay", mutate: func(k *Kind) { k.Retry = []time.Duration{time.Second, -time.Second} }},
		{name: "NoRetry with Retry", mutate: func(k *Kind) { k.NoRetry = true; k.Retry = []time.Duration{} }},
		{name: "negative every", mutate: func(k *Kind) { k.Every = -time.Hour }},
		{name: "every 59 s", mutate: func(k *Kind) { k.Every = 59 * time.Second }},
		{name: "every not whole seconds", mutate: func(k *Kind) { k.Every = 90*time.Second + 500*time.Millisecond }},
		{name: "offset equals every", mutate: func(k *Kind) { k.Every = time.Hour; k.Offset = time.Hour }},
		{name: "offset negative", mutate: func(k *Kind) { k.Every = time.Hour; k.Offset = -time.Second }},
		{name: "offset not whole seconds", mutate: func(k *Kind) { k.Every = time.Hour; k.Offset = 1500 * time.Millisecond }},
		{name: "offset on non-recurring", mutate: func(k *Kind) { k.Offset = time.Second }},
		{name: "period key on non-recurring", mutate: func(k *Kind) { k.PeriodKey = "all" }},
		{name: "period key too long", mutate: func(k *Kind) { k.Every = time.Hour; k.PeriodKey = strings.Repeat("k", 129) }},
		{name: "max waiting on recurring", mutate: func(k *Kind) { k.Every = time.Hour; k.MaxWaiting = 5 }},
		{name: "after recover", setup: func(s *Scheduler) { s.recovered = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestScheduler()
			if tt.setup != nil {
				tt.setup(s)
			}
			k := baseKind()
			if tt.mutate != nil {
				tt.mutate(&k)
			}
			if err := s.Register(k); err == nil {
				t.Fatalf("Register(%+v) = nil, want an error", k)
			}
		})
	}
}

func TestRegisterNormalises(t *testing.T) {
	t.Parallel()
	s := newTestScheduler()
	if err := s.Register(baseKind()); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(recurringKind()); err != nil {
		t.Fatal(err)
	}
	retry := []time.Duration{time.Minute}
	if err := s.Register(Kind{Name: "notify.chat", Handler: noopHandler, Retry: retry, PeriodKey: "", Concurrency: 3}); err != nil {
		t.Fatal(err)
	}
	retry[0] = time.Hour // the registered kind keeps its own copy

	base := s.kinds["doc.extract"]
	if base.kind.Concurrency != 1 || base.kind.Timeout != DefaultTimeout || base.kind.PeriodKey != "" {
		t.Errorf("base kind = %+v, want Concurrency 1, Timeout %v, PeriodKey \"\"", base.kind, DefaultTimeout)
	}
	if rec := s.kinds["cve.scan"]; rec.kind.PeriodKey != "period" {
		t.Errorf("recurring PeriodKey = %q, want %q", rec.kind.PeriodKey, "period")
	}
	n := s.kinds["notify.chat"]
	if n.kind.Concurrency != 3 {
		t.Errorf("Concurrency = %d, want 3", n.kind.Concurrency)
	}
	if got := n.delay(1); got != time.Minute {
		t.Errorf("delay(1) after caller changed its slice = %v, want 1m", got)
	}
}

func TestMaxAttemptsAndDelay(t *testing.T) {
	t.Parallel()
	s := newTestScheduler()
	for _, k := range []Kind{
		{Name: "default", Handler: noopHandler},
		{Name: "custom", Handler: noopHandler, Retry: []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}},
		{Name: "once", Handler: noopHandler, NoRetry: true},
	} {
		if err := s.Register(k); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		kind   string
		max    int
		delays []time.Duration
	}{
		{kind: "default", max: 3, delays: []time.Duration{30 * time.Second, 2 * time.Minute}},
		{kind: "custom", max: 4, delays: []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}},
		{kind: "once", max: 1},
	}
	for _, tt := range tests {
		ks := s.kinds[tt.kind]
		if got := ks.maxAttempts(); got != tt.max {
			t.Errorf("%s: maxAttempts() = %d, want %d", tt.kind, got, tt.max)
		}
		for i, want := range tt.delays {
			if got := ks.delay(i + 1); got != want {
				t.Errorf("%s: delay(%d) = %v, want %v", tt.kind, i+1, got, want)
			}
		}
	}
}

func TestPermanent(t *testing.T) {
	t.Parallel()
	e := errors.New("bad input")
	if !errors.Is(Permanent(e), e) {
		t.Error("errors.Is(Permanent(e), e) = false, want true")
	}
	if got := Permanent(e).Error(); got != e.Error() {
		t.Errorf("Permanent(e).Error() = %q, want %q", got, e.Error())
	}
	if !isPermanent(fmt.Errorf("x: %w", Permanent(e))) {
		t.Error("isPermanent(wrapped Permanent) = false, want true")
	}
	if isPermanent(e) {
		t.Error("isPermanent(plain error) = true, want false")
	}
	if isPermanent(nil) {
		t.Error("isPermanent(nil) = true, want false")
	}
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) != nil")
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	s := newTestScheduler()
	if _, ok := s.clock.(clock.System); !ok {
		t.Errorf("clock = %T, want clock.System", s.clock)
	}
	if s.log != slog.Default() {
		t.Error("logger is not slog.Default()")
	}
	in := "GET https://u:p@example.test/x?k=v" //nolint:gosec // test fixture, not a credential
	if got, want := s.scrub(in), ScrubURLs(in); got != want {
		t.Errorf("scrub(%q) = %q, want ScrubURLs %q", in, got, want)
	}
	if cap(s.nudge) != 1 {
		t.Errorf("cap(nudge) = %d, want 1", cap(s.nudge))
	}
	if s.kinds == nil || s.jobs == nil || s.steps == nil || s.hookBackoff == nil {
		t.Error("maps are not created in New")
	}
	if s.jobCtx == nil || s.cancelJobs == nil || s.jobCtx.Err() != nil {
		t.Error("job context is not created live in New")
	}
	want := timings{
		poll:           time.Second,
		drain:          6 * time.Second,
		cancelWait:     2 * time.Second,
		finishTimeout:  30 * time.Second,
		finishRetries:  []time.Duration{0, time.Second, 5 * time.Second},
		sweepEvery:     time.Minute,
		stepBackoffMax: 30 * time.Second,
		hookBackoff:    time.Minute,
		hookBackoffMax: 30 * time.Minute,
	}
	if !reflect.DeepEqual(s.t, want) {
		t.Errorf("timings = %+v, want %+v", s.t, want)
	}
}

func TestNewUsesOptions(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	scrub := func(string) string { return "x" }
	c := fixedClock{}
	s := New(nil, nil, Options{Clock: c, Logger: logger, Scrub: scrub})
	if s.clock != c || s.log != logger || s.scrub("a") != "x" {
		t.Error("New ignored its options")
	}
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(0, 0).UTC() }
