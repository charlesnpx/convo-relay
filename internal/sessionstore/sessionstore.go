// Package sessionstore addresses, lists, and removes canonical v2 sessions.
package sessionstore

import (
	"bytes"
	"context"
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
	"github.com/charlesnpx/convo-relay/internal/workspace"
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
		nameMatches := strings.HasPrefix(entry.Name(), prefix)
		sess, err := session.Open(candidate)
		if err != nil {
			if nameMatches {
				return "", fmt.Errorf("candidate %s is not a readable v2 session: %w", candidate, err)
			}
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

	type listedSession struct {
		item       map[string]any
		createdAt  time.Time
		hasCreated bool
		sessionID  string
	}
	items := []listedSession{}
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
		statusView := sessionview.Status(sess.Plan, events)
		status := sessionview.PublicStatus(statusView.Status, statusView.StopReason)
		createdAt, hasCreated := firstEventTime(events)
		createdAtValue := any(nil)
		if hasCreated {
			createdAtValue = createdAt.Format(time.RFC3339Nano)
		}
		turns := participantRoundCount(events)
		item := map[string]any{
			"session_id":    sess.Plan.SessionID,
			"path":          sessionDir,
			"status":        status,
			"mode":          sess.Plan.Mode,
			"task":          sess.Plan.Task,
			"title":         sess.Plan.Task,
			"agents":        participantBackends(sess.Plan),
			"actual_rounds": turns,
			"created_at":    createdAtValue,
		}
		if sess.Plan.Provenance == session.ProvenanceRecipe || sess.Plan.Provenance == session.ProvenanceChild {
			item["root"] = rootSummary(sess, events, status, turns)
		}
		items = append(items, listedSession{
			item:       item,
			createdAt:  createdAt,
			hasCreated: hasCreated,
			sessionID:  sess.Plan.SessionID,
		})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].hasCreated != items[right].hasCreated {
			return items[left].hasCreated
		}
		if items[left].hasCreated && !items[left].createdAt.Equal(items[right].createdAt) {
			return items[left].createdAt.After(items[right].createdAt)
		}
		if items[left].sessionID != items[right].sessionID {
			return items[left].sessionID < items[right].sessionID
		}
		return stringValue(items[left].item["path"]) < stringValue(items[right].item["path"])
	})
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, item.item)
	}
	if len(result) > limit {
		return result[:limit], nil
	}
	return result, nil
}

// CleanSession removes a single v2 session after proving no event writer owns
// its runtime lock, unless force bypasses that check.
func CleanSession(sessionDir string, force bool) (map[string]any, error) {
	return cleanSessionWithRemover(sessionDir, os.RemoveAll, force)
}

// cleanSessionWithRemover is the removal seam for callers that need to verify
// the exact target without deleting a real directory.
func cleanSessionWithRemover(sessionDir string, removeSession func(string) error, force bool) (map[string]any, error) {
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
	if err := workspace.Cleanup(context.Background(), sess); err != nil {
		return nil, fmt.Errorf("cleanup session workspace: %w", err)
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
	sessions, err := ListSessions(home, limit)
	if err != nil {
		return nil, err
	}
	marked := []any{}
	for _, summary := range sessions {
		if stringValue(summary["status"]) != "running" {
			continue
		}
		report, err := cleanSessionWithRemover(stringValue(summary["path"]), removeSession, force)
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

func firstEventTime(events []eventlog.Event) (time.Time, bool) {
	if len(events) == 0 {
		return time.Time{}, false
	}
	return events[0].Time.UTC(), true
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
