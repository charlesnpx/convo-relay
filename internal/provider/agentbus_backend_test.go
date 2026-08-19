package provider

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/agentbus/engine/adapter/codexcli"
)

type fakeEmbeddedEngineBackend struct {
	name          string
	startSession  engine.Session
	startSessions []engine.Session
	resumeSession engine.Session
	startErr      error
	resumeErr     error
	startOptions  []engine.SessionOpts
	resumeCalls   []fakeEmbeddedResumeCall
}

type fakeEmbeddedResumeCall struct {
	id      string
	options engine.SessionOpts
}

func (b *fakeEmbeddedEngineBackend) Name() string {
	return b.name
}

func (b *fakeEmbeddedEngineBackend) Preflight(context.Context) (engine.Health, error) {
	return engine.Health{Backend: b.name}, nil
}

func (b *fakeEmbeddedEngineBackend) Start(_ context.Context, options engine.SessionOpts) (engine.Session, error) {
	b.startOptions = append(b.startOptions, options)
	if b.startErr != nil {
		return nil, b.startErr
	}
	if call := len(b.startOptions) - 1; call < len(b.startSessions) {
		return b.startSessions[call], nil
	}
	return b.startSession, nil
}

func (b *fakeEmbeddedEngineBackend) Resume(_ context.Context, id string, options engine.SessionOpts) (engine.Session, error) {
	b.resumeCalls = append(b.resumeCalls, fakeEmbeddedResumeCall{id: id, options: options})
	if b.resumeErr != nil {
		return nil, b.resumeErr
	}
	return b.resumeSession, nil
}

type fakeEmbeddedSession struct {
	id             string
	events         []engine.Event
	turnErr        error
	turnInputs     []engine.TurnInput
	interruptCalls int
	onTurn         func(context.Context, engine.TurnInput) (<-chan engine.Event, error)
	onInterrupt    func(context.Context) error
}

func (s *fakeEmbeddedSession) ID() string {
	return s.id
}

func (s *fakeEmbeddedSession) Turn(ctx context.Context, input engine.TurnInput) (<-chan engine.Event, error) {
	s.turnInputs = append(s.turnInputs, input)
	if s.onTurn != nil {
		return s.onTurn(ctx, input)
	}
	if s.turnErr != nil {
		return nil, s.turnErr
	}
	events := make(chan engine.Event, len(s.events))
	for _, event := range s.events {
		events <- event
	}
	close(events)
	return events, nil
}

func (s *fakeEmbeddedSession) Interrupt(ctx context.Context) error {
	s.interruptCalls++
	if s.onInterrupt != nil {
		return s.onInterrupt(ctx)
	}
	return nil
}

func TestEmbeddedCodexStateRoundTripAndValidation(t *testing.T) {
	backend := newEmbeddedBackend("codex", t.TempDir(), "slot_0", "Codex", "/initial", SlotConfig{}, &fakeEmbeddedEngineBackend{name: "codex"})
	initial := backend.SessionState()
	wantInitial := map[string]any{
		"thread_id":  "",
		"started":    false,
		"cwd":        "/initial",
		"profile_id": nil,
		"model":      nil,
		"effort":     nil,
	}
	if !reflect.DeepEqual(initial, wantInitial) {
		t.Fatalf("initial state = %#v, want %#v", initial, wantInitial)
	}
	if err := backend.RestoreState(initial, SlotConfig{}); err != nil {
		t.Fatalf("round-trip initial state: %v", err)
	}

	state := map[string]any{
		"thread_id":  "thread-restored",
		"started":    true,
		"cwd":        "/restored",
		"profile_id": "stored-profile",
		"model":      "stored-model",
		"effort":     "stored-effort",
	}
	if err := backend.RestoreState(state, SlotConfig{Model: "override-model"}); err != nil {
		t.Fatalf("restore state: %v", err)
	}
	wantRestored := map[string]any{
		"thread_id":  "thread-restored",
		"started":    true,
		"cwd":        "/restored",
		"profile_id": "stored-profile",
		"model":      "override-model",
		"effort":     "stored-effort",
	}
	if got := backend.SessionState(); !reflect.DeepEqual(got, wantRestored) {
		t.Fatalf("restored state = %#v, want %#v", got, wantRestored)
	}
	if err := backend.RestoreState(map[string]any{"started": true}, SlotConfig{}); err == nil || err.Error() != "started codex slots require thread_id" {
		t.Fatalf("missing thread error = %v", err)
	}
	if err := backend.RestoreState(map[string]any{"started": "yes"}, SlotConfig{}); err == nil || err.Error() != "started must be a bool" {
		t.Fatalf("invalid started error = %v", err)
	}
}

