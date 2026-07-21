//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package readiness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestProbeTimeoutTerminatesDescendantsThatRetainStandardIO(t *testing.T) {
	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("locate sleep executable: %v", err)
	}
	t.Setenv("READINESS_CHILD_PID", childPIDPath)
	t.Setenv("READINESS_SLEEP", sleepPath)
	probePath := writeProbeExecutable(t, dir, "codex", `
if [ "$1" = "--version" ]; then
  "$READINESS_SLEEP" 60 &
  child_pid=$!
  printf '%s\n' "$child_pid" > "$READINESS_CHILD_PID"
  wait "$child_pid"
fi
exit 90`)

	ctx := newTriggeredDeadlineContext()
	resultChannel := make(chan commandResult, 1)
	go func() {
		resultChannel <- runCommand(ctx, probePath, "--version")
	}()

	var data []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err = os.ReadFile(childPIDPath)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			ctx.expire()
			select {
			case <-resultChannel:
			case <-time.After(time.Second):
			}
			t.Fatalf("read descendant pid: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancelledAt := time.Now()
	ctx.expire()
	var result commandResult
	select {
	case result = <-resultChannel:
	case <-time.After(2 * time.Second):
		t.Fatal("timed-out process tree did not return within 2s")
	}
	if elapsed := time.Since(cancelledAt); elapsed > 2*time.Second {
		t.Fatalf("timed-out process tree returned after %s", elapsed)
	}
	probe := probeFromResult("codex", []string{"--version"}, result)
	if probe.Status != ProbeStatusTimedOut {
		t.Fatalf("timed-out process-tree probe = %#v", probe)
	}

	childPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || childPID <= 0 {
		t.Fatalf("descendant pid = %q, err=%v", data, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	reapDeadline := time.Now().Add(time.Second)
	for processExists(childPID) && time.Now().Before(reapDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(childPID) {
		t.Fatalf("descendant process %d survived probe timeout", childPID)
	}
}

type triggeredDeadlineContext struct {
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func newTriggeredDeadlineContext() *triggeredDeadlineContext {
	return &triggeredDeadlineContext{done: make(chan struct{})}
}

func (*triggeredDeadlineContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (ctx *triggeredDeadlineContext) Done() <-chan struct{} { return ctx.done }

func (ctx *triggeredDeadlineContext) Err() error {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	return ctx.err
}

func (*triggeredDeadlineContext) Value(any) any { return nil }

func (ctx *triggeredDeadlineContext) expire() {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.err != nil {
		return
	}
	ctx.err = context.DeadlineExceeded
	close(ctx.done)
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
