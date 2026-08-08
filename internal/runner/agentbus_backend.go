package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/agentbus/engine/adapter/claudecli"
	"github.com/charlesnpx/agentbus/engine/adapter/codexcli"
)

var newEmbeddedCodexEngine = func(options codexcli.Options) engine.Backend {
	return codexcli.New(options)
}

var newEmbeddedClaudeEngine = func(options claudecli.Options) engine.Backend {
	return claudecli.New(options)
}

// legacyClaudeDefaultStallTimeoutSeconds is the 300-second default used by the
// legacy Claude backend when no positive stall timeout was configured.
const legacyClaudeDefaultStallTimeoutSeconds = 300

// embeddedClaudeDefaultStallTimeout retains the legacy default. Tests may
// temporarily override this package variable to exercise the watchdog quickly.
var embeddedClaudeDefaultStallTimeout = time.Duration(legacyClaudeDefaultStallTimeoutSeconds) * time.Second

// embeddedBackend adapts one in-process AgentBus backend to the runner's
// persisted slot interface. The engine owns process supervision; this wrapper
// owns only the runner-visible session state and per-slot environment overlay.
type embeddedBackend struct {
	engineBackend engine.Backend
	backendName   string
	sessionRoot   string
	slotID        string
	label         string
	cwd           string
	profileID     string
	model         string
	effort        string
	sessionID     string
	started       bool
	session       engine.Session

	codexHome      string
	codexHomeReady bool
}

func newEmbeddedCodexBackend(sessionRoot string, slotID string, label string, cwd string, config SlotConfig) *embeddedBackend {
	return newEmbeddedBackend(
		"codex",
		sessionRoot,
		slotID,
		label,
		cwd,
		config,
		newEmbeddedCodexEngine(codexcli.Options{
			Binary:           "codex",
			CachePath:        "",
			SupportedModels:  nil,
			SupportedEfforts: nil,
			// Trusted posture is the user-decided policy for convo-relay.
			WritePolicy: codexcli.WritePolicyTrusted,
		}),
	)
}

func newEmbeddedClaudeBackend(sessionRoot string, slotID string, label string, cwd string, config SlotConfig) *embeddedBackend {
	return newEmbeddedBackend(
		"claude",
		sessionRoot,
		slotID,
		label,
		cwd,
		config,
		newEmbeddedClaudeEngine(claudecli.Options{
			Binary:           "claude",
			CachePath:        "",
			SupportedModels:  nil,
			SupportedEfforts: nil,
		}),
	)
}

func newEmbeddedBackend(backendName string, sessionRoot string, slotID string, label string, cwd string, config SlotConfig, engineBackend engine.Backend) *embeddedBackend {
	if cwd == "" {
		cwd = sessionRoot
	}
	backend := &embeddedBackend{
		engineBackend: engineBackend,
		backendName:   backendName,
		sessionRoot:   sessionRoot,
		slotID:        slotID,
		label:         label,
		cwd:           cwd,
		profileID:     config.ProfileID,
		model:         config.Model,
		effort:        config.Effort,
	}
	if backendName == "codex" {
		backend.codexHome = filepath.Join(sessionRoot, "codex", slotID)
	}
	if backendName == "claude" {
		// Keep Claude's initial persisted state wire-compatible with the
		// subprocess backend. AgentBus Start supplies the provider-confirmed ID.
		sessionID, err := newUUID()
		if err != nil {
			sessionID = fmt.Sprintf("relay-%d", time.Now().UnixNano())
		}
		backend.sessionID = sessionID
	}
	return backend
}

func (b *embeddedBackend) Name() string {
	return b.backendName
}

func (b *embeddedBackend) SlotID() string {
	return b.slotID
}

func (b *embeddedBackend) Label() string {
	return b.label
}

