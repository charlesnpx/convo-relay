package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

func ResolveSessionDir(home string, sessionDir string, sessionIDPrefix string) (string, error) {
	if strings.TrimSpace(sessionDir) != "" {
		return sessionDir, nil
	}
	prefix := strings.TrimSpace(sessionIDPrefix)
	if prefix == "" {
		return "", fmt.Errorf("--session-dir or session id is required")
	}
	sessionsDir := filepath.Join(resolveRelayHome(home), "sessions")
	exact := filepath.Join(sessionsDir, prefix)
	if info, err := os.Stat(exact); err == nil && info.IsDir() {
		return exact, nil
	}
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no relay sessions found")
		}
		return "", err
	}
	matches := []string{}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			matches = append(matches, filepath.Join(sessionsDir, entry.Name()))
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session matching %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q: %d matches; use a longer prefix", prefix, len(matches))
	}
}

func ListSessions(home string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 20
	}
	sessionsDir := filepath.Join(resolveRelayHome(home), "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	sessions := []map[string]any{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionDir := filepath.Join(sessionsDir, entry.Name())
		meta, err := loadMeta(sessionDir)
		if err != nil {
			continue
		}
		meta["session_id"] = entry.Name()
		meta["path"] = sessionDir
		if meta["status"] == "running" {
			pid, err := readPID(sessionDir)
			if err == nil && !processAlive(pid) {
				meta["status"] = "orphaned"
			}
		}
		sessions = append(sessions, meta)
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return stringFromAny(sessions[i]["created_at"]) > stringFromAny(sessions[j]["created_at"])
	})
	if len(sessions) > limit {
		return sessions[:limit], nil
	}
	return sessions, nil
}

func QueueSteeringPrompt(sessionDir string, prompt string) (map[string]any, error) {
	text := strings.TrimSpace(prompt)
	if text == "" {
		return nil, fmt.Errorf("steering prompt cannot be empty")
	}
	if _, err := loadMeta(sessionDir); err != nil {
		return nil, err
	}
	item := map[string]any{
		"id":         steeringID(),
		"prompt":     text,
		"source":     "steer",
		"created_at": utcNow(),
	}
	prompts, err := loadSteeringPrompts(sessionDir)
	if err != nil {
		return nil, err
	}
	prompts = append(prompts, item)
	if err := saveSteeringPrompts(sessionDir, prompts); err != nil {
		return nil, err
	}
	return item, nil
}

func CleanSession(sessionDir string) (map[string]any, error) {
	return cleanSessionWithRemover(sessionDir, os.RemoveAll)
}

