package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

type rootBackendCall struct {
	ContextID string
	Backend   string
	SlotID    string
	Label     string
	CWD       string
	Prompt    string
	Call      int
	Options   TurnOptions
}

type rootBackendRecorder struct {
	mu      sync.Mutex
	nextID  int
	calls   []rootBackendCall
	handler func(context.Context, rootBackendCall) (TurnResult, error)
}

type recordedRootBackend struct {
	recorder  *rootBackendRecorder
	contextID string
	backend   string
	slotID    string
	label     string
	cwd       string
	config    SlotConfig
	callCount int
}

func (r *rootBackendRecorder) factory() rootBackendFactory {
	return func(backend string, _ string, slotID string, label string, cwd string, config SlotConfig) (Backend, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.nextID++
		return &recordedRootBackend{
			recorder:  r,
			contextID: fmt.Sprintf("context-%d", r.nextID),
			backend:   backend,
			slotID:    slotID,
			label:     label,
			cwd:       cwd,
			config:    config,
		}, nil
	}
}

func (r *rootBackendRecorder) snapshotCalls() []rootBackendCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rootBackendCall{}, r.calls...)
}

func (b *recordedRootBackend) Name() string   { return b.backend }
func (b *recordedRootBackend) SlotID() string { return b.slotID }
func (b *recordedRootBackend) Label() string  { return b.label }

func (b *recordedRootBackend) RunTurn(ctx context.Context, prompt string, options TurnOptions) (TurnResult, error) {
	b.recorder.mu.Lock()
	b.callCount++
	call := rootBackendCall{
		ContextID: b.contextID,
		Backend:   b.backend,
		SlotID:    b.slotID,
		Label:     b.label,
		CWD:       b.cwd,
		Prompt:    prompt,
		Call:      b.callCount,
		Options:   options,
	}
	b.recorder.calls = append(b.recorder.calls, call)
	handler := b.recorder.handler
	b.recorder.mu.Unlock()
	if handler != nil {
		return handler(ctx, call)
	}
	if b.slotID == "facilitator" {
		return successfulRootTurn(b.backend, "{\"settled\":[\"complete\"],\"contested\":[],\"withdrawn\":[]}"), nil
	}
	return successfulRootTurn(b.backend, fmt.Sprintf("response from %s call %d", b.slotID, call.Call)), nil
}

func (b *recordedRootBackend) SessionState() map[string]any {
	b.recorder.mu.Lock()
	defer b.recorder.mu.Unlock()
	return map[string]any{
		"started":    b.callCount > 0,
		"cwd":        b.cwd,
		"profile_id": emptyStringAsNil(b.config.ProfileID),
		"model":      emptyStringAsNil(b.config.Model),
		"effort":     emptyStringAsNil(b.config.Effort),
		"context_id": b.contextID,
		"call_count": b.callCount,
	}
}

func (b *recordedRootBackend) RestoreState(map[string]any, SlotConfig) error { return nil }
func (b *recordedRootBackend) Cleanup() error                                { return nil }

func successfulRootTurn(backend string, content string) TurnResult {
	return TurnResult{
		Content: content,
		ProviderResult: ProviderResult{
			Backend:         backend,
			ReturnCode:      0,
			ReturnCodeKnown: true,
			Warnings:        []string{},
		},
	}
}

func successfulRootBackendFactory() rootBackendFactory {
	return (&rootBackendRecorder{}).factory()
}

