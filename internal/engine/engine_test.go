package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/plan"
	"github.com/charlesnpx/convo-relay/internal/provider"
	"github.com/charlesnpx/convo-relay/internal/session"
	"github.com/charlesnpx/convo-relay/internal/sessionview"
)

func TestRunDialogueEventOrderAndBlobs(t *testing.T) {
	plan := dialoguePlan(4)
	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "alpha one"}, {content: "alpha two"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta", responses: []fakeResponse{{content: "beta one"}, {content: "beta two"}}}
	sess := createSession(t, plan)

	outcome, err := Run(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Result != "beta two" {
		t.Fatalf("outcome = %#v", outcome)
	}

	events := sessionEvents(t, sess)
	wantTypes := []eventlog.Type{
		eventlog.SessionStarted,
		eventlog.TurnStarted, eventlog.AttemptStarted, eventlog.AttemptFinished, eventlog.TurnFinished,
		eventlog.TurnStarted, eventlog.AttemptStarted, eventlog.AttemptFinished, eventlog.TurnFinished,
		eventlog.TurnStarted, eventlog.AttemptStarted, eventlog.AttemptFinished, eventlog.TurnFinished,
		eventlog.TurnStarted, eventlog.AttemptStarted, eventlog.AttemptFinished, eventlog.TurnFinished,
		eventlog.ResultProduced, eventlog.SessionFinished,
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event types = %v, want %v", got, wantTypes)
	}
	store, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		t.Fatalf("open blobs: %v", err)
	}
	wantTurns := []struct {
		actor string
		text  string
	}{{"alpha", "alpha one"}, {"beta", "beta one"}, {"alpha", "alpha two"}, {"beta", "beta two"}}
	var gotTurns []struct {
		actor string
		text  string
	}
	for _, event := range events {
		payload, ok := turnFinished(event)
		if !ok {
			continue
		}
		body := readBlob(t, store, payload.Content)
		gotTurns = append(gotTurns, struct {
			actor string
			text  string
		}{payload.ActorID, body})
	}
	if !reflect.DeepEqual(gotTurns, wantTurns) {
		t.Fatalf("turn blobs = %#v, want %#v", gotTurns, wantTurns)
	}
}

func TestDialogueStopsConvergedAndNoLedgerSignal(t *testing.T) {
	cases := []struct {
		name       string
		responses  []fakeResponse
		stopReason string
	}{
		{
			name: "converged_done_signals",
			responses: []fakeResponse{
				{content: "opening"}, {content: "response"},
				{content: "task is complete"}, {content: "work is complete"},
			},
			stopReason: stopConverged,
		},
		{
			name: "stalled_no_ledger_signal",
			responses: []fakeResponse{
				{content: "opening"}, {content: "response"},
				{content: "more analysis"}, {content: "another view"},
			},
			stopReason: stopNoLedgerSignal,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan := dialoguePlan(6)
			plan.Schedule.StopOnConvergence = true
			alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{test.responses[0], test.responses[2]}}
			beta := &fakeBackend{name: "codex", slotID: "beta", responses: []fakeResponse{test.responses[1], test.responses[3]}}
			sess := createSession(t, plan)

			_, err := Run(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta}))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			events := sessionEvents(t, sess)
			if got := countType(events, eventlog.TurnFinished); got != 4 {
				t.Fatalf("finished participant turns = %d, want 4", got)
			}
			finished := sessionFinished(t, events)
			if finished.StopReason != test.stopReason {
				t.Fatalf("session.finished = %#v", finished)
			}
		})
	}
	t.Run("converged_clean_facilitator_ledger", func(t *testing.T) {
		plan := dialoguePlan(6)
		plan.Schedule.StopOnConvergence = true
		plan.Actors = append(plan.Actors, session.Actor{ID: "facilitator", Backend: "codex"})
		plan.Facilitator = &session.Facilitator{Actor: "facilitator", Cadence: 1}
		alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "alpha one"}, {content: "alpha two"}}}
		beta := &fakeBackend{name: "codex", slotID: "beta", responses: []fakeResponse{{content: "beta one"}, {content: "beta two"}}}
		facilitator := &fakeBackend{name: "codex", slotID: "facilitator", responses: []fakeResponse{
			{content: `{"settled":[],"contested":[],"withdrawn":[]}`},
			{content: `{"settled":[],"contested":[],"withdrawn":[]}`},
			{content: `{"settled":[],"contested":[],"withdrawn":[]}`},
			{content: `{"settled":["agreement"],"contested":[],"withdrawn":[]}`},
		}}
		sess := createSession(t, plan)

		_, err := Run(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta, "facilitator": facilitator}))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		events := sessionEvents(t, sess)
		if got := countType(events, eventlog.TurnFinished); got != 8 {
			t.Fatalf("finished turns = %d, want 8", got)
		}
		if got := sessionFinished(t, events).StopReason; got != stopConverged {
			t.Fatalf("stop reason = %q, want %q", got, stopConverged)
		}
	})
}

