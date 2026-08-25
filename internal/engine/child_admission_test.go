package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/plan"
	"github.com/charlesnpx/convo-relay/internal/provider"
	"github.com/charlesnpx/convo-relay/internal/session"
)

func TestAskChildRequestWaitsForOperatorDecision(t *testing.T) {
	parent := dialoguePlan(2)
	parent.ChildPolicy = askChildPolicy(1, 1, 1)
	sess := createSessionIn(t, t.TempDir(), parent)
	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "parent before decision"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta"}
	child := &fakeBackend{name: "codex", slotID: "child-alpha"}
	deps := testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta, "child-alpha": child})
	deps.Recipes = []plan.Recipe{childRecipe()}
	deps.ChildRequestExtractor = childRequests(childRequest("child-request", "wait for an operator"))

	outcome, err := Run(context.Background(), sess, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Status != statusAwaitingDecision || len(alpha.prompts) != 1 || len(beta.prompts) != 0 || len(child.prompts) != 0 {
		t.Fatalf("outcome=%#v alpha=%d beta=%d child=%d", outcome, len(alpha.prompts), len(beta.prompts), len(child.prompts))
	}
	resumed, err := Resume(context.Background(), sess, deps, "", 0)
	if err != nil || resumed.Status != statusAwaitingDecision || len(alpha.prompts) != 1 || len(beta.prompts) != 0 {
		t.Fatalf("pending Resume outcome=%#v err=%v alpha=%d beta=%d", resumed, err, len(alpha.prompts), len(beta.prompts))
	}
	events := sessionEvents(t, sess)
	if countType(events, eventlog.ChildRequested) != 1 || len(childDecisions(events)) != 0 ||
		countType(events, eventlog.ResultProduced) != 0 || countType(events, eventlog.SessionFinished) != 0 {
		t.Fatalf("ask-mode events = %v", eventTypes(events))
	}
	pending, err := PendingChildren(sess)
	if err != nil {
		t.Fatalf("PendingChildren: %v", err)
	}
	if len(pending) != 1 || pending[0].RequestID != "child-request" || pending[0].Question != "wait for an operator" {
		t.Fatalf("pending children = %#v", pending)
	}
}

func TestApprovePendingChildThenResume(t *testing.T) {
	parent := dialoguePlan(2)
	parent.ChildPolicy = askChildPolicy(1, 1, 1)
	home := t.TempDir()
	sess := createSessionIn(t, home, parent)
	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "parent before approval"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta", responses: []fakeResponse{{content: "parent after approval"}}}
	child := &fakeBackend{name: "codex", slotID: "child-alpha", responses: []fakeResponse{{content: "child result once"}}}
	deps := testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta, "child-alpha": child})
	deps.Recipes = []plan.Recipe{childRecipe()}
	deps.ChildRequestExtractor = childRequests(childRequest("child-request", "resolve the child question"))

	if _, err := Run(context.Background(), sess, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := ApproveChild(context.Background(), sess, "child-request", deps.Recipes); err != nil {
		t.Fatalf("ApproveChild: %v", err)
	}
	decision, found := childDecisionFor(sessionEvents(t, sess), "child-request")
	if !found || !decision.Admitted || decision.Plan == nil || decision.Reason != "admitted by operator" {
		t.Fatalf("admission decision = %#v found=%t", decision, found)
	}

	outcome, err := Resume(context.Background(), sess, deps, "", 0)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if outcome.Status != statusCompleted || len(child.prompts) != 1 || len(beta.prompts) != 1 || strings.Count(beta.prompts[0], "child result once") != 1 {
		t.Fatalf("outcome=%#v child=%d beta=%#v", outcome, len(child.prompts), beta.prompts)
	}
	events := sessionEvents(t, sess)
	completed := childCompletions(events)
	if len(completed) != 1 || completed[0].RequestID != "child-request" || completed[0].Status != statusCompleted {
		t.Fatalf("child completions = %#v", completed)
	}
	childSession := findSessionByID(t, home, completed[0].ChildSessionID)
	if got := countType(sessionEvents(t, childSession), eventlog.TurnFinished); got != 1 {
		t.Fatalf("child turn count = %d, want 1", got)
	}
}