func TestRunRecipeExecutesExactAlternatingParticipantsAndFacilitator(t *testing.T) {
	config := rootRecipeRuntimeConfig("")
	config.RelayRecipes["neutral-root"]["participant_turns"] = 4
	config.RelayRecipes["neutral-root"]["max_rounds"] = 4
	recorder := &rootBackendRecorder{}
	sessionDir := filepath.Join(t.TempDir(), "session")

	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:          sessionDir,
		Task:                "Complete every compiled participant turn",
		RecipeID:            "neutral-root",
		LaunchCWD:           t.TempDir(),
		TimeoutSeconds:      37,
		StallTimeoutSeconds: 19,
		RuntimeConfig:       config,
		ReadinessCheck:      readyRootRecipeCheck,
		backendFactory:      recorder.factory(),
	})
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	if result["status"] != rootParticipantsCompleteStatus ||
		intFromAny(result["actual_participant_turns"], 0) != 4 ||
		intFromAny(result["actual_rounds"], 0) != 4 {
		t.Fatalf("root participant result = %#v", result)
	}
	calls := recorder.snapshotCalls()
	wantOrder := []string{"slot_0", "facilitator", "slot_1", "facilitator", "slot_0", "facilitator", "slot_1", "facilitator"}
	if len(calls) != len(wantOrder) {
		t.Fatalf("provider calls = %d, want %d: %#v", len(calls), len(wantOrder), calls)
	}
	contexts := map[string]string{}
	for index, call := range calls {
		if call.SlotID != wantOrder[index] {
			t.Fatalf("provider call %d slot = %q, want %q: %#v", index, call.SlotID, wantOrder[index], calls)
		}
		if call.Options.TimeoutSeconds != 37 || call.Options.StallTimeoutSeconds != 19 {
			t.Fatalf("%s turn options = %#v", call.SlotID, call.Options)
		}
		if previous, exists := contexts[call.SlotID]; exists && previous != call.ContextID {
			t.Fatalf("slot %s changed provider context from %s to %s", call.SlotID, previous, call.ContextID)
		}
		contexts[call.SlotID] = call.ContextID
	}
	if len(contexts) != 3 || contexts["slot_0"] == contexts["slot_1"] || contexts["slot_0"] == contexts["facilitator"] || contexts["slot_1"] == contexts["facilitator"] {
		t.Fatalf("provider contexts are not separate: %#v", contexts)
	}
	transcript := result["transcript"].([]any)
	if len(transcript) != 4 {
		t.Fatalf("transcript entries = %d, want 4", len(transcript))
	}
	for index, raw := range transcript {
		entry := raw.(map[string]any)
		if intFromAny(entry["round"], 0) != index+1 || entry["provider_result"] == nil || entry["facilitator_output_ref"] == nil {
			t.Fatalf("transcript entry %d = %#v", index+1, entry)
		}
		participantProvider := entry["provider_result"].(map[string]any)
		facilitatorProvider := entry["facilitator_provider_result"].(map[string]any)
		if strings.TrimSpace(stringFromAny(participantProvider["backend"])) == "" ||
			intFromAny(participantProvider["return_code"], -1) != 0 ||
			participantProvider["timed_out"] != false || participantProvider["stalled"] != false {
			t.Fatalf("participant provider result %d = %#v", index+1, participantProvider)
		}
		if strings.TrimSpace(stringFromAny(facilitatorProvider["backend"])) == "" ||
			intFromAny(facilitatorProvider["return_code"], -1) != 0 ||
			facilitatorProvider["timed_out"] != false || facilitatorProvider["stalled"] != false {
			t.Fatalf("facilitator provider result %d = %#v", index+1, facilitatorProvider)
		}
	}
	outputRefs := result["facilitator_output_refs"].([]any)
	if len(outputRefs) != 4 {
		t.Fatalf("facilitator output refs = %#v", outputRefs)
	}
	slots := result["slots"].([]any)
	if len(slots) != 2 {
		t.Fatalf("persisted participant slots = %#v", slots)
	}
	for index, raw := range slots {
		envelope := raw.(map[string]any)
		state := envelope["state"].(map[string]any)
		if envelope["profile_id"] == nil || intFromAny(state["call_count"], 0) != 2 || stringFromAny(state["cwd"]) == "" {
			t.Fatalf("participant slot %d metadata = %#v", index, envelope)
		}
	}
	facilitatorState := result["facilitator_provider_state"].(map[string]any)
	if facilitatorState["slot_id"] != "facilitator" ||
		intFromAny(facilitatorState["state"].(map[string]any)["call_count"], 0) != 4 ||
		result["facilitator_profile_id"] == nil {
		t.Fatalf("facilitator metadata = %#v / %#v", facilitatorState, result["facilitator_profile_id"])
	}
	st := store.New(sessionDir)
	for _, rawRef := range outputRefs {
		output, err := st.LoadArtifact(rawRef.(map[string]any))
		if err != nil {
			t.Fatalf("load facilitator output: %v", err)
		}
		providerResult, _ := output["provider_result"].(map[string]any)
		providerState, _ := output["provider_state"].(map[string]any)
		if output["status"] != "completed" || strings.TrimSpace(stringFromAny(output["content"])) == "" ||
			strings.TrimSpace(stringFromAny(providerResult["backend"])) == "" ||
			intFromAny(providerResult["return_code"], -1) != 0 ||
			strings.TrimSpace(stringFromAny(providerState["context_id"])) == "" {
			t.Fatalf("facilitator output metadata = %#v", output)
		}
	}
	checkpoint := assertRootRecipeArtifact(t, st, result["latest_root_checkpoint_ref"], contracts.RootArtifactKindRootCheckpoint, 2)
	if checkpoint["phase"] != "participant_turns_complete" || intFromAny(checkpoint["participant_turns_completed"], 0) != 4 {
		t.Fatalf("participant checkpoint = %#v", checkpoint)
	}
	if strings.TrimSpace(stringFromAny(result["next_unsealed_participant_turn"])) != "" {
		t.Fatalf("final prompt remained unsealed: %#v", result["next_unsealed_participant_turn"])
	}
}