func TestSequenceRunsOrderAndReducer(t *testing.T) {
	plan := sequencePlan()
	callOrder := []string{}
	alpha := &fakeBackend{name: "codex", slotID: "alpha", calls: &callOrder, responses: []fakeResponse{{content: "alpha result"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta", calls: &callOrder, responses: []fakeResponse{{content: "beta result"}}}
	reducer := &fakeBackend{name: "codex", slotID: "reducer", calls: &callOrder, responses: []fakeResponse{{content: "reduced result"}}}
	sess := createSession(t, plan)

	outcome, err := Run(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta, "reducer": reducer}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Result != "reduced result" {
		t.Fatalf("result = %q, want reducer output", outcome.Result)
	}
	if want := []string{"alpha", "beta", "reducer"}; !reflect.DeepEqual(callOrder, want) {
		t.Fatalf("backend order = %v, want %v", callOrder, want)
	}
	var started []eventlog.TurnStartedPayload
	for _, event := range sessionEvents(t, sess) {
		if payload, ok := turnStarted(event); ok {
			started = append(started, payload)
		}
	}
	if got := []string{started[0].ActorID, started[1].ActorID, started[2].ActorID}; !reflect.DeepEqual(got, []string{"alpha", "beta", "reducer"}) {
		t.Fatalf("turn order = %v", got)
	}
	if started[2].Role != eventlog.ReducerRole {
		t.Fatalf("reducer role = %s", started[2].Role)
	}
}

func TestProviderRetryAndAuthFailure(t *testing.T) {
	cases := []struct {
		name         string
		responses    []fakeResponse
		wantCalls    int
		wantStatus   string
		wantCategory string
		wantError    bool
	}{
		{
			name: "transient_retries_then_succeeds",
			responses: []fakeResponse{
				{err: provider.RetryableProviderError{Detail: "temporarily unavailable"}},
				{content: "recovered"},
			},
			wantCalls:    2,
			wantStatus:   statusCompleted,
			wantCategory: "transient",
		},
		{
			name:         "auth_never_retries",
			responses:    []fakeResponse{{err: provider.BackendRunError{Detail: "unauthorized"}}},
			wantCalls:    1,
			wantStatus:   statusFailed,
			wantCategory: "auth",
			wantError:    true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan := dialoguePlan(1)
			plan.ProviderRetry = session.ProviderRetry{Mode: "allow", MaxAttempts: 2}
			alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: test.responses}
			beta := &fakeBackend{name: "codex", slotID: "beta"}
			sess := createSession(t, plan)

			outcome, err := Run(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta}))
			if (err != nil) != test.wantError {
				t.Fatalf("Run error = %v, want error=%t", err, test.wantError)
			}
			if sessionFinished(t, sessionEvents(t, sess)).Status != test.wantStatus || len(alpha.prompts) != test.wantCalls {
				t.Fatalf("outcome=%#v calls=%d", outcome, len(alpha.prompts))
			}
			failures := providerFailures(sessionEvents(t, sess))
			if len(failures) != 1 || failures[0].Category != test.wantCategory {
				t.Fatalf("provider failures = %#v", failures)
			}
		})
	}
}

func TestResumeReportsAndRetriesAbandonedAttempt(t *testing.T) {
	plan := dialoguePlan(1)
	plan.ProviderRetry = session.ProviderRetry{Mode: "allow", MaxAttempts: 2}
	sess := createSession(t, plan)
	store, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		t.Fatalf("open blobs: %v", err)
	}
	writer, err := sess.EventWriter(store)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	appendEvent(t, writer, eventlog.SessionStartedPayload{PlanDigest: sess.Digest, SessionID: plan.SessionID})
	appendEvent(t, writer, eventlog.TurnStartedPayload{ActorID: "alpha", Round: 1, Role: eventlog.ParticipantRole})
	appendEvent(t, writer, eventlog.AttemptStartedPayload{ActorID: "alpha", Attempt: 1})
	if err := writer.Close(); err != nil {
		t.Fatalf("close seed writer: %v", err)
	}

	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "resumed"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta"}
	_, err = Resume(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta}), "continue")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(alpha.prompts) != 1 || !strings.Contains(alpha.prompts[0], "Resume direction: continue") {
		t.Fatalf("resumed prompts = %#v", alpha.prompts)
	}
	events := sessionEvents(t, sess)
	diagnostics, err := sessionview.Diagnostics(plan, events, store)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if got := diagnostics.AbandonedAttempts; len(got) != 1 || got[0].ActorID != "alpha" || got[0].Attempt != 1 {
		t.Fatalf("abandoned attempts = %#v", got)
	}
	if got := countType(events, eventlog.AttemptStarted); got != 2 {
		t.Fatalf("attempt.started count = %d, want 2", got)
	}
	if got := countType(events, eventlog.AttemptFinished); got != 1 {
		t.Fatalf("attempt.finished count = %d, want 1", got)
	}
	if got := countType(events, eventlog.TurnStarted); got != 1 {
		t.Fatalf("turn.started count = %d, want 1", got)
	}
	if got := countType(events, eventlog.SteeringQueued); got != 1 {
		t.Fatalf("steering.queued count = %d, want 1", got)
	}
	if got := countType(events, eventlog.SteeringApplied); got != 1 {
		t.Fatalf("steering.applied count = %d, want 1", got)
	}
}