func cleanSessionWithRemover(sessionDir string, removeSession func(string) error) (map[string]any, error) {
	if removeSession == nil {
		return nil, errors.New("session remover is required")
	}
	lock, err := lockSessionMutation(sessionDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = lock.Unlock()
	}()
	if err := ensureSessionNotRunning(sessionDir, "cleaning it"); err != nil {
		return nil, err
	}
	meta, err := loadMeta(sessionDir)
	if err != nil {
		return nil, err
	}
	st := store.New(sessionDir)
	report := map[string]any{
		"session_id":  sessionIDFromDir(sessionDir),
		"session_dir": sessionDir,
		"title":       firstNonEmpty(stringFromAny(meta["title"]), stringFromAny(meta["task"])),
	}
	attempt := nextWorkspaceCleanupAttempt(meta)
	meta = recordWorkspaceCleanupCheckpoint(meta, attempt, "pending", "source_finalization", nil, nil)
	if err := st.SaveMetaMap(meta); err != nil {
		return nil, err
	}

	terminalMeta, finalized, finalizeErr := finalizeTerminalWorkspace(context.Background(), st, model.NewSessionMeta(meta))
	meta = terminalMeta.ToMap()
	if finalizeErr != nil && !isSourceMutationError(finalizeErr) {
		return nil, persistWorkspaceCleanupFailure(st, meta, attempt, "source_finalization", finalizeErr)
	}
	finalizationDetails := map[string]any{"managed": finalized != nil && finalized.Managed}
	if finalized != nil && finalized.Managed {
		finalizationDetails["source_changed"] = finalized.SourceChanged
		finalizationDetails["source_mutated"] = finalized.SourceMutated
		finalizationDetails["source_after_digest"] = emptyStringAsNil(finalized.SourceAfterDigest)
	}
	meta = recordWorkspaceCleanupCheckpoint(meta, attempt, "pending", "provider_artifacts", nil, map[string]any{"source_finalization": finalizationDetails})
	if err := st.SaveMetaMap(meta); err != nil {
		return nil, err
	}
	if err := cleanupBackendArtifactsForSession(meta, sessionDir); err != nil {
		return nil, persistWorkspaceCleanupFailure(st, meta, attempt, "provider_artifacts", err)
	}
	meta = recordWorkspaceCleanupCheckpoint(meta, attempt, "pending", "execution_workspace", nil, map[string]any{
		"source_finalization": finalizationDetails,
		"provider_artifacts":  map[string]any{"status": "complete"},
	})
	if err := st.SaveMetaMap(meta); err != nil {
		return nil, err
	}
	workspaceResult, err := workspace.Cleanup(context.Background(), st)
	if err != nil {
		return nil, persistWorkspaceCleanupFailure(st, meta, attempt, "execution_workspace", err)
	}
	workspaceDetails := workspaceCleanupResultMap(workspaceResult)
	completeDetails := map[string]any{
		"source_finalization": finalizationDetails,
		"provider_artifacts":  map[string]any{"status": "complete"},
		"execution_workspace": workspaceDetails,
	}
	meta = recordWorkspaceCleanupCheckpoint(meta, attempt, "complete", "ready_to_delete", nil, completeDetails)
	if err := st.SaveMetaMap(meta); err != nil {
		return nil, err
	}
	if err := removeSession(sessionDir); err != nil {
		return nil, persistWorkspaceCleanupFailure(st, meta, attempt, "session_delete", err)
	}
	report["status"] = "deleted"
	report["workspace_cleanup"] = completeDetails
	return report, nil
}

func cleanupBackendArtifactsForSession(meta map[string]any, sessionDir string) error {
	records, hasParticipantSlots, hasFacilitatorState, err := persistedBackendCleanupRecords(meta, sessionDir)
	if err != nil {
		return err
	}
	var errs []error
	for _, record := range records {
		cwd := firstNonEmpty(stringFromAny(record.state["cwd"]), stringFromAny(meta["execution_cwd"]), stringFromAny(meta["launch_cwd"]), sessionDir)
		backend, err := newBackend(record.backend, sessionDir, record.slotID, record.label, cwd, SlotConfig{})
		if err != nil {
			errs = append(errs, fmt.Errorf("restore %s provider %s: %w", record.role, record.slotID, err))
			continue
		}
		if err := backend.RestoreState(record.state, SlotConfig{}); err != nil {
			errs = append(errs, fmt.Errorf("%s provider %s has invalid %s state: %w", record.role, record.slotID, record.backend, err))
			continue
		}
		if err := backend.Cleanup(); err != nil {
			errs = append(errs, fmt.Errorf("cleanup %s %s provider %s: %w", record.backend, record.role, record.slotID, err))
		}
	}
	if !hasParticipantSlots {
		if sessionID := stringFromAny(meta["claude_session_id"]); sessionID != "" {
			cwd := firstNonEmpty(stringFromAny(meta["launch_cwd"]), sessionDir)
			slotID := "slot_0"
			if stringFromAny(meta["first"]) != "claude" {
				slotID = "slot_1"
			}
			backend, err := newBackend("claude", sessionDir, slotID, backendLabel("claude"), cwd, SlotConfig{})
			if err != nil {
				errs = append(errs, err)
				return errors.Join(errs...)
			}
			if err := backend.RestoreState(map[string]any{"session_id": sessionID, "cwd": cwd}, SlotConfig{}); err != nil {
				errs = append(errs, fmt.Errorf("legacy claude state is invalid: %w", err))
			} else if err := backend.Cleanup(); err != nil {
				errs = append(errs, fmt.Errorf("cleanup legacy claude slot: %w", err))
			}
		}
	}
	if !hasFacilitatorState && shouldCleanClaudeFacilitatorArtifacts(meta) {
		if err := os.RemoveAll(claudeProjectDir(sessionDir)); err != nil {
			errs = append(errs, fmt.Errorf("cleanup claude facilitator artifacts: %w", err))
		}
	}
	return errors.Join(errs...)
}