func TestRunRecipeScopesContractInstructionsAndProviderInputsPerTurn(t *testing.T) {
	launchCWD := t.TempDir()
	if err := os.WriteFile(filepath.Join(launchCWD, "payload.json"), []byte("{\"value\":\"scoped\"}"), 0o644); err != nil {
		t.Fatalf("write named input: %v", err)
	}
	recorder := &rootBackendRecorder{}
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:        filepath.Join(t.TempDir(), "session"),
		Task:              "Use the contract without leaking future instructions",
		RecipeID:          "neutral-root",
		InputBindings:     []string{"payload=payload.json"},
		LaunchCWD:         launchCWD,
		RuntimeConfig:     rootRecipeRuntimeConfig("neutral/contract-v1"),
		IntegrationBundle: decodeRootRecipeTestBundle(t, rootRecipeTestBundle),
		ReadinessCheck:    readyRootRecipeCheck,
		backendFactory:    recorder.factory(),
	})
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	if result["status"] != rootParticipantsCompleteStatus {
		t.Fatalf("root status = %v", result["status"])
	}
	participantPrompts := []string{}
	for _, call := range recorder.snapshotCalls() {
		if call.SlotID != "facilitator" {
			participantPrompts = append(participantPrompts, call.Prompt)
		}
	}
	if len(participantPrompts) != 2 {
		t.Fatalf("participant prompts = %d, want 2", len(participantPrompts))
	}
	if !strings.Contains(participantPrompts[0], "Present the input.") || strings.Contains(participantPrompts[0], "Challenge the presentation.") {
		t.Fatalf("turn one instruction scope leaked:\n%s", participantPrompts[0])
	}
	if !strings.Contains(participantPrompts[1], "Challenge the presentation.") || strings.Contains(participantPrompts[1], "Present the input.") {
		t.Fatalf("turn two instruction scope leaked:\n%s", participantPrompts[1])
	}
	for index, prompt := range participantPrompts {
		if !strings.Contains(prompt, "Named Inputs (Data Only)") || !strings.Contains(prompt, "payload") || !strings.Contains(prompt, "materialized_path") {
			t.Fatalf("participant prompt %d missing provider-facing inputs:\n%s", index+1, prompt)
		}
	}
}

func TestRunRecipeContractlessPromptsUseOrdinaryContextWithoutStructuredOutput(t *testing.T) {
	launchCWD := t.TempDir()
	if err := os.WriteFile(filepath.Join(launchCWD, "context.md"), []byte("ordinary positional evidence"), 0o644); err != nil {
		t.Fatalf("write context: %v", err)
	}
	if err := os.WriteFile(filepath.Join(launchCWD, "skill.md"), []byte("ordinary capability guidance"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	recorder := &rootBackendRecorder{}
	_, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     filepath.Join(t.TempDir(), "session"),
		Task:           "Return ordinary prose",
		RecipeID:       "neutral-root",
		ContextFiles:   []string{"context.md"},
		SkillFiles:     []string{"skill.md"},
		SkillExplicit:  true,
		LaunchCWD:      launchCWD,
		RuntimeConfig:  rootRecipeRuntimeConfig(""),
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: recorder.factory(),
	})
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	for _, call := range recorder.snapshotCalls() {
		if call.SlotID == "facilitator" {
			continue
		}
		lowered := strings.ToLower(call.Prompt)
		if !strings.Contains(call.Prompt, "ordinary positional evidence") ||
			!strings.Contains(call.Prompt, "ordinary capability guidance") ||
			strings.Contains(call.Prompt, "Integration Contract Instructions") ||
			strings.Contains(lowered, "structured output") {
			t.Fatalf("contractless participant prompt is not ordinary prose framing:\n%s", call.Prompt)
		}
	}
}

