package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestRunAndResumeCodexSession(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "go-phase5")

	result, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Run two Codex slots from Go",
		Agents:         []string{"codex", "codex"},
		Rounds:         2,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result["status"] != "completed" {
		t.Fatalf("status = %v, want completed", result["status"])
	}
	if result["actual_rounds"] != 2 {
		t.Fatalf("actual_rounds = %v, want 2", result["actual_rounds"])
	}
	meta := mustLoadMeta(t, sessionDir)
	if meta["round_limit_mode"] != "fixed" {
		t.Fatalf("round_limit_mode = %v, want fixed", meta["round_limit_mode"])
	}
	ledger := meta["ledger"].(map[string]any)
	if settled := ledger["settled"].([]any); len(settled) != 1 || settled[0] != "done" {
		t.Fatalf("ledger was not updated by facilitator: %#v", ledger)
	}
	slots := meta["slots"].([]any)
	if slots[0].(map[string]any)["label"] != "Codex (A)" || slots[1].(map[string]any)["label"] != "Codex (B)" {
		t.Fatalf("slot labels = %#v", slots)
	}
	if slots[0].(map[string]any)["state"].(map[string]any)["thread_id"] == slots[1].(map[string]any)["state"].(map[string]any)["thread_id"] {
		t.Fatalf("codex slot thread IDs should be distinct: %#v", slots)
	}
	transcript := mustLoadTranscript(t, sessionDir)
	if len(transcript) != 2 {
		t.Fatalf("transcript entries = %d, want 2", len(transcript))
	}
	if transcript[0]["slot_id"] != "slot_0" || transcript[1]["slot_id"] != "slot_1" {
		t.Fatalf("transcript slot ids = %#v", transcript)
	}
	report, err := inspect.BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("contracts report: %v", err)
	}
	validation := report["validation"].(map[string]any)
	if validation["ok"] != true || validation["event_count"] != 4 {
		t.Fatalf("validation = %#v", validation)
	}

	resumed, err := Resume(context.Background(), sessionDir, ResumeOptions{
		Prompt:         "Add one final note",
		Rounds:         1,
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed["actual_rounds"] != 3 {
		t.Fatalf("resumed actual_rounds = %v, want 3", resumed["actual_rounds"])
	}
	transcript = mustLoadTranscript(t, sessionDir)
	if len(transcript) != 3 {
		t.Fatalf("resumed transcript entries = %d, want 3", len(transcript))
	}
	report, err = inspect.BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("resumed contracts report: %v", err)
	}
	validation = report["validation"].(map[string]any)
	if validation["ok"] != true || validation["event_count"] != 7 {
		t.Fatalf("resumed validation = %#v", validation)
	}

	st := store.New(sessionDir)
	graphReport, err := inspect.BuildShowGraphReport(sessionDir)
	if err != nil {
		t.Fatalf("graph report: %v", err)
	}
	graphValidation := graphReport["validation"].(map[string]any)
	if graphValidation["ok"] != true {
		t.Fatalf("graph validation = %#v", graphValidation)
	}
	if _, _, err := graph.RepairAndSaveFromEvents(st); err != nil {
		t.Fatalf("repair graph: %v", err)
	}
}

func TestRunBuildsSlotsFromBackendProfiles(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "go-phase5-profiles")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Run profile-backed Codex slots from Go",
		Agents:         []string{"codex-fast", "codex-deep"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run profiles: %v", err)
	}
	meta := mustLoadMeta(t, sessionDir)
	slots := meta["slots"].([]any)
	firstState := slots[0].(map[string]any)["state"].(map[string]any)
	secondState := slots[1].(map[string]any)["state"].(map[string]any)
	if firstState["model"] != "gpt-5.5" || firstState["effort"] != "medium" {
		t.Fatalf("first profile state = %#v", firstState)
	}
	if secondState["model"] != "gpt-5.5" || secondState["effort"] != "xhigh" {
		t.Fatalf("second profile state = %#v", secondState)
	}
}

func TestRunPersistsRuntimeConfigSnapshot(t *testing.T) {
	env := setupFakeCodex(t)
	settingsPath := writeNestedRelaySettings(t, env)
	settings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	settings = append(settings, []byte("\n[limits]\nintegration_bundle_max_bytes = 2097152\n")...)
	if err := os.WriteFile(settingsPath, settings, 0o644); err != nil {
		t.Fatalf("write settings with limits: %v", err)
	}
	sessionDir := filepath.Join(env.relayHome, "sessions", "runtime-snapshot")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Persist runtime snapshot",
		Agents:         []string{"codex-fast", "codex-deep"},
		Rounds:         1,
		TimeoutSeconds: 5,
		SettingsPath:   settingsPath,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run with settings: %v", err)
	}

	meta := mustLoadMeta(t, sessionDir)
	ref, ok := meta["runtime_config_ref"].(map[string]any)
	if !ok {
		t.Fatalf("runtime_config_ref missing: %#v", meta)
	}
	if meta["runtime_config_version"] != RuntimeConfigSnapshotVersion {
		t.Fatalf("runtime_config_version = %v", meta["runtime_config_version"])
	}
	st := store.New(sessionDir)
	artifact, err := st.LoadArtifact(ref)
	if err != nil {
		t.Fatalf("load runtime snapshot: %v", err)
	}
	profiles := artifact["backend_profiles"].(map[string]any)
	relayRecipes := artifact["relay_recipes"].(map[string]any)
	if profiles["codex-fast"] == nil || relayRecipes["outer-review"] == nil {
		t.Fatalf("runtime snapshot missing resolved defaults/settings: %#v", artifact)
	}
	limits := artifact["limits"].(map[string]any)
	if fmt.Sprint(limits["integration_bundle_max_bytes"]) != "2097152" {
		t.Fatalf("runtime snapshot limits = %#v", limits)
	}
	graphLimits := st.LoadGraph()["runtime_limits"].(map[string]any)
	if fmt.Sprint(graphLimits["integration_bundle_max_bytes"]) != "2097152" {
		t.Fatalf("graph runtime limits = %#v", graphLimits)
	}
	report, err := inspect.BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("contracts report: %v", err)
	}
	runtimeStatus := report["runtime_config_ref"].(map[string]any)
	if runtimeStatus["ok"] != true || runtimeStatus["digest_valid"] != true {
		t.Fatalf("runtime config contract status = %#v", runtimeStatus)
	}
}

func TestResumeUsesRuntimeSnapshotAfterSettingsMutation(t *testing.T) {
	env := setupFakeCodex(t)
	settingsPath := writeResumeRuntimeSettings(t, env, true)
	sessionDir := filepath.Join(env.relayHome, "sessions", "runtime-resume")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Resume relay slot with saved recipe",
		Agents:         []string{"codex", "relay"},
		SlotConfigs:    []SlotConfig{{}, {Model: "stable-review", Effort: "1"}},
		Rounds:         1,
		TimeoutSeconds: 5,
		SettingsPath:   settingsPath,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run seed: %v", err)
	}
	writeResumeRuntimeSettings(t, env, false)

	if _, err := Resume(context.Background(), sessionDir, ResumeOptions{
		Rounds:         1,
		TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("resume should use saved runtime snapshot after settings mutation: %v", err)
	}
	meta := mustLoadMeta(t, sessionDir)
	slots := meta["slots"].([]any)
	relayState := slots[1].(map[string]any)["state"].(map[string]any)
	if childIDs := relayState["child_session_ids"].([]any); len(childIDs) != 1 {
		t.Fatalf("relay slot did not run child from saved snapshot: %#v", relayState)
	}
}

func TestResumeRejectsSettingsReplacementWhenSnapshotExists(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "runtime-reject-settings")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Reject settings replacement",
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run seed: %v", err)
	}
	_, err := Resume(context.Background(), sessionDir, ResumeOptions{
		Rounds:         1,
		TimeoutSeconds: 5,
		SettingsPath:   filepath.Join(env.relayHome, "replacement.toml"),
	})
	if err == nil || !strings.Contains(err.Error(), "resume --settings is not supported") {
		t.Fatalf("expected resume --settings rejection, got %v", err)
	}
}

func TestResumeRuntimeOverridesRecordDistinctEvents(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "runtime-override-events")

	if _, err := Run(context.Background(), Options{
		SessionDir:        sessionDir,
		Task:              "Record override events",
		Agents:            []string{"codex", "codex"},
		SlotConfigs:       []SlotConfig{{Model: "initial-a"}, {Model: "initial-b"}},
		FacilitatorModel:  "facilitator-initial",
		FacilitatorEffort: "medium",
		Rounds:            1,
		TimeoutSeconds:    5,
		LaunchCWD:         env.projectDir,
	}); err != nil {
		t.Fatalf("run seed: %v", err)
	}
	if _, err := Resume(context.Background(), sessionDir, ResumeOptions{
		Rounds:            1,
		TimeoutSeconds:    5,
		SlotConfigs:       []SlotConfig{{Model: "override-a"}, {}},
		FacilitatorModel:  "facilitator-override",
		FacilitatorEffort: "high",
	}); err != nil {
		t.Fatalf("resume with overrides: %v", err)
	}
	events, err := store.New(sessionDir).ReadEvents()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	slotEvent := firstEventOfType(events, "slot_runtime_override")
	if slotEvent == nil {
		t.Fatalf("missing slot_runtime_override event: %#v", events)
	}
	slotPayload := slotEvent["payload"].(map[string]any)
	if slotPayload["slot_id"] != "slot_0" {
		t.Fatalf("slot override payload = %#v", slotPayload)
	}
	modelOverride := slotPayload["overrides"].(map[string]any)["model"].(map[string]any)
	if modelOverride["previous"] != "initial-a" || modelOverride["requested"] != "override-a" {
		t.Fatalf("slot model override = %#v", modelOverride)
	}

	facilitatorEvent := firstEventOfType(events, "facilitator_runtime_override")
	if facilitatorEvent == nil {
		t.Fatalf("missing facilitator_runtime_override event: %#v", events)
	}
	facilitatorPayload := facilitatorEvent["payload"].(map[string]any)
	if facilitatorPayload["backend"] != "codex" {
		t.Fatalf("facilitator override payload = %#v", facilitatorPayload)
	}
	facilitatorModel := facilitatorPayload["overrides"].(map[string]any)["model"].(map[string]any)
	if facilitatorModel["previous"] != "facilitator-initial" || facilitatorModel["requested"] != "facilitator-override" {
		t.Fatalf("facilitator model override = %#v", facilitatorModel)
	}
}