func TestResumeFinalizesRecordedSuccessWithoutProvider(t *testing.T) {
	plan := dialoguePlan(1)
	sess := createSession(t, plan)
	var content blobstore.BlobRef
	seedLog(t, sess, func(store *blobstore.Store, writer *eventlog.Writer) {
		content = putSeedText(t, store, "durable provider result")
		appendEvent(t, writer, eventlog.TurnStartedPayload{ActorID: "alpha", Round: 1, Role: eventlog.ParticipantRole})
		appendEvent(t, writer, eventlog.AttemptStartedPayload{ActorID: "alpha", Attempt: 1})
		appendEvent(t, writer, eventlog.AttemptFinishedPayload{ActorID: "alpha", Attempt: 1, Outcome: "success", Content: content})
	})
	alpha := &fakeBackend{name: "codex", slotID: "alpha"}
	beta := &fakeBackend{name: "codex", slotID: "beta"}
	outcome, err := Resume(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta}), "")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if outcome.Result != "durable provider result" || len(alpha.prompts) != 0 || len(beta.prompts) != 0 {
		t.Fatalf("outcome=%#v alpha=%#v beta=%#v", outcome, alpha.prompts, beta.prompts)
	}
	events := sessionEvents(t, sess)
	if countType(events, eventlog.TurnFinished) != 1 || sessionFinished(t, events).Status != statusCompleted {
		t.Fatalf("durable attempt did not complete: %#v", eventTypes(events))
	}
}

func TestResumeRecordedAuthFailureIsTerminal(t *testing.T) {
	plan := dialoguePlan(1)
	sess := createSession(t, plan)
	seedLog(t, sess, func(store *blobstore.Store, writer *eventlog.Writer) {
		content := putSeedText(t, store, "unauthorized")
		appendEvent(t, writer, eventlog.TurnStartedPayload{ActorID: "alpha", Round: 1, Role: eventlog.ParticipantRole})
		appendEvent(t, writer, eventlog.AttemptStartedPayload{ActorID: "alpha", Attempt: 1})
		appendEvent(t, writer, eventlog.AttemptFinishedPayload{ActorID: "alpha", Attempt: 1, Outcome: "failed", Content: content})
		appendEvent(t, writer, eventlog.ProviderFailedPayload{
			ActorID: "alpha", Backend: "codex", Category: "auth", Retryable: false, Attempts: 1,
			RemediationCode: "authenticate", SanitizedDetail: "unauthorized",
		})
	})
	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "must not run"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta"}
	if _, err := Resume(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta}), ""); err == nil {
		t.Fatal("Resume accepted a recorded non-retryable auth failure")
	}
	events := sessionEvents(t, sess)
	if len(alpha.prompts) != 0 || sessionFinished(t, events).Status != statusFailed {
		t.Fatalf("auth replay calls=%d status=%s", len(alpha.prompts), sessionFinished(t, events).Status)
	}
}

func TestResumeServicesDueFacilitatorBeforeNextParticipant(t *testing.T) {
	plan := dialoguePlan(4)
	plan.Actors = append(plan.Actors, session.Actor{ID: "facilitator", Backend: "codex"})
	plan.Facilitator = &session.Facilitator{Actor: "facilitator", Cadence: 1}
	sess := createSession(t, plan)
	seedLog(t, sess, func(store *blobstore.Store, writer *eventlog.Writer) {
		content := putSeedText(t, store, "first participant")
		appendEvent(t, writer, eventlog.TurnStartedPayload{ActorID: "alpha", Round: 1, Role: eventlog.ParticipantRole})
		appendEvent(t, writer, eventlog.AttemptStartedPayload{ActorID: "alpha", Attempt: 1})
		appendEvent(t, writer, eventlog.AttemptFinishedPayload{ActorID: "alpha", Attempt: 1, Outcome: "success", Content: content})
		appendEvent(t, writer, eventlog.TurnFinishedPayload{ActorID: "alpha", Round: 1, Content: content})
	})
	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "third participant"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta", responses: []fakeResponse{{content: "second participant"}, {content: "fourth participant"}}}
	facilitator := &fakeBackend{name: "codex", slotID: "facilitator", responses: []fakeResponse{
		{content: "{\"settled\":[],\"contested\":[],\"withdrawn\":[]}"},
		{content: "{\"settled\":[],\"contested\":[],\"withdrawn\":[]}"},
		{content: "{\"settled\":[],\"contested\":[],\"withdrawn\":[]}"},
		{content: "{\"settled\":[],\"contested\":[],\"withdrawn\":[]}"},
	}}
	if _, err := Resume(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta, "facilitator": facilitator}), ""); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := len(facilitator.prompts); got != 4 {
		t.Fatalf("facilitator calls = %d, want 4", got)
	}
}

