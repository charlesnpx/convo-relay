package relayv2

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/session"
	"github.com/charlesnpx/convo-relay/internal/sessionview"
	"github.com/charlesnpx/convo-relay/internal/store"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

// ProjectionOptions alters only the CLI projection of a just-completed
// invocation. It never changes durable engine state.
type ProjectionOptions struct {
	Status     string
	StopReason string
}

// Open identifies and opens a v2 session. A missing session.json is a normal
// legacy-session result; an existing invalid session.json is an error.
func Open(root string) (*session.Session, bool, error) {
	filename := filepath.Join(filepath.Clean(root), session.SessionFilename)
	if _, err := os.Lstat(filename); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	sess, err := session.Open(root)
	if err != nil {
		return nil, true, err
	}
	return sess, true, nil
}

// Events reads the typed v2 event authority without consulting legacy store
// files that happen to share the old events.jsonl name.
func Events(sess *session.Session) ([]eventlog.Event, error) {
	if sess == nil {
		return nil, errors.New("session is required")
	}
	body, err := os.ReadFile(filepath.Join(sess.Root, eventlog.EventsFilename))
	if err != nil {
		return nil, err
	}
	return eventlog.Replay(bytes.NewReader(body))
}

// BuildReport projects a session entirely from its immutable plan, event log,
// blobs, and the existing workspace artifact. It deliberately keeps Outcome
// minimal and does not widen it into a report transport.
func BuildReport(sess *session.Session, options ProjectionOptions) (map[string]any, error) {
	events, err := Events(sess)
	if err != nil {
		return nil, fmt.Errorf("read v2 events: %w", err)
	}
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		return nil, fmt.Errorf("open v2 blobs: %w", err)
	}
	statusView := sessionview.Status(sess.Plan, events)
	transcriptView, err := sessionview.Transcript(sess.Plan, events, blobs)
	if err != nil {
		return nil, fmt.Errorf("derive v2 transcript: %w", err)
	}
	ledgerView := sessionview.Ledger(sess.Plan, events)
	diagnostics, err := sessionview.Diagnostics(sess.Plan, events, blobs)
	if err != nil {
		return nil, fmt.Errorf("derive v2 diagnostics: %w", err)
	}
	providerSessions := sessionview.ProviderSessions(sess.Plan, events)
	entries, latestLedger, err := publicTranscript(sess.Plan, transcriptView, events, blobs)
	if err != nil {
		return nil, err
	}
	participantTurns := participantTurnCount(transcriptView)
	status := firstNonEmpty(options.Status, statusView.Status)
	if status == "" {
		status = "running"
	}
	stopReason := firstNonEmpty(options.StopReason, statusView.StopReason)
	if stopReason == "" && status == "awaiting_decision" {
		stopReason = "awaiting_decision"
	}
	status = sessionview.PublicStatus(status, stopReason)
	result, validation := resultProjection(events, blobs)
	if validation == "" {
		validation = "pending"
	}
	if validation == "valid" {
		validation = "validated"
	} else if validation == "invalid" {
		validation = "invalid"
	}
	workspaceState, err := workspaceProjection(sess)
	if err != nil {
		return nil, err
	}
	reducerAttempts := reducerAttemptCount(sess.Plan, ledgerView)
	providerFailures := providerFailureMaps(ledgerView)
	slots := slotProjection(sess.Plan, providerSessions)
	counts := latestLedgerCounts(latestLedger)
	maxRounds := sess.Plan.Schedule.Turns + grantedTurns(events)
	summary := map[string]any{
		"status":            status,
		"mode":              sess.Plan.Mode,
		"agents":            participantBackends(sess.Plan),
		"configured_rounds": sess.Plan.Schedule.Turns,
		"max_rounds":        maxRounds,
		"actual_rounds":     participantTurns,
		"filtered_rounds":   participantTurns,
		"ledger_counts":     counts,
	}
	report := map[string]any{
		"session_id":                    sess.Plan.SessionID,
		"session_dir":                   sess.Root,
		"execution_kind":                executionKind(sess.Plan),
		"recipe_id":                     sess.Plan.RecipeID,
		"task":                          sess.Plan.Task,
		"title":                         sess.Plan.Task,
		"mode":                          sess.Plan.Mode,
		"investigation_mode":            sess.Plan.Investigation,
		"timeout_seconds":               sess.Plan.Timeouts.TurnSeconds,
		"stall_timeout_seconds":         sess.Plan.Timeouts.StallSeconds,
		"status":                        status,
		"stop_reason":                   stopReason,
		"summary":                       summary,
		"transcript":                    entries,
		"transcript_payload":            entries,
		"result":                        result,
		"result_source":                 sess.Plan.Result.Source,
		"validation_status":             validation,
		"reducer_attempts":              map[string]any{"count": reducerAttempts},
		"recipe":                        recipeProjection(sess.Plan),
		"source":                        sess.Plan.Provenance,
		"slots":                         slots,
		"actual_rounds":                 participantTurns,
		"actual_participant_turns":      participantTurns,
		"participant_turns":             sess.Plan.Schedule.Turns,
		"max_rounds":                    maxRounds,
		"round_limit_mode":              roundLimitMode(sess.Plan),
		"provider_failures":             providerFailures,
		"provider_retry":                sess.Plan.ProviderRetry.Mode,
		"workspace_content_source":      workspaceState[workspace.WorkspaceContentSourceKey],
		"working_tree_changes_included": workspaceState[workspace.WorkingTreeChangesIncludedKey],
		"diagnostics": map[string]any{
			"abandoned_attempts": abandonedAttemptMaps(diagnostics),
			"unreferenced_blobs": unreferencedBlobMaps(diagnostics),
			"budget_state":       diagnostics.BudgetState,
		},
	}
	if sess.Plan.Provenance == session.ProvenanceRecipe || sess.Plan.Provenance == session.ProvenanceChild {
		report["root"] = rootProjection(sess.Plan, status, participantTurns, result, validation, workspaceState, providerSessions, len(ledgerView.Attempts), reducerAttempts)
	}
	return report, nil
}