func TestResumeTypedModeControlAppliesAndRecordsEvent(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "typed-mode-control")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Switch debate mode",
		Agents:         []string{"codex", "codex"},
		Mode:           "adversarial",
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run seed: %v", err)
	}
	if _, err := Resume(context.Background(), sessionDir, ResumeOptions{
		Mode:           "steelman",
		Rounds:         1,
		TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("resume with mode control: %v", err)
	}

	meta := mustLoadMeta(t, sessionDir)
	if meta["mode"] != "steelman" {
		t.Fatalf("mode = %v, want steelman", meta["mode"])
	}
	history := meta["mode_history"].([]any)
	if len(history) != 2 {
		t.Fatalf("mode history = %#v", history)
	}
	transcript := mustLoadTranscript(t, sessionDir)
	if len(transcript) != 2 {
		t.Fatalf("transcript entries = %d, want 2", len(transcript))
	}
	if transcript[0]["mode"] != "adversarial" || transcript[1]["mode"] != "steelman" {
		t.Fatalf("transcript modes = %#v", transcript)
	}
	events, err := store.New(sessionDir).ReadEvents()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	modeEvent := firstEventOfType(events, "mode_control_applied")
	if modeEvent == nil {
		t.Fatalf("missing mode_control_applied event: %#v", events)
	}
	payload := modeEvent["payload"].(map[string]any)
	if payload["previous_mode"] != "adversarial" || payload["requested_mode"] != "steelman" || payload["applied_mode"] != "steelman" {
		t.Fatalf("mode event payload = %#v", payload)
	}
	if intFromAny(payload["queued_round"], 0) != 1 || intFromAny(payload["applied_round"], 0) != 2 {
		t.Fatalf("mode event rounds = %#v", payload)
	}
}

func TestResumeRejectsInvalidTypedModeControl(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "typed-mode-rejected")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Reject invalid mode",
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run seed: %v", err)
	}
	_, err := Resume(context.Background(), sessionDir, ResumeOptions{
		Mode:           "combative",
		Rounds:         1,
		TimeoutSeconds: 5,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported relay mode") {
		t.Fatalf("invalid mode error = %v", err)
	}

	meta := mustLoadMeta(t, sessionDir)
	if meta["status"] != "attention_required" || meta["attention_required"] != true {
		t.Fatalf("invalid mode meta = %#v", meta)
	}
	transcript := mustLoadTranscript(t, sessionDir)
	if len(transcript) != 1 {
		t.Fatalf("invalid mode should not run another turn: %#v", transcript)
	}
	events, err := store.New(sessionDir).ReadEvents()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if event := firstEventOfType(events, "mode_control_rejected"); event == nil {
		t.Fatalf("missing mode_control_rejected event: %#v", events)
	}
	report, err := inspect.BuildShowTranscriptReport(sessionDir, 0, "")
	if err != nil {
		t.Fatalf("show report: %v", err)
	}
	diagnostics := report["diagnostics"].(map[string]any)
	if diagnostics["attention_required"] != true || len(asSlice(diagnostics["mode_control_rejections"])) != 1 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestResumePromptMentioningModeDoesNotMutateMode(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "free-text-mode")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Free text should not control mode",
		Agents:         []string{"codex", "codex"},
		Mode:           "adversarial",
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run seed: %v", err)
	}
	if _, err := Resume(context.Background(), sessionDir, ResumeOptions{
		Prompt:         "Please switch to steelman mode for the next response.",
		Rounds:         1,
		TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("resume with free-text mode request: %v", err)
	}

	meta := mustLoadMeta(t, sessionDir)
	if meta["mode"] != "adversarial" {
		t.Fatalf("free-text prompt mutated mode: %#v", meta)
	}
	transcript := mustLoadTranscript(t, sessionDir)
	if transcript[1]["mode"] != "adversarial" {
		t.Fatalf("free-text transcript mode = %#v", transcript)
	}
	events, err := store.New(sessionDir).ReadEvents()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if event := firstEventOfType(events, "mode_control_applied"); event != nil {
		t.Fatalf("free-text prompt should not create mode control event: %#v", event)
	}
}