func (b *embeddedBackend) RunTurn(ctx context.Context, prompt string, options TurnOptions) (TurnResult, error) {
	if err := ctx.Err(); err != nil {
		return TurnResult{}, err
	}
	if b.backendName == "codex" {
		if err := b.ensureCodexHome(); err != nil {
			return TurnResult{}, err
		}
	}
	sessionOptions := b.sessionOptions(options.TimeoutSeconds)
	if b.session == nil {
		var (
			session engine.Session
			err     error
		)
		if b.started && b.sessionID != "" {
			session, err = b.engineBackend.Resume(ctx, b.sessionID, sessionOptions)
		} else {
			session, err = b.engineBackend.Start(ctx, sessionOptions)
		}
		if err != nil {
			return TurnResult{}, err
		}
		b.session = session
	}

	stallTimeoutSeconds := options.StallTimeoutSeconds
	watchdogTimeout := embeddedTurnTimeout(stallTimeoutSeconds)
	if b.backendName == "claude" && stallTimeoutSeconds <= 0 {
		stallTimeoutSeconds = legacyClaudeDefaultStallTimeoutSeconds
		watchdogTimeout = embeddedClaudeDefaultStallTimeout
	}
	result, final, err := runEmbeddedTurnWithWatchdogTimeout(ctx, b.session, b.backendName, b.label, prompt, sessionOptions.Write, options.TimeoutSeconds, stallTimeoutSeconds, watchdogTimeout)
	if final != nil && final.TimedOut && result.Content == "" && !result.Recovered {
		var backendErr BackendRunError
		if errors.As(err, &backendErr) && backendErr.Detail == embeddedTurnFailureDetail(final) {
			// The supervised process can report its deadline retirement as both a
			// timeout and a SIGTERM execution failure. Preserve the runner's
			// established timeout outcome when no semantic provider failure was
			// emitted.
			timeoutDetail := fmt.Sprintf("%s timed out after %ds with no recoverable response", b.backendName, options.TimeoutSeconds)
			result.Content = fmt.Sprintf("[%s timed out after %ds]", b.label, options.TimeoutSeconds)
			result.TimedOut = true
			result.ProviderResult.TimedOut = true
			result.ProviderResult.Warnings = append(result.ProviderResult.Warnings, timeoutDetail)
			err = nil
		}
	}
	b.captureSessionID(final)
	if err != nil || result.Stalled || (final != nil && (final.ExecutionFailed || final.TimedOut || final.Canceled)) {
		b.session = nil
	}
	return result, err
}

func (b *embeddedBackend) SessionState() map[string]any {
	sessionIDKey := "session_id"
	if b.backendName == "codex" {
		sessionIDKey = "thread_id"
	}
	state := map[string]any{
		sessionIDKey: b.sessionID,
		"started":    b.started,
		"cwd":        b.cwd,
		"profile_id": nil,
		"model":      nil,
		"effort":     nil,
	}
	if b.profileID != "" {
		state["profile_id"] = b.profileID
	}
	if b.model != "" {
		state["model"] = b.model
	}
	if b.effort != "" {
		state["effort"] = b.effort
	}
	return state
}

func (b *embeddedBackend) RestoreState(state map[string]any, override SlotConfig) error {
	if b.backendName == "codex" {
		return b.restoreCodexState(state, override)
	}
	return b.restoreClaudeState(state, override)
}

func (b *embeddedBackend) restoreCodexState(state map[string]any, override SlotConfig) error {
	if state == nil {
		state = map[string]any{}
	}
	started, ok := state["started"].(bool)
	if state["started"] != nil && !ok {
		return fmt.Errorf("started must be a bool")
	}
	threadID, _ := state["thread_id"].(string)
	if started && threadID == "" {
		return fmt.Errorf("started codex slots require thread_id")
	}
	cwd, _ := state["cwd"].(string)
	profileID, _ := state["profile_id"].(string)
	model, _ := state["model"].(string)
	effort, _ := state["effort"].(string)
	b.sessionID = threadID
	b.started = started || threadID != ""
	b.session = nil
	if cwd != "" {
		b.cwd = cwd
	}
	b.applyStateConfig(profileID, model, effort, override)
	return nil
}

func (b *embeddedBackend) restoreClaudeState(state map[string]any, override SlotConfig) error {
	if state == nil {
		state = map[string]any{}
	}
	sessionID, ok := state["session_id"].(string)
	if !ok || sessionID == "" {
		return fmt.Errorf("session_id must be a non-empty string")
	}
	if err := validateClaudeSessionID(sessionID); err != nil {
		return err
	}
	started, ok := state["started"].(bool)
	if state["started"] != nil && !ok {
		return fmt.Errorf("started must be a bool")
	}
	cwd, ok := state["cwd"].(string)
	if state["cwd"] != nil && !ok {
		return fmt.Errorf("cwd must be a string")
	}
	profileID, ok := state["profile_id"].(string)
	if state["profile_id"] != nil && !ok {
		return fmt.Errorf("profile_id must be a string")
	}
	model, ok := state["model"].(string)
	if state["model"] != nil && !ok {
		return fmt.Errorf("model must be a string")
	}
	effort, ok := state["effort"].(string)
	if state["effort"] != nil && !ok {
		return fmt.Errorf("effort must be a string")
	}
	b.sessionID = sessionID
	b.started = started || state["started"] == nil
	b.session = nil
	if cwd != "" {
		b.cwd = cwd
	}
	b.applyStateConfig(profileID, model, effort, override)
	return nil
}