type backendCleanupRecord struct {
	role    string
	backend string
	slotID  string
	label   string
	state   map[string]any
}

func persistedBackendCleanupRecords(meta map[string]any, sessionDir string) ([]backendCleanupRecord, bool, bool, error) {
	records := make([]backendCleanupRecord, 0)
	hasParticipantSlots := false
	if rawSlotsValue, exists := meta["slots"]; exists {
		rawSlots, ok := rawSlotsValue.([]any)
		if !ok {
			return nil, false, false, errors.New("participant slots metadata must be an array")
		}
		hasParticipantSlots = true
		for index, rawSlot := range rawSlots {
			slot, ok := rawSlot.(map[string]any)
			if !ok {
				return nil, true, false, fmt.Errorf("participant slot %d metadata must be an object", index)
			}
			record, err := backendCleanupRecordFromEnvelope("participant", slot, fmt.Sprintf("slot_%d", index), "")
			if err != nil {
				return nil, true, false, err
			}
			records = append(records, record)
		}
	}
	st := store.New(sessionDir)
	hasFacilitatorState := false
	for _, role := range []string{"facilitator", "reducer"} {
		roleRecords, found, err := persistedRoleCleanupRecords(st, meta, role)
		if err != nil {
			return nil, hasParticipantSlots, hasFacilitatorState, err
		}
		if role == "facilitator" {
			hasFacilitatorState = found
		}
		records = append(records, roleRecords...)
	}

	seen := map[string]bool{}
	deduplicated := make([]backendCleanupRecord, 0, len(records))
	for _, record := range records {
		keyPayload := map[string]any{"backend": record.backend, "slot_id": record.slotID, "state": record.state}
		body, err := contracts.CanonicalJSONBytes(keyPayload)
		if err != nil {
			return nil, hasParticipantSlots, hasFacilitatorState, err
		}
		key := string(body)
		if seen[key] {
			continue
		}
		seen[key] = true
		deduplicated = append(deduplicated, record)
	}
	return deduplicated, hasParticipantSlots, hasFacilitatorState, nil
}

func persistedRoleCleanupRecords(st *store.Store, meta map[string]any, role string) ([]backendCleanupRecord, bool, error) {
	candidates := []struct {
		value    any
		rawState bool
	}{
		{value: meta[role+"_provider"]},
		{value: meta[role+"_provider_state"]},
		{value: meta[role+"_state"], rawState: true},
		{value: meta[role+"_provider_ref"]},
		{value: meta[role+"_provider_state_ref"]},
		{value: meta[role+"_state_ref"], rawState: true},
	}
	if rawProviderStates, exists := meta["provider_states"]; exists {
		providerStates, ok := rawProviderStates.(map[string]any)
		if !ok {
			return nil, true, errors.New("provider_states must be an object")
		}
		candidates = append(candidates, struct {
			value    any
			rawState bool
		}{value: providerStates[role]})
	}
	records := make([]backendCleanupRecord, 0, 1)
	for _, candidate := range candidates {
		if candidate.value == nil {
			continue
		}
		value := candidate.value
		if contracts.IsArtifactRef(value) {
			ref, _ := value.(map[string]any)
			loaded, err := st.LoadArtifact(ref)
			if err != nil {
				return nil, true, fmt.Errorf("load %s provider state: %w", role, err)
			}
			value = loaded
		}
		envelope, ok := value.(map[string]any)
		if !ok {
			return nil, true, fmt.Errorf("%s provider state must be an object or artifact ref", role)
		}
		if candidate.rawState && envelope["state"] == nil {
			state := envelope
			backendName := firstNonEmpty(stringFromAny(envelope["backend"]), stringFromAny(meta[role+"_backend"]))
			envelope = map[string]any{
				"backend": backendName,
				"slot_id": role,
				"label":   providerRoleLabel(role),
				"state":   state,
			}
		}
		record, err := backendCleanupRecordFromEnvelope(role, envelope, role, providerRoleLabel(role))
		if err != nil {
			return nil, true, err
		}
		records = append(records, record)
	}
	return records, len(records) > 0, nil
}