// BuildGraphReport retains the public graph envelope while deriving every
// node and proposal from v2 events. It does not create legacy proposal files.
func BuildGraphReport(sess *session.Session) (map[string]any, error) {
	events, err := Events(sess)
	if err != nil {
		return nil, err
	}
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		return nil, err
	}
	view := sessionview.Graph(sess.Plan, events)
	nodes := make(map[string]any, len(view.Nodes))
	for _, node := range view.Nodes {
		nodes[node.ID] = map[string]any{
			"id": node.ID, "kind": node.Kind, "actor_id": node.ActorID, "round": node.Round,
			"role": string(node.Role), "request_id": node.RequestID, "recipe_id": node.RecipeID,
			"child_session_id": node.ChildSessionID, "status": node.Status, "budget_state": node.BudgetState,
		}
	}
	edges := make([]any, 0, len(view.Edges))
	for _, edge := range view.Edges {
		edges = append(edges, map[string]any{"from": edge.From, "to": edge.To, "kind": edge.Kind})
	}
	proposals, err := proposalProjection(events, blobs)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"graph": map[string]any{
			"nodes":     nodes,
			"edges":     edges,
			"proposals": proposals,
		},
		"events":     eventSummaries(events),
		"validation": map[string]any{"ok": true, "mode": "v2"},
	}, nil
}