func TestEmbeddedClaudeStateRoundTripAndValidation(t *testing.T) {
	backend := newEmbeddedBackend("claude", t.TempDir(), "slot_1", "Claude Code", "/initial", SlotConfig{}, &fakeEmbeddedEngineBackend{name: "claude"})
	initial := backend.SessionState()
	if sessionID, ok := initial["session_id"].(string); !ok || sessionID == "" {
		t.Fatalf("initial session_id = %#v, want a non-empty string", initial["session_id"])
	}
	if initial["started"] != false || initial["cwd"] != "/initial" || initial["profile_id"] != nil || initial["model"] != nil || initial["effort"] != nil {
		t.Fatalf("initial claude state = %#v", initial)
	}
	if err := backend.RestoreState(initial, SlotConfig{}); err != nil {
		t.Fatalf("round-trip initial state: %v", err)
	}

	state := map[string]any{
		"session_id": "claude-restored",
		"started":    true,
		"cwd":        "/restored",
		"profile_id": "stored-profile",
		"model":      "stored-model",
		"effort":     "stored-effort",
	}
	if err := backend.RestoreState(state, SlotConfig{Effort: "override-effort"}); err != nil {
		t.Fatalf("restore state: %v", err)
	}
	wantRestored := map[string]any{
		"session_id": "claude-restored",
		"started":    true,
		"cwd":        "/restored",
		"profile_id": "stored-profile",
		"model":      "stored-model",
		"effort":     "override-effort",
	}
	if got := backend.SessionState(); !reflect.DeepEqual(got, wantRestored) {
		t.Fatalf("restored state = %#v, want %#v", got, wantRestored)
	}
	if err := backend.RestoreState(map[string]any{"started": true}, SlotConfig{}); err == nil || err.Error() != "session_id must be a non-empty string" {
		t.Fatalf("missing session error = %v", err)
	}
	if err := backend.RestoreState(map[string]any{"session_id": "nested/session"}, SlotConfig{}); err == nil || err.Error() != "session_id must be a safe path component" {
		t.Fatalf("unsafe session error = %v", err)
	}
	if err := backend.RestoreState(map[string]any{"session_id": "claude-id", "model": 7}, SlotConfig{}); err == nil || err.Error() != "model must be a string" {
		t.Fatalf("invalid model error = %v", err)
	}
}