func TestResumeRebuildsChildBudgets(t *testing.T) {
	parent := dialoguePlan(2)
	parent.ChildPolicy = session.ChildPolicy{Mode: "allow", MaxDepth: 1, MaxChildren: 1, MaxTurns: 1, AllowedRecipes: []string{"child"}}
	sess := createSession(t, parent)
	seedLog(t, sess, func(store *blobstore.Store, writer *eventlog.Writer) {
		content := putSeedText(t, store, "first participant")
		question := putSeedText(t, store, "first child question")
		childResult := putSeedText(t, store, "first child result")
		appendEvent(t, writer, eventlog.TurnStartedPayload{ActorID: "alpha", Round: 1, Role: eventlog.ParticipantRole})
		appendEvent(t, writer, eventlog.AttemptStartedPayload{ActorID: "alpha", Attempt: 1})
		appendEvent(t, writer, eventlog.ChildRequestedPayload{RequestID: "child-one", RequesterActorID: "alpha", RecipeID: "child", Question: question})
		appendEvent(t, writer, eventlog.AttemptFinishedPayload{ActorID: "alpha", Attempt: 1, Outcome: "success", Content: content})
		appendEvent(t, writer, eventlog.TurnFinishedPayload{ActorID: "alpha", Round: 1, Content: content})
		appendEvent(t, writer, eventlog.ChildDecidedPayload{RequestID: "child-one", Admitted: true, Reason: "admitted", BudgetState: "available;child_turns=1"})
		appendEvent(t, writer, eventlog.ChildCompletedPayload{RequestID: "child-one", ChildSessionID: "first-child", Result: childResult})
	})
	alpha := &fakeBackend{name: "codex", slotID: "alpha"}
	beta := &fakeBackend{name: "codex", slotID: "beta", responses: []fakeResponse{{content: "second participant"}}}
	child := &fakeBackend{name: "codex", slotID: "child-alpha", responses: []fakeResponse{{content: "must not run"}}}
	deps := testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta, "child-alpha": child})
	deps.Recipes = []plan.Recipe{childRecipe()}
	deps.ChildRequestExtractor = func(actor session.Actor, role eventlog.Role, _ provider.TurnResult) []ChildRequest {
		if actor.ID == "beta" && role == eventlog.ParticipantRole {
			return []ChildRequest{{ID: "child-two", Request: plan.ChildRequest{RecipeID: "child", Question: "second child question"}}}
		}
		return nil
	}
	if _, err := Resume(context.Background(), sess, deps, ""); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	decisions := childDecisions(sessionEvents(t, sess))
	if len(decisions) != 2 || decisions[1].Admitted || len(child.prompts) != 0 {
		t.Fatalf("decisions=%#v child calls=%d", decisions, len(child.prompts))
	}
}

func TestExtractedChildRequestSurvivesBeforeParentCompletion(t *testing.T) {
	parent := dialoguePlan(1)
	parent.ChildPolicy = session.ChildPolicy{Mode: "allow", MaxDepth: 1, MaxChildren: 1, MaxTurns: 1, AllowedRecipes: []string{"child"}}
	sess := createSession(t, parent)
	seedLog(t, sess, func(store *blobstore.Store, writer *eventlog.Writer) {
		content := putSeedText(t, store, "parent response")
		question := putSeedText(t, store, "durable child request")
		appendEvent(t, writer, eventlog.TurnStartedPayload{ActorID: "alpha", Round: 1, Role: eventlog.ParticipantRole})
		appendEvent(t, writer, eventlog.AttemptStartedPayload{ActorID: "alpha", Attempt: 1})
		appendEvent(t, writer, eventlog.ChildRequestedPayload{RequestID: "child-one", RequesterActorID: "alpha", RecipeID: "child", Question: question})
		appendEvent(t, writer, eventlog.AttemptFinishedPayload{ActorID: "alpha", Attempt: 1, Outcome: "success", Content: content})
	})
	alpha := &fakeBackend{name: "codex", slotID: "alpha"}
	beta := &fakeBackend{name: "codex", slotID: "beta"}
	child := &fakeBackend{name: "codex", slotID: "child-alpha", responses: []fakeResponse{{content: "recovered child"}}}
	deps := testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta, "child-alpha": child})
	deps.Recipes = []plan.Recipe{childRecipe()}
	if _, err := Resume(context.Background(), sess, deps, ""); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	events := sessionEvents(t, sess)
	if len(alpha.prompts) != 0 || len(child.prompts) != 1 || countType(events, eventlog.TurnFinished) != 1 || countType(events, eventlog.ChildCompleted) != 1 {
		t.Fatalf("alpha=%d child=%d events=%#v", len(alpha.prompts), len(child.prompts), eventTypes(events))
	}
}

func TestResumeReplaysQueuedSteering(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprintf("applied=%t", applied), func(t *testing.T) {
			plan := dialoguePlan(1)
			sess := createSession(t, plan)
			seedLog(t, sess, func(store *blobstore.Store, writer *eventlog.Writer) {
				direction := putSeedText(t, store, "take the conservative route")
				appendEvent(t, writer, eventlog.TurnStartedPayload{ActorID: "alpha", Round: 1, Role: eventlog.ParticipantRole})
				appendEvent(t, writer, eventlog.SteeringQueuedPayload{Prompt: direction})
				if applied {
					appendEvent(t, writer, eventlog.SteeringAppliedPayload{Prompt: direction, Round: 1})
				}
			})
			alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "steered"}}}
			beta := &fakeBackend{name: "codex", slotID: "beta"}
			if _, err := Resume(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta}), ""); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if len(alpha.prompts) != 1 || !strings.Contains(alpha.prompts[0], "Resume direction: take the conservative route") {
				t.Fatalf("resumed prompts = %#v", alpha.prompts)
			}
			if got := countType(sessionEvents(t, sess), eventlog.SteeringApplied); got != 1 {
				t.Fatalf("steering.applied count = %d, want 1", got)
			}
		})
	}
}