func publicTranscript(value session.Plan, transcript sessionview.TranscriptView, events []eventlog.Event, blobs *blobstore.Store) ([]any, map[string]any, error) {
	actors := actorByID(value)
	entries := make([]any, 0, len(transcript.Entries))
	participantIndexes := map[int]int{}
	latestLedger := emptyLedger()
	for _, entry := range transcript.Entries {
		switch entry.Role {
		case eventlog.ParticipantRole:
			public := map[string]any{
				"round":   entry.Round,
				"from":    entry.ActorID,
				"content": entry.Text,
				"mode":    value.Mode,
				"ledger":  cloneMap(latestLedger),
			}
			participantIndexes[entry.Round] = len(entries)
			entries = append(entries, public)
		case eventlog.FacilitatorRole:
			ledger, ok := ledgerDocument(entry.Text)
			if ok {
				latestLedger = ledger
			}
			index, found := participantIndexes[entry.Round]
			if !found && len(entries) > 0 {
				index, found = len(entries)-1, true
			}
			if found {
				participant, _ := entries[index].(map[string]any)
				participant["ledger"] = cloneMap(latestLedger)
				actor := actors[entry.ActorID]
				participant["facilitator_provider_result"] = map[string]any{"backend": actor.Backend, "actor_id": actor.ID}
			}
		}
	}
	for _, event := range events {
		payload, ok := event.Payload.(eventlog.ChildCompletedPayload)
		if !ok || payload.Status != "completed" {
			continue
		}
		content, err := readBlob(blobs, payload.Result)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, map[string]any{
			"round":     participantTurnCount(transcript),
			"from":      "child:" + payload.RequestID,
			"content":   content,
			"mode":      value.Mode,
			"ledger":    cloneMap(latestLedger),
			"synthetic": true,
		})
	}
	return entries, latestLedger, nil
}

func proposalProjection(events []eventlog.Event, blobs *blobstore.Store) (map[string]any, error) {
	proposals := map[string]any{}
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.ChildRequestedPayload:
			question, err := readBlob(blobs, payload.Question)
			if err != nil {
				return nil, err
			}
			proposals[payload.RequestID] = map[string]any{
				"proposal_id":        payload.RequestID,
				"selected_recipe_id": payload.RecipeID,
				"delegated_question": question,
				"status":             "proposed",
				"requester_actor_id": payload.RequesterActorID,
			}
		case eventlog.ChildDecidedPayload:
			proposal, _ := proposals[payload.RequestID].(map[string]any)
			if proposal == nil {
				proposal = map[string]any{"proposal_id": payload.RequestID}
			}
			if payload.Admitted {
				proposal["status"] = "admitted"
			} else {
				proposal["status"] = "rejected"
			}
			proposal["reason"] = payload.Reason
			proposal["budget_state"] = payload.BudgetState
			proposals[payload.RequestID] = proposal
		case eventlog.ChildCompletedPayload:
			proposal, _ := proposals[payload.RequestID].(map[string]any)
			if proposal == nil {
				proposal = map[string]any{"proposal_id": payload.RequestID}
			}
			if payload.Status == "completed" {
				proposal["status"] = "collapsed"
			} else {
				proposal["status"] = payload.Status
			}
			proposal["child_session_id"] = payload.ChildSessionID
			proposals[payload.RequestID] = proposal
		}
	}
	return proposals, nil
}

func resultProjection(events []eventlog.Event, blobs *blobstore.Store) (string, string) {
	var ref *blobstore.BlobRef
	validation := ""
	for _, event := range events {
		if payload, ok := event.Payload.(eventlog.ResultProducedPayload); ok {
			copy := payload.Result
			ref = &copy
			validation = payload.ValidationOutcome
		}
	}
	if ref == nil {
		return "", validation
	}
	text, err := readBlob(blobs, *ref)
	if err != nil {
		return "", validation
	}
	return text, validation
}

func rootProjection(value session.Plan, status string, turns int, result string, validation string, workspaceState map[string]any, providerSessions map[string]string, invocations int, reducerAttempts int) map[string]any {
	return map[string]any{
		"execution_kind": "recipe",
		"status":         status,
		"recipe":         recipeProjection(value),
		"turns": map[string]any{
			"configured": value.Schedule.Turns,
			"completed":  turns,
		},
		"result": map[string]any{
			"source":            value.Result.Source,
			"validation_status": validation,
			"value":             result,
		},
		"workspace":        workspaceState,
		"providers":        providerSessions,
		"provider_retry":   value.ProviderRetry.Mode,
		"invocations":      map[string]any{"count": invocations},
		"reducer_attempts": map[string]any{"count": reducerAttempts},
	}
}

func recipeProjection(value session.Plan) map[string]any {
	if strings.TrimSpace(value.RecipeID) == "" {
		return map[string]any{}
	}
	return map[string]any{"id": value.RecipeID}
}