func providerRoleLabel(role string) string {
	if role == "" {
		return ""
	}
	return strings.ToUpper(role[:1]) + role[1:]
}

func backendCleanupRecordFromEnvelope(role string, envelope map[string]any, fallbackSlotID string, fallbackLabel string) (backendCleanupRecord, error) {
	backendName := strings.TrimSpace(stringFromAny(envelope["backend"]))
	if backendName == "" {
		return backendCleanupRecord{}, fmt.Errorf("%s provider state is missing backend", role)
	}
	state, ok := envelope["state"].(map[string]any)
	if !ok {
		return backendCleanupRecord{}, fmt.Errorf("%s provider state for %s must contain a state object", role, backendName)
	}
	slotID := firstNonEmpty(stringFromAny(envelope["slot_id"]), fallbackSlotID)
	label := firstNonEmpty(stringFromAny(envelope["label"]), fallbackLabel, backendLabel(backendName))
	return backendCleanupRecord{role: role, backend: backendName, slotID: slotID, label: label, state: state}, nil
}

func nextWorkspaceCleanupAttempt(meta map[string]any) int {
	checkpoint, _ := meta["workspace_cleanup"].(map[string]any)
	return intFromAny(checkpoint["attempt"], 0) + 1
}

func recordWorkspaceCleanupCheckpoint(meta map[string]any, attempt int, status string, stage string, cause error, details map[string]any) map[string]any {
	next, _ := contracts.Materialize(meta).(map[string]any)
	checkpoint, _ := next["workspace_cleanup"].(map[string]any)
	if checkpoint == nil {
		checkpoint = map[string]any{}
	}
	history, _ := checkpoint["history"].([]any)
	event := map[string]any{
		"attempt": attempt,
		"status":  status,
		"stage":   stage,
		"at":      utcNow(),
	}
	if cause != nil {
		event["error"] = cause.Error()
	}
	history = append(history, event)
	checkpoint["attempt"] = attempt
	checkpoint["status"] = status
	checkpoint["stage"] = stage
	checkpoint["updated_at"] = event["at"]
	checkpoint["history"] = history
	if cause != nil {
		checkpoint["error"] = cause.Error()
		checkpoint["failed_at"] = event["at"]
	} else {
		delete(checkpoint, "error")
		delete(checkpoint, "failed_at")
	}
	if details != nil {
		checkpoint["details"] = contracts.Materialize(details)
	}
	if status == "complete" {
		checkpoint["completed_at"] = event["at"]
	}
	next["workspace_cleanup"] = checkpoint
	return next
}

func persistWorkspaceCleanupFailure(st *store.Store, meta map[string]any, attempt int, stage string, cause error) error {
	failed := recordWorkspaceCleanupCheckpoint(meta, attempt, "failed", stage, cause, nil)
	if err := st.SaveMetaMap(failed); err != nil {
		return errors.Join(cause, fmt.Errorf("persist workspace cleanup failure: %w", err))
	}
	return cause
}

func workspaceCleanupResultMap(result *workspace.CleanupResult) map[string]any {
	if result == nil {
		return map[string]any{"status": "complete", "managed": false}
	}
	return map[string]any{
		"status":             "complete",
		"managed":            result.Managed,
		"worktree_path":      emptyStringAsNil(result.WorktreePath),
		"registration_found": result.RegistrationFound,
		"worktree_removed":   result.WorktreeRemoved,
		"metadata_pruned":    result.MetadataPruned,
	}
}

func shouldCleanClaudeFacilitatorArtifacts(meta map[string]any) bool {
	facilitator := strings.TrimSpace(stringFromAny(meta["facilitator_backend"]))
	if facilitator == "" {
		if _, hasSlots := meta["slots"]; hasSlots {
			facilitator = "codex"
		} else {
			facilitator = "claude"
		}
	}
	return facilitator == "claude"
}

