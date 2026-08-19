package sessionview

import (
	"bytes"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/session"
)

func TestAllViewsAreDerivedFromPlanEventsAndBlobs(t *testing.T) {
	root := t.TempDir()
	store, err := blobstore.New(root, blobstore.Limits{})
	if err != nil {
		t.Fatalf("new blob store: %v", err)
	}
	content, err := store.PutMediaType(bytes.NewReader([]byte("participant text")), "text/plain")
	if err != nil {
		t.Fatalf("put content: %v", err)
	}
	orphan, err := store.PutMediaType(bytes.NewReader([]byte("unreferenced")), "text/plain")
	if err != nil {
		t.Fatalf("put orphan: %v", err)
	}
	plan := viewPlan()
	events := []eventlog.Event{
		{Seq: 1, Type: eventlog.TurnStarted, Payload: eventlog.TurnStartedPayload{ActorID: "actor-a", Round: 1, Role: eventlog.ParticipantRole}},
		{Seq: 2, Type: eventlog.TurnFinished, Payload: eventlog.TurnFinishedPayload{ActorID: "actor-a", Round: 1, Content: content}},
		{Seq: 3, Type: eventlog.AttemptStarted, Payload: eventlog.AttemptStartedPayload{ActorID: "actor-a", Attempt: 1}},
		{Seq: 4, Type: eventlog.AttemptStarted, Payload: eventlog.AttemptStartedPayload{ActorID: "actor-a", Attempt: 2}},
		{Seq: 5, Type: eventlog.AttemptFinished, Payload: eventlog.AttemptFinishedPayload{ActorID: "actor-a", Attempt: 2, Outcome: "success", ProviderSessionID: "provider-continuation", Content: content}},
		{Seq: 6, Type: eventlog.ProviderFailed, Payload: eventlog.ProviderFailedPayload{ActorID: "actor-a", Backend: "codex", Category: "transport", Retryable: true, Attempts: 1, RemediationCode: "retry", SanitizedDetail: "transient"}},
		{Seq: 7, Type: eventlog.ChildRequested, Payload: eventlog.ChildRequestedPayload{RequestID: "request-one", RequesterActorID: "actor-a", RecipeID: "review", Question: content}},
		{Seq: 8, Type: eventlog.ChildDecided, Payload: eventlog.ChildDecidedPayload{RequestID: "request-one", Admitted: true, Reason: "budget available", BudgetState: "remaining"}},
		{Seq: 9, Type: eventlog.ChildCompleted, Payload: eventlog.ChildCompletedPayload{RequestID: "request-one", ChildSessionID: "child-one", Result: content}},
		{Seq: 10, Type: eventlog.SessionFinished, Payload: eventlog.SessionFinishedPayload{Status: "completed", StopReason: "converged"}},
	}

	status := Status(plan, events)
	if !status.Terminal || status.Status != "completed" || status.StopReason != "converged" || status.CurrentRound != 1 {
		t.Fatalf("status = %#v", status)
	}
	if status.Counts.AttemptsStarted != 2 || status.Counts.AttemptsFinished != 1 || status.Counts.ChildrenCompleted != 1 {
		t.Fatalf("status counts = %#v", status.Counts)
	}

	transcript, err := Transcript(plan, events, store)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if len(transcript.Entries) != 1 || transcript.Entries[0].Text != "participant text" || transcript.Entries[0].Role != eventlog.ParticipantRole {
		t.Fatalf("transcript = %#v", transcript)
	}

	ledger := Ledger(plan, events)
	if len(ledger.Attempts) != 1 || ledger.Attempts[0].Attempt != 2 || ledger.Retries != 1 || len(ledger.Failures) != 1 {
		t.Fatalf("ledger = %#v", ledger)
	}

	graph := Graph(plan, events)
	if !hasNode(graph, "child:request-one", "completed") || !hasEdge(graph, "actor:actor-a", "child:request-one", "child") {
		t.Fatalf("derived child graph = %#v", graph)
	}
	if !hasEdge(graph, "actor:actor-a", "turn:1", "turn") {
		t.Fatalf("derived turn graph = %#v", graph)
	}

	diagnostics, err := Diagnostics(plan, events, store)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if len(diagnostics.AbandonedAttempts) != 1 || diagnostics.AbandonedAttempts[0].Attempt != 1 {
		t.Fatalf("abandoned attempts = %#v", diagnostics.AbandonedAttempts)
	}
	if len(diagnostics.UnreferencedBlobs) != 1 || diagnostics.UnreferencedBlobs[0].SHA256 != orphan.SHA256 || diagnostics.BudgetState != "remaining" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}

	providerSessions := ProviderSessions(plan, events)
	if providerSessions["actor-a"] != "provider-continuation" {
		t.Fatalf("provider sessions = %#v", providerSessions)
	}
}