func TestEmbeddedCodexUsesHomeOverlayTrustedPolicyAndLiveSession(t *testing.T) {
	engineBackend := &fakeEmbeddedEngineBackend{name: "codex"}
	originalFactory := newEmbeddedCodexEngine
	var captured codexcli.Options
	newEmbeddedCodexEngine = func(options codexcli.Options) engine.Backend {
		captured = options
		return engineBackend
	}
	t.Cleanup(func() {
		newEmbeddedCodexEngine = originalFactory
	})

	root := t.TempDir()
	session := &fakeEmbeddedSession{
		id: "session-id",
		events: []engine.Event{{
			Type: engine.EventTurnFinal,
			TurnFinal: &engine.TurnFinalObservation{
				BackendSessionID: "thread-confirmed",
				ReturnCodeKnown:  true,
			},
		}},
	}
	engineBackend.startSession = session
	backend := newEmbeddedCodexBackend(root, "slot_7", "Codex", "/workspace", SlotConfig{Model: "model-a", Effort: "high"})
	if captured.WritePolicy != codexcli.WritePolicyTrusted {
		t.Fatalf("write policy = %v, want trusted", captured.WritePolicy)
	}
	// The overlay is independent of home seeding. Keep this fake-engine test
	// hermetic rather than consulting the operator's ~/.codex directory.
	backend.codexHomeReady = true

	for _, prompt := range []string{"first", "second"} {
		if _, err := backend.RunTurn(context.Background(), prompt, TurnOptions{TimeoutSeconds: 7}); err != nil {
			t.Fatalf("run %q: %v", prompt, err)
		}
	}
	if len(engineBackend.startOptions) != 1 {
		t.Fatalf("start calls = %d, want one", len(engineBackend.startOptions))
	}
	if len(engineBackend.resumeCalls) != 0 {
		t.Fatalf("resume calls = %#v, want none", engineBackend.resumeCalls)
	}
	startOptions := engineBackend.startOptions[0]
	wantHome := filepath.Join(root, "codex", "slot_7")
	if startOptions.EnvOverlay["CODEX_HOME"] != wantHome {
		t.Fatalf("CODEX_HOME overlay = %q, want %q", startOptions.EnvOverlay["CODEX_HOME"], wantHome)
	}
	if startOptions.CWD != "/workspace" || !startOptions.Write || startOptions.Model != "model-a" || startOptions.Effort != "high" || startOptions.Timeout != 7*time.Second {
		t.Fatalf("start options = %#v", startOptions)
	}
	if len(session.turnInputs) != 2 {
		t.Fatalf("turn calls = %d, want two", len(session.turnInputs))
	}
	if session.turnInputs[0].Prompt != "first" || !session.turnInputs[0].Write || session.turnInputs[0].Timeout != 7*time.Second {
		t.Fatalf("first turn input = %#v", session.turnInputs[0])
	}
	if got := backend.SessionState()["thread_id"]; got != "thread-confirmed" {
		t.Fatalf("thread_id = %#v, want provider-confirmed id", got)
	}
}

func TestEmbeddedBackendDropsErroredSessionAndResumesConfirmedID(t *testing.T) {
	failedSession := &fakeEmbeddedSession{
		id: "live-session-id",
		events: []engine.Event{{
			Type: engine.EventTurnFinal,
			TurnFinal: &engine.TurnFinalObservation{
				BackendSessionID: "thread-confirmed",
				ExecutionFailed:  true,
			},
		}},
	}
	resumedSession := &fakeEmbeddedSession{events: []engine.Event{{
		Type:      engine.EventTurnFinal,
		TurnFinal: &engine.TurnFinalObservation{BackendSessionID: "thread-confirmed"},
	}}}
	engineBackend := &fakeEmbeddedEngineBackend{
		name:          "codex",
		startSession:  failedSession,
		resumeSession: resumedSession,
	}
	backend := newEmbeddedBackend("codex", t.TempDir(), "slot_0", "Codex", "/workspace", SlotConfig{}, engineBackend)
	backend.codexHomeReady = true

	if _, err := backend.RunTurn(context.Background(), "first", TurnOptions{}); err == nil {
		t.Fatal("first turn succeeded, want an error")
	}
	if backend.session != nil {
		t.Fatalf("session = %#v after errored turn, want nil", backend.session)
	}
	if backend.sessionID != "thread-confirmed" || !backend.started {
		t.Fatalf("captured state = sessionID %q, started %t", backend.sessionID, backend.started)
	}

	if _, err := backend.RunTurn(context.Background(), "second", TurnOptions{}); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if len(engineBackend.startOptions) != 1 {
		t.Fatalf("start calls = %d, want one", len(engineBackend.startOptions))
	}
	if len(engineBackend.resumeCalls) != 1 || engineBackend.resumeCalls[0].id != "thread-confirmed" {
		t.Fatalf("resume calls = %#v, want one for thread-confirmed", engineBackend.resumeCalls)
	}
}

