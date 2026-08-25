// Package sessionview derives all v2 read models from the immutable plan,
// append-only events, and content-addressed blobs. It never writes a cache.
package sessionview

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/session"
)

type Counts struct {
	Events            int
	TurnsStarted      int
	TurnsFinished     int
	AttemptsStarted   int
	AttemptsFinished  int
	ProviderFailures  int
	ChildrenRequested int
	ChildrenCompleted int
}

type StatusView struct {
	Terminal     bool
	Status       string
	StopReason   string
	Counts       Counts
	CurrentRound int
}

// Status derives terminal state, stop reason, counts, and the last active
// round without consulting any cache or runtime state.
func Status(_ session.Plan, events []eventlog.Event) StatusView {
	view := StatusView{Status: "running", Counts: Counts{Events: len(events)}}
	pendingChildren := make(map[string]bool)
	abandonedAttempts := make(map[string]bool)
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.SessionFinishedPayload:
			view.Terminal = true
			view.Status = payload.Status
			view.StopReason = payload.StopReason
		case *eventlog.SessionFinishedPayload:
			if payload != nil {
				view.Terminal = true
				view.Status = payload.Status
				view.StopReason = payload.StopReason
			}
		case eventlog.TurnBudgetGrantedPayload:
			view.Terminal = false
			view.Status = "running"
			view.StopReason = ""
		case eventlog.TurnStartedPayload:
			view.Counts.TurnsStarted++
			view.CurrentRound = max(view.CurrentRound, payload.Round)
		case *eventlog.TurnStartedPayload:
			if payload != nil {
				view.Counts.TurnsStarted++
				view.CurrentRound = max(view.CurrentRound, payload.Round)
			}
		case eventlog.TurnFinishedPayload:
			view.Counts.TurnsFinished++
			view.CurrentRound = max(view.CurrentRound, payload.Round)
		case *eventlog.TurnFinishedPayload:
			if payload != nil {
				view.Counts.TurnsFinished++
				view.CurrentRound = max(view.CurrentRound, payload.Round)
			}
		case eventlog.AttemptStartedPayload:
			view.Counts.AttemptsStarted++
			abandonedAttempts[attemptKey(payload.ActorID, payload.Attempt)] = true
		case *eventlog.AttemptStartedPayload:
			if payload != nil {
				view.Counts.AttemptsStarted++
				abandonedAttempts[attemptKey(payload.ActorID, payload.Attempt)] = true
			}
		case eventlog.AttemptFinishedPayload:
			view.Counts.AttemptsFinished++
			delete(abandonedAttempts, attemptKey(payload.ActorID, payload.Attempt))
		case *eventlog.AttemptFinishedPayload:
			if payload != nil {
				view.Counts.AttemptsFinished++
				delete(abandonedAttempts, attemptKey(payload.ActorID, payload.Attempt))
			}
		case eventlog.ProviderFailedPayload:
			view.Counts.ProviderFailures++
			delete(abandonedAttempts, attemptKey(payload.ActorID, payload.Attempts))
		case *eventlog.ProviderFailedPayload:
			if payload != nil {
				view.Counts.ProviderFailures++
				delete(abandonedAttempts, attemptKey(payload.ActorID, payload.Attempts))
			}
		case eventlog.ChildRequestedPayload:
			view.Counts.ChildrenRequested++
			pendingChildren[payload.RequestID] = true
		case *eventlog.ChildRequestedPayload:
			if payload != nil {
				view.Counts.ChildrenRequested++
				pendingChildren[payload.RequestID] = true
			}
		case eventlog.ChildDecidedPayload:
			delete(pendingChildren, payload.RequestID)
		case *eventlog.ChildDecidedPayload:
			if payload != nil {
				delete(pendingChildren, payload.RequestID)
			}
		case eventlog.ChildCompletedPayload:
			view.Counts.ChildrenCompleted++
		case *eventlog.ChildCompletedPayload:
			if payload != nil {
				view.Counts.ChildrenCompleted++
			}
		}
	}
	if !view.Terminal {
		if len(pendingChildren) != 0 {
			view.Status = "awaiting_decision"
			view.StopReason = "awaiting_decision"
		} else if len(abandonedAttempts) != 0 {
			view.Status = "interrupted"
			view.StopReason = "interrupted"
		}
	}
	return view
}

type TranscriptEntry struct {
	ActorID string
	Role    eventlog.Role
	Round   int
	Text    string
	Content blobstore.BlobRef
}

type TranscriptView struct {
	Entries []TranscriptEntry
}