func TestResumePersistsInputBundlesAndInjectsPrompt(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "resume-input-bundles")
	contextPath := filepath.Join(env.projectDir, "resume-context.md")
	skillPath := filepath.Join(env.projectDir, "resume-skill.md")
	if err := os.WriteFile(contextPath, []byte("Resume context marker\n"), 0o644); err != nil {
		t.Fatalf("write context: %v", err)
	}
	if err := os.WriteFile(skillPath, []byte("Resume skill marker\n"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Resume with input bundles",
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run seed: %v", err)
	}
	if _, err := Resume(context.Background(), sessionDir, ResumeOptions{
		ContextFiles:   []string{contextPath},
		SkillFiles:     []string{skillPath},
		Rounds:         1,
		TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("resume with input bundles: %v", err)
	}

	meta := mustLoadMeta(t, sessionDir)
	resumeRefs := meta["resume_input_bundle_refs"].([]any)
	if len(resumeRefs) != 2 {
		t.Fatalf("resume input refs = %#v", resumeRefs)
	}
	kinds := map[string]bool{}
	st := store.New(sessionDir)
	for _, rawRef := range resumeRefs {
		ref := rawRef.(map[string]any)
		kinds[stringFromAny(ref["bundle_kind"])] = true
		if ref["phase"] != "resume_round_2" {
			t.Fatalf("resume ref phase = %#v", ref)
		}
		artifact, err := st.LoadArtifact(ref["artifact_ref"].(map[string]any))
		if err != nil {
			t.Fatalf("load input artifact: %v", err)
		}
		if artifact["kind"] != "input_bundle" || artifact["content"] == "" {
			t.Fatalf("input artifact = %#v", artifact)
		}
	}
	if !kinds["context"] || !kinds["skill"] {
		t.Fatalf("input bundle kinds = %#v", kinds)
	}
	transcript := mustLoadTranscript(t, sessionDir)
	if len(transcript) != 2 || !strings.Contains(stringFromAny(transcript[1]["content"]), "resume input bundles") {
		t.Fatalf("resumed transcript did not reflect input bundles: %#v", transcript)
	}
}

func TestResumeSlotReplacementMaintainsLogicalAlternationAndHistory(t *testing.T) {
	env := setupPhase11FakeProviders(t)
	settingsPath := writePhase11ProviderRelaySettings(t, env)
	sessionDir := filepath.Join(env.relayHome, "sessions", "slot-replacement")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Exercise slot replacement",
		Agents:         []string{"codex", "codex"},
		SettingsPath:   settingsPath,
		Rounds:         2,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run seed: %v", err)
	}

	if _, err := Resume(context.Background(), sessionDir, ResumeOptions{
		ReplaceAgents:  []string{"phase11-gemini", ""},
		Rounds:         1,
		TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("resume with gemini replacement: %v", err)
	}
	transcript := mustLoadTranscript(t, sessionDir)
	if len(transcript) != 3 {
		t.Fatalf("transcript entries after first replacement = %d", len(transcript))
	}
	replacedTurn := transcript[2]
	if replacedTurn["slot_id"] != "slot_0_gen2" || replacedTurn["logical_slot_id"] != "slot_0" || intFromAny(replacedTurn["slot_generation"], 0) != 2 {
		t.Fatalf("first replacement turn slot metadata = %#v", replacedTurn)
	}
	if !strings.Contains(stringFromAny(replacedTurn["content"]), "Fake Gemini slot_0_gen2") {
		t.Fatalf("replacement turn did not use gemini provider: %#v", replacedTurn)
	}

	if _, err := Resume(context.Background(), sessionDir, ResumeOptions{
		ReplaceAgents:  []string{"codex", ""},
		Rounds:         2,
		TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("resume with second replacement: %v", err)
	}
	transcript = mustLoadTranscript(t, sessionDir)
	if len(transcript) != 5 {
		t.Fatalf("transcript entries after second replacement = %d", len(transcript))
	}
	if transcript[3]["slot_id"] != "slot_1" || transcript[3]["logical_slot_id"] != "slot_1" {
		t.Fatalf("logical alternation should continue with slot_1 after replacing slot_0: %#v", transcript[3])
	}
	if transcript[4]["slot_id"] != "slot_0_gen3" || transcript[4]["logical_slot_id"] != "slot_0" || intFromAny(transcript[4]["slot_generation"], 0) != 3 {
		t.Fatalf("second replacement turn slot metadata = %#v", transcript[4])
	}

	meta := mustLoadMeta(t, sessionDir)
	slots := meta["slots"].([]any)
	slot0 := slots[0].(map[string]any)
	if slot0["backend"] != "codex" || slot0["slot_id"] != "slot_0_gen3" || slot0["logical_slot_id"] != "slot_0" || intFromAny(slot0["generation"], 0) != 3 {
		t.Fatalf("current replacement slot = %#v", slot0)
	}
	history := meta["slot_replacement_history"].([]any)
	if len(history) != 2 {
		t.Fatalf("slot replacement history = %#v", history)
	}
	firstReplacement := history[0].(map[string]any)
	if firstReplacement["previous_slot_id"] != "slot_0" || firstReplacement["new_slot_id"] != "slot_0_gen2" || firstReplacement["new_profile_id"] != "phase11-gemini" {
		t.Fatalf("first replacement history = %#v", firstReplacement)
	}
	if firstReplacement["cleanup_preserves_artifacts"] != true || len(asSlice(firstReplacement["preserved_artifact_roots"])) == 0 {
		t.Fatalf("replacement cleanup hints missing: %#v", firstReplacement)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "codex", "slot_0")); err != nil {
		t.Fatalf("old codex artifacts were not preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "gemini", "slot_0_gen2")); err != nil {
		t.Fatalf("old gemini replacement artifacts were not preserved: %v", err)
	}

	events, err := store.New(sessionDir).ReadEvents()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(eventsOfType(events, "slot_replaced")) != 2 {
		t.Fatalf("slot replacement events missing: %#v", events)
	}
	graphReport, err := inspect.BuildShowGraphReport(sessionDir)
	if err != nil {
		t.Fatalf("graph report: %v", err)
	}
	graphData := graphReport["graph"].(map[string]any)
	if replacements := asSlice(graphData["slot_replacements"]); len(replacements) != 2 {
		t.Fatalf("graph slot replacements = %#v", replacements)
	}
	contractsReport, err := inspect.BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("contracts report: %v", err)
	}
	if replacements := asSlice(contractsReport["slot_replacement_history"]); len(replacements) != 2 {
		t.Fatalf("contracts replacement history = %#v", replacements)
	}
	if formatted := inspect.FormatContractsReport(contractsReport); !strings.Contains(formatted, "Slot replacements: 2") {
		t.Fatalf("contracts output missing replacements:\n%s", formatted)
	}
	showReport, err := inspect.BuildShowTranscriptReport(sessionDir, 0, "")
	if err != nil {
		t.Fatalf("show report: %v", err)
	}
	if markdown := inspect.FormatTranscriptMarkdown(showReport); !strings.Contains(markdown, "## Slot Replacements") || !strings.Contains(markdown, "slot_0 generation 1 -> 2") {
		t.Fatalf("show markdown missing replacement history:\n%s", markdown)
	}
	exportReport, err := inspect.BuildExportReport(sessionDir, false)
	if err != nil {
		t.Fatalf("export report: %v", err)
	}
	if markdown := inspect.FormatExportMarkdown(exportReport); !strings.Contains(markdown, "slot_0 generation 2 -> 3") {
		t.Fatalf("export markdown missing replacement history:\n%s", markdown)
	}
	displayHTML, err := inspect.BuildDisplayHTML(sessionDir)
	if err != nil {
		t.Fatalf("display html: %v", err)
	}
	if !strings.Contains(displayHTML, "Slot Replacements") || !strings.Contains(displayHTML, "slot_0 generation 1 -&gt; 2") {
		t.Fatalf("display HTML missing replacement history:\n%s", displayHTML)
	}
}

func TestRunPersistsTransientRecipeFileAndContractsVisibility(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "transient-recipe-file")
	recipePath := filepath.Join(env.projectDir, "transient-recipes.toml")
	if err := os.WriteFile(recipePath, []byte(`
[backend_profiles.local-codex]
backend = "codex"
model = "local-model"

[relay_recipes.transient-review]
participants = ["local-codex", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "steelman"
max_rounds = 1
max_depth = 1
`), 0o644); err != nil {
		t.Fatalf("write recipe file: %v", err)
	}

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Persist transient recipe",
		Agents:         []string{"codex", "codex"},
		RecipeFiles:    []string{recipePath},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run with transient recipe file: %v", err)
	}

	meta := mustLoadMeta(t, sessionDir)
	recipeRefs := meta["transient_recipe_refs"].([]any)
	if len(recipeRefs) != 1 {
		t.Fatalf("transient recipe refs = %#v", recipeRefs)
	}
	firstRef := recipeRefs[0].(map[string]any)
	if ids := firstRef["recipe_ids"].([]any); len(ids) != 1 || ids[0] != "transient-review" {
		t.Fatalf("recipe ids = %#v", firstRef["recipe_ids"])
	}
	sourceDigest := stringFromAny(firstRef["source_digest"])
	recipeDigests := firstRef["recipe_digests"].(map[string]any)
	recipeDigest := stringFromAny(recipeDigests["transient-review"])
	if sourceDigest == "" || recipeDigest == "" {
		t.Fatalf("transient digest pair missing: %#v", firstRef)
	}
	st := store.New(sessionDir)
	artifact, err := st.LoadArtifact(firstRef["artifact_ref"].(map[string]any))
	if err != nil {
		t.Fatalf("load transient recipe artifact: %v", err)
	}
	if artifact["kind"] != "transient_recipe_file" || !strings.Contains(stringFromAny(artifact["content"]), "transient-review") {
		t.Fatalf("transient recipe artifact = %#v", artifact)
	}
	if artifact["source_digest"] != sourceDigest || artifact["recipe_digests"].(map[string]any)["transient-review"] != recipeDigest {
		t.Fatalf("transient recipe artifact digests = %#v, want source %s recipe %s", artifact, sourceDigest, recipeDigest)
	}
	eagerRefs := meta["transient_recipe_contract_refs"].([]any)
	if len(eagerRefs) != 1 {
		t.Fatalf("transient recipe contract refs = %#v", eagerRefs)
	}
	eagerRecipeRef := eagerRefs[0].(map[string]any)["recipe_ref"].(map[string]any)
	if eagerRecipeRef["id"] != "recipe:transient-review" || eagerRecipeRef["digest"] != recipeDigest {
		t.Fatalf("eager recipe ref = %#v, want recipe digest %s", eagerRecipeRef, recipeDigest)
	}
	eagerRecipe, err := st.LoadArtifactPayloadRaw(eagerRecipeRef)
	if err != nil {
		t.Fatalf("load eager recipe artifact: %v", err)
	}
	if eagerRecipe["kind"] != "recipe" || eagerRecipe["id"] != "transient-review" {
		t.Fatalf("eager recipe artifact = %#v", eagerRecipe)
	}
	snapshot, err := st.LoadArtifact(meta["runtime_config_ref"].(map[string]any))
	if err != nil {
		t.Fatalf("load runtime snapshot: %v", err)
	}
	if snapshot["relay_recipes"].(map[string]any)["transient-review"] == nil {
		t.Fatalf("runtime snapshot missing transient recipe: %#v", snapshot["relay_recipes"])
	}
	report, err := inspect.BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("contracts report: %v", err)
	}
	contractRefs := report["transient_recipe_refs"].([]any)
	if len(contractRefs) != 1 || contractRefs[0].(map[string]any)["ok"] != true {
		t.Fatalf("contract transient refs = %#v", contractRefs)
	}
	graphReport, err := inspect.BuildShowGraphReport(sessionDir)
	if err != nil {
		t.Fatalf("graph report: %v", err)
	}
	artifacts := graphReport["graph"].(map[string]any)["artifacts"].(map[string]any)
	if artifacts["transient_recipes/recipe_file1"] == nil {
		t.Fatalf("graph artifacts missing transient recipe: %#v", artifacts)
	}
}

func TestRunRejectsInvalidTransientRecipeFile(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "invalid-transient-recipe-file")
	recipePath := filepath.Join(env.projectDir, "invalid-recipes.toml")
	if err := os.WriteFile(recipePath, []byte(`
[relay_recipes.invalid-review]
participants = ["missing-profile", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
`), 0o644); err != nil {
		t.Fatalf("write recipe file: %v", err)
	}

	_, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Reject transient recipe",
		Agents:         []string{"codex", "codex"},
		RecipeFiles:    []string{recipePath},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err == nil || !strings.Contains(err.Error(), "Transient recipe file") {
		t.Fatalf("transient recipe validation error = %v", err)
	}
	if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("invalid transient recipe should not create a session, stat err: %v", statErr)
	}
}

