package relayv2

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/v2/internal/blobstore"
	"github.com/charlesnpx/convo-relay/v2/internal/engine"
	"github.com/charlesnpx/convo-relay/v2/internal/eventlog"
	"github.com/charlesnpx/convo-relay/v2/internal/plan"
	"github.com/charlesnpx/convo-relay/v2/internal/provider"
	"github.com/charlesnpx/convo-relay/v2/internal/session"
)

func TestOpeningClaimDoesNotBlockSecondParticipantWithProductionExtractor(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "fresh_success"
		if recovered {
			name = "successful_attempt_recovery"
		}
		t.Run(name, func(t *testing.T) {
			parent := openingClaimChildPlan(name)
			sess, err := session.CreateWithOptions(session.CreateOptions{RelayHome: t.TempDir(), Plan: parent})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			if recovered {
				seedSuccessfulOpeningFacilitator(t, sess)
			}

			alpha := &relayv2TestBackend{id: "alpha", responses: []string{"opening contribution"}}
			beta := &relayv2TestBackend{id: "beta", responses: []string{"second participant reply"}}
			facilitatorReplies := []string{
				`{"settled":[],"contested":["replied claim"],"withdrawn":[]}`,
			}
			if !recovered {
				facilitatorReplies = append([]string{
					`{"settled":[],"contested":["opening claim"],"withdrawn":[]}`,
				}, facilitatorReplies...)
			}
			facilitator := &relayv2TestBackend{id: "facilitator", responses: facilitatorReplies}
			backends := map[string]*relayv2TestBackend{
				"alpha":       alpha,
				"beta":        beta,
				"facilitator": facilitator,
			}
			runtime := Runtime{Recipes: []plan.Recipe{{ID: "child"}}}
			deps := NewDeps(runtime, ".")
			deps.BackendFactory = func(_ *session.Session, actor session.Actor) (provider.Backend, error) {
				backend, ok := backends[actor.ID]
				if !ok {
					return nil, fmt.Errorf("unexpected actor %q", actor.ID)
				}
				return backend, nil
			}
			deps.ChildRequestExtractor = NewChildRequestExtractor(runtime.Recipes)

			var outcome engine.Outcome
			if recovered {
				outcome, err = engine.Resume(context.Background(), sess, deps, "", 0)
			} else {
				outcome, err = engine.Run(context.Background(), sess, deps)
			}
			if err != nil {
				t.Fatalf("execute session: %v", err)
			}
			if outcome.Status != "awaiting_decision" {
				t.Fatalf("outcome status = %q, want awaiting_decision from the later contested item", outcome.Status)
			}
			if beta.calls != 1 {
				t.Fatalf("second participant calls = %d, want one; opening claim blocked the participant", beta.calls)
			}

			events, err := Events(sess)
			if err != nil {
				t.Fatalf("read events: %v", err)
			}
			requests := make([]eventlog.ChildRequestedPayload, 0, 1)
			for _, event := range events {
				if request, ok := event.Payload.(eventlog.ChildRequestedPayload); ok {
					requests = append(requests, request)
				}
			}
			if len(requests) != 1 {
				t.Fatalf("child requests = %#v, want only the later-turn request", requests)
			}
			store, err := sess.BlobStore(blobstore.Limits{})
			if err != nil {
				t.Fatalf("open blob store: %v", err)
			}
			reader, err := store.Open(requests[0].Question)
			if err != nil {
				t.Fatalf("open child request question: %v", err)
			}
			question, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil {
				t.Fatalf("read child request question: %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("close child request question: %v", closeErr)
			}
			if string(question) != "Resolve this contested parent-relay item: replied claim" {
				t.Fatalf("child request question = %q, want later contested item", question)
			}
		})
	}
}