func TestRunRecipeUsesMappedExecutionCWDForEveryProvider(t *testing.T) {
	sourceRoot := t.TempDir()
	runTestGit(t, sourceRoot, "init")
	runTestGit(t, sourceRoot, "config", "user.email", "root-execution@example.invalid")
	runTestGit(t, sourceRoot, "config", "user.name", "Root Execution Test")
	if err := os.WriteFile(filepath.Join(sourceRoot, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatalf("write tracked source: %v", err)
	}
	runTestGit(t, sourceRoot, "add", "tracked.txt")
	runTestGit(t, sourceRoot, "commit", "-m", "fixture")

	config := rootRecipeRuntimeConfig("")
	lifecycle := config.RelayRecipes["neutral-root"]["lifecycle"].(map[string]any)
	lifecycle["workspace_isolation"] = "ephemeral"
	recorder := &rootBackendRecorder{}
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     filepath.Join(t.TempDir(), "session"),
		Task:           "Execute inside the mapped worktree",
		RecipeID:       "neutral-root",
		LaunchCWD:      sourceRoot,
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: recorder.factory(),
	})
	if err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
	executionCWD := stringFromAny(result["execution_cwd"])
	canonicalSource, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		t.Fatalf("canonical source: %v", err)
	}
	if executionCWD == "" || executionCWD == canonicalSource {
		t.Fatalf("execution CWD = %q, source = %q", executionCWD, canonicalSource)
	}
	for _, call := range recorder.snapshotCalls() {
		if call.CWD != executionCWD {
			t.Fatalf("%s CWD = %q, want %q", call.SlotID, call.CWD, executionCWD)
		}
	}
	runTestGit(t, sourceRoot, "worktree", "remove", "--force", executionCWD)
}

func TestRunRecipeProviderConstructionFailureFinalizesManagedWorkspace(t *testing.T) {
	sourceRoot := t.TempDir()
	runTestGit(t, sourceRoot, "init")
	runTestGit(t, sourceRoot, "config", "user.email", "root-execution@example.invalid")
	runTestGit(t, sourceRoot, "config", "user.name", "Root Execution Test")
	if err := os.WriteFile(filepath.Join(sourceRoot, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatalf("write tracked source: %v", err)
	}
	runTestGit(t, sourceRoot, "add", "tracked.txt")
	runTestGit(t, sourceRoot, "commit", "-m", "fixture")

	config := rootRecipeRuntimeConfig("")
	lifecycle := config.RelayRecipes["neutral-root"]["lifecycle"].(map[string]any)
	lifecycle["workspace_isolation"] = "ephemeral"
	recorder := &rootBackendRecorder{}
	participantFactory := recorder.factory()
	factory := func(backend string, sessionRoot string, slotID string, label string, cwd string, slotConfig SlotConfig) (Backend, error) {
		if slotID == "facilitator" {
			return nil, errors.New("construct facilitator fixture")
		}
		return participantFactory(backend, sessionRoot, slotID, label, cwd, slotConfig)
	}
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     filepath.Join(t.TempDir(), "session"),
		Task:           "Finalize after provider construction fails",
		RecipeID:       "neutral-root",
		LaunchCWD:      sourceRoot,
		RuntimeConfig:  config,
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: factory,
	})
	if err == nil || !strings.Contains(err.Error(), "construct facilitator fixture") {
		t.Fatalf("provider construction failure = %v", err)
	}
	if result["status"] != "failed" || result["execution_phase"] != "participant_setup" ||
		intFromAny(result["actual_participant_turns"], -1) != 0 || result["source_mutated"] == true {
		t.Fatalf("provider construction result = %#v", result)
	}
	before := strings.TrimSpace(stringFromAny(result["source_before_digest"]))
	after := strings.TrimSpace(stringFromAny(result["source_after_digest"]))
	if before == "" || after != before {
		t.Fatalf("terminal source digests = before %q after %q", before, after)
	}
	executionCWD := strings.TrimSpace(stringFromAny(result["execution_cwd"]))
	if executionCWD == "" {
		t.Fatalf("missing execution CWD: %#v", result)
	}
	runTestGit(t, sourceRoot, "worktree", "remove", "--force", executionCWD)
}

