package provider

import (
	"testing"
	"time"
)

func TestEmbeddedClaudeDefaultStallTimeoutIs300Seconds(t *testing.T) {
	if got := embeddedClaudeDefaultStallTimeout; got != 300*time.Second {
		t.Fatalf("Claude watchdog default = %s, want 300s", got)
	}
}
