package clock_test

import (
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/clock"
)

func TestSystemClockIsUTC(t *testing.T) {
	t.Parallel()

	var c clock.Clock = clock.System{}
	if got := c.Now().Location(); got != time.UTC {
		t.Errorf("System.Now().Location() = %v, want UTC", got)
	}
}