func TestResultValidationRecordsValidAndInvalidOutcomes(t *testing.T) {
	for _, test := range []struct {
		name       string
		content    string
		wantStatus string
		wantValid  string
		wantErr    bool
	}{
		{name: "valid", content: "{\"answer\":\"yes\"}", wantStatus: statusCompleted, wantValid: "valid"},
		{name: "invalid format", content: "not json", wantStatus: statusFailed, wantValid: "invalid", wantErr: true},
		{name: "invalid schema", content: "{}", wantStatus: statusFailed, wantValid: "invalid", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := dialoguePlan(1)
			plan.Result = session.Result{
				Source: "last_turn",
				Format: "json",
				Schema: []byte("{\"required\":[\"answer\"],\"type\":\"object\"}"),
			}
			sess := createSession(t, plan)
			alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: test.content}}}
			beta := &fakeBackend{name: "codex", slotID: "beta"}
			_, err := Run(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta}))
			if (err != nil) != test.wantErr {
				t.Fatalf("Run error=%v, want error=%t", err, test.wantErr)
			}
			events := sessionEvents(t, sess)
			produced, found := producedResult(events)
			if !found || produced.ValidationOutcome != test.wantValid || sessionFinished(t, events).Status != test.wantStatus {
				t.Fatalf("result=%#v found=%t finished=%#v", produced, found, sessionFinished(t, events))
			}
		})
	}
}

func TestResumeLifecycleGuardsBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name     string
		resume   string
		steering string
		prompt   string
	}{
		{name: "resume forbidden", resume: "forbid", steering: "allow"},
		{name: "steering forbidden", resume: "allow", steering: "forbid", prompt: "new direction"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := dialoguePlan(1)
			plan.Lifecycle = &session.Lifecycle{
				Resume: test.resume, Steering: test.steering, Dynamic: "forbid", WorkspaceIsolation: "inherited",
			}
			sess := createSession(t, plan)
			seedLog(t, sess, func(_ *blobstore.Store, _ *eventlog.Writer) {})
			before, err := os.ReadFile(filepath.Join(sess.Root, eventlog.EventsFilename))
			if err != nil {
				t.Fatalf("read before: %v", err)
			}
			alpha := &fakeBackend{name: "codex", slotID: "alpha"}
			beta := &fakeBackend{name: "codex", slotID: "beta"}
			if _, err := Resume(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta}), test.prompt); err == nil {
				t.Fatal("Resume ignored lifecycle guard")
			}
			after, err := os.ReadFile(filepath.Join(sess.Root, eventlog.EventsFilename))
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("lifecycle rejection mutated log: before=%q after=%q", before, after)
			}
		})
	}
}

func TestProviderContinuationIsRecordedAndRestored(t *testing.T) {
	t.Run("records successful continuation", func(t *testing.T) {
		plan := dialoguePlan(1)
		sess := createSession(t, plan)
		alpha := &fakeBackend{
			name: "codex", slotID: "alpha", state: provider.SlotState{"thread_id": "thread-one"},
			responses: []fakeResponse{{content: "first"}},
		}
		beta := &fakeBackend{name: "codex", slotID: "beta"}
		if _, err := Run(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta})); err != nil {
			t.Fatalf("Run: %v", err)
		}
		attempt, found := successfulAttempt(sessionEvents(t, sess), "alpha", 1)
		if !found || attempt.ProviderSessionID != "thread-one" {
			t.Fatalf("attempt=%#v found=%t", attempt, found)
		}
	})

	t.Run("restores before resumed call", func(t *testing.T) {
		plan := dialoguePlan(3)
		sess := createSession(t, plan)
		seedLog(t, sess, func(store *blobstore.Store, writer *eventlog.Writer) {
			content := putSeedText(t, store, "first")
			appendEvent(t, writer, eventlog.TurnStartedPayload{ActorID: "alpha", Round: 1, Role: eventlog.ParticipantRole})
			appendEvent(t, writer, eventlog.AttemptStartedPayload{ActorID: "alpha", Attempt: 1})
			appendEvent(t, writer, eventlog.AttemptFinishedPayload{
				ActorID: "alpha", Attempt: 1, Outcome: "success", ProviderSessionID: "thread-one", Content: content,
			})
			appendEvent(t, writer, eventlog.TurnFinishedPayload{ActorID: "alpha", Round: 1, Content: content})
		})
		alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "third"}}}
		beta := &fakeBackend{name: "codex", slotID: "beta", responses: []fakeResponse{{content: "second"}}}
		if _, err := Resume(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta}), ""); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if len(alpha.restored) != 1 || alpha.restored[0]["thread_id"] != "thread-one" || len(alpha.prompts) != 1 {
			t.Fatalf("restored=%#v alpha prompts=%#v", alpha.restored, alpha.prompts)
		}
	})
}

