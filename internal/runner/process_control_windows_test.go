//go:build windows

package runner

import (
	"errors"
	"testing"
)

func TestWindowsGracefulStopFailsBeforeProcessLookup(t *testing.T) {
	if err := requestProcessStop(-1, false); !errors.Is(err, errGracefulStopUnsupported) {
		t.Fatalf("Windows graceful stop error = %v", err)
	}
}