func TestNestedAskWaitsForGrandchildDecision(t *testing.T) {
	parent := dialoguePlan(2)
	parent.ChildPolicy = askChildPolicy(2, 2, 2)
	home := t.TempDir()
	sess := createSessionIn(t, home, parent)
	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "parent asks"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta", responses: []fakeResponse{{content: "parent completes"}}}
	child := &fakeBackend{name: "codex", slotID: "child-alpha", responses: []fakeResponse{{content: "child result after asking grandchild"}}}
	grandchild := &fakeBackend{name: "codex", slotID: "grandchild-alpha", responses: []fakeResponse{{content: "grandchild result"}}}
	deps := testDeps(map[string]*fakeBackend{
		"alpha":            alpha,
		"beta":             beta,
		"child-alpha":      child,
		"grandchild-alpha": grandchild,
	})
	nestedChildRecipe := childRecipe()
	nestedChildRecipe.ChildPolicy = session.ChildPolicy{
		Mode:           "ask",
		MaxDepth:       2,
		MaxChildren:    2,
		MaxTurns:       2,
		AllowedRecipes: []string{"grandchild"},
	}
	grandchildRecipe := childRecipe()
	grandchildRecipe.ID = "grandchild"
	grandchildRecipe.Actors = []session.Actor{{ID: "grandchild-alpha", Backend: "codex"}}
	grandchildRecipe.Schedule.Order = []string{"grandchild-alpha"}
	deps.Recipes = []plan.Recipe{nestedChildRecipe, grandchildRecipe}
	deps.ChildRequestExtractor = func(actor session.Actor, role eventlog.Role, _ provider.TurnResult) []ChildRequest {
		if role != eventlog.ParticipantRole {
			return nil
		}
		switch actor.ID {
		case "alpha":
			return []ChildRequest{childRequest("child-request", "ask the child")}
		case "child-alpha":
			return []ChildRequest{{ID: "grandchild-request", Request: plan.ChildRequest{RecipeID: "grandchild", Question: "ask the grandchild"}}}
		default:
			return nil
		}
	}

	if _, err := Run(context.Background(), sess, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := ApproveChild(context.Background(), sess, "child-request", deps.Recipes); err != nil {
		t.Fatalf("ApproveChild child: %v", err)
	}
	waiting, err := Resume(context.Background(), sess, deps, "", 0)
	if err != nil || waiting.Status != statusAwaitingDecision || waiting.Status == statusFailed {
		t.Fatalf("waiting outcome=%#v err=%v", waiting, err)
	}
	parentEvents := sessionEvents(t, sess)
	if countType(parentEvents, eventlog.SessionFinished) != 0 || len(childCompletions(parentEvents)) != 0 {
		t.Fatalf("waiting parent events = %v", eventTypes(parentEvents))
	}
	childSession := findSessionByID(t, home, parent.SessionID+"-child-child-request")
	pending, err := PendingChildren(childSession)
	if err != nil || len(pending) != 1 || pending[0].RequestID != "grandchild-request" {
		t.Fatalf("grandchild pending=%#v err=%v", pending, err)
	}
	resumed, err := Resume(context.Background(), sess, deps, "", 0)
	if err != nil || resumed.Status != statusAwaitingDecision || len(child.prompts) != 1 {
		t.Fatalf("resumed outcome=%#v err=%v child=%d", resumed, err, len(child.prompts))
	}
	if err := ApproveChild(context.Background(), childSession, "grandchild-request", deps.Recipes); err != nil {
		t.Fatalf("ApproveChild grandchild: %v", err)
	}
	completed, err := Resume(context.Background(), sess, deps, "", 0)
	if err != nil || completed.Status != statusCompleted || len(beta.prompts) != 1 || strings.Count(beta.prompts[0], "child result after asking grandchild") != 1 {
		t.Fatalf("completed outcome=%#v err=%v beta=%#v", completed, err, beta.prompts)
	}
	parentCompletions := childCompletions(sessionEvents(t, sess))
	if len(parentCompletions) != 1 || parentCompletions[0].RequestID != "child-request" || parentCompletions[0].Status != statusCompleted {
		t.Fatalf("parent child completions=%#v", parentCompletions)
	}
	nestedCompletions := childCompletions(sessionEvents(t, childSession))
	if len(nestedCompletions) != 1 || nestedCompletions[0].RequestID != "grandchild-request" || nestedCompletions[0].Status != statusCompleted {
		t.Fatalf("grandchild completions=%#v", nestedCompletions)
	}
}

func TestRejectPendingChild(t *testing.T) {
	parent := dialoguePlan(2)
	parent.ChildPolicy = askChildPolicy(1, 1, 1)
	sess := createSessionIn(t, t.TempDir(), parent)
	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "parent before rejection"}}}
	beta := &fakeBackend{name: "codex", slotID: "beta", responses: []fakeResponse{{content: "parent after rejection"}}}
	child := &fakeBackend{name: "codex", slotID: "child-alpha"}
	deps := testDeps(map[string]*fakeBackend{"alpha": alpha, "beta": beta, "child-alpha": child})
	deps.Recipes = []plan.Recipe{childRecipe()}
	deps.ChildRequestExtractor = childRequests(childRequest("child-request", "do not run this child"))

	if _, err := Run(context.Background(), sess, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := RejectChild(context.Background(), sess, "child-request", ""); err != nil {
		t.Fatalf("RejectChild: %v", err)
	}
	decision, found := childDecisionFor(sessionEvents(t, sess), "child-request")
	if !found || decision.Admitted || decision.Plan != nil || decision.Reason != "rejected by operator" {
		t.Fatalf("rejection decision = %#v found=%t", decision, found)
	}

	outcome, err := Resume(context.Background(), sess, deps, "", 0)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if outcome.Status != statusCompleted || len(child.prompts) != 0 || len(childCompletions(sessionEvents(t, sess))) != 0 {
		t.Fatalf("outcome=%#v child=%d completions=%#v", outcome, len(child.prompts), childCompletions(sessionEvents(t, sess)))
	}
}