func TestGraphMarksOnlyMatchingActorRoundFinished(t *testing.T) {
	plan := viewPlan()
	plan.Actors = append(plan.Actors, session.Actor{ID: "actor-b", Backend: "claude", Model: "model", Effort: "medium"})
	content := blobstore.BlobRef{SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 1, MediaType: "text/plain"}
	events := []eventlog.Event{
		{Seq: 1, Type: eventlog.TurnStarted, Payload: eventlog.TurnStartedPayload{ActorID: "actor-a", Round: 1, Role: eventlog.ParticipantRole}},
		{Seq: 2, Type: eventlog.TurnStarted, Payload: eventlog.TurnStartedPayload{ActorID: "actor-b", Round: 1, Role: eventlog.ParticipantRole}},
		{Seq: 3, Type: eventlog.TurnFinished, Payload: eventlog.TurnFinishedPayload{ActorID: "actor-b", Round: 1, Content: content}},
	}
	graph := Graph(plan, events)
	if !hasNode(graph, "turn:1", "started") {
		t.Fatalf("actor-a turn should remain started: %#v", graph)
	}
	if !hasNode(graph, "turn:2", "finished") {
		t.Fatalf("actor-b turn should be finished: %#v", graph)
	}
}

func TestProviderSessionsKeepsLatestSuccessfulSessionAfterFailure(t *testing.T) {
	content := blobstore.BlobRef{SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 1, MediaType: "text/plain"}
	events := []eventlog.Event{
		{Seq: 1, Type: eventlog.AttemptFinished, Payload: eventlog.AttemptFinishedPayload{ActorID: "actor-a", Attempt: 1, Outcome: "success", ProviderSessionID: "successful-session", Content: content}},
		{Seq: 2, Type: eventlog.AttemptFinished, Payload: eventlog.AttemptFinishedPayload{ActorID: "actor-a", Attempt: 2, Outcome: "failed", ProviderSessionID: "failed-session", Content: content}},
	}
	sessions := ProviderSessions(viewPlan(), events)
	if got, want := sessions["actor-a"], "successful-session"; got != want {
		t.Fatalf("provider session = %q, want %q", got, want)
	}
}

func hasNode(graph GraphView, identifier string, status string) bool {
	for _, node := range graph.Nodes {
		if node.ID == identifier && node.Status == status {
			return true
		}
	}
	return false
}

func hasEdge(graph GraphView, from string, to string, kind string) bool {
	for _, edge := range graph.Edges {
		if edge.From == from && edge.To == to && edge.Kind == kind {
			return true
		}
	}
	return false
}

func viewPlan() session.Plan {
	return session.Plan{
		Kind:          session.PlanKind,
		Provenance:    session.ProvenanceOrdinary,
		Task:          "trace task",
		Timeouts:      session.Timeouts{TurnSeconds: 30, StallSeconds: 30},
		Mode:          session.ModeAdversarial,
		Investigation: session.InvestigationAuto,
		SchemaVersion: session.SchemaVersion,
		SessionID:     "session-one",
		Actors: []session.Actor{
			{ID: "actor-a", Backend: "codex", Model: "model", Effort: "medium"},
			{ID: "actor-b", Backend: "claude", Model: "model", Effort: "medium"},
		},
		Schedule:      session.Schedule{Kind: "dialogue", Turns: 2},
		ProviderRetry: session.ProviderRetry{Mode: "allow", MaxAttempts: 2},
		Workspace:     session.Workspace{Mode: "current"},
		Inputs:        []session.Input{},
		ChildPolicy:   session.ChildPolicy{Mode: "deny", MaxDepth: 0, MaxChildren: 0, MaxTurns: 0, AllowedRecipes: []string{}},
		Result:        session.Result{Source: "last_turn", Format: "text"},
	}
}