func TestChildRequestsAdmitAndRejectWithoutRunningDeniedChildren(t *testing.T) {
	cases := []struct {
		name             string
		policy           session.ChildPolicy
		wantAdmitted     bool
		wantChildCalls   int
		wantSessionCount int
	}{
		{
			name:             "admitted",
			policy:           session.ChildPolicy{Mode: "allow", MaxDepth: 1, MaxChildren: 1, MaxTurns: 1, AllowedRecipes: []string{"child"}},
			wantAdmitted:     true,
			wantChildCalls:   1,
			wantSessionCount: 2,
		},
		{
			name:             "denied",
			policy:           session.ChildPolicy{Mode: "deny", MaxDepth: 1, MaxChildren: 1, MaxTurns: 1, AllowedRecipes: []string{"child"}},
			wantChildCalls:   0,
			wantSessionCount: 1,
		},
		{
			name:             "over_budget",
			policy:           session.ChildPolicy{Mode: "allow", MaxDepth: 1, MaxChildren: 0, MaxTurns: 1, AllowedRecipes: []string{"child"}},
			wantChildCalls:   0,
			wantSessionCount: 1,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			parent := dialoguePlan(1)
			parent.ChildPolicy = test.policy
			home := t.TempDir()
			sess := createSessionIn(t, home, parent)
			alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "request child"}}}
			beta := &fakeBackend{name: "codex", slotID: "beta"}
			child := &fakeBackend{name: "codex", slotID: "child-alpha", responses: []fakeResponse{{content: "child result"}}}
			deps := testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta, "child-alpha": child})
			deps.Recipes = []plan.Recipe{childRecipe()}
			deps.ChildRequestExtractor = func(actor session.Actor, role eventlog.Role, _ provider.TurnResult) []ChildRequest {
				if actor.ID != "alpha" || role != eventlog.ParticipantRole {
					return nil
				}
				return []ChildRequest{{ID: "child-request", Request: plan.ChildRequest{RecipeID: "child", Question: "resolve the child question"}}}
			}

			_, err := Run(context.Background(), sess, deps)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			decisions := childDecisions(sessionEvents(t, sess))
			if len(decisions) != 1 || decisions[0].Admitted != test.wantAdmitted {
				t.Fatalf("child decisions = %#v", decisions)
			}
			if test.wantAdmitted && indexOfType(sessionEvents(t, sess), eventlog.ChildRequested) > indexOfType(sessionEvents(t, sess), eventlog.TurnFinished) {
				t.Fatal("child request was not durable before its parent turn finished")
			}
			if got := len(child.prompts); got != test.wantChildCalls {
				t.Fatalf("child backend calls = %d, want %d", got, test.wantChildCalls)
			}
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatalf("read session home: %v", err)
			}
			if len(entries) != test.wantSessionCount {
				t.Fatalf("session count = %d, want %d", len(entries), test.wantSessionCount)
			}
			completed := childCompletions(sessionEvents(t, sess))
			if test.wantAdmitted {
				if len(completed) != 1 {
					t.Fatalf("child.completed = %#v", completed)
				}
				store, err := sess.BlobStore(blobstore.Limits{})
				if err != nil {
					t.Fatalf("open parent blobs: %v", err)
				}
				if body := readBlob(t, store, completed[0].Result); body != "child result" {
					t.Fatalf("parent-facing result = %q", body)
				}
			} else if len(completed) != 0 {
				t.Fatalf("denied child completed = %#v", completed)
			}
		})
	}
}

func TestRunWritesOnlySessionAuthorityFiles(t *testing.T) {
	sess := createSession(t, dialoguePlan(1))
	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "one"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta"}
	if _, err := Run(context.Background(), sess, testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries, err := os.ReadDir(sess.Root)
	if err != nil {
		t.Fatalf("read session root: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if want := []string{"blobs", "events.jsonl", "runtime", "session.json"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("session entries = %v, want %v", names, want)
	}
}

func TestBlobReferencesAreVerifiedBeforeEventAppend(t *testing.T) {
	sess := createSession(t, dialoguePlan(1))
	store, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		t.Fatalf("open blobs: %v", err)
	}
	verifier := &recordingVerifier{store: store}
	writer, err := eventlog.OpenWriter(sess.Root, verifier)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close()
	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "durable"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta"}
	deps := testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta})
	deps.Writer = writer
	if _, err := Run(context.Background(), sess, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(verifier.refs) == 0 {
		t.Fatal("writer never verified a blob reference")
	}
	for _, event := range sessionEvents(t, sess) {
		for _, ref := range eventlog.BlobRefs([]eventlog.Event{event}) {
			if !verifier.saw(ref) {
				t.Fatalf("event ref %s was not verified during append", ref.SHA256)
			}
		}
	}
}

type fakeResponse struct {
	content string
	err     error
}

type fakeBackend struct {
	name       string
	slotID     string
	responses  []fakeResponse
	prompts    []string
	calls      *[]string
	state      provider.SlotState
	restored   []provider.SlotState
	restoreErr error
}