func (b *embeddedBackend) applyStateConfig(profileID string, model string, effort string, override SlotConfig) {
	if override.ProfileID != "" {
		b.profileID = override.ProfileID
	} else if profileID != "" {
		b.profileID = profileID
	}
	if override.Model != "" {
		b.model = override.Model
	} else if model != "" {
		b.model = model
	}
	if override.Effort != "" {
		b.effort = override.Effort
	} else if effort != "" {
		b.effort = effort
	}
}

func (b *embeddedBackend) Cleanup() error {
	if b.backendName != "claude" {
		return nil
	}
	projectDir, jsonlPath, sessionDir, err := claudeCleanupPaths(b.cwd, b.sessionID)
	if err != nil {
		return err
	}
	if err := os.Remove(jsonlPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(sessionDir); err != nil {
		return err
	}
	if err := os.Remove(projectDir); err != nil && !os.IsNotExist(err) {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil
		}
		return err
	}
	return nil
}

func claudeCleanupPaths(cwd string, sessionID string) (string, string, string, error) {
	if err := validateClaudeSessionID(sessionID); err != nil {
		return "", "", "", err
	}
	projectDir := filepath.Clean(claudeProjectDir(cwd))
	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	sessionDir := filepath.Join(projectDir, sessionID)
	if filepath.Clean(filepath.Dir(jsonlPath)) != projectDir || filepath.Clean(filepath.Dir(sessionDir)) != projectDir {
		return "", "", "", fmt.Errorf("session_id resolves outside the Claude project directory")
	}
	return projectDir, jsonlPath, sessionDir, nil
}

func (b *embeddedBackend) sessionOptions(timeoutSeconds int) engine.SessionOpts {
	options := engine.SessionOpts{
		CWD:     b.cwd,
		Write:   true,
		Model:   b.model,
		Effort:  b.effort,
		Timeout: embeddedTurnTimeout(timeoutSeconds),
	}
	if b.backendName == "codex" {
		options.EnvOverlay = map[string]string{"CODEX_HOME": b.codexHome}
	}
	return options
}

func (b *embeddedBackend) captureSessionID(final *engine.TurnFinalObservation) {
	if b.session == nil {
		return
	}
	sessionID := b.session.ID()
	if final != nil && final.BackendSessionID != "" {
		sessionID = final.BackendSessionID
	}
	if sessionID == "" {
		return
	}
	b.sessionID = sessionID
	b.started = true
}

func (b *embeddedBackend) ensureCodexHome() error {
	if b.codexHomeReady {
		return nil
	}
	if err := os.MkdirAll(b.codexHome, 0o755); err != nil {
		return err
	}
	realCodexHome, err := os.UserHomeDir()
	if err == nil {
		realCodexHome = filepath.Join(realCodexHome, ".codex")
		for _, name := range []string{"auth.json", "config.toml"} {
			src := filepath.Join(realCodexHome, name)
			dst := filepath.Join(b.codexHome, name)
			if _, err := os.Stat(src); err == nil {
				if _, err := os.Lstat(dst); os.IsNotExist(err) {
					_ = os.Symlink(src, dst)
				}
			}
		}
	}
	b.codexHomeReady = true
	return nil
}

func validateClaudeSessionID(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session_id must be a non-empty string")
	}
	if sessionID == "." || sessionID == ".." {
		return fmt.Errorf("session_id must be a safe path component")
	}
	for _, ch := range sessionID {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return fmt.Errorf("session_id must be a safe path component")
	}
	return nil
}

func newUUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	hexText := hex.EncodeToString(data[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexText[0:8], hexText[8:12], hexText[12:16], hexText[16:20], hexText[20:32]), nil
}