func TestRunAppliesGeneratedTransientRecipeOverride(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "generated-transient-recipe")
	source := recipes.TransientRecipeSource{
		SourceType:  recipes.TransientRecipeSourceGenerated,
		Path:        "-",
		DisplayName: "generated-stdin",
		RawTOML: []byte(`
[backend_profiles.gen-generated-slot-a]
backend = "codex"
model = "generated-model"

[relay_recipes.generated-review]
participants = ["gen-generated-slot-a", "codex-deep"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
auto_approval = "auto-safe"
`),
	}

	if _, err := Run(context.Background(), Options{
		SessionDir:             sessionDir,
		Task:                   "Persist generated transient recipe",
		Agents:                 []string{"relay", "codex"},
		SlotConfigs:            []SlotConfig{{Model: "generated-review", Effort: "1"}, {}},
		TransientRecipeSources: []recipes.TransientRecipeSource{source},
		Rounds:                 1,
		TimeoutSeconds:         5,
		LaunchCWD:              env.projectDir,
	}); err != nil {
		t.Fatalf("run with generated transient recipe: %v", err)
	}

	meta := mustLoadMeta(t, sessionDir)
	recipeRefs := meta["transient_recipe_refs"].([]any)
	if len(recipeRefs) != 1 || recipeRefs[0].(map[string]any)["source_type"] != recipes.TransientRecipeSourceGenerated {
		t.Fatalf("transient recipe refs = %#v", recipeRefs)
	}
	firstRef := recipeRefs[0].(map[string]any)
	sourceDigest := stringFromAny(firstRef["source_digest"])
	recipeDigest := stringFromAny(firstRef["recipe_digests"].(map[string]any)["generated-review"])
	if sourceDigest == "" || recipeDigest == "" {
		t.Fatalf("generated digest pair missing: %#v", firstRef)
	}
	st := store.New(sessionDir)
	eagerRefs := meta["transient_recipe_contract_refs"].([]any)
	if len(eagerRefs) != 1 {
		t.Fatalf("generated recipe contract refs = %#v", eagerRefs)
	}
	eagerRecipeRef := eagerRefs[0].(map[string]any)["recipe_ref"].(map[string]any)
	if eagerRecipeRef["id"] != "recipe:generated-review" || eagerRecipeRef["digest"] != recipeDigest {
		t.Fatalf("generated eager recipe ref = %#v, want digest %s", eagerRecipeRef, recipeDigest)
	}
	snapshot, err := st.LoadArtifact(meta["runtime_config_ref"].(map[string]any))
	if err != nil {
		t.Fatalf("load runtime snapshot: %v", err)
	}
	recipe := snapshot["relay_recipes"].(map[string]any)["generated-review"].(map[string]any)
	if recipe["auto_approval"] != "never" || recipe["origin"] != "generated" || recipe["generated_from_ref"] != "-" {
		t.Fatalf("generated recipe snapshot = %#v", recipe)
	}
	profile := snapshot["backend_profiles"].(map[string]any)["gen-generated-slot-a"].(map[string]any)
	if profile["origin"] != "generated" || profile["generated_from_ref"] != "-" {
		t.Fatalf("generated profile snapshot = %#v", profile)
	}
	graphPayload := st.LoadGraph()
	graphRecipe := graphPayload["relay_recipes"].(map[string]any)["generated-review"].(map[string]any)
	if graphRecipe["origin"] != "generated" {
		t.Fatalf("generated recipe graph = %#v", graphRecipe)
	}
	snapshotConfig, err := loadRuntimeConfigFromSnapshot(st, meta["runtime_config_ref"])
	if err != nil {
		t.Fatalf("load runtime config from snapshot: %v", err)
	}
	if snapshotConfig.RelayRecipes["generated-review"]["origin"] != "generated" {
		t.Fatalf("snapshot config recipe = %#v", snapshotConfig.RelayRecipes["generated-review"])
	}
	resumeConfig, _, err := effectiveRuntimeConfigForResume(st, meta, ResumeOptions{})
	if err != nil {
		t.Fatalf("effective runtime config for resume: %v", err)
	}
	if resumeConfig.RelayRecipes["generated-review"]["origin"] != "generated" {
		t.Fatalf("resume config recipe = %#v", resumeConfig.RelayRecipes["generated-review"])
	}
	report, err := inspect.BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("contracts report: %v", err)
	}
	relayBundles := report["relay_backend_child_contract_bundles"].([]any)
	if len(relayBundles) != 1 {
		t.Fatalf("relay backend bundle count = %d, want 1", len(relayBundles))
	}
	refs := relayBundles[0].(map[string]any)["refs"].(map[string]any)
	laterRecipeStatus := refs["recipe_ref"].(map[string]any)
	laterRecipeRef := laterRecipeStatus["ref"].(map[string]any)
	if laterRecipeRef["id"] != eagerRecipeRef["id"] || laterRecipeRef["digest"] != eagerRecipeRef["digest"] {
		t.Fatalf("later recipe ref = %#v, want eager %#v", laterRecipeRef, eagerRecipeRef)
	}
	compiledPlanRef := refs["compiled_plan_ref"].(map[string]any)["ref"].(map[string]any)
	compiledPlan, err := st.LoadArtifactPayloadRaw(compiledPlanRef)
	if err != nil {
		t.Fatalf("load compiled plan artifact: %v", err)
	}
	compiledRecipeRef := compiledPlan["recipe_ref"].(map[string]any)
	if compiledRecipeRef["id"] != eagerRecipeRef["id"] || compiledRecipeRef["digest"] != eagerRecipeRef["digest"] {
		t.Fatalf("compiled plan recipe_ref = %#v, want eager %#v", compiledRecipeRef, eagerRecipeRef)
	}
	if digests := artifactDigestsForRefID(st, "recipe:generated-review"); len(digests) != 1 || !digests[recipeDigest] {
		t.Fatalf("recipe artifact digests for generated-review = %#v, want only %s", digests, recipeDigest)
	}
}

func TestRunRejectsOversizedTransientRecipeBeforeSessionCreation(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "oversized-transient-recipe")
	recipePath := filepath.Join(env.projectDir, "oversized-recipes.toml")
	if err := os.WriteFile(recipePath, []byte(strings.Repeat("x", int(recipes.TransientRecipeSourceMaxBytes)+1)), 0o644); err != nil {
		t.Fatalf("write oversized recipe file: %v", err)
	}

	_, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Reject oversized transient recipe",
		Agents:         []string{"codex", "codex"},
		RecipeFiles:    []string{recipePath},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized transient recipe error = %v", err)
	}
	if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("oversized transient recipe should not create a session, stat err: %v", statErr)
	}
}

func TestMutateSessionRuntimeConfigPersistsSnapshotGraphAndSupportsApproval(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "runtime-config-mutation")
	runtimeConfig, err := recipes.LoadRuntimeConfig(filepath.Join(env.relayHome, "missing-runtime-limits.toml"))
	if err != nil {
		t.Fatalf("load runtime config: %v", err)
	}
	runtimeConfig.Limits.IntegrationBundleMaxBytes = 2_097_152
	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Runtime config mutation seed",
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
		RuntimeConfig:  runtimeConfig,
	}); err != nil {
		t.Fatalf("run seed: %v", err)
	}

	st := store.New(sessionDir)
	originalMeta := mustLoadMeta(t, sessionDir)
	source := recipes.TransientRecipeSource{
		SourceType:  recipes.TransientRecipeSourceGenerated,
		Path:        "generated.toml",
		DisplayName: "generated.toml",
		RawTOML: []byte(`
[relay_recipes.gen-approval-review]
participants = ["codex-fast", "codex-deep"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
`),
	}
	report, err := mutateSessionRuntimeConfig(sessionDir, []recipes.TransientRecipeSource{source})
	if err != nil {
		t.Fatalf("mutate runtime config: %v", err)
	}
	if ids := report["recipe_ids"].([]any); len(ids) != 1 || ids[0] != "gen-approval-review" {
		t.Fatalf("mutation report = %#v", report)
	}

	mutatedMeta := mustLoadMeta(t, sessionDir)
	if fmt.Sprint(mutatedMeta["runtime_config_ref"]) == fmt.Sprint(originalMeta["runtime_config_ref"]) {
		t.Fatalf("runtime_config_ref did not change")
	}
	snapshot, err := st.LoadArtifact(mutatedMeta["runtime_config_ref"].(map[string]any))
	if err != nil {
		t.Fatalf("load mutated runtime snapshot: %v", err)
	}
	snapshotRecipe := snapshot["relay_recipes"].(map[string]any)["gen-approval-review"].(map[string]any)
	if snapshotRecipe["origin"] != "generated" {
		t.Fatalf("snapshot recipe = %#v", snapshotRecipe)
	}
	graphPayload := st.LoadGraph()
	graphRecipe := graphPayload["relay_recipes"].(map[string]any)["gen-approval-review"].(map[string]any)
	if graphRecipe["origin"] != "generated" || fmt.Sprint(graphPayload["runtime_config_ref"]) != fmt.Sprint(mutatedMeta["runtime_config_ref"]) {
		t.Fatalf("graph runtime config = %#v", graphPayload)
	}

	proposal := map[string]any{
		"proposal_id":          "sp_generated",
		"parent_node_id":       graph.RootNodeID,
		"trigger_event_id":     "evt_generated",
		"proposed_by":          "test",
		"contested_lineage_id": "ln_generated",
		"delegated_question":   "Resolve generated approval",
		"selected_recipe_id":   "gen-approval-review",
		"requested_depth":      1,
		"requested_rounds":     1,
		"status":               "proposed",
		"created_at":           utcNow(),
		"updated_at":           utcNow(),
	}
	if err := st.SaveProposalMap(proposal); err != nil {
		t.Fatalf("save generated proposal: %v", err)
	}
	graphPayload = st.LoadGraph()
	delete(graphPayload, "backend_profiles")
	delete(graphPayload, "relay_recipes")
	if err := st.SaveGraph(graphPayload); err != nil {
		t.Fatalf("remove graph runtime config: %v", err)
	}

	approval, err := ApproveProposal(context.Background(), sessionDir, ApproveOptions{
		ProposalID:          "sp_generated",
		Rounds:              1,
		TimeoutSeconds:      5,
		StallTimeoutSeconds: 5,
		SettingsPath:        filepath.Join(env.relayHome, "missing-live-settings.toml"),
	})
	if err != nil {
		t.Fatalf("approve generated proposal via snapshot: %v", err)
	}
	if approval["status"] != "collapsed" {
		t.Fatalf("approval = %#v", approval)
	}
	childSessionID := stringFromAny(approval["child_session_id"])
	childDir := filepath.Join(env.relayHome, "sessions", childSessionID)
	childStore := store.New(childDir)
	childMeta := mustLoadMeta(t, childDir)
	childSnapshot, err := childStore.LoadArtifact(childMeta["runtime_config_ref"].(map[string]any))
	if err != nil {
		t.Fatalf("load child runtime snapshot: %v", err)
	}
	childSnapshotLimits := childSnapshot["limits"].(map[string]any)
	if fmt.Sprint(childSnapshotLimits["integration_bundle_max_bytes"]) != "2097152" {
		t.Fatalf("child runtime snapshot limits = %#v", childSnapshotLimits)
	}
	childGraphLimits := childStore.LoadGraph()["runtime_limits"].(map[string]any)
	if fmt.Sprint(childGraphLimits["integration_bundle_max_bytes"]) != "2097152" {
		t.Fatalf("child graph runtime limits = %#v", childGraphLimits)
	}
}