func (b *fakeBackend) Name() string   { return b.name }
func (b *fakeBackend) SlotID() string { return b.slotID }
func (b *fakeBackend) Label() string  { return b.slotID }

func (b *fakeBackend) RunTurn(_ context.Context, prompt string, _ provider.TurnOptions) (provider.TurnResult, error) {
	b.prompts = append(b.prompts, prompt)
	if b.calls != nil {
		*b.calls = append(*b.calls, b.slotID)
	}
	index := len(b.prompts) - 1
	if index >= len(b.responses) {
		return provider.TurnResult{Content: "default"}, nil
	}
	response := b.responses[index]
	return provider.TurnResult{Content: response.content}, response.err
}

func (b *fakeBackend) SessionState() provider.SlotState { return b.state }
func (b *fakeBackend) RestoreState(state provider.SlotState, _ provider.SlotConfig) error {
	b.restored = append(b.restored, state)
	return b.restoreErr
}
func (b *fakeBackend) Cleanup() error { return nil }

type recordingVerifier struct {
	store *blobstore.Store
	refs  []blobstore.BlobRef
}

func (v *recordingVerifier) Verify(ref blobstore.BlobRef) error {
	if err := v.store.Verify(ref); err != nil {
		return err
	}
	v.refs = append(v.refs, ref)
	return nil
}

func (v *recordingVerifier) saw(ref blobstore.BlobRef) bool {
	for _, seen := range v.refs {
		if seen.Equal(ref) {
			return true
		}
	}
	return false
}

func dialoguePlan(turns int) session.Plan {
	return session.Plan{
		Kind:          session.PlanKind,
		SchemaVersion: session.SchemaVersion,
		SessionID:     "engine-session",
		Provenance:    session.ProvenanceOrdinary,
		Task:          "test the engine",
		Timeouts:      session.Timeouts{TurnSeconds: 5, StallSeconds: 5},
		Mode:          session.ModeCooperative,
		Investigation: session.InvestigationNormal,
		Actors: []session.Actor{
			{ID: "alpha", Backend: "codex"},
			{ID: "beta", Backend: "codex"},
		},
		Schedule:      session.Schedule{Kind: "dialogue", Turns: turns},
		ProviderRetry: session.ProviderRetry{Mode: "allow", MaxAttempts: 2},
		Workspace:     session.Workspace{Mode: "current"},
		Inputs:        []session.Input{},
		Context:       []session.Input{},
		Skills:        []session.Input{},
		MatchKeywords: []string{},
		ChildPolicy:   session.ChildPolicy{Mode: "deny", MaxDepth: 0, MaxChildren: 0, MaxTurns: 0, AllowedRecipes: []string{}},
		Result:        session.Result{Source: "last_turn", Format: "text"},
	}
}

func sequencePlan() session.Plan {
	return session.Plan{
		Kind:          session.PlanKind,
		SchemaVersion: session.SchemaVersion,
		SessionID:     "sequence-session",
		Provenance:    session.ProvenanceOrdinary,
		Task:          "reduce the sequence",
		Timeouts:      session.Timeouts{TurnSeconds: 5, StallSeconds: 5},
		Mode:          session.ModeCooperative,
		Investigation: session.InvestigationNormal,
		Actors: []session.Actor{
			{ID: "alpha", Backend: "codex"},
			{ID: "beta", Backend: "codex"},
			{ID: "reducer", Backend: "codex"},
		},
		Schedule:      session.Schedule{Kind: "sequence", Turns: 2, Order: []string{"alpha", "beta"}},
		Reducer:       &session.Reducer{Actor: "reducer"},
		ProviderRetry: session.ProviderRetry{Mode: "allow", MaxAttempts: 2},
		Workspace:     session.Workspace{Mode: "current"},
		Inputs:        []session.Input{},
		Context:       []session.Input{},
		Skills:        []session.Input{},
		MatchKeywords: []string{},
		ChildPolicy:   session.ChildPolicy{Mode: "deny", MaxDepth: 0, MaxChildren: 0, MaxTurns: 0, AllowedRecipes: []string{}},
		Result:        session.Result{Source: "reducer", Format: "text"},
	}
}

func childRecipe() plan.Recipe {
	return plan.Recipe{
		ID:            "child",
		Actors:        []session.Actor{{ID: "child-alpha", Backend: "codex"}},
		Schedule:      session.Schedule{Kind: "sequence", Turns: 1, Order: []string{"child-alpha"}},
		Mode:          session.ModeCooperative,
		Investigation: session.InvestigationNormal,
		ProviderRetry: session.ProviderRetry{Mode: "allow", MaxAttempts: 1},
		Workspace:     session.Workspace{Mode: "current"},
		Inputs:        []session.Input{},
		ChildPolicy:   session.ChildPolicy{Mode: "deny", MaxDepth: 0, MaxChildren: 0, MaxTurns: 0, AllowedRecipes: []string{}},
		Result:        session.Result{Source: "last_turn", Format: "text"},
	}
}

func createSession(t *testing.T, plan session.Plan) *session.Session {
	t.Helper()
	return createSessionIn(t, t.TempDir(), plan)
}