// Transcript resolves turn-finished blob content into an ordered conversation.
func Transcript(_ session.Plan, events []eventlog.Event, blobs *blobstore.Store) (TranscriptView, error) {
	roles := make(map[string]eventlog.Role)
	view := TranscriptView{}
	for _, event := range events {
		if payload, ok := asTurnStarted(event.Payload); ok {
			roles[turnKey(payload.ActorID, payload.Round)] = payload.Role
			continue
		}
		payload, ok := asTurnFinished(event.Payload)
		if !ok {
			continue
		}
		if blobs == nil {
			return TranscriptView{}, errors.New("blob store is required to derive transcript")
		}
		reader, err := blobs.Open(payload.Content)
		if err != nil {
			return TranscriptView{}, fmt.Errorf("open turn blob: %w", err)
		}
		body, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return TranscriptView{}, fmt.Errorf("read turn blob: %w", readErr)
		}
		if closeErr != nil {
			return TranscriptView{}, fmt.Errorf("verify turn blob: %w", closeErr)
		}
		view.Entries = append(view.Entries, TranscriptEntry{
			ActorID: payload.ActorID,
			Role:    roles[turnKey(payload.ActorID, payload.Round)],
			Round:   payload.Round,
			Text:    string(body),
			Content: payload.Content,
		})
	}
	return view, nil
}

type LedgerAttempt struct {
	ActorID           string
	Attempt           int
	Outcome           string
	ProviderSessionID string
	Content           blobstore.BlobRef
}

type ProviderFailure struct {
	ActorID         string
	Backend         string
	Category        string
	Retryable       bool
	Attempts        int
	RemediationCode string
	SanitizedDetail string
}

type LedgerView struct {
	Attempts []LedgerAttempt
	Retries  int
	Failures []ProviderFailure
}

// Ledger contains only finished attempts. A started-but-unfinished attempt is
// intentionally absent; Diagnostics reports it as abandoned instead.
func Ledger(_ session.Plan, events []eventlog.Event) LedgerView {
	view := LedgerView{}
	for _, event := range events {
		if payload, ok := asAttemptFinished(event.Payload); ok {
			view.Attempts = append(view.Attempts, LedgerAttempt{
				ActorID:           payload.ActorID,
				Attempt:           payload.Attempt,
				Outcome:           payload.Outcome,
				ProviderSessionID: payload.ProviderSessionID,
				Content:           payload.Content,
			})
			if payload.Attempt > 1 {
				view.Retries++
			}
		}
		if payload, ok := asProviderFailed(event.Payload); ok {
			view.Failures = append(view.Failures, ProviderFailure{
				ActorID:         payload.ActorID,
				Backend:         payload.Backend,
				Category:        payload.Category,
				Retryable:       payload.Retryable,
				Attempts:        payload.Attempts,
				RemediationCode: payload.RemediationCode,
				SanitizedDetail: payload.SanitizedDetail,
			})
		}
	}
	return view
}

type GraphNode struct {
	ID             string
	Kind           string
	ActorID        string
	Round          int
	Role           eventlog.Role
	RequestID      string
	RecipeID       string
	ChildSessionID string
	Status         string
	BudgetState    string
}

type GraphEdge struct {
	From string
	To   string
	Kind string
}

type GraphView struct {
	Nodes []GraphNode
	Edges []GraphEdge
}

// Graph derives actors, turns, child requests, decisions, and completions from
// event data alone. request_id is the portable join key for child lifecycle.
func Graph(plan session.Plan, events []eventlog.Event) GraphView {
	nodes := make(map[string]GraphNode)
	edges := make(map[string]GraphEdge)
	turnNodes := make(map[string]string)
	rootID := "session:" + plan.SessionID
	nodes[rootID] = GraphNode{ID: rootID, Kind: "session", Status: Status(plan, events).Status}
	for _, actor := range plan.Actors {
		ensureActor(nodes, edges, rootID, actor.ID)
	}
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.TurnStartedPayload:
			turnNodes[turnKey(payload.ActorID, payload.Round)] = addTurnNode(nodes, edges, rootID, event.Seq, payload)
		case *eventlog.TurnStartedPayload:
			if payload != nil {
				turnNodes[turnKey(payload.ActorID, payload.Round)] = addTurnNode(nodes, edges, rootID, event.Seq, *payload)
			}
		case eventlog.TurnFinishedPayload:
			finishTurnNode(nodes, edges, rootID, turnNodes, payload)
		case *eventlog.TurnFinishedPayload:
			if payload != nil {
				finishTurnNode(nodes, edges, rootID, turnNodes, *payload)
			}
		case eventlog.ChildRequestedPayload:
			addChildRequest(nodes, edges, rootID, payload)
		case *eventlog.ChildRequestedPayload:
			if payload != nil {
				addChildRequest(nodes, edges, rootID, *payload)
			}
		case eventlog.ChildDecidedPayload:
			applyChildDecision(nodes, payload)
		case *eventlog.ChildDecidedPayload:
			if payload != nil {
				applyChildDecision(nodes, *payload)
			}
		case eventlog.ChildCompletedPayload:
			applyChildCompletion(nodes, payload)
		case *eventlog.ChildCompletedPayload:
			if payload != nil {
				applyChildCompletion(nodes, *payload)
			}
		}
	}
	view := GraphView{Nodes: make([]GraphNode, 0, len(nodes)), Edges: make([]GraphEdge, 0, len(edges))}
	for _, node := range nodes {
		view.Nodes = append(view.Nodes, node)
	}
	for _, edge := range edges {
		view.Edges = append(view.Edges, edge)
	}
	sort.Slice(view.Nodes, func(left, right int) bool { return view.Nodes[left].ID < view.Nodes[right].ID })
	sort.Slice(view.Edges, func(left, right int) bool {
		leftKey := edgeKey(view.Edges[left])
		rightKey := edgeKey(view.Edges[right])
		return leftKey < rightKey
	})
	return view
}

