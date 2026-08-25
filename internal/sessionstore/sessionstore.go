// Package sessionstore addresses, lists, and removes canonical v2 sessions.
package sessionstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/session"
	"github.com/charlesnpx/convo-relay/internal/sessionview"
)

var errSessionRunning = errors.New("session is running")

// ResolveSessionDir selects an explicit directory or resolves a unique v2
// session id prefix below relayHome/sessions.
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
	if sessionIsReadable(exact) {
		return exact, nil
	}
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no relay sessions found")
		}
		return "", err
	}

	exactPlanMatches := []string{}
	matches := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(sessionsDir, entry.Name())
		sess, err := session.Open(candidate)
		if err != nil {
			continue
		}
		if sess.Plan.SessionID == prefix {
			exactPlanMatches = append(exactPlanMatches, candidate)
			continue
		}
		if strings.HasPrefix(sess.Plan.SessionID, prefix) || strings.HasPrefix(entry.Name(), prefix) {
			matches = append(matches, candidate)
		}
	}
	if len(exactPlanMatches) == 1 {
		return exactPlanMatches[0], nil
	}
	if len(exactPlanMatches) > 1 {
		return "", fmt.Errorf("ambiguous session id %q: %d matching v2 sessions", prefix, len(exactPlanMatches))
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

// ListSessions reads v2 session.json and events.jsonl records below
// relayHome/sessions. Directories that are not readable v2 sessions are not
// part of the v2 listing.
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

	items := []map[string]any{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionDir := filepath.Join(sessionsDir, entry.Name())
		sess, err := session.Open(sessionDir)
		if err != nil {
			continue
		}
		events, err := readEvents(sess)
		if err != nil {
			continue
		}
		status := listStatus(sess, events)
		item := map[string]any{
			"session_id":    sess.Plan.SessionID,
			"path":          sessionDir,
			"status":        status,
			"mode":          sess.Plan.Mode,
			"task":          sess.Plan.Task,
			"agents":        participantBackends(sess.Plan),
			"actual_rounds": participantRoundCount(events),
			"created_at":    firstEventTime(events),
		}
		if sess.Plan.Provenance == session.ProvenanceRecipe || sess.Plan.Provenance == session.ProvenanceChild {
			item["root"] = rootSummary(sess, events, status, participantRoundCount(events))
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(left, right int) bool {
		return stringValue(items[left]["created_at"]) > stringValue(items[right]["created_at"])
	})
	if len(items) > limit {
		return items[:limit], nil
	}
	return items, nil
}

// CleanSession removes a single v2 session after proving no event writer owns
// its runtime lock.
func CleanSession(sessionDir string) (map[string]any, error) {
	return cleanSessionWithRemover(sessionDir, os.RemoveAll)
}

// cleanSessionWithRemover is the removal seam for callers that need to verify
// the exact target without deleting a real directory.
func cleanSessionWithRemover(sessionDir string, removeSession func(string) error) (map[string]any, error) {
	return cleanSessionWithRemoverMode(sessionDir, removeSession, false)
}

func cleanSessionWithRemoverMode(sessionDir string, removeSession func(string) error, force bool) (map[string]any, error) {
	if removeSession == nil {
		return nil, errors.New("session remover is required")
	}
	sess, err := session.Open(sessionDir)
	if err != nil {
		return nil, err
	}
	if !force {
		lease, err := eventlog.AcquireWriterLease(sess.Root)
		if err != nil {
			var locked *eventlog.WriterLockedError
			if errors.As(err, &locked) {
				return nil, fmt.Errorf("%w; stop it before cleaning it", errSessionRunning)
			}
			return nil, err
		}
		defer func() {
			_ = lease.Release()
		}()
	}
	report := map[string]any{
		"session_id":  sess.Plan.SessionID,
		"session_dir": sess.Root,
		"title":       sess.Plan.Task,
	}
	if err := removeSession(sess.Root); err != nil {
		return nil, err
	}
	report["status"] = "deleted"
	return report, nil
}

// CleanupSessions removes stale running sessions under a limit. A free writer
// lock is authoritative evidence that a running session is orphaned; force
// bypasses that liveness check.
func CleanupSessions(home string, limit int, force bool) (map[string]any, error) {
	return cleanupSessionsWithRemover(home, limit, force, os.RemoveAll)
}

func cleanupSessionsWithRemover(home string, limit int, force bool, removeSession func(string) error) (map[string]any, error) {
	if removeSession == nil {
		return nil, errors.New("session remover is required")
	}
	sessions, err := ListSessions(home, limit)
	if err != nil {
		return nil, err
	}
	marked := []any{}
	for _, summary := range sessions {
		status := stringValue(summary["status"])
		if status != "running" && status != "orphaned" {
			continue
		}
		if status == "running" && !force {
			continue
		}
		report, err := cleanSessionWithRemoverMode(stringValue(summary["path"]), removeSession, force)
		if err != nil {
			if errors.Is(err, errSessionRunning) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		marked = append(marked, map[string]any{
			"session_id":  report["session_id"],
			"session_dir": report["session_dir"],
			"title":       report["title"],
		})
	}
	return map[string]any{
		"orphaned_count": len(marked),
		"orphaned":       marked,
	}, nil
}

func readEvents(sess *session.Session) ([]eventlog.Event, error) {
	if sess == nil {
		return nil, errors.New("session is required")
	}
	body, err := os.ReadFile(filepath.Join(sess.Root, eventlog.EventsFilename))
	if err != nil {
		return nil, err
	}
	return eventlog.Replay(bytes.NewReader(body))
}

func listStatus(sess *session.Session, events []eventlog.Event) string {
	view := sessionview.Status(sess.Plan, events)
	status := view.Status
	if status == "failed" && view.StopReason == "invalid_result" {
		status = "invalid_result"
	}
	if status != "running" {
		return status
	}
	lease, err := eventlog.AcquireWriterLease(sess.Root)
	if err != nil {
		return status
	}
	if err := lease.Release(); err != nil {
		return status
	}
	return "orphaned"
}

func rootSummary(sess *session.Session, events []eventlog.Event, status string, turns int) map[string]any {
	ledger := sessionview.Ledger(sess.Plan, events)
	validation, result := resultSummary(sess, events)
	return map[string]any{
		"execution_kind": "recipe",
		"status":         status,
		"recipe":         recipeSummary(sess.Plan),
		"turns": map[string]any{
			"configured": sess.Plan.Schedule.Turns,
			"completed":  turns,
		},
		"result": map[string]any{
			"source":            sess.Plan.Result.Source,
			"validation_status": validation,
			"value":             result,
		},
		"workspace":        workspaceSummary(sess.Plan, events),
		"providers":        sessionview.ProviderSessions(sess.Plan, events),
		"provider_retry":   sess.Plan.ProviderRetry.Mode,
		"invocations":      map[string]any{"count": len(ledger.Attempts)},
		"reducer_attempts": map[string]any{"count": reducerAttemptCount(sess.Plan, ledger)},
	}
}

func recipeSummary(value session.Plan) map[string]any {
	if strings.TrimSpace(value.RecipeID) == "" {
		return map[string]any{}
	}
	return map[string]any{"id": value.RecipeID}
}

func resultSummary(sess *session.Session, events []eventlog.Event) (string, string) {
	var ref *blobstore.BlobRef
	validation := ""
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.ResultProducedPayload:
			value := payload.Result
			ref = &value
			validation = payload.ValidationOutcome
		case *eventlog.ResultProducedPayload:
			if payload != nil {
				value := payload.Result
				ref = &value
				validation = payload.ValidationOutcome
			}
		}
	}
	if validation == "" {
		validation = "pending"
	} else if validation == "valid" {
		validation = "validated"
	}
	if ref == nil {
		return validation, ""
	}
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		return validation, ""
	}
	reader, err := blobs.Open(*ref)
	if err != nil {
		return validation, ""
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return validation, ""
	}
	return validation, string(body)
}