func TestRunRecipePersistsParticipantBeforeFacilitatorFailure(t *testing.T) {
	recorder := &rootBackendRecorder{}
	recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "facilitator" {
			return TurnResult{
				Content: "partial facilitator output",
				ProviderResult: ProviderResult{
					Backend:         call.Backend,
					ReturnCode:      7,
					ReturnCodeKnown: true,
				},
			}, BackendRunError{Label: call.Label, Detail: "facilitator failed"}
		}
		return successfulRootTurn(call.Backend, "durable participant response"), nil
	}
	result, err := RunRecipe(context.Background(), RecipeOptions{
		SessionDir:     filepath.Join(t.TempDir(), "session"),
		Task:           "Preserve successful participants",
		RecipeID:       "neutral-root",
		LaunchCWD:      t.TempDir(),
		RuntimeConfig:  rootRecipeRuntimeConfig(""),
		ReadinessCheck: readyRootRecipeCheck,
		backendFactory: recorder.factory(),
	})
	if err == nil || !strings.Contains(err.Error(), "facilitator failed") {
		t.Fatalf("facilitator failure = %v", err)
	}
	if result["status"] != "failed" || intFromAny(result["actual_participant_turns"], 0) != 1 {
		t.Fatalf("failed root result = %#v", result)
	}
	transcript := result["transcript"].([]any)
	if len(transcript) != 1 {
		t.Fatalf("failed transcript = %#v", transcript)
	}
	entry := transcript[0].(map[string]any)
	if entry["content"] != "durable participant response" || entry["facilitator_failed"] != true || entry["facilitator_output_ref"] == nil {
		t.Fatalf("durable participant entry = %#v", entry)
	}
	ref := entry["facilitator_output_ref"].(map[string]any)
	attempt, loadErr := store.New(stringFromAny(result["session_dir"])).LoadArtifact(ref)
	if loadErr != nil || attempt["content"] != "partial facilitator output" || attempt["status"] != "failed" {
		t.Fatalf("facilitator attempt = %#v, %v", attempt, loadErr)
	}
}

func TestRunRecipeParticipantTimeoutAndCancellationPersistCounts(t *testing.T) {
	tests := []struct {
		name       string
		runContext func() context.Context
		runError   error
		wantStatus string
	}{
		{
			name:       "timeout",
			runContext: context.Background,
			runError:   BackendRunError{Label: "participant", Detail: "timed out"},
			wantStatus: "failed",
		},
		{
			name:       "cancellation",
			runContext: context.Background,
			runError:   context.Canceled,
			wantStatus: "interrupted",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &rootBackendRecorder{}
			recorder.handler = func(_ context.Context, call rootBackendCall) (TurnResult, error) {
				if call.SlotID == "facilitator" {
					t.Fatal("facilitator ran after failed participant")
				}
				return TurnResult{
					TimedOut: test.name == "timeout",
					ProviderResult: ProviderResult{
						Backend:  call.Backend,
						TimedOut: test.name == "timeout",
					},
				}, test.runError
			}
			result, err := RunRecipe(test.runContext(), RecipeOptions{
				SessionDir:     filepath.Join(t.TempDir(), "session"),
				Task:           "Fail before a participant response",
				RecipeID:       "neutral-root",
				LaunchCWD:      t.TempDir(),
				RuntimeConfig:  rootRecipeRuntimeConfig(""),
				ReadinessCheck: readyRootRecipeCheck,
				backendFactory: recorder.factory(),
			})
			if err == nil {
				t.Fatal("participant failure unexpectedly succeeded")
			}
			if test.name == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error = %v", err)
			}
			if result["status"] != test.wantStatus ||
				intFromAny(result["actual_participant_turns"], -1) != 0 ||
				len(result["transcript"].([]any)) != 0 {
				t.Fatalf("participant failure result = %#v", result)
			}
		})
	}
}