func CleanupSessions(home string, limit int, force bool) (map[string]any, error) {
	sessions, err := ListSessions(home, limit)
	if err != nil {
		return nil, err
	}
	marked := []any{}
	for _, summary := range sessions {
		if summary["status"] != "running" && summary["status"] != "orphaned" {
			continue
		}
		sessionDir := stringFromAny(summary["path"])
		pid, pidErr := readPID(sessionDir)
		if pidErr != nil && !force {
			continue
		}
		if pidErr == nil && processAlive(pid) {
			continue
		}
		meta, err := loadMeta(sessionDir)
		if err != nil {
			continue
		}
		meta["status"] = "orphaned"
		meta["orphaned_at"] = utcNow()
		if transcript, err := loadTranscript(sessionDir); err == nil {
			meta["actual_rounds"] = len(transcript)
		}
		st := store.New(sessionDir)
		terminalMeta, err := finalizeAdministrativeTerminal(st, model.NewSessionMeta(meta))
		if err != nil {
			if saveErr := st.SaveMeta(terminalMeta); saveErr != nil {
				return nil, errors.Join(err, saveErr)
			}
			return nil, err
		}
		meta = terminalMeta.ToMap()
		if err := st.SaveMetaMap(meta); err != nil {
			return nil, err
		}
		removePID(sessionDir)
		marked = append(marked, map[string]any{
			"session_id":  sessionIDFromDir(sessionDir),
			"session_dir": sessionDir,
			"title":       firstNonEmpty(stringFromAny(meta["title"]), stringFromAny(meta["task"])),
		})
	}
	return map[string]any{
		"orphaned_count": len(marked),
		"orphaned":       marked,
	}, nil
}

func resolveRelayHome(home string) string {
	if strings.TrimSpace(home) != "" {
		return home
	}
	return defaultRelayHome()
}

func loadSteeringPrompts(sessionDir string) ([]map[string]any, error) {
	path := filepath.Join(sessionDir, "steering.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	value, err := contracts.DecodeJSONBytes(data)
	if err != nil {
		return nil, err
	}
	rawItems, ok := value.([]any)
	if !ok {
		return []map[string]any{}, nil
	}
	prompts := make([]map[string]any, 0, len(rawItems))
	for _, rawItem := range rawItems {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		text := strings.TrimSpace(stringFromAny(item["prompt"]))
		if text == "" {
			continue
		}
		item["prompt"] = text
		prompts = append(prompts, item)
	}
	return prompts, nil
}

func saveSteeringPrompts(sessionDir string, prompts []map[string]any) error {
	items := make([]any, 0, len(prompts))
	for _, prompt := range prompts {
		items = append(items, prompt)
	}
	body, err := contracts.CanonicalJSONBytes(items)
	if err != nil {
		return err
	}
	return store.AtomicWriteFile(filepath.Join(sessionDir, "steering.json"), body)
}

func consumeSteeringPrompts(sessionDir string) ([]map[string]any, error) {
	prompts, err := loadSteeringPrompts(sessionDir)
	if err != nil {
		return nil, err
	}
	if len(prompts) > 0 {
		if err := saveSteeringPrompts(sessionDir, []map[string]any{}); err != nil {
			return nil, err
		}
	}
	return prompts, nil
}

func appendSteeringBlock(prompt string, steeringPrompts []map[string]any) string {
	items := []string{}
	for _, item := range steeringPrompts {
		text := strings.TrimSpace(stringFromAny(item["prompt"]))
		if text != "" {
			items = append(items, text)
		}
	}
	if len(items) == 0 {
		return prompt
	}
	lines := []string{
		"",
		"--- Operator Steering ---",
		"The relay operator added the following instruction(s) while this conversation was running.",
		"Treat them as current direction for this turn without discarding the original task.",
	}
	for index, item := range items {
		lines = append(lines, fmt.Sprintf("%d. %s", index+1, item))
	}
	return strings.TrimRight(prompt, " \t\r\n") + "\n" + strings.Join(lines, "\n")
}

func steeringID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return generateSessionID()
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(raw[0:4]),
		hex.EncodeToString(raw[4:6]),
		hex.EncodeToString(raw[6:8]),
		hex.EncodeToString(raw[8:10]),
		hex.EncodeToString(raw[10:16]),
	)
}