func ensureActor(nodes map[string]GraphNode, edges map[string]GraphEdge, rootID string, actorID string) {
	actorNodeID := "actor:" + actorID
	if _, exists := nodes[actorNodeID]; !exists {
		nodes[actorNodeID] = GraphNode{ID: actorNodeID, Kind: "actor", ActorID: actorID, Status: "active"}
	}
	addEdge(edges, GraphEdge{From: rootID, To: actorNodeID, Kind: "actor"})
}

func addTurnNode(nodes map[string]GraphNode, edges map[string]GraphEdge, rootID string, seq uint64, payload eventlog.TurnStartedPayload) string {
	ensureActor(nodes, edges, rootID, payload.ActorID)
	turnID := "turn:" + strconv.FormatUint(seq, 10)
	nodes[turnID] = GraphNode{ID: turnID, Kind: "turn", ActorID: payload.ActorID, Round: payload.Round, Role: payload.Role, Status: "started"}
	addEdge(edges, GraphEdge{From: "actor:" + payload.ActorID, To: turnID, Kind: "turn"})
	return turnID
}

func finishTurnNode(nodes map[string]GraphNode, edges map[string]GraphEdge, rootID string, turnNodes map[string]string, payload eventlog.TurnFinishedPayload) {
	ensureActor(nodes, edges, rootID, payload.ActorID)
	turnID, found := turnNodes[turnKey(payload.ActorID, payload.Round)]
	if !found {
		return
	}
	node, found := nodes[turnID]
	if !found {
		return
	}
	node.Status = "finished"
	nodes[turnID] = node
}

func addChildRequest(nodes map[string]GraphNode, edges map[string]GraphEdge, rootID string, payload eventlog.ChildRequestedPayload) {
	ensureActor(nodes, edges, rootID, payload.RequesterActorID)
	childID := childNodeID(payload.RequestID)
	nodes[childID] = GraphNode{ID: childID, Kind: "child", ActorID: payload.RequesterActorID, RequestID: payload.RequestID, RecipeID: payload.RecipeID, Status: "requested"}
	addEdge(edges, GraphEdge{From: "actor:" + payload.RequesterActorID, To: childID, Kind: "child"})
}

func applyChildDecision(nodes map[string]GraphNode, payload eventlog.ChildDecidedPayload) {
	identifier := childNodeID(payload.RequestID)
	node, exists := nodes[identifier]
	if !exists {
		node = GraphNode{ID: identifier, Kind: "child", RequestID: payload.RequestID}
	}
	if payload.Admitted {
		node.Status = "admitted"
	} else {
		node.Status = "rejected"
	}
	node.BudgetState = payload.BudgetState
	nodes[identifier] = node
}

func applyChildCompletion(nodes map[string]GraphNode, payload eventlog.ChildCompletedPayload) {
	identifier := childNodeID(payload.RequestID)
	node, exists := nodes[identifier]
	if !exists {
		node = GraphNode{ID: identifier, Kind: "child", RequestID: payload.RequestID}
	}
	node.Status = payload.Status
	node.ChildSessionID = payload.ChildSessionID
	nodes[identifier] = node
}

func childNodeID(requestID string) string { return "child:" + requestID }

func addEdge(edges map[string]GraphEdge, edge GraphEdge) { edges[edgeKey(edge)] = edge }

func edgeKey(edge GraphEdge) string { return edge.From + "\x00" + edge.To + "\x00" + edge.Kind }

type AbandonedAttempt struct {
	ActorID string
	Attempt int
}

type DiagnosticsView struct {
	AbandonedAttempts []AbandonedAttempt
	UnreferencedBlobs []blobstore.BlobRef
	BudgetState       string
}