func TestEffectiveRuntimeConfigForLegacySessionsFallsBackToGraphThenSettings(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "legacy-session")
	st := store.New(sessionDir)
	graphConfig, err := recipes.LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if err != nil {
		t.Fatalf("load graph config seed: %v", err)
	}
	graphConfig.RelayRecipes["graph-review"] = map[string]any{
		"kind":           "recipe",
		"schema_version": 1,
		"id":             "graph-review",
		"participants":   []any{"codex-fast", "codex-deep"},
		"facilitator":    "codex-fast",
		"reducer":        "codex-deep",
		"mode":           "adversarial",
		"max_rounds":     1,
		"max_depth":      1,
		"auto_approval":  "ask",
		"match_keywords": []any{},
	}
	if err := st.SaveGraph(map[string]any{
		"nodes":            map[string]any{},
		"edges":            []any{},
		"backend_profiles": graphConfig.BackendProfiles,
		"relay_recipes":    graphConfig.RelayRecipes,
	}); err != nil {
		t.Fatalf("save graph config: %v", err)
	}
	config, _, err := effectiveRuntimeConfigForSession(st, map[string]any{}, "")
	if err != nil {
		t.Fatalf("effective graph config: %v", err)
	}
	if config.RelayRecipes["graph-review"] == nil {
		t.Fatalf("graph config recipe missing: %#v", config.RelayRecipes)
	}

	settingsPath := filepath.Join(t.TempDir(), "settings.toml")
	if err := os.WriteFile(settingsPath, []byte(`
[relay_recipes.live-review]
participants = ["codex-fast", "codex-deep"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
`), 0o644); err != nil {
		t.Fatalf("write live settings: %v", err)
	}
	st = store.New(filepath.Join(t.TempDir(), "legacy-live-session"))
	config, _, err = effectiveRuntimeConfigForSession(st, map[string]any{"settings_path": settingsPath}, "")
	if err != nil {
		t.Fatalf("effective live config: %v", err)
	}
	if config.RelayRecipes["live-review"] == nil {
		t.Fatalf("live config recipe missing: %#v", config.RelayRecipes)
	}
}

func TestGeneratedRecipeCannotAutoAdmitUnderAutoSafe(t *testing.T) {
	decision := makeInitialAdmissionDecision(
		map[string]any{"proposal_id": "sp_generated", "requested_rounds": 1},
		"auto-safe",
		map[string]any{"id": "gen-review", "origin": "generated", "auto_approval": "auto-safe"},
	)
	if decision["decision"] != "require_user" {
		t.Fatalf("decision = %#v", decision)
	}
	reasons := decision["reasons"].([]any)
	if len(reasons) != 1 || reasons[0] != "generated recipe requires operator review" {
		t.Fatalf("decision reasons = %#v", decision)
	}
}

func TestRunPersistsLaunchPlanAndInitialPromptContext(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "go-phase14-launch-plan")
	contextPath := filepath.Join(env.projectDir, "context.md")
	if err := os.WriteFile(contextPath, []byte("context body\n"), 0o644); err != nil {
		t.Fatalf("write context: %v", err)
	}

	if _, err := Run(context.Background(), Options{
		SessionDir:        sessionDir,
		Task:              "Original task",
		ContextFiles:      []string{contextPath},
		SkillsText:        "\n### capability.md\n````text\nskill body\n````\n",
		InvestigationMode: "context_only",
		LaunchPlan: map[string]any{
			"explanation": "Prepare the relay",
			"plan": []any{
				map[string]any{"step": "Persist launch plan", "status": "completed"},
			},
		},
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run with context and plan: %v", err)
	}

	meta := mustLoadMeta(t, sessionDir)
	if meta["task"] != "Original task" || meta["initial_prompt"] != "Original task" {
		t.Fatalf("task metadata should remain original: %#v", meta)
	}
	plan, _ := meta["launch_plan"].(map[string]any)
	if plan["explanation"] != "Prepare the relay" {
		t.Fatalf("launch plan = %#v", meta["launch_plan"])
	}
	if meta["investigation_mode"] != "context_only" || meta["prompt_policy_version"] != PromptPolicyVersion {
		t.Fatalf("prompt policy metadata = %#v", meta)
	}
	contextRefs := meta["launch_context_refs"].([]any)
	if len(contextRefs) != 1 {
		t.Fatalf("launch context refs = %#v", contextRefs)
	}
	contextRef := contextRefs[0].(map[string]any)
	if contextRef["label"] != "ctx1" || contextRef["embedded"] != true || !strings.HasPrefix(stringFromAny(contextRef["digest"]), "sha256:") {
		t.Fatalf("bad context ref metadata: %#v", contextRef)
	}
	artifactRef, ok := contextRef["artifact_ref"].(map[string]any)
	if !ok {
		t.Fatalf("context ref missing artifact ref: %#v", contextRef)
	}
	st := store.New(sessionDir)
	artifact, err := st.LoadArtifact(artifactRef)
	if err != nil {
		t.Fatalf("load launch context artifact: %v", err)
	}
	if artifact["content"] != "context body\n" || artifact["label"] != "ctx1" {
		t.Fatalf("launch context artifact = %#v", artifact)
	}

	policy, err := BuildPromptPolicy("context_only", true)
	if err != nil {
		t.Fatalf("build policy: %v", err)
	}
	taskWithContext := BuildTaskWithLaunchContext("Original task", []LaunchContext{{
		Label:       "ctx1",
		DisplayName: "context.md",
		SourcePath:  contextPath,
		Digest:      stringFromAny(contextRef["digest"]),
		SizeBytes:   int64(len("context body\n")),
		Embedded:    true,
		Content:     "context body\n",
	}})
	prompt := frameInitial("Original task", "Codex (B)", 1, "adversarial", true, taskWithContext, "skill body", policy)
	if !strings.Contains(prompt, "context body") || !strings.Contains(prompt, "--- Available Capabilities ---") || !strings.Contains(prompt, "skill body") || !strings.Contains(prompt, "do not inspect repositories") {
		t.Fatalf("initial prompt missing context or skills:\n%s", prompt)
	}
}

func TestRunRejectsInvalidContextBeforeSessionCreation(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "invalid-context")
	contextPath := filepath.Join(env.projectDir, "binary.dat")
	if err := os.WriteFile(contextPath, []byte{0xff, 0x00, 0x01}, 0o644); err != nil {
		t.Fatalf("write invalid context: %v", err)
	}

	_, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Invalid context should fail before session",
		ContextFiles:   []string{contextPath},
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err == nil || !strings.Contains(err.Error(), "binary or unsupported") {
		t.Fatalf("expected unsupported context error, got %v", err)
	}
	if _, statErr := os.Stat(sessionDir); !os.IsNotExist(statErr) {
		t.Fatalf("session dir should not be created on context preflight failure, stat err: %v", statErr)
	}
}

func TestPreflightLaunchContextsRejectsDuplicateAndOversizedInputs(t *testing.T) {
	dir := t.TempDir()
	contextPath := filepath.Join(dir, "context.md")
	if err := os.WriteFile(contextPath, []byte("context"), 0o644); err != nil {
		t.Fatalf("write context: %v", err)
	}
	if _, err := PreflightLaunchContexts([]string{contextPath, contextPath}); err == nil || !strings.Contains(err.Error(), "duplicate context") {
		t.Fatalf("expected duplicate context error, got %v", err)
	}

	largePath := filepath.Join(dir, "large.md")
	largeBody := strings.Repeat("x", int(MaxLaunchContextFileBytes)+1)
	if err := os.WriteFile(largePath, []byte(largeBody), 0o644); err != nil {
		t.Fatalf("write large context: %v", err)
	}
	if _, err := PreflightLaunchContexts([]string{largePath}); err == nil || !strings.Contains(err.Error(), "limit is") {
		t.Fatalf("expected oversized context error, got %v", err)
	}
}

func TestPromptPolicyFragmentAppearsAcrossPromptTypes(t *testing.T) {
	autoPolicy, err := BuildPromptPolicy("auto", true)
	if err != nil {
		t.Fatalf("auto policy: %v", err)
	}
	initial := frameInitial("Investigate repo", "Codex (B)", 2, "adversarial", true, "Investigate repo", "", autoPolicy)
	relay := frameRelay("Prior claim", "Codex (A)", 2, 2, "Investigate repo", "adversarial", emptyLedger(), autoPolicy)
	resume := frameResumeDirection(map[string]any{"from": "Codex (A)", "content": "Prior claim"}, 2, 2, "Investigate repo", "Check files", "adversarial", emptyLedger(), autoPolicy)
	for name, prompt := range map[string]string{"initial": initial, "relay": relay, "resume": resume} {
		if !strings.Contains(prompt, "Evidence policy: inspect relevant repository files") || !strings.Contains(prompt, "[ctx1]") {
			t.Fatalf("%s prompt missing evidence policy:\n%s", name, prompt)
		}
	}

	normalPolicy, err := BuildPromptPolicy("normal", false)
	if err != nil {
		t.Fatalf("normal policy: %v", err)
	}
	normal := frameInitial("Conceptual debate", "Codex (B)", 1, "adversarial", true, "", "", normalPolicy)
	if strings.Contains(normal, "Evidence policy") {
		t.Fatalf("normal prompt should not force evidence policy:\n%s", normal)
	}
}

