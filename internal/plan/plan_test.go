package plan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/session"
)

func TestFromFlagsCarriesPromptInputsAndProviderDefaults(t *testing.T) {
	context := input("context", "a")
	skill := input("skill", "b")
	compiled, err := FromFlags(Flags{
		SessionID:           "flags-dialogue",
		Task:                "review the compiler boundary",
		Agents:              "codex,claude",
		Rounds:              4,
		TimeoutSeconds:      45,
		StallTimeoutSeconds: 15,
		Dynamic:             "ask",
		Context:             []session.Input{context},
		Skills:              []session.Input{skill},
		TaskPlan:            json.RawMessage(`{"steps":["inspect"]}`),
	})
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if compiled.Kind != session.PlanKind || compiled.Provenance != session.ProvenanceOrdinary {
		t.Fatalf("plan identity = %#v", compiled)
	}
	if compiled.Schedule.Kind != "dialogue" || compiled.Schedule.Turns != 4 || compiled.Schedule.StopOnConvergence {
		t.Fatalf("schedule = %#v", compiled.Schedule)
	}
	if got := actor(t, compiled, "slot_0"); got.Backend != "codex" || got.Model != "" || got.Effort != "" {
		t.Fatalf("slot_0 provider-default projection = %#v", got)
	}
	if got := actor(t, compiled, "slot_1"); got.Backend != "claude" || got.Model != "" || got.Effort != "" {
		t.Fatalf("slot_1 provider-default projection = %#v", got)
	}
	if compiled.ChildPolicy.Mode != "ask" || len(compiled.Context) != 1 || len(compiled.Skills) != 1 || string(compiled.TaskPlan) != `{"steps":["inspect"]}` {
		t.Fatalf("prompt inputs or policy = %#v", compiled)
	}
	if compiled.ProviderRetry != (session.ProviderRetry{Mode: "allow", MaxAttempts: 7}) {
		t.Fatalf("provider retry = %#v", compiled.ProviderRetry)
	}
}

func TestFromRecipeCompilesCanonicalNormalizedProjection(t *testing.T) {
	recipe := canonicalRecipe("normalized-v2")
	compiled, err := FromRecipe(RecipeInput{
		SessionID: "recipe-v2",
		Task:      "produce a reduced answer",
		RecipeID:  recipe.ID,
		Recipes:   []Recipe{recipe},
	})
	if err != nil {
		t.Fatalf("FromRecipe canonical normalized recipe: %v", err)
	}
	if compiled.Schedule.Turns != 3 || compiled.Result.Source != "reducer" || compiled.Reducer == nil {
		t.Fatalf("schedule/result projection = %#v/%#v", compiled.Schedule, compiled.Result)
	}
	if compiled.IntegrationContract != "example/review-v2" || compiled.Lifecycle == nil || compiled.Lifecycle.Resume != "forbid" || len(compiled.MatchKeywords) != 1 {
		t.Fatalf("contract/lifecycle projection = %#v", compiled)
	}
	if compiled.Workspace.Mode != "head-copy" || compiled.Workspace.Isolation != "ephemeral" || compiled.ChildPolicy.Mode != "allow" {
		t.Fatalf("lifecycle execution projection = workspace %#v child %#v", compiled.Workspace, compiled.ChildPolicy)
	}
	if compiled.ProviderRetry != (session.ProviderRetry{Mode: "forbid", MaxAttempts: 1}) {
		t.Fatalf("recipe retry = %#v", compiled.ProviderRetry)
	}
}