func TestRootRecipeSteeringTargetsNextTurnAndConsumesFIFOOnce(t *testing.T) {
	turnOneStarted := make(chan struct{})
	releaseTurnOne := make(chan struct{})
	var startOnce sync.Once
	recorder := &rootBackendRecorder{}
	recorder.handler = func(ctx context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "slot_0" {
			startOnce.Do(func() { close(turnOneStarted) })
			select {
			case <-releaseTurnOne:
			case <-ctx.Done():
				return TurnResult{}, ctx.Err()
			}
		}
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, "{\"settled\":[],\"contested\":[\"continue\"],\"withdrawn\":[]}"), nil
		}
		return successfulRootTurn(call.Backend, "participant response"), nil
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	launchCWD := t.TempDir()
	type runOutcome struct {
		result map[string]any
		err    error
	}
	outcome := make(chan runOutcome, 1)
	go func() {
		result, err := RunRecipe(context.Background(), RecipeOptions{
			SessionDir:     sessionDir,
			Task:           "Apply queued direction exactly once",
			RecipeID:       "neutral-root",
			LaunchCWD:      launchCWD,
			RuntimeConfig:  rootRecipeRuntimeConfig(""),
			ReadinessCheck: readyRootRecipeCheck,
			backendFactory: recorder.factory(),
		})
		outcome <- runOutcome{result: result, err: err}
	}()
	<-turnOneStarted
	first, err := QueueSteeringPrompt(sessionDir, "first queued direction")
	if err != nil {
		t.Fatalf("queue first steering: %v", err)
	}
	second, err := QueueSteeringPrompt(sessionDir, "second queued direction")
	if err != nil {
		t.Fatalf("queue second steering: %v", err)
	}
	if intFromAny(first["target_participant_turn"], 0) != 2 || intFromAny(second["target_participant_turn"], 0) != 2 {
		t.Fatalf("steering targets = %#v / %#v", first, second)
	}
	close(releaseTurnOne)
	completed := <-outcome
	if completed.err != nil {
		t.Fatalf("RunRecipe: %v", completed.err)
	}
	participantPrompts := []string{}
	for _, call := range recorder.snapshotCalls() {
		if call.SlotID != "facilitator" {
			participantPrompts = append(participantPrompts, call.Prompt)
		}
	}
	if len(participantPrompts) != 2 ||
		strings.Contains(participantPrompts[0], "first queued direction") ||
		strings.Contains(participantPrompts[0], "second queued direction") ||
		!strings.Contains(participantPrompts[1], "first queued direction") ||
		!strings.Contains(participantPrompts[1], "second queued direction") ||
		strings.Index(participantPrompts[1], "first queued direction") > strings.Index(participantPrompts[1], "second queued direction") {
		t.Fatalf("steering prompt delivery = %#v", participantPrompts)
	}
	meta, err := loadSessionMeta(sessionDir)
	if err != nil {
		t.Fatalf("load root meta: %v", err)
	}
	history := meta.Slice("steering_history")
	if len(history) != 2 ||
		history[0].(map[string]any)["id"] != first["id"] ||
		history[1].(map[string]any)["id"] != second["id"] {
		t.Fatalf("steering history = %#v", history)
	}
	remaining, err := loadSteeringPrompts(sessionDir)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining steering = %#v, %v", remaining, err)
	}
	beforeMeta, err := os.ReadFile(filepath.Join(sessionDir, "meta.json"))
	if err != nil {
		t.Fatalf("read completed meta: %v", err)
	}
	beforeQueue, err := os.ReadFile(filepath.Join(sessionDir, "steering.json"))
	if err != nil {
		t.Fatalf("read completed steering queue: %v", err)
	}
	if _, err := QueueSteeringPrompt(sessionDir, "too late"); err == nil {
		t.Fatal("steering after participant completion unexpectedly succeeded")
	}
	afterMeta, _ := os.ReadFile(filepath.Join(sessionDir, "meta.json"))
	afterQueue, _ := os.ReadFile(filepath.Join(sessionDir, "steering.json"))
	if string(beforeMeta) != string(afterMeta) || string(beforeQueue) != string(afterQueue) {
		t.Fatal("rejected post-completion steering mutated metadata or queue")
	}
}