func TestPromptPolicyRejectsContextOnlyWithoutContext(t *testing.T) {
	if _, err := BuildPromptPolicy("context_only", false); err == nil || !strings.Contains(err.Error(), "requires at least one --context file") {
		t.Fatalf("expected context_only without context error, got %v", err)
	}
}

func TestRunRelayBackendExecutesNestedRelayProfiles(t *testing.T) {
	env := setupFakeCodex(t)
	settingsPath := writeNestedRelaySettings(t, env)
	sessionDir := filepath.Join(env.relayHome, "sessions", "go-phase6-nested")

	result, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Run nested relay profiles from Go",
		Agents:         []string{"relay", "relay"},
		SlotConfigs:    []SlotConfig{{Model: "outer-review", Effort: "1"}, {Model: "outer-review", Effort: "1"}},
		Rounds:         2,
		TimeoutSeconds: 5,
		SettingsPath:   settingsPath,
		LaunchCWD:      env.projectDir,
	})
	if err != nil {
		t.Fatalf("run nested relay: %v", err)
	}
	if result["status"] != "completed" || result["actual_rounds"] != 2 {
		t.Fatalf("nested result = %#v", result)
	}
	report, err := inspect.BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("contracts report: %v", err)
	}
	relayBundles := report["relay_backend_child_contract_bundles"].([]any)
	if len(relayBundles) != 2 {
		t.Fatalf("relay backend bundle count = %d, want 2", len(relayBundles))
	}
	meta := mustLoadMeta(t, sessionDir)
	slots := meta["slots"].([]any)
	firstState := slots[0].(map[string]any)["state"].(map[string]any)
	childIDs := firstState["child_session_ids"].([]any)
	if len(childIDs) != 1 {
		t.Fatalf("first relay child IDs = %#v", childIDs)
	}
	if refs := firstState["child_contract_refs"].([]any); len(refs) != 1 {
		t.Fatalf("first relay child refs = %#v", refs)
	}
	childDir := filepath.Join(env.relayHome, "sessions", childIDs[0].(string))
	childGraph, err := inspect.BuildShowGraphReport(childDir)
	if err != nil {
		t.Fatalf("child graph report: %v", err)
	}
	nodes := childGraph["graph"].(map[string]any)["nodes"].(map[string]any)
	foundNested := false
	for _, rawNode := range nodes {
		node, _ := rawNode.(map[string]any)
		if node["kind"] == "relay_backend_child" && node["composition_path"] == "root.slot_0.slot_0" {
			foundNested = true
		}
	}
	if !foundNested {
		t.Fatalf("child graph missing nested relay backend node: %#v", nodes)
	}

	resumed, err := Resume(context.Background(), sessionDir, ResumeOptions{
		Rounds:         1,
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("resume nested relay: %v", err)
	}
	if resumed["actual_rounds"] != 3 {
		t.Fatalf("resumed actual_rounds = %v, want 3", resumed["actual_rounds"])
	}
}

func TestRunRelayBackendEnforcesNestedMaxDepth(t *testing.T) {
	env := setupFakeCodex(t)
	settingsPath := writeNestedRelaySettings(t, env)
	sessionDir := filepath.Join(env.relayHome, "sessions", "go-phase6-depth")

	_, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Depth guard",
		Agents:         []string{"relay", "codex"},
		SlotConfigs:    []SlotConfig{{Model: "too-shallow", Effort: "1"}, {}},
		Rounds:         1,
		TimeoutSeconds: 5,
		SettingsPath:   settingsPath,
		LaunchCWD:      env.projectDir,
	})
	if err == nil || !strings.Contains(err.Error(), "relay_backend_depth_exceeded") {
		t.Fatalf("depth error = %v, want relay_backend_depth_exceeded", err)
	}
}

func TestDynamicProposalApprovalRunsChildAndCollapses(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "go-phase6-dynamic")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Persistent contested dynamic",
		Agents:         []string{"codex", "codex"},
		Rounds:         2,
		TimeoutSeconds: 5,
		DynamicMode:    "ask",
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run dynamic: %v", err)
	}
	proposalReport, err := Proposals(sessionDir)
	if err != nil {
		t.Fatalf("proposals: %v", err)
	}
	proposals := proposalReport["proposals"].([]any)
	if len(proposals) != 1 {
		t.Fatalf("proposal count = %d, want 1", len(proposals))
	}
	proposal := proposals[0].(map[string]any)
	if proposal["status"] != "proposed" {
		t.Fatalf("proposal status = %v, want proposed", proposal["status"])
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "relay.pid"), []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		t.Fatalf("write live pid: %v", err)
	}
	if _, err := ApproveProposal(context.Background(), sessionDir, ApproveOptions{
		ProposalID:          proposal["proposal_id"].(string),
		Rounds:              1,
		TimeoutSeconds:      5,
		StallTimeoutSeconds: 5,
	}); err == nil || !strings.Contains(err.Error(), "stop it before approving proposals") {
		t.Fatalf("approve live parent error = %v", err)
	}
	if err := os.Remove(filepath.Join(sessionDir, "relay.pid")); err != nil {
		t.Fatalf("remove live pid: %v", err)
	}
	proposalReport, err = Proposals(sessionDir)
	if err != nil {
		t.Fatalf("proposals after blocked approve: %v", err)
	}
	proposal = proposalReport["proposals"].([]any)[0].(map[string]any)
	if proposal["status"] != "proposed" {
		t.Fatalf("blocked approval mutated proposal = %#v", proposal)
	}
	lock, err := lockSessionMutation(sessionDir)
	if err != nil {
		t.Fatalf("lock session mutation: %v", err)
	}
	type approvalResult struct {
		report map[string]any
		err    error
	}
	approvalDone := make(chan approvalResult, 1)
	go func() {
		report, err := ApproveProposal(context.Background(), sessionDir, ApproveOptions{
			ProposalID:          proposal["proposal_id"].(string),
			Rounds:              1,
			TimeoutSeconds:      5,
			StallTimeoutSeconds: 5,
		})
		approvalDone <- approvalResult{report: report, err: err}
	}()
	select {
	case result := <-approvalDone:
		t.Fatalf("approval completed while session mutation lock was held: report=%#v err=%v", result.report, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock session mutation: %v", err)
	}
	var approval map[string]any
	select {
	case result := <-approvalDone:
		if result.err != nil {
			t.Fatalf("approve proposal: %v", result.err)
		}
		approval = result.report
	case <-time.After(10 * time.Second):
		t.Fatalf("approval did not complete after mutation lock release")
	}
	if approval["status"] != "collapsed" {
		t.Fatalf("approval = %#v", approval)
	}
	transcript := mustLoadTranscript(t, sessionDir)
	if len(transcript) != 3 || transcript[2]["source_type"] != "child_result" {
		t.Fatalf("collapsed transcript = %#v", transcript)
	}
	report, err := inspect.BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("contracts report: %v", err)
	}
	dynamicBundles := report["dynamic_child_contract_bundles"].([]any)
	if len(dynamicBundles) != 1 {
		t.Fatalf("dynamic bundle count = %d, want 1", len(dynamicBundles))
	}
	graphReport, err := inspect.BuildShowGraphReport(sessionDir)
	if err != nil {
		t.Fatalf("graph report: %v", err)
	}
	graphPayload := graphReport["graph"].(map[string]any)
	graphProposals := graphPayload["proposals"].(map[string]any)
	if graphProposals[proposal["proposal_id"].(string)].(map[string]any)["status"] != "collapsed" {
		t.Fatalf("graph proposal = %#v", graphProposals)
	}
}

func TestRunAutoStopsOnConvergedLedger(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "go-phase5-auto")

	result, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Stop when the ledger converges",
		Agents:         []string{"codex", "codex"},
		MaxRounds:      5,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err != nil {
		t.Fatalf("run auto: %v", err)
	}
	if result["round_limit_mode"] != "auto" {
		t.Fatalf("round_limit_mode = %v, want auto", result["round_limit_mode"])
	}
	if result["actual_rounds"] != 4 {
		t.Fatalf("actual_rounds = %v, want 4", result["actual_rounds"])
	}
	if result["stop_reason"] != "converged" {
		t.Fatalf("stop_reason = %v, want converged", result["stop_reason"])
	}
}

func TestRunAutoStopsOnRepeatedExplicitEmptyLedger(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "empty-ledger-explicit")

	result, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Explicit empty ledger stall",
		Agents:         []string{"codex", "codex"},
		MaxRounds:      6,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err != nil {
		t.Fatalf("run empty ledger: %v", err)
	}
	if result["stop_reason"] != "stalled_no_ledger_signal" {
		t.Fatalf("stop_reason = %v, want stalled_no_ledger_signal", result["stop_reason"])
	}
	if result["actual_rounds"] != 4 {
		t.Fatalf("actual_rounds = %v, want 4", result["actual_rounds"])
	}
	events, err := store.New(sessionDir).ReadEvents()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if unparsed := eventsOfType(events, "facilitator_ledger_unparsed"); len(unparsed) != 0 {
		t.Fatalf("explicit empty JSON should not emit unparsed event: %#v", unparsed)
	}
	for _, event := range eventsOfType(events, "turn_completed") {
		payload := event["payload"].(map[string]any)
		if payload["parse_status"] != string(LedgerParseParsedFull) {
			t.Fatalf("turn parse_status = %#v", payload)
		}
	}
}

