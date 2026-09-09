package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/v2/internal/recipes"
)

type TurnOptions struct {
	TimeoutSeconds      int
	StallTimeoutSeconds int
}

type TurnResult struct {
	Content           string
	TimedOut          bool
	Stalled           bool
	Recovered         bool
	ProviderResult    ProviderResult
	ProviderResultRef Metadata
}

type SlotConfig struct {
	ProfileID       string
	Model           string
	Effort          string
	SettingsPath    string
	CompositionPath string
	RuntimeConfig   recipes.RuntimeConfig
	Depth           int
	MaxDepth        int
}

// providerState keeps the provider-owned persisted state wire-compatible with
// existing sessions while preventing the raw session metadata map from
// crossing the exported provider boundary.
type providerState = map[string]any

// SlotState is the persisted state of one provider slot.
type SlotState = providerState

// Metadata is provider-owned structured metadata retained for compatibility.
type Metadata = providerState

type Backend interface {
	Name() string
	SlotID() string
	Label() string
	RunTurn(context.Context, string, TurnOptions) (TurnResult, error)
	SessionState() SlotState
	RestoreState(SlotState, SlotConfig) error
	Cleanup() error
}

type processResult struct {
	Stdout     string
	Stderr     string
	ReturnCode int
	TimedOut   bool
}

var backendLabels = map[string]string{
	"claude": "Claude Code",
	"codex":  "Codex",
	"gemini": "Gemini",
}

func knownBackend(name string) bool {
	_, ok := backendLabels[strings.TrimSpace(name)]
	return ok
}

func backendLabel(name string) string {
	name = strings.TrimSpace(name)
	if label, ok := backendLabels[name]; ok {
		return label
	}
	return name
}

func newBackend(backendName string, sessionRoot string, slotID string, label string, cwd string, config SlotConfig) (Backend, error) {
	backendName = strings.TrimSpace(backendName)
	if label == "" {
		label = backendLabel(backendName)
	}
	switch backendName {
	case "claude":
		return newEmbeddedClaudeBackend(sessionRoot, slotID, label, cwd, config), nil
	case "codex":
		return newEmbeddedCodexBackend(sessionRoot, slotID, label, cwd, config), nil
	case "gemini":
		return newGeminiBackend(sessionRoot, slotID, label, cwd, config), nil
	default:
		return nil, fmt.Errorf("unsupported backend %q for Go runner", backendName)
	}
}

// NewBackend constructs one declared external provider adapter.
func NewBackend(backendName string, sessionRoot string, slotID string, label string, cwd string, config SlotConfig) (Backend, error) {
	return newBackend(backendName, sessionRoot, slotID, label, cwd, config)
}

func KnownBackend(name string) bool {
	return knownBackend(name)
}

func BackendLabel(name string) string {
	return backendLabel(name)
}

func FacilitatorBackendAllowed(name string) bool {
	return facilitatorBackendAllowed(name)
}

func facilitatorBackendAllowed(name string) bool {
	switch strings.TrimSpace(name) {
	case "claude", "codex", "gemini":
		return true
	default:
		return false
	}
}

func runSubprocess(ctx context.Context, command []string, prompt string, cwd string, env []string, timeoutSeconds int) (processResult, error) {
	if len(command) == 0 {
		return processResult{ReturnCode: -1}, fmt.Errorf("empty command")
	}
	runCtx := ctx
	cancel := func() {}
	if timeoutSeconds > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	}
	defer cancel()

	cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = cwd
	cmd.Env = env
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := processResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ReturnCode: 0,
		TimedOut:   errors.Is(runCtx.Err(), context.DeadlineExceeded),
	}
	if cmd.ProcessState != nil {
		result.ReturnCode = cmd.ProcessState.ExitCode()
	} else if err != nil {
		result.ReturnCode = -1
	}
	if errors.Is(ctx.Err(), context.Canceled) && !result.TimedOut {
		return result, ctx.Err()
	}
	if result.TimedOut {
		return result, nil
	}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if result.ReturnCode == 0 {
			result.ReturnCode = exitErr.ExitCode()
		}
		return result, nil
	}
	return result, err
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