func TestForChildUsesRequestedRecipeAndParentBudget(t *testing.T) {
	parent, err := FromFlags(Flags{
		SessionID: "parent-plan",
		Task:      "parent question",
		Agents:    "codex,claude",
		Rounds:    5,
		Dynamic:   "auto-safe",
		ChildPolicy: session.ChildPolicy{
			MaxDepth:       2,
			MaxChildren:    3,
			MaxTurns:       2,
			AllowedRecipes: []string{},
		},
	})
	if err != nil {
		t.Fatalf("FromFlags parent: %v", err)
	}
	childRecipe := canonicalRecipe("child-review")
	childRecipe.Mode = session.ModeCooperative
	childRecipe.ParticipantTurns = 4
	childRecipe.Actors[0].ID = "child-alpha"
	childRecipe.Actors[1].ID = "child-beta"
	childRecipe.Actors[2].ID = "child-facilitator"
	childRecipe.Actors[3].ID = "child-reducer"
	childRecipe.Facilitator = &session.Facilitator{Actor: "child-facilitator", Cadence: 1}
	childRecipe.Reducer = &session.Reducer{Actor: "child-reducer"}
	child, err := ForChild(parent, ChildRequest{
		SessionID: "child-plan",
		RecipeID:  childRecipe.ID,
		Question:  "resolve the disputed design",
		Turns:     8,
	}, []Recipe{childRecipe})
	if err != nil {
		t.Fatalf("ForChild: %v", err)
	}
	if child.Provenance != session.ProvenanceChild || child.RecipeID != childRecipe.ID || child.Mode != session.ModeCooperative {
		t.Fatalf("child identity/behaviour = %#v", child)
	}
	actor(t, child, "child-alpha")
	if child.Schedule.Turns != 2 || child.ChildPolicy.MaxDepth != 1 || child.ChildPolicy.MaxChildren != 2 || child.ChildPolicy.MaxTurns != 1 {
		t.Fatalf("child bounds = schedule %#v policy %#v", child.Schedule, child.ChildPolicy)
	}
	if _, err := ForChild(parent, ChildRequest{RecipeID: "missing", Question: "no fallback"}, []Recipe{childRecipe}); err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("unresolvable child recipe error = %v", err)
	}
}

func TestForChildPreservesRecipeInputsAndInvestigation(t *testing.T) {
	recipe := canonicalRecipe("child-owned-inputs")
	recipe.Investigation = ""
	recipe.Inputs = []session.Input{input("recipe-input", "a")}
	launchContext := input("launch-context", "b")
	launchSkill := input("launch-skill", "c")
	taskPlan := json.RawMessage(`{"steps":["inspect"]}`)
	root, err := FromRecipe(RecipeInput{
		SessionID: "recipe-root",
		Task:      "compile the recipe directly",
		Inline:    &recipe,
		Timeouts:  session.Timeouts{TurnSeconds: 30, StallSeconds: 15},
		Context:   []session.Input{launchContext},
		Skills:    []session.Input{launchSkill},
		TaskPlan:  taskPlan,
	})
	if err != nil {
		t.Fatalf("FromRecipe root: %v", err)
	}
	parent, err := FromFlags(Flags{
		SessionID:     "input-parent",
		Task:          "delegate to the recipe",
		Agents:        "codex,claude",
		Investigation: session.InvestigationContextOnly,
		Inputs:        []session.Input{input("parent-input", "d")},
		Context:       []session.Input{launchContext},
		Skills:        []session.Input{launchSkill},
		TaskPlan:      taskPlan,
		Dynamic:       "auto-safe",
		ChildPolicy: session.ChildPolicy{
			MaxDepth:       2,
			MaxChildren:    2,
			MaxTurns:       3,
			AllowedRecipes: []string{},
		},
	})
	if err != nil {
		t.Fatalf("FromFlags parent: %v", err)
	}
	child, err := ForChild(parent, ChildRequest{
		SessionID: "recipe-child",
		RecipeID:  recipe.ID,
		Question:  "compile the same recipe as a child",
	}, []Recipe{recipe})
	if err != nil {
		t.Fatalf("ForChild: %v", err)
	}
	if !reflect.DeepEqual(child.Inputs, root.Inputs) {
		t.Fatalf("child inputs = %#v, root recipe inputs = %#v", child.Inputs, root.Inputs)
	}
	if child.Investigation != root.Investigation || child.Investigation != session.InvestigationAuto {
		t.Fatalf("child investigation = %q, root/default = %q/%q", child.Investigation, root.Investigation, session.InvestigationAuto)
	}
}

func TestForResumeAcceptsOnlyPromptInputs(t *testing.T) {
	parent, err := FromFlags(Flags{Task: "resume parent"})
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	resume, err := ForResume(parent, ResumeInput{
		Prompt:  "focus on the counterexample",
		Context: []session.Input{input("resume-context", "c")},
		Skills:  []session.Input{input("resume-skill", "d")},
	})
	if err != nil {
		t.Fatalf("ForResume: %v", err)
	}
	if resume.Prompt != "focus on the counterexample" || len(resume.Context) != 1 || len(resume.Skills) != 1 {
		t.Fatalf("resume input = %#v", resume)
	}
}