func TestRunAutoStopsOnMalformedEmptyLedgerFallback(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "empty-ledger-malformed")

	result, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Malformed ledger stall",
		Agents:         []string{"codex", "codex"},
		MaxRounds:      6,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err != nil {
		t.Fatalf("run malformed ledger: %v", err)
	}
	if result["stop_reason"] != "stalled_no_ledger_signal" {
		t.Fatalf("stop_reason = %v, want stalled_no_ledger_signal", result["stop_reason"])
	}
	if result["actual_rounds"] != 4 {
		t.Fatalf("actual_rounds = %v, want 4", result["actual_rounds"])
	}
	events, err := store.New(sessionDir).ReadEvents()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	turns := eventsOfType(events, "turn_completed")
	if len(turns) != 4 {
		t.Fatalf("turn_completed count = %d, want 4", len(turns))
	}
	for _, event := range turns {
		payload := event["payload"].(map[string]any)
		if payload["parse_status"] != string(LedgerParseFallback) {
			t.Fatalf("turn parse_status = %#v", payload)
		}
	}
	unparsed := eventsOfType(events, "facilitator_ledger_unparsed")
	if len(unparsed) != 4 {
		t.Fatalf("unparsed event count = %d, want 4: %#v", len(unparsed), unparsed)
	}
	payload := unparsed[0]["payload"].(map[string]any)
	if payload["parse_status"] != string(LedgerParseFallback) ||
		payload["raw_digest"] == "" ||
		intFromAny(payload["raw_bytes"], 0) == 0 ||
		payload["raw_excerpt"] == "" ||
		payload["excerpt_truncated"] != true {
		t.Fatalf("unparsed payload = %#v", payload)
	}
	counts := payload["fallback_ledger_counts"].(map[string]any)
	if intFromAny(counts["settled"], -1) != 0 || intFromAny(counts["contested"], -1) != 0 || intFromAny(counts["withdrawn"], -1) != 0 {
		t.Fatalf("fallback counts = %#v", counts)
	}
}

func TestRunEmptyLedgerWithPairedDoneSignalsConverges(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "empty-ledger-done")

	result, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Empty ledger done convergence",
		Agents:         []string{"codex", "codex"},
		MaxRounds:      6,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	})
	if err != nil {
		t.Fatalf("run done convergence: %v", err)
	}
	if result["stop_reason"] != "converged" {
		t.Fatalf("stop_reason = %v, want converged", result["stop_reason"])
	}
	if result["actual_rounds"] != 4 {
		t.Fatalf("actual_rounds = %v, want 4", result["actual_rounds"])
	}
}

func TestRunCancellationMarksSessionInterrupted(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "go-phase5-interrupted")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, Options{
			SessionDir:     sessionDir,
			Task:           "Slow cancellation check",
			Agents:         []string{"codex", "codex"},
			Rounds:         2,
			TimeoutSeconds: 30,
			LaunchCWD:      env.projectDir,
		})
		done <- err
	}()

	waitForFile(t, filepath.Join(sessionDir, "relay.pid"))
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run error = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("run did not stop after cancellation")
	}
	meta := mustLoadMeta(t, sessionDir)
	if meta["status"] != "interrupted" {
		t.Fatalf("status = %v, want interrupted", meta["status"])
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "relay.pid")); !os.IsNotExist(err) {
		t.Fatalf("relay.pid still exists after interruption")
	}
}