func TestEmbeddedBackendDropsRecoveredExecutionFailedSessionAndResumesConfirmedID(t *testing.T) {
	failedSession := &fakeEmbeddedSession{
		id: "live-session-id",
		events: []engine.Event{
			{Type: engine.EventAgentText, Text: "partial response"},
			{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{
				BackendSessionID: "thread-confirmed",
				ExecutionFailed:  true,
			}},
		},
	}
	resumedSession := &fakeEmbeddedSession{events: []engine.Event{{
		Type:      engine.EventTurnFinal,
		TurnFinal: &engine.TurnFinalObservation{BackendSessionID: "thread-confirmed"},
	}}}
	engineBackend := &fakeEmbeddedEngineBackend{
		name:          "codex",
		startSession:  failedSession,
		resumeSession: resumedSession,
	}
	backend := newEmbeddedBackend("codex", t.TempDir(), "slot_0", "Codex", "/workspace", SlotConfig{}, engineBackend)
	backend.codexHomeReady = true

	result, err := backend.RunTurn(context.Background(), "first", TurnOptions{})
	if err != nil {
		t.Fatalf("recovered turn: %v", err)
	}
	if result.Content != "partial response" || !result.Recovered || !result.ProviderResult.Recovered {
		t.Fatalf("recovered result = %#v", result)
	}
	if backend.session != nil {
		t.Fatalf("session = %#v after recovered execution failure, want nil", backend.session)
	}
	if backend.sessionID != "thread-confirmed" || !backend.started {
		t.Fatalf("captured state = sessionID %q, started %t", backend.sessionID, backend.started)
	}

	if _, err := backend.RunTurn(context.Background(), "second", TurnOptions{}); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if len(engineBackend.startOptions) != 1 {
		t.Fatalf("start calls = %d, want one", len(engineBackend.startOptions))
	}
	if len(engineBackend.resumeCalls) != 1 || engineBackend.resumeCalls[0].id != "thread-confirmed" {
		t.Fatalf("resume calls = %#v, want one for thread-confirmed", engineBackend.resumeCalls)
	}
}