func openingClaimChildPlan(sessionID string) session.Plan {
	return session.Plan{
		Kind:          session.PlanKind,
		SchemaVersion: session.SchemaVersion,
		SessionID:     sessionID,
		Provenance:    session.ProvenanceOrdinary,
		Task:          "do not block the second participant on an opening claim",
		Timeouts:      session.Timeouts{TurnSeconds: 5, StallSeconds: 5},
		Mode:          session.ModeCooperative,
		Investigation: session.InvestigationNormal,
		Actors: []session.Actor{
			{ID: "alpha", Backend: "codex"},
			{ID: "beta", Backend: "codex"},
			{ID: "facilitator", Backend: "codex"},
		},
		Schedule:      session.Schedule{Kind: "dialogue", Turns: 2},
		Facilitator:   &session.Facilitator{Actor: "facilitator", Cadence: 1},
		ProviderRetry: session.ProviderRetry{Mode: "forbid", MaxAttempts: 1},
		Workspace:     session.Workspace{Mode: "current"},
		Inputs:        []session.Input{},
		Context:       []session.Input{},
		Skills:        []session.Input{},
		ChildPolicy: session.ChildPolicy{
			Mode: "ask", MaxDepth: 1, MaxChildren: 1, MaxTurns: 1, AllowedRecipes: []string{"child"},
		},
		Result: session.Result{Source: "last_turn", Format: "text"},
	}
}

func seedSuccessfulOpeningFacilitator(t *testing.T, sess *session.Session) {
	t.Helper()
	store, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	opening, err := store.PutBytes([]byte("opening contribution"), "text/plain; charset=utf-8")
	if err != nil {
		t.Fatalf("store opening contribution: %v", err)
	}
	ledger, err := store.PutBytes([]byte(`{"settled":[],"contested":["opening claim"],"withdrawn":[]}`), "text/plain; charset=utf-8")
	if err != nil {
		t.Fatalf("store facilitator ledger: %v", err)
	}
	writer, err := sess.EventWriter(store)
	if err != nil {
		t.Fatalf("open event writer: %v", err)
	}
	defer writer.Close()
	for _, payload := range []eventlog.Payload{
		eventlog.SessionStartedPayload{PlanDigest: sess.Digest, SessionID: sess.Plan.SessionID},
		eventlog.TurnStartedPayload{ActorID: "alpha", Round: 1, Role: eventlog.ParticipantRole},
		eventlog.AttemptStartedPayload{ActorID: "alpha", Attempt: 1},
		eventlog.AttemptFinishedPayload{ActorID: "alpha", Attempt: 1, Outcome: "success", ProviderOutcome: "completed", Content: opening},
		eventlog.TurnFinishedPayload{ActorID: "alpha", Round: 1, Content: opening},
		eventlog.TurnStartedPayload{ActorID: "facilitator", Round: 1, Role: eventlog.FacilitatorRole},
		eventlog.AttemptStartedPayload{ActorID: "facilitator", Attempt: 1},
		eventlog.AttemptFinishedPayload{ActorID: "facilitator", Attempt: 1, Outcome: "success", ProviderOutcome: "completed", Content: ledger},
	} {
		event := eventlog.NewEvent(fmt.Sprintf("seed-%d", writer.NextSeq()), time.Unix(1700000000, 0), payload)
		if _, err := writer.Append(event); err != nil {
			t.Fatalf("append %T: %v", payload, err)
		}
	}
}

type relayv2TestBackend struct {
	id        string
	responses []string
	calls     int
}

func (b *relayv2TestBackend) Name() string   { return "codex" }
func (b *relayv2TestBackend) SlotID() string { return b.id }
func (b *relayv2TestBackend) Label() string  { return b.id }
func (b *relayv2TestBackend) RunTurn(context.Context, string, provider.TurnOptions) (provider.TurnResult, error) {
	if b.calls >= len(b.responses) {
		return provider.TurnResult{}, fmt.Errorf("unexpected call %d for %s", b.calls+1, b.id)
	}
	response := b.responses[b.calls]
	b.calls++
	return provider.TurnResult{Content: response}, nil
}
func (b *relayv2TestBackend) SessionState() provider.SlotState {
	return provider.SlotState{"session_id": b.id + "-session"}
}
func (*relayv2TestBackend) RestoreState(provider.SlotState, provider.SlotConfig) error { return nil }
func (*relayv2TestBackend) Cleanup() error                                             { return nil }

var _ provider.Backend = (*relayv2TestBackend)(nil)