func TestRootRecipeSteeringRejectsDuringInFlightFinalPromptWithoutMutation(t *testing.T) {
	finalStarted := make(chan struct{})
	releaseFinal := make(chan struct{})
	var startOnce sync.Once
	recorder := &rootBackendRecorder{}
	recorder.handler = func(ctx context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "slot_0" {
			startOnce.Do(func() { close(finalStarted) })
			select {
			case <-releaseFinal:
			case <-ctx.Done():
				return TurnResult{}, ctx.Err()
			}
		}
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, "{\"settled\":[],\"contested\":[],\"withdrawn\":[]}"), nil
		}
		return successfulRootTurn(call.Backend, "final response"), nil
	}
	config := rootRecipeRuntimeConfig("")
	config.RelayRecipes["neutral-root"]["participant_turns"] = 1
	config.RelayRecipes["neutral-root"]["max_rounds"] = 1
	sessionDir := filepath.Join(t.TempDir(), "session")
	launchCWD := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := RunRecipe(context.Background(), RecipeOptions{
			SessionDir:     sessionDir,
			Task:           "Seal the final prompt before dispatch",
			RecipeID:       "neutral-root",
			LaunchCWD:      launchCWD,
			RuntimeConfig:  config,
			ReadinessCheck: readyRootRecipeCheck,
			backendFactory: recorder.factory(),
		})
		done <- err
	}()
	<-finalStarted
	beforeMeta, err := os.ReadFile(filepath.Join(sessionDir, "meta.json"))
	if err != nil {
		t.Fatalf("read meta before rejected steering: %v", err)
	}
	if _, err := QueueSteeringPrompt(sessionDir, "cannot target sealed final turn"); err == nil {
		t.Fatal("steering during final in-flight prompt unexpectedly succeeded")
	}
	afterMeta, err := os.ReadFile(filepath.Join(sessionDir, "meta.json"))
	if err != nil {
		t.Fatalf("read meta after rejected steering: %v", err)
	}
	if string(beforeMeta) != string(afterMeta) {
		t.Fatal("rejected final-turn steering mutated metadata")
	}
	if prompts, err := loadSteeringPrompts(sessionDir); err != nil || len(prompts) != 0 {
		t.Fatalf("rejected final-turn steering mutated queue: %#v, %v", prompts, err)
	}
	close(releaseFinal)
	if err := <-done; err != nil {
		t.Fatalf("RunRecipe: %v", err)
	}
}