func workspaceSummary(value session.Plan, events []eventlog.Event) map[string]any {
	result := map[string]any{}
	if value.Workspace.Mode != "" {
		result["mode"] = value.Workspace.Mode
	}
	if value.Workspace.Isolation != "" {
		result["isolation"] = value.Workspace.Isolation
	}
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.WorkspacePreparedPayload:
			result["mode"] = payload.Mode
			result["commit"] = payload.Commit
			result["tree_hash"] = payload.TreeHash
		case *eventlog.WorkspacePreparedPayload:
			if payload != nil {
				result["mode"] = payload.Mode
				result["commit"] = payload.Commit
				result["tree_hash"] = payload.TreeHash
			}
		}
	}
	return result
}

func participantBackends(value session.Plan) []any {
	controls := map[string]bool{}
	if value.Facilitator != nil {
		controls[value.Facilitator.Actor] = true
	}
	if value.Reducer != nil {
		controls[value.Reducer.Actor] = true
	}
	items := []any{}
	for _, actor := range value.Actors {
		if !controls[actor.ID] {
			items = append(items, actor.Backend)
		}
	}
	return items
}

func participantRoundCount(events []eventlog.Event) int {
	roles := map[string]eventlog.Role{}
	count := 0
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.TurnStartedPayload:
			roles[turnKey(payload.ActorID, payload.Round)] = payload.Role
		case *eventlog.TurnStartedPayload:
			if payload != nil {
				roles[turnKey(payload.ActorID, payload.Round)] = payload.Role
			}
		case eventlog.TurnFinishedPayload:
			if roles[turnKey(payload.ActorID, payload.Round)] == eventlog.ParticipantRole {
				count++
			}
		case *eventlog.TurnFinishedPayload:
			if payload != nil && roles[turnKey(payload.ActorID, payload.Round)] == eventlog.ParticipantRole {
				count++
			}
		}
	}
	return count
}

func reducerAttemptCount(value session.Plan, ledger sessionview.LedgerView) int {
	if value.Reducer == nil {
		return 0
	}
	count := 0
	for _, attempt := range ledger.Attempts {
		if attempt.ActorID == value.Reducer.Actor {
			count++
		}
	}
	return count
}

func firstEventTime(events []eventlog.Event) any {
	if len(events) == 0 {
		return nil
	}
	return events[0].Time.UTC().Format(time.RFC3339Nano)
}

func sessionIsReadable(sessionDir string) bool {
	_, err := session.Open(sessionDir)
	return err == nil
}

func resolveRelayHome(home string) string {
	if strings.TrimSpace(home) != "" {
		return home
	}
	if value := strings.TrimSpace(os.Getenv("CODEX_CLAUDE_HOME")); value != "" {
		return value
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ".codex-claude"
	}
	return filepath.Join(userHome, ".codex-claude")
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func turnKey(actorID string, round int) string {
	return actorID + "\x00" + fmt.Sprint(round)
}