func TestPendingChildCannotBeDecidedTwice(t *testing.T) {
	parent := dialoguePlan(2)
	parent.ChildPolicy = askChildPolicy(1, 1, 1)
	sess := createSessionIn(t, t.TempDir(), parent)
	alpha := &fakeBackend{name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "parent before approval"}}}
	deps := testDeps(map[string]*fakeBackend{
		"alpha":       alpha,
		"beta":        {name: "codex", slotID: "beta"},
		"child-alpha": {name: "codex", slotID: "child-alpha"},
	})
	deps.Recipes = []plan.Recipe{childRecipe()}
	deps.ChildRequestExtractor = childRequests(childRequest("child-request", "resolve once"))

	if _, err := Run(context.Background(), sess, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := ApproveChild(context.Background(), sess, "child-request", deps.Recipes); err != nil {
		t.Fatalf("ApproveChild: %v", err)
	}
	err := RejectChild(context.Background(), sess, "child-request", "")
	if err == nil || !strings.Contains(err.Error(), "existing admitted decision") {
		t.Fatalf("second decision error = %v", err)
	}
	decisions := childDecisions(sessionEvents(t, sess))
	if len(decisions) != 1 || !decisions[0].Admitted {
		t.Fatalf("child decisions = %#v", decisions)
	}
}

func TestApprovePendingChildRefusesExhaustedBudgets(t *testing.T) {
	cases := []struct {
		name            string
		policy          session.ChildPolicy
		requests        []ChildRequest
		approveFirst    string
		approveRequest  string
		wantReason      string
		wantBudgetState string
	}{
		{
			name:            "depth",
			policy:          askChildPolicy(0, 1, 1),
			requests:        []ChildRequest{childRequest("child-depth", "depth request")},
			approveRequest:  "child-depth",
			wantReason:      "child policy has no remaining depth",
			wantBudgetState: "rejected",
		},
		{
			name:            "turns",
			policy:          askChildPolicy(1, 2, 1),
			requests:        []ChildRequest{childRequest("child-first", "first child"), childRequest("child-turns", "turn budget request")},
			approveFirst:    "child-first",
			approveRequest:  "child-turns",
			wantReason:      "child turn budget exhausted",
			wantBudgetState: "turns_exhausted",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			parent := dialoguePlan(2)
			parent.ChildPolicy = test.policy
			sess := createSessionIn(t, t.TempDir(), parent)
			deps := testDeps(map[string]*fakeBackend{
				"alpha":       {name: "codex", slotID: "alpha", responses: []fakeResponse{{content: "request child"}}},
				"beta":        {name: "codex", slotID: "beta"},
				"child-alpha": {name: "codex", slotID: "child-alpha"},
			})
			deps.Recipes = []plan.Recipe{childRecipe()}
			deps.ChildRequestExtractor = childRequests(test.requests...)

			if _, err := Run(context.Background(), sess, deps); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if test.approveFirst != "" {
				if err := ApproveChild(context.Background(), sess, test.approveFirst, deps.Recipes); err != nil {
					t.Fatalf("approve first child: %v", err)
				}
			}
			if err := ApproveChild(context.Background(), sess, test.approveRequest, deps.Recipes); err != nil {
				t.Fatalf("ApproveChild: %v", err)
			}
			decision, found := childDecisionFor(sessionEvents(t, sess), test.approveRequest)
			if !found || decision.Admitted || decision.Reason != test.wantReason || decision.BudgetState != test.wantBudgetState {
				t.Fatalf("budget decision = %#v found=%t", decision, found)
			}
		})
	}
}

func askChildPolicy(depth, children, turns int) session.ChildPolicy {
	return session.ChildPolicy{
		Mode:           "ask",
		MaxDepth:       depth,
		MaxChildren:    children,
		MaxTurns:       turns,
		AllowedRecipes: []string{"child"},
	}
}

func childRequest(identifier, question string) ChildRequest {
	return ChildRequest{ID: identifier, Request: plan.ChildRequest{RecipeID: "child", Question: question}}
}

func childRequests(requests ...ChildRequest) ChildRequestExtractor {
	return func(actor session.Actor, role eventlog.Role, _ provider.TurnResult) []ChildRequest {
		if actor.ID != "alpha" || role != eventlog.ParticipantRole {
			return nil
		}
		return requests
	}
}

func childDecisionFor(events []eventlog.Event, requestID string) (eventlog.ChildDecidedPayload, bool) {
	for _, decision := range childDecisions(events) {
		if decision.RequestID == requestID {
			return decision, true
		}
	}
	return eventlog.ChildDecidedPayload{}, false
}
