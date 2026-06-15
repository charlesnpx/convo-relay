package runner

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
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
	meta, err := loadMeta(sessionDir)
	if err != nil {
		return nil, err
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
	report := map[string]any{
		"session_id":  sessionIDFromDir(sessionDir),
		"session_dir": sessionDir,
		"title":       firstNonEmpty(stringFromAny(meta["title"]), stringFromAny(meta["task"])),
	}
	if err := cleanupBackendArtifactsForSession(meta, sessionDir); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(sessionDir); err != nil {
		return nil, err
	}
	report["status"] = "deleted"
	return report, nil
}

func cleanupBackendArtifactsForSession(meta map[string]any, sessionDir string) error {
	var errs []error
	if rawSlots, ok := meta["slots"].([]any); ok {
		for index, rawSlot := range rawSlots {
			slotEntry, ok := rawSlot.(map[string]any)
			if !ok {
				continue
			}
			backendName := stringFromAny(slotEntry["backend"])
			state, _ := slotEntry["state"].(map[string]any)
			slotID := firstNonEmpty(stringFromAny(slotEntry["slot_id"]), fmt.Sprintf("slot_%d", index))
			label := firstNonEmpty(stringFromAny(slotEntry["label"]), backendLabel(backendName))
			backend, err := newBackend(backendName, sessionDir, slotID, label, sessionDir, SlotConfig{})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if err := backend.RestoreState(state, SlotConfig{}); err != nil {
				errs = append(errs, fmt.Errorf("slot %s has invalid %s state: %w", slotID, backendName, err))
				continue
			}
			if err := backend.Cleanup(); err != nil {
				errs = append(errs, fmt.Errorf("cleanup %s slot %s: %w", backendName, slotID, err))
			}
		}
	} else if sessionID := stringFromAny(meta["claude_session_id"]); sessionID != "" {
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
	if shouldCleanClaudeFacilitatorArtifacts(meta) {
		if err := os.RemoveAll(claudeProjectDir(sessionDir)); err != nil {
			errs = append(errs, fmt.Errorf("cleanup claude facilitator artifacts: %w", err))
		}
	}
	return errors.Join(errs...)
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
		if err := store.New(sessionDir).SaveMetaMap(meta); err != nil {
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