func workspaceProjection(sess *session.Session) (map[string]any, error) {
	return workspaceProjectionForSession(sess, map[string]bool{})
}

func workspaceProjectionForSession(sess *session.Session, visited map[string]bool) (map[string]any, error) {
	if sess == nil {
		return nil, errors.New("session is required for workspace projection")
	}
	if visited[sess.Root] {
		return nil, errors.New("cycle while resolving v2 workspace provenance")
	}
	visited[sess.Root] = true
	result := map[string]any{}
	st := store.New(sess.Root)
	index := st.ArtifactIndex()
	entries, _ := index["entries"].([]any)
	for index := len(entries) - 1; index >= 0; index-- {
		entry, _ := entries[index].(map[string]any)
		ref, _ := entry["ref"].(map[string]any)
		if ref == nil || ref["id"] != "execution_workspace:selected" {
			continue
		}
		artifact, err := st.LoadArtifactPayloadRaw(ref)
		if err != nil {
			return nil, fmt.Errorf("load execution workspace: %w", err)
		}
		projection, err := workspace.ProvenanceProjection(artifact)
		if err != nil {
			return nil, fmt.Errorf("derive execution workspace provenance: %w", err)
		}
		for key, value := range projection {
			result[key] = value
		}
		if mode, exists := artifact["mode"]; exists {
			result["mode"] = mode
		}
		if policy, ok := artifact["policy"].(map[string]any); ok {
			result["policy"] = cloneMap(policy)
		}
		return result, nil
	}
	if sess.Plan.Provenance != session.ProvenanceChild {
		return nil, errors.New("v2 session has no durable execution workspace artifact")
	}
	return inheritedWorkspaceProjection(sess, visited)
}

// inheritedWorkspaceProjection follows the durable child.completed edge back
// to the parent whose engine dependency boundary executed the child. The
// parent workspace artifact is therefore the actual source for a child that
// shares that dependency; no source path or timestamp is projected.
func inheritedWorkspaceProjection(child *session.Session, visited map[string]bool) (map[string]any, error) {
	home := filepath.Dir(child.Root)
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, fmt.Errorf("read child session home: %w", err)
	}
	var parent *session.Session
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidateRoot := filepath.Join(home, entry.Name())
		if candidateRoot == child.Root {
			continue
		}
		candidate, found, openErr := Open(candidateRoot)
		if openErr != nil {
			return nil, fmt.Errorf("open possible parent session %q: %w", entry.Name(), openErr)
		}
		if !found {
			continue
		}
		events, eventsErr := Events(candidate)
		if eventsErr != nil {
			return nil, fmt.Errorf("read possible parent events %q: %w", entry.Name(), eventsErr)
		}
		matched := false
		for _, event := range events {
			payload, ok := event.Payload.(eventlog.ChildCompletedPayload)
			if ok && payload.ChildSessionID == child.Plan.SessionID {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if parent != nil {
			return nil, fmt.Errorf("multiple durable parents reference child session %q", child.Plan.SessionID)
		}
		parent = candidate
	}
	if parent == nil {
		return nil, fmt.Errorf("child session %q has no completed parent workspace reference", child.Plan.SessionID)
	}
	return workspaceProjectionForSession(parent, visited)
}

func slotProjection(value session.Plan, providerSessions map[string]string) []any {
	controls := map[string]bool{}
	if value.Facilitator != nil {
		controls[value.Facilitator.Actor] = true
	}
	if value.Reducer != nil {
		controls[value.Reducer.Actor] = true
	}
	slots := []any{}
	for _, actor := range value.Actors {
		if controls[actor.ID] {
			continue
		}
		state := map[string]any{}
		if continuation := strings.TrimSpace(providerSessions[actor.ID]); continuation != "" {
			switch actor.Backend {
			case "codex":
				state["thread_id"] = continuation
			case "gemini":
				state["session_ref"] = continuation
			default:
				state["session_id"] = continuation
			}
		}
		slots = append(slots, map[string]any{
			"slot_id":    actor.ID,
			"profile_id": actor.ProfileID,
			"backend":    actor.Backend,
			"label":      actor.Backend,
			"model":      actor.Model,
			"effort":     actor.Effort,
			"state":      state,
		})
	}
	return slots
}