func TestEmbeddedBackendDropsStalledSessionAndResumesConfirmedID(t *testing.T) {
	events := make(chan engine.Event, 1)
	stalledSession := &fakeEmbeddedSession{id: "live-session-id"}
	stalledSession.onTurn = func(context.Context, engine.TurnInput) (<-chan engine.Event, error) {
		return events, nil
	}
	stalledSession.onInterrupt = func(context.Context) error {
		events <- engine.Event{
			Type:      engine.EventTurnFinal,
			TurnFinal: &engine.TurnFinalObservation{BackendSessionID: "thread-confirmed"},
		}
		close(events)
		return nil
	}
	resumedSession := &fakeEmbeddedSession{events: []engine.Event{{
		Type:      engine.EventTurnFinal,
		TurnFinal: &engine.TurnFinalObservation{BackendSessionID: "thread-confirmed"},
	}}}
	engineBackend := &fakeEmbeddedEngineBackend{
		name:          "codex",
		startSession:  stalledSession,
		resumeSession: resumedSession,
	}
	backend := newEmbeddedBackend("codex", t.TempDir(), "slot_0", "Codex", "/workspace", SlotConfig{}, engineBackend)
	backend.codexHomeReady = true

	result, err := backend.RunTurn(context.Background(), "first", TurnOptions{StallTimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("stalled turn: %v", err)
	}
	if result.Content != "[Codex stalled after 1s of no stream activity]" || !result.Stalled || !result.ProviderResult.Stalled || result.Recovered {
		t.Fatalf("stalled result = %#v", result)
	}
	if !reflect.DeepEqual(result.ProviderResult.Warnings, []string{"codex stalled - no stream activity for 1s, interrupting turn"}) {
		t.Fatalf("stalled warnings = %#v", result.ProviderResult.Warnings)
	}
	if stalledSession.interruptCalls != 1 {
		t.Fatalf("interrupt calls = %d, want one", stalledSession.interruptCalls)
	}
	if backend.session != nil {
		t.Fatalf("session = %#v after stalled turn, want nil", backend.session)
	}
	if backend.sessionID != "thread-confirmed" || !backend.started {
		t.Fatalf("captured state = sessionID %q, started %t", backend.sessionID, backend.started)
	}

	if _, err := backend.RunTurn(context.Background(), "second", TurnOptions{}); err != nil {
		t.Fatalf("resumed turn: %v", err)
	}
	if len(engineBackend.startOptions) != 1 {
		t.Fatalf("start calls = %d, want one", len(engineBackend.startOptions))
	}
	if len(engineBackend.resumeCalls) != 1 || engineBackend.resumeCalls[0].id != "thread-confirmed" {
		t.Fatalf("resume calls = %#v, want one for thread-confirmed", engineBackend.resumeCalls)
	}
}

func TestEmbeddedBackendRestartsAfterErroredTurnWithoutConfirmedID(t *testing.T) {
	failedSession := &fakeEmbeddedSession{}
	freshSession := &fakeEmbeddedSession{events: []engine.Event{{
		Type:      engine.EventTurnFinal,
		TurnFinal: &engine.TurnFinalObservation{},
	}}}
	engineBackend := &fakeEmbeddedEngineBackend{
		name:          "codex",
		startSessions: []engine.Session{failedSession, freshSession},
	}
	backend := newEmbeddedBackend("codex", t.TempDir(), "slot_0", "Codex", "/workspace", SlotConfig{}, engineBackend)
	backend.codexHomeReady = true

	if _, err := backend.RunTurn(context.Background(), "first", TurnOptions{}); err == nil {
		t.Fatal("first turn succeeded, want an error")
	}
	if backend.session != nil {
		t.Fatalf("session = %#v after errored turn, want nil", backend.session)
	}
	if backend.sessionID != "" || backend.started {
		t.Fatalf("captured state = sessionID %q, started %t, want no confirmed ID", backend.sessionID, backend.started)
	}

	if _, err := backend.RunTurn(context.Background(), "second", TurnOptions{}); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if len(engineBackend.startOptions) != 2 {
		t.Fatalf("start calls = %d, want two", len(engineBackend.startOptions))
	}
	if len(engineBackend.resumeCalls) != 0 {
		t.Fatalf("resume calls = %#v, want none", engineBackend.resumeCalls)
	}
}

func TestEmbeddedBackendReusesSuccessfulSessionAndForwardsTurnInput(t *testing.T) {
	session := &fakeEmbeddedSession{
		id: "session-id",
		events: []engine.Event{{
			Type:      engine.EventTurnFinal,
			TurnFinal: &engine.TurnFinalObservation{},
		}},
	}
	engineBackend := &fakeEmbeddedEngineBackend{name: "claude", startSession: session}
	backend := newEmbeddedBackend("claude", t.TempDir(), "slot_0", "Claude Code", "/workspace", SlotConfig{}, engineBackend)

	for _, prompt := range []string{"first", "second"} {
		if _, err := backend.RunTurn(context.Background(), prompt, TurnOptions{TimeoutSeconds: 9}); err != nil {
			t.Fatalf("run %q: %v", prompt, err)
		}
	}
	if len(engineBackend.startOptions) != 1 {
		t.Fatalf("start calls = %d, want one", len(engineBackend.startOptions))
	}
	if len(engineBackend.resumeCalls) != 0 {
		t.Fatalf("resume calls = %#v, want none", engineBackend.resumeCalls)
	}
	if len(session.turnInputs) != 2 {
		t.Fatalf("turn calls = %d, want two", len(session.turnInputs))
	}
	input := session.turnInputs[0]
	if input.Prompt != "first" || !input.Write || input.Timeout != 9*time.Second {
		t.Fatalf("first turn input = %#v", input)
	}
	if input.Write != engineBackend.startOptions[0].Write {
		t.Fatalf("turn write = %t, session write = %t", input.Write, engineBackend.startOptions[0].Write)
	}
}

func TestEmbeddedClaudeBackendUsesLegacyDefaultStallWatchdog(t *testing.T) {
	originalDefault := embeddedClaudeDefaultStallTimeout
	embeddedClaudeDefaultStallTimeout = 10 * time.Millisecond // Test-only override avoids a five-minute wait.
	t.Cleanup(func() {
		embeddedClaudeDefaultStallTimeout = originalDefault
	})

	events := make(chan engine.Event, 1)
	session := &fakeEmbeddedSession{}
	session.onTurn = func(context.Context, engine.TurnInput) (<-chan engine.Event, error) {
		return events, nil
	}
	session.onInterrupt = func(context.Context) error {
		events <- engine.Event{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{}}
		close(events)
		return nil
	}
	engineBackend := &fakeEmbeddedEngineBackend{name: "claude", startSession: session}
	backend := newEmbeddedBackend("claude", t.TempDir(), "slot_0", "Claude Code", "/workspace", SlotConfig{}, engineBackend)

	result, err := backend.RunTurn(context.Background(), "first", TurnOptions{})
	if err != nil {
		t.Fatalf("stalled turn: %v", err)
	}
	if result.Content != "[Claude Code stalled after 300s of no stream activity]" || !result.Stalled || !result.ProviderResult.Stalled || result.Recovered {
		t.Fatalf("stalled result = %#v", result)
	}
	if !reflect.DeepEqual(result.ProviderResult.Warnings, []string{"claude stalled - no stream activity for 300s, interrupting turn"}) {
		t.Fatalf("stalled warnings = %#v", result.ProviderResult.Warnings)
	}
	if session.interruptCalls != 1 {
		t.Fatalf("interrupt calls = %d, want one", session.interruptCalls)
	}
}

func TestEmbeddedBackendResumesRestoredSessionLazily(t *testing.T) {
	resumedSession := &fakeEmbeddedSession{events: []engine.Event{{
		Type: engine.EventTurnFinal,
		TurnFinal: &engine.TurnFinalObservation{
			BackendSessionID: "thread-restored",
			ReturnCodeKnown:  true,
		},
	}}}
	engineBackend := &fakeEmbeddedEngineBackend{name: "codex", resumeSession: resumedSession}
	backend := newEmbeddedBackend("codex", t.TempDir(), "slot_0", "Codex", "/workspace", SlotConfig{}, engineBackend)
	if err := backend.RestoreState(map[string]any{"thread_id": "thread-restored", "started": true}, SlotConfig{}); err != nil {
		t.Fatalf("restore state: %v", err)
	}
	if _, err := backend.RunTurn(context.Background(), "continue", TurnOptions{}); err != nil {
		t.Fatalf("run restored turn: %v", err)
	}
	if len(engineBackend.startOptions) != 0 {
		t.Fatalf("start calls = %d, want none", len(engineBackend.startOptions))
	}
	if len(engineBackend.resumeCalls) != 1 || engineBackend.resumeCalls[0].id != "thread-restored" {
		t.Fatalf("resume calls = %#v", engineBackend.resumeCalls)
	}
}

func TestEmbeddedTurnReturnsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	session := &fakeEmbeddedSession{
		onTurn: func(ctx context.Context, _ engine.TurnInput) (<-chan engine.Event, error) {
			events := make(chan engine.Event, 1)
			close(started)
			go func() {
				<-ctx.Done()
				events <- engine.Event{Type: engine.EventTurnFinal, TurnFinal: &engine.TurnFinalObservation{Canceled: true}}
				close(events)
			}()
			return events, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type turnOutcome struct {
		err error
	}
	done := make(chan turnOutcome, 1)
	go func() {
		_, _, err := runEmbeddedTurn(ctx, session, "codex", "Codex", "wait", true, 0, 0)
		done <- turnOutcome{err: err}
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want context.Canceled", outcome.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not return after cancellation")
	}
}