func TestExplicitRetryAttemptBudgetIsPreserved(t *testing.T) {
	recipe := canonicalRecipe("explicit-retry")
	recipe.ProviderRetry = session.ProviderRetry{Mode: "allow", MaxAttempts: 2}
	compiled, err := FromRecipe(RecipeInput{Task: "keep explicit retry budget", Inline: &recipe})
	if err != nil {
		t.Fatalf("FromRecipe: %v", err)
	}
	if compiled.ProviderRetry.MaxAttempts != 2 {
		t.Fatalf("explicit max attempts = %d, want 2", compiled.ProviderRetry.MaxAttempts)
	}
}

func canonicalRecipe(id string) Recipe {
	canonical := recipes.NormalizeRelayRecipes(map[string]any{
		id: map[string]any{
			"kind":                  "recipe",
			"schema_version":        2,
			"purpose":               "review canonical recipe behaviour",
			"participants":          []any{"codex", "claude"},
			"facilitator":           "gemini",
			"reducer":               "codex",
			"mode":                  "steelman",
			"max_rounds":            5,
			"participant_turns":     3,
			"result_source":         "reducer",
			"provider_retry":        "forbid",
			"integration_contract":  "example/review-v2",
			"max_depth":             3,
			"required_capabilities": []any{"filesystem"},
			"auto_approval":         "auto-safe",
			"match_keywords":        []any{"review"},
			"lifecycle": map[string]any{
				"resume":              "forbid",
				"steering":            "forbid",
				"dynamic":             "allow",
				"workspace_isolation": "ephemeral",
			},
		},
	})[id]
	lifecycle := canonical["lifecycle"].(map[string]any)
	return Recipe{
		Kind:          canonical["kind"].(string),
		SchemaVersion: canonical["schema_version"].(int),
		ID:            id,
		Purpose:       canonical["purpose"].(string),
		Actors: []session.Actor{
			{ID: "alpha", Backend: "codex", Model: "gpt-5.5", Effort: "high"},
			{ID: "beta", Backend: "claude", Model: "sonnet", Effort: "medium"},
			{ID: "facilitator", Backend: "gemini", Model: "gemini-2.5-pro", Effort: "medium"},
			{ID: "reducer", Backend: "codex", Model: "gpt-5.5", Effort: "medium"},
		},
		Schedule:             session.Schedule{Kind: "dialogue"},
		Mode:                 canonical["mode"].(string),
		Facilitator:          &session.Facilitator{Actor: "facilitator", Cadence: 1},
		Reducer:              &session.Reducer{Actor: "reducer"},
		MaxRounds:            canonical["max_rounds"].(int),
		ParticipantTurns:     canonical["participant_turns"].(int),
		ResultSource:         canonical["result_source"].(string),
		ProviderRetry:        session.ProviderRetry{Mode: canonical["provider_retry"].(string)},
		IntegrationContract:  canonical["integration_contract"].(string),
		MaxDepth:             canonical["max_depth"].(int),
		RequiredCapabilities: []string{canonical["required_capabilities"].([]any)[0].(string)},
		AutoApproval:         canonical["auto_approval"].(string),
		MatchKeywords:        []string{canonical["match_keywords"].([]any)[0].(string)},
		Lifecycle: session.Lifecycle{
			Resume:             lifecycle["resume"].(string),
			Steering:           lifecycle["steering"].(string),
			Dynamic:            lifecycle["dynamic"].(string),
			WorkspaceIsolation: lifecycle["workspace_isolation"].(string),
		},
		Result: session.Result{Format: "text"},
	}
}

func input(name string, suffix string) session.Input {
	return session.Input{
		Name: name,
		Content: blobstore.BlobRef{
			SHA256:    strings.Repeat(suffix, 64),
			Size:      1,
			MediaType: "text/plain",
		},
	}
}

func actor(t *testing.T, compiled session.Plan, id string) session.Actor {
	t.Helper()
	for _, value := range compiled.Actors {
		if value.ID == id {
			return value
		}
	}
	t.Fatalf("actor %q not found in %#v", id, compiled.Actors)
	return session.Actor{}
}