func providerFailureMaps(value sessionview.LedgerView) []any {
	items := make([]any, 0, len(value.Failures))
	for _, failure := range value.Failures {
		items = append(items, map[string]any{
			"actor_id": failure.ActorID, "backend": failure.Backend, "category": failure.Category,
			"retryable": failure.Retryable, "attempts": failure.Attempts,
			"remediation_code": failure.RemediationCode, "sanitized_detail": failure.SanitizedDetail,
		})
	}
	return items
}

func abandonedAttemptMaps(value sessionview.DiagnosticsView) []any {
	items := make([]any, 0, len(value.AbandonedAttempts))
	for _, attempt := range value.AbandonedAttempts {
		items = append(items, map[string]any{"actor_id": attempt.ActorID, "attempt": attempt.Attempt})
	}
	return items
}

func unreferencedBlobMaps(value sessionview.DiagnosticsView) []any {
	items := make([]any, 0, len(value.UnreferencedBlobs))
	for _, ref := range value.UnreferencedBlobs {
		items = append(items, map[string]any{"sha256": ref.SHA256, "size": ref.Size, "media_type": ref.MediaType})
	}
	return items
}

func eventSummaries(events []eventlog.Event) []any {
	items := make([]any, 0, len(events))
	for _, event := range events {
		items = append(items, map[string]any{"seq": event.Seq, "event_id": event.EventID, "time": event.Time, "type": string(event.Type)})
	}
	return items
}

func grantedTurns(events []eventlog.Event) int {
	total := 0
	for _, event := range events {
		if payload, ok := event.Payload.(eventlog.TurnBudgetGrantedPayload); ok {
			total += payload.Turns
		}
	}
	return total
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

func participantTurnCount(value sessionview.TranscriptView) int {
	count := 0
	for _, entry := range value.Entries {
		if entry.Role == eventlog.ParticipantRole {
			count++
		}
	}
	return count
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

func roundLimitMode(value session.Plan) string {
	if value.Schedule.StopOnConvergence {
		return "auto"
	}
	return "fixed"
}

func executionKind(value session.Plan) string {
	if value.Provenance == session.ProvenanceRecipe {
		return "recipe"
	}
	if value.Provenance == session.ProvenanceChild {
		return "child"
	}
	return "ordinary"
}

func actorByID(value session.Plan) map[string]session.Actor {
	items := make(map[string]session.Actor, len(value.Actors))
	for _, actor := range value.Actors {
		items[actor.ID] = actor
	}
	return items
}

func emptyLedger() map[string]any {
	return map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}}
}

func ledgerDocument(text string) (map[string]any, bool) {
	var value struct {
		Settled   []string `json:"settled"`
		Contested []string `json:"contested"`
		Withdrawn []string `json:"withdrawn"`
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(text)))
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	return map[string]any{
		"settled":   stringsToAny(value.Settled),
		"contested": stringsToAny(value.Contested),
		"withdrawn": stringsToAny(value.Withdrawn),
	}, true
}

func latestLedgerCounts(value map[string]any) map[string]any {
	return map[string]any{
		"settled":   len(anyItems(value["settled"])),
		"contested": len(anyItems(value["contested"])),
		"withdrawn": len(anyItems(value["withdrawn"])),
	}
}

func stringsToAny(items []string) []any {
	result := make([]any, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	return result
}

func anyItems(value any) []any {
	items, _ := value.([]any)
	return items
}

func cloneMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func readBlob(blobs *blobstore.Store, ref blobstore.BlobRef) (string, error) {
	reader, err := blobs.Open(ref)
	if err != nil {
		return "", err
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return string(body), nil
}

// SortProposalIDs is useful to callers that need deterministic command-line
// reports without imposing an order on the graph map itself.
func SortProposalIDs(proposals map[string]any) []string {
	ids := make([]string, 0, len(proposals))
	for id := range proposals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