// Diagnostics exposes recovery-relevant facts without changing authority.
func Diagnostics(plan session.Plan, events []eventlog.Event, blobs *blobstore.Store) (DiagnosticsView, error) {
	started := make(map[string]AbandonedAttempt)
	view := DiagnosticsView{}
	for _, event := range events {
		if payload, ok := asAttemptStarted(event.Payload); ok {
			started[attemptKey(payload.ActorID, payload.Attempt)] = AbandonedAttempt{ActorID: payload.ActorID, Attempt: payload.Attempt}
		}
		if payload, ok := asAttemptFinished(event.Payload); ok {
			delete(started, attemptKey(payload.ActorID, payload.Attempt))
		}
		if payload, ok := asProviderFailed(event.Payload); ok {
			// provider.failed is a durable classified failed outcome even when
			// interruption prevented the following attempt.finished event.
			delete(started, attemptKey(payload.ActorID, payload.Attempts))
		}
		if payload, ok := asChildDecided(event.Payload); ok {
			view.BudgetState = payload.BudgetState
		}
	}
	for _, attempt := range started {
		view.AbandonedAttempts = append(view.AbandonedAttempts, attempt)
	}
	sort.Slice(view.AbandonedAttempts, func(left, right int) bool {
		if view.AbandonedAttempts[left].ActorID == view.AbandonedAttempts[right].ActorID {
			return view.AbandonedAttempts[left].Attempt < view.AbandonedAttempts[right].Attempt
		}
		return view.AbandonedAttempts[left].ActorID < view.AbandonedAttempts[right].ActorID
	})
	if blobs == nil {
		return view, nil
	}
	references := append(session.BlobRefs(plan), eventlog.BlobRefs(events)...)
	report, err := blobs.Sweep(references)
	if err != nil {
		return DiagnosticsView{}, err
	}
	view.UnreferencedBlobs = report.Unreferenced
	return view, nil
}

// ProviderSessions derives the latest successful provider continuation state
// per actor. It stores no provider CWD or runtime adapter state.
func ProviderSessions(_ session.Plan, events []eventlog.Event) map[string]string {
	sessions := make(map[string]string)
	for _, event := range events {
		payload, ok := asAttemptFinished(event.Payload)
		if !ok || payload.Outcome != "success" || strings.TrimSpace(payload.ProviderSessionID) == "" {
			continue
		}
		sessions[payload.ActorID] = payload.ProviderSessionID
	}
	return sessions
}

func turnKey(actorID string, round int) string      { return actorID + "\x00" + strconv.Itoa(round) }
func attemptKey(actorID string, attempt int) string { return actorID + "\x00" + strconv.Itoa(attempt) }
func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func asTurnStarted(payload eventlog.Payload) (eventlog.TurnStartedPayload, bool) {
	switch value := payload.(type) {
	case eventlog.TurnStartedPayload:
		return value, true
	case *eventlog.TurnStartedPayload:
		if value != nil {
			return *value, true
		}
	}
	return eventlog.TurnStartedPayload{}, false
}

func asTurnFinished(payload eventlog.Payload) (eventlog.TurnFinishedPayload, bool) {
	switch value := payload.(type) {
	case eventlog.TurnFinishedPayload:
		return value, true
	case *eventlog.TurnFinishedPayload:
		if value != nil {
			return *value, true
		}
	}
	return eventlog.TurnFinishedPayload{}, false
}

func asAttemptStarted(payload eventlog.Payload) (eventlog.AttemptStartedPayload, bool) {
	switch value := payload.(type) {
	case eventlog.AttemptStartedPayload:
		return value, true
	case *eventlog.AttemptStartedPayload:
		if value != nil {
			return *value, true
		}
	}
	return eventlog.AttemptStartedPayload{}, false
}

func asAttemptFinished(payload eventlog.Payload) (eventlog.AttemptFinishedPayload, bool) {
	switch value := payload.(type) {
	case eventlog.AttemptFinishedPayload:
		return value, true
	case *eventlog.AttemptFinishedPayload:
		if value != nil {
			return *value, true
		}
	}
	return eventlog.AttemptFinishedPayload{}, false
}

func asProviderFailed(payload eventlog.Payload) (eventlog.ProviderFailedPayload, bool) {
	switch value := payload.(type) {
	case eventlog.ProviderFailedPayload:
		return value, true
	case *eventlog.ProviderFailedPayload:
		if value != nil {
			return *value, true
		}
	}
	return eventlog.ProviderFailedPayload{}, false
}

func asChildDecided(payload eventlog.Payload) (eventlog.ChildDecidedPayload, bool) {
	switch value := payload.(type) {
	case eventlog.ChildDecidedPayload:
		return value, true
	case *eventlog.ChildDecidedPayload:
		if value != nil {
			return *value, true
		}
	}
	return eventlog.ChildDecidedPayload{}, false
}