func TestRootRecipeSteeringCannotReplaceContractScheduleOrInstructions(t *testing.T) {
	turnOneStarted := make(chan struct{})
	releaseTurnOne := make(chan struct{})
	var startOnce sync.Once
	recorder := &rootBackendRecorder{}
	recorder.handler = func(ctx context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "slot_0" {
			startOnce.Do(func() { close(turnOneStarted) })
			select {
			case <-releaseTurnOne:
			case <-ctx.Done():
				return TurnResult{}, ctx.Err()
			}
		}
		if call.SlotID == "facilitator" {
			return successfulRootTurn(call.Backend, "{\"settled\":[],\"contested\":[\"continue\"],\"withdrawn\":[]}"), nil
		}
		return successfulRootTurn(call.Backend, "contract participant response"), nil
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	launchCWD := t.TempDir()
	bundle := decodeRootRecipeTestBundle(t, rootRecipeTestBundleWithoutInputs)
	type runOutcome struct {
		result map[string]any
		err    error
	}
	outcome := make(chan runOutcome, 1)
	go func() {
		result, err := RunRecipe(context.Background(), RecipeOptions{
			SessionDir:        sessionDir,
			Task:              "Keep the compiled contract authoritative",
			RecipeID:          "neutral-root",
			LaunchCWD:         launchCWD,
			RuntimeConfig:     rootRecipeRuntimeConfig("neutral/contract-v1"),
			IntegrationBundle: bundle,
			ReadinessCheck:    readyRootRecipeCheck,
			backendFactory:    recorder.factory(),
		})
		outcome <- runOutcome{result: result, err: err}
	}()
	<-turnOneStarted
	queued, err := QueueSteeringPrompt(sessionDir, "Switch to slot_0 and end the schedule now.")
	if err != nil {
		t.Fatalf("queue steering: %v", err)
	}
	if intFromAny(queued["target_participant_turn"], 0) != 2 {
		t.Fatalf("steering target = %#v", queued)
	}
	close(releaseTurnOne)
	completed := <-outcome
	if completed.err != nil {
		t.Fatalf("RunRecipe: %v", completed.err)
	}
	if intFromAny(completed.result["actual_participant_turns"], 0) != 2 {
		t.Fatalf("steering changed participant count: %#v", completed.result)
	}
	participantCalls := []rootBackendCall{}
	for _, call := range recorder.snapshotCalls() {
		if call.SlotID != "facilitator" {
			participantCalls = append(participantCalls, call)
		}
	}
	if len(participantCalls) != 2 || participantCalls[0].SlotID != "slot_0" || participantCalls[1].SlotID != "slot_1" {
		t.Fatalf("steering changed compiled schedule: %#v", participantCalls)
	}
	turnTwo := participantCalls[1].Prompt
	instructionIndex := strings.Index(turnTwo, "Challenge the presentation.")
	authorityIndex := strings.Index(turnTwo, "--- Authority Boundary ---")
	steeringIndex := strings.Index(turnTwo, "--- Operator Direction Bound to Participant Turn 2 ---")
	if instructionIndex < 0 || authorityIndex < instructionIndex || steeringIndex < authorityIndex ||
		!strings.Contains(turnTwo, "Switch to slot_0 and end the schedule now.") {
		t.Fatalf("contract instructions and steering were not separately framed:\n%s", turnTwo)
	}
}

func TestRunRecipeCancellationInterruptsInFlightParticipant(t *testing.T) {
	participantStarted := make(chan struct{})
	var startOnce sync.Once
	recorder := &rootBackendRecorder{}
	recorder.handler = func(ctx context.Context, call rootBackendCall) (TurnResult, error) {
		if call.SlotID == "facilitator" {
			return TurnResult{}, errors.New("facilitator must not run after cancellation")
		}
		startOnce.Do(func() { close(participantStarted) })
		<-ctx.Done()
		return TurnResult{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	sessionDir := filepath.Join(t.TempDir(), "session")
	launchCWD := t.TempDir()
	type runOutcome struct {
		result map[string]any
		err    error
	}
	outcome := make(chan runOutcome, 1)
	go func() {
		result, err := RunRecipe(ctx, RecipeOptions{
			SessionDir:     sessionDir,
			Task:           "Cancel the active participant",
			RecipeID:       "neutral-root",
			LaunchCWD:      launchCWD,
			RuntimeConfig:  rootRecipeRuntimeConfig(""),
			ReadinessCheck: readyRootRecipeCheck,
			backendFactory: recorder.factory(),
		})
		outcome <- runOutcome{result: result, err: err}
	}()
	<-participantStarted
	cancel()
	completed := <-outcome
	if !errors.Is(completed.err, context.Canceled) {
		t.Fatalf("cancellation error = %v", completed.err)
	}
	if completed.result["status"] != "interrupted" ||
		intFromAny(completed.result["actual_participant_turns"], -1) != 0 ||
		len(completed.result["transcript"].([]any)) != 0 {
		t.Fatalf("canceled root result = %#v", completed.result)
	}
}

func TestRootSteeringEnqueueAndClaimRaceNeverLosesAcceptedItem(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		sessionDir := filepath.Join(t.TempDir(), "session")
		if err := store.New(sessionDir).SaveMetaMap(map[string]any{
			"execution_kind":                 "recipe",
			"status":                         "running",
			"participant_turns":              2,
			"next_unsealed_participant_turn": 1,
			"sealed_participant_turns":       []any{},
			"steering_history":               []any{},
			"lifecycle":                      map[string]any{"steering": "allow"},
		}); err != nil {
			t.Fatalf("save race fixture: %v", err)
		}
		start := make(chan struct{})
		var queued map[string]any
		var queueErr error
		var claimed []map[string]any
		var claimErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			queued, queueErr = QueueSteeringPrompt(sessionDir, fmt.Sprintf("race direction %d", iteration))
		}()
		go func() {
			defer wait.Done()
			<-start
			claimed, _, claimErr = claimRootParticipantSteering(sessionDir, 1)
		}()
		close(start)
		wait.Wait()
		if queueErr != nil || claimErr != nil {
			t.Fatalf("race errors = %v / %v", queueErr, claimErr)
		}
		meta, err := loadSessionMeta(sessionDir)
		if err != nil {
			t.Fatalf("load race meta: %v", err)
		}
		remaining, err := loadSteeringPrompts(sessionDir)
		if err != nil {
			t.Fatalf("load race queue: %v", err)
		}
		history := meta.Slice("steering_history")
		occurrences := 0
		for _, raw := range history {
			if raw.(map[string]any)["id"] == queued["id"] {
				occurrences++
			}
		}
		for _, item := range remaining {
			if item["id"] == queued["id"] {
				occurrences++
			}
		}
		if occurrences != 1 {
			t.Fatalf("accepted steering occurrences = %d, history=%#v queue=%#v claimed=%#v", occurrences, history, remaining, claimed)
		}
		if len(claimed) == 1 {
			if claimed[0]["id"] != queued["id"] || len(history) != 1 || len(remaining) != 0 {
				t.Fatalf("queue-first race state = claimed %#v history %#v remaining %#v", claimed, history, remaining)
			}
		} else if len(claimed) == 0 {
			if len(history) != 0 || len(remaining) != 1 || intFromAny(remaining[0]["target_participant_turn"], 0) != 2 {
				t.Fatalf("claim-first race state = claimed %#v history %#v remaining %#v", claimed, history, remaining)
			}
		} else {
			t.Fatalf("claimed steering = %#v", claimed)
		}
	}
}