func createSessionIn(t *testing.T, home string, plan session.Plan) *session.Session {
	t.Helper()
	sess, err := session.Create(home, plan)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

func seedLog(t *testing.T, sess *session.Session, appendPayloads func(*blobstore.Store, *eventlog.Writer)) {
	t.Helper()
	store, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		t.Fatalf("open blobs: %v", err)
	}
	writer, err := sess.EventWriter(store)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	appendEvent(t, writer, eventlog.SessionStartedPayload{PlanDigest: sess.Digest, SessionID: sess.Plan.SessionID})
	appendPayloads(store, writer)
	if err := writer.Close(); err != nil {
		t.Fatalf("close seed writer: %v", err)
	}
}

func putSeedText(t *testing.T, store *blobstore.Store, text string) blobstore.BlobRef {
	t.Helper()
	ref, err := store.PutBytes([]byte(text), mediaTypePlainTextUTF8)
	if err != nil {
		t.Fatalf("put seed text: %v", err)
	}
	return ref
}

func testDeps(backends map[string]*fakeBackend) Deps {
	return Deps{
		BackendFactory: func(_ *session.Session, actor session.Actor) (provider.Backend, error) {
			backend, ok := backends[actor.ID]
			if !ok {
				return nil, fmt.Errorf("unexpected actor %s", actor.ID)
			}
			return backend, nil
		},
	}
}

func appendEvent(t *testing.T, writer *eventlog.Writer, payload eventlog.Payload) {
	t.Helper()
	if _, err := writer.Append(eventlog.NewEvent(fmt.Sprintf("seed-%d", writer.NextSeq()), time.Unix(1700000000, 0), payload)); err != nil {
		t.Fatalf("append %T: %v", payload, err)
	}
}

func sessionEvents(t *testing.T, sess *session.Session) []eventlog.Event {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(sess.Root, eventlog.EventsFilename))
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	events, err := eventlog.Replay(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("replay events: %v", err)
	}
	return events
}

func eventTypes(events []eventlog.Event) []eventlog.Type {
	types := make([]eventlog.Type, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func countType(events []eventlog.Event, kind eventlog.Type) int {
	count := 0
	for _, event := range events {
		if event.Type == kind {
			count++
		}
	}
	return count
}

func indexOfType(events []eventlog.Event, kind eventlog.Type) int {
	for index, event := range events {
		if event.Type == kind {
			return index
		}
	}
	return len(events)
}

func turnStarted(event eventlog.Event) (eventlog.TurnStartedPayload, bool) {
	switch payload := event.Payload.(type) {
	case eventlog.TurnStartedPayload:
		return payload, true
	}
	return eventlog.TurnStartedPayload{}, false
}

func turnFinished(event eventlog.Event) (eventlog.TurnFinishedPayload, bool) {
	switch payload := event.Payload.(type) {
	case eventlog.TurnFinishedPayload:
		return payload, true
	}
	return eventlog.TurnFinishedPayload{}, false
}

func sessionFinished(t *testing.T, events []eventlog.Event) eventlog.SessionFinishedPayload {
	t.Helper()
	for index := len(events) - 1; index >= 0; index-- {
		switch payload := events[index].Payload.(type) {
		case eventlog.SessionFinishedPayload:
			return payload
		}
	}
	t.Fatal("missing session.finished")
	return eventlog.SessionFinishedPayload{}
}

func providerFailures(events []eventlog.Event) []eventlog.ProviderFailedPayload {
	failures := []eventlog.ProviderFailedPayload{}
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.ProviderFailedPayload:
			failures = append(failures, payload)
		}
	}
	return failures
}

func childDecisions(events []eventlog.Event) []eventlog.ChildDecidedPayload {
	decisions := []eventlog.ChildDecidedPayload{}
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.ChildDecidedPayload:
			decisions = append(decisions, payload)
		}
	}
	return decisions
}

func childCompletions(events []eventlog.Event) []eventlog.ChildCompletedPayload {
	completed := []eventlog.ChildCompletedPayload{}
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.ChildCompletedPayload:
			completed = append(completed, payload)
		}
	}
	return completed
}

func producedResult(events []eventlog.Event) (eventlog.ResultProducedPayload, bool) {
	for _, event := range events {
		if payload, ok := event.Payload.(eventlog.ResultProducedPayload); ok {
			return payload, true
		}
	}
	return eventlog.ResultProducedPayload{}, false
}

func successfulAttempt(events []eventlog.Event, actorID string, number int) (eventlog.AttemptFinishedPayload, bool) {
	for _, event := range events {
		payload, ok := event.Payload.(eventlog.AttemptFinishedPayload)
		if ok && payload.ActorID == actorID && payload.Attempt == number && payload.Outcome == "success" {
			return payload, true
		}
	}
	return eventlog.AttemptFinishedPayload{}, false
}

func readBlob(t *testing.T, store *blobstore.Store, ref blobstore.BlobRef) string {
	t.Helper()
	reader, err := store.Open(ref)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read blob: read=%v close=%v", readErr, closeErr)
	}
	return string(body)
}

var _ provider.Backend = (*fakeBackend)(nil)
var _ eventlog.BlobVerifier = (*recordingVerifier)(nil)