func TestStopMarksOrphanedAndKillMarksKilled(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "go-phase5-stop")
	st := store.New(sessionDir)
	if err := st.EnsureSession(); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := st.SaveMetaMap(map[string]any{"status": "running"}); err != nil {
		t.Fatalf("save meta: %v", err)
	}
	if err := st.SaveTranscriptItems([]any{}); err != nil {
		t.Fatalf("save transcript: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "relay.pid"), []byte("99999999"), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	report, err := Stop(sessionDir, StopOptions{})
	if err != nil {
		t.Fatalf("stop orphan: %v", err)
	}
	if report["status"] != "orphaned" {
		t.Fatalf("orphan report = %#v", report)
	}
	meta := mustLoadMeta(t, sessionDir)
	if meta["status"] != "orphaned" {
		t.Fatalf("orphan meta status = %v", meta["status"])
	}

	if err := st.SaveMetaMap(map[string]any{"status": "running"}); err != nil {
		t.Fatalf("save running meta: %v", err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer cmd.Process.Kill()
	if err := os.WriteFile(filepath.Join(sessionDir, "relay.pid"), []byte(jsonNumberText(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatalf("write live pid: %v", err)
	}
	report, err = Stop(sessionDir, StopOptions{ForceKill: true})
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	if report["status"] != "killed" {
		t.Fatalf("kill report = %#v", report)
	}
	meta = mustLoadMeta(t, sessionDir)
	if meta["status"] != "killed" || meta["stop_reason"] != "killed" {
		t.Fatalf("killed meta = %#v", meta)
	}
}

func TestSessionAdminResolvesListsCleansAndCleansUp(t *testing.T) {
	env := setupFakeCodex(t)
	deadDir := filepath.Join(env.relayHome, "sessions", "phase7-dead")
	liveDir := filepath.Join(env.relayHome, "sessions", "phase7-live")
	cleanDir := filepath.Join(env.relayHome, "sessions", "phase7-clean")
	liveCleanDir := filepath.Join(env.relayHome, "sessions", "phase7-clean-live")
	rootDir := filepath.Join(env.relayHome, "sessions", "phase7-root")
	saveAdminSession(t, deadDir, map[string]any{
		"session_id":    "phase7-dead",
		"title":         "Dead running session",
		"status":        "running",
		"mode":          "adversarial",
		"actual_rounds": 1,
		"created_at":    "2026-05-19T00:02:00.000000+00:00",
	})
	saveAdminSession(t, liveDir, map[string]any{
		"session_id":    "phase7-live",
		"title":         "Completed session",
		"status":        "completed",
		"mode":          "cooperative",
		"actual_rounds": 2,
		"created_at":    "2026-05-19T00:03:00.000000+00:00",
	})
	saveAdminSession(t, cleanDir, map[string]any{
		"session_id":    "phase7-clean",
		"title":         "Clean me",
		"status":        "completed",
		"mode":          "steelman",
		"actual_rounds": 0,
		"created_at":    "2026-05-19T00:01:00.000000+00:00",
	})
	saveAdminSession(t, liveCleanDir, map[string]any{
		"session_id":    "phase7-clean-live",
		"title":         "Do not clean live",
		"status":        "completed",
		"mode":          "steelman",
		"actual_rounds": 0,
		"created_at":    "2026-05-19T00:04:00.000000+00:00",
	})
	saveAdminSession(t, rootDir, map[string]any{
		"session_id":                  "phase7-root",
		"title":                       "Root inspection session",
		"status":                      "completed",
		"mode":                        "cooperative",
		"execution_kind":              "recipe",
		"recipe_id":                   "neutral-root",
		"participant_turns":           2,
		"participant_turns_completed": 2,
		"actual_participant_turns":    2,
		"result_source":               "reducer",
		"validation_status":           "not_required",
		"facilitator_provider_state":  map[string]any{"backend": "codex", "state": map[string]any{"secret": "do-not-list"}},
		"created_at":                  "2026-05-19T00:00:00.000000+00:00",
	})
	if err := os.WriteFile(filepath.Join(deadDir, "relay.pid"), []byte("99999999"), 0o644); err != nil {
		t.Fatalf("write dead pid: %v", err)
	}
	if err := os.WriteFile(filepath.Join(liveCleanDir, "relay.pid"), []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		t.Fatalf("write live clean pid: %v", err)
	}

	resolved, err := ResolveSessionDir(env.relayHome, "", "phase7-d")
	if err != nil {
		t.Fatalf("resolve prefix: %v", err)
	}
	if resolved != deadDir {
		t.Fatalf("resolved prefix = %s, want %s", resolved, deadDir)
	}
	if _, err := ResolveSessionDir(env.relayHome, "", "phase7-"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prefix error = %v", err)
	}

	sessions, err := ListSessions(env.relayHome, 10)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	deadSummary := findSessionSummary(sessions, "phase7-dead")
	if deadSummary["status"] != "orphaned" {
		t.Fatalf("dead summary status = %#v", deadSummary)
	}
	rootSummary := findSessionSummary(sessions, "phase7-root")
	if rootSummary["root"] == nil {
		t.Fatalf("root list summary = %#v", rootSummary)
	}
	encodedRootSummary, err := json.Marshal(rootSummary)
	if err != nil {
		t.Fatalf("marshal root list summary: %v", err)
	}
	if strings.Contains(string(encodedRootSummary), "do-not-list") {
		t.Fatalf("root list exposed provider state: %s", encodedRootSummary)
	}

	cleanup, err := CleanupSessions(env.relayHome, 10, false)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if cleanup["orphaned_count"] != 1 {
		t.Fatalf("cleanup report = %#v", cleanup)
	}
	deadMeta := mustLoadMeta(t, deadDir)
	if deadMeta["status"] != "orphaned" {
		t.Fatalf("dead meta status = %#v", deadMeta)
	}
	if _, err := os.Stat(filepath.Join(deadDir, "relay.pid")); !os.IsNotExist(err) {
		t.Fatalf("dead pid still exists after cleanup")
	}

	if _, err := CleanSession(liveCleanDir); err == nil || !strings.Contains(err.Error(), "stop it before cleaning it") {
		t.Fatalf("clean live session error = %v", err)
	}
	if _, err := os.Stat(liveCleanDir); err != nil {
		t.Fatalf("live session dir should remain after blocked clean: %v", err)
	}

	cleanReport, err := CleanSession(cleanDir)
	if err != nil {
		t.Fatalf("clean session: %v", err)
	}
	if cleanReport["status"] != "deleted" {
		t.Fatalf("clean report = %#v", cleanReport)
	}
	if _, err := os.Stat(cleanDir); !os.IsNotExist(err) {
		t.Fatalf("cleaned session still exists")
	}
}

func TestRunConsumesQueuedSteeringOnResume(t *testing.T) {
	env := setupFakeCodex(t)
	sessionDir := filepath.Join(env.relayHome, "sessions", "phase7-steering")

	if _, err := Run(context.Background(), Options{
		SessionDir:     sessionDir,
		Task:           "Verify queued steering",
		Agents:         []string{"codex", "codex"},
		Rounds:         1,
		TimeoutSeconds: 5,
		LaunchCWD:      env.projectDir,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	queued, err := QueueSteeringPrompt(sessionDir, "Please switch to steelman mode. Phase 7 steering marker")
	if err != nil {
		t.Fatalf("queue steering: %v", err)
	}
	if _, err := Resume(context.Background(), sessionDir, ResumeOptions{
		Rounds:         1,
		TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	meta := mustLoadMeta(t, sessionDir)
	if meta["mode"] != "adversarial" {
		t.Fatalf("free-text steering mutated mode: %#v", meta)
	}
	history := meta["steering_history"].([]any)
	if len(history) != 1 || history[0].(map[string]any)["id"] != queued["id"] {
		t.Fatalf("steering history = %#v, queued = %#v", history, queued)
	}
	data, err := os.ReadFile(filepath.Join(sessionDir, "steering.json"))
	if err != nil {
		t.Fatalf("read steering queue: %v", err)
	}
	var remaining []any
	if err := json.Unmarshal(data, &remaining); err != nil {
		t.Fatalf("decode steering queue: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("steering queue not consumed: %#v", remaining)
	}
	transcript := mustLoadTranscript(t, sessionDir)
	if len(transcript) != 2 || !strings.Contains(stringFromAny(transcript[1]["content"]), "Phase 7 steering marker") {
		t.Fatalf("resumed transcript did not reflect steering: %#v", transcript)
	}
	if transcript[1]["mode"] != "adversarial" {
		t.Fatalf("free-text steering transcript mode = %#v", transcript)
	}
}

func TestLastBackendEntrySkipsSyntheticChildResults(t *testing.T) {
	transcript := []map[string]any{
		{"round": 1, "slot_id": "slot_0", "from": "Codex", "content": "backend"},
		{"round": 2, "slot_id": "child:abc123", "from": "Child Relay", "content": "collapsed", "synthetic": true, "source_type": "child_result"},
	}
	last := lastBackendEntry(transcript)
	if last == nil || last["slot_id"] != "slot_0" {
		t.Fatalf("last backend entry = %#v", last)
	}
	if last := lastBackendEntry(transcript[1:]); last != nil {
		t.Fatalf("synthetic-only transcript returned %#v", last)
	}
}

type fakeEnv struct {
	relayHome  string
	projectDir string
}

func setupFakeCodex(t *testing.T) fakeEnv {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	homeDir := filepath.Join(root, "home")
	relayHome := filepath.Join(root, "relayhome")
	projectDir := filepath.Join(root, "project")
	for _, dir := range []string{binDir, homeDir, relayHome, projectDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := exec.Command("git", "init", projectDir).Run(); err != nil {
		t.Fatalf("git init project: %v", err)
	}
	fakeCodex := `#!/usr/bin/env python3
import json
import os
import sys
import time
from pathlib import Path

def main():
    prompt = sys.stdin.read()
    if "Slow cancellation check" in prompt:
        time.sleep(30)
    codex_home = Path(os.environ.get("CODEX_HOME", ""))
    home_suffix = codex_home.name or "default"
    if "resume" in sys.argv:
        idx = sys.argv.index("resume")
        thread_id = sys.argv[idx + 2] if idx + 2 < len(sys.argv) and sys.argv[idx + 1] == "--json" else "thread-resumed"
    else:
        thread_id = f"thread-{home_suffix}"
        print(json.dumps({"type": "thread.started", "thread_id": thread_id}), flush=True)
    if "Return the updated ledger as JSON" in prompt and "Persistent contested dynamic" in prompt:
        text = '{"settled":[],"contested":["phase risk"],"withdrawn":[]}'
    elif "Return the updated ledger as JSON" in prompt and "Malformed ledger stall" in prompt:
        text = "not a ledger " + ("\U0001F4A5" * 60)
    elif "Return the updated ledger as JSON" in prompt and ("Explicit empty ledger stall" in prompt or "Empty ledger done convergence" in prompt):
        text = '{"settled":[],"contested":[],"withdrawn":[]}'
    elif "Return the updated ledger as JSON" in prompt:
        text = '{"settled":["done"],"contested":[],"withdrawn":[]}'
    elif "Empty ledger done convergence" in prompt:
        text = "task is complete; no further changes"
    elif "Explicit empty ledger stall" in prompt:
        text = "Explicit empty ledger stall response"
    elif "Malformed ledger stall" in prompt:
        text = "Malformed ledger stall response"
    elif "Resume context marker" in prompt and "Resume skill marker" in prompt:
        text = "Fake Codex saw resume input bundles"
    elif "Phase 7 steering marker" in prompt:
        text = "Fake Codex saw Phase 7 steering marker"
    elif "Persistent contested dynamic" in prompt:
        text = "Persistent contested dynamic phase risk remains unresolved"
    else:
        first = prompt.splitlines()[0] if prompt.splitlines() else ""
        text = f"Fake Codex {home_suffix}: {first[:80]}"
    print(json.dumps({"type": "item.completed", "item": {"text": text}}), flush=True)
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
`
	codexPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codexPath, []byte(fakeCodex), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", homeDir)
	t.Setenv("CODEX_CLAUDE_HOME", relayHome)
	return fakeEnv{relayHome: relayHome, projectDir: projectDir}
}

func mustLoadMeta(t *testing.T, sessionDir string) map[string]any {
	t.Helper()
	meta, err := loadMeta(sessionDir)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	return meta
}

func mustLoadTranscript(t *testing.T, sessionDir string) []map[string]any {
	t.Helper()
	transcript, err := loadTranscript(sessionDir)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	return transcript
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func jsonNumberText(value int) []byte {
	return []byte(json.Number(fmt.Sprint(value)).String())
}

func saveAdminSession(t *testing.T, sessionDir string, meta map[string]any) {
	t.Helper()
	st := store.New(sessionDir)
	if err := st.EnsureSession(); err != nil {
		t.Fatalf("ensure admin session: %v", err)
	}
	if err := st.SaveMetaMap(meta); err != nil {
		t.Fatalf("save admin meta: %v", err)
	}
	if err := st.SaveTranscriptItems([]any{}); err != nil {
		t.Fatalf("save admin transcript: %v", err)
	}
}

func findSessionSummary(sessions []map[string]any, sessionID string) map[string]any {
	for _, session := range sessions {
		if session["session_id"] == sessionID {
			return session
		}
	}
	return nil
}

func writeNestedRelaySettings(t *testing.T, env fakeEnv) string {
	t.Helper()
	settingsPath := filepath.Join(env.relayHome, "settings.toml")
	settings := `
[backend_profiles.inner-panel]
backend = "relay"
model = "inner-review"
effort = "1"
capabilities = ["composite"]

[relay_recipes.outer-review]
participants = ["inner-panel", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "adversarial"
max_rounds = 1
max_depth = 3

[relay_recipes.inner-review]
participants = ["codex-fast", "codex-deep"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "adversarial"
max_rounds = 1
max_depth = 1

[relay_recipes.too-shallow]
participants = ["inner-panel", "codex-fast"]
facilitator = "codex-fast"
reducer = "codex-deep"
mode = "adversarial"
max_rounds = 1
max_depth = 1
`
	if err := os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return settingsPath
}

func writeResumeRuntimeSettings(t *testing.T, env fakeEnv, includeStableRecipe bool) string {
	t.Helper()
	settingsPath := filepath.Join(env.relayHome, "runtime-resume-settings.toml")
	settings := `
[backend_profiles.codex-one]
backend = "codex"
model = "snapshot-one"
effort = "low"

[backend_profiles.codex-two]
backend = "codex"
model = "snapshot-two"
effort = "low"
`
	if includeStableRecipe {
		settings += `
[relay_recipes.stable-review]
participants = ["codex-one", "codex-two"]
facilitator = "codex-one"
reducer = "codex-two"
mode = "adversarial"
max_rounds = 1
max_depth = 1
`
	} else {
		settings += `
[relay_recipes.changed-review]
participants = ["codex-one", "codex-two"]
facilitator = "codex-one"
reducer = "codex-two"
mode = "adversarial"
max_rounds = 1
max_depth = 1
`
	}
	if err := os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		t.Fatalf("write runtime resume settings: %v", err)
	}
	return settingsPath
}

func firstEventOfType(events []map[string]any, eventType string) map[string]any {
	for _, event := range events {
		if event["event_type"] == eventType {
			return event
		}
	}
	return nil
}

func eventsOfType(events []map[string]any, eventType string) []map[string]any {
	matches := []map[string]any{}
	for _, event := range events {
		if event["event_type"] == eventType {
			matches = append(matches, event)
		}
	}
	return matches
}

func artifactDigestsForRefID(st *store.Store, refID string) map[string]bool {
	digests := map[string]bool{}
	entries, _ := st.ArtifactIndex()["entries"].([]any)
	for _, rawEntry := range entries {
		entry, _ := rawEntry.(map[string]any)
		ref, _ := entry["ref"].(map[string]any)
		if ref["id"] == refID {
			digests[stringFromAny(ref["digest"])] = true
		}
	}
	return digests
}
