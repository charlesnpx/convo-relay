package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/session"
)

func TestFromFlagsCompilesDialogue(t *testing.T) {
	compiled, err := FromFlags(Flags{
		SessionID:           "flags-dialogue",
		Task:                "review the compiler boundary",
		Agents:              "codex,claude",
		Rounds:              4,
		EffortA:             "high",
		EffortB:             "low",
		FacilitatorBackend:  "gemini",
		TimeoutSeconds:      45,
		StallTimeoutSeconds: 15,
		Dynamic:             "ask",
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
	if got := actor(t, compiled, "slot_0"); got.Backend != "codex" || got.Effort != "high" {
		t.Fatalf("slot_0 = %#v", got)
	}
	if got := actor(t, compiled, "slot_1"); got.Backend != "claude" || got.Effort != "low" {
		t.Fatalf("slot_1 = %#v", got)
	}
	if got := actor(t, compiled, "facilitator"); got.Backend != "gemini" {
		t.Fatalf("facilitator = %#v", got)
	}
	if compiled.Facilitator == nil || compiled.Facilitator.Actor != "facilitator" || compiled.ChildPolicy.Mode != "ask" {
		t.Fatalf("roles or policy = facilitator %#v policy %#v", compiled.Facilitator, compiled.ChildPolicy)
	}
	if compiled.Timeouts != (session.Timeouts{TurnSeconds: 45, StallSeconds: 15}) {
		t.Fatalf("timeouts = %#v", compiled.Timeouts)
	}
	if compiled.ProviderRetry != (session.ProviderRetry{Mode: "allow", MaxAttempts: 7}) {
		t.Fatalf("provider retry = %#v", compiled.ProviderRetry)
	}
	requireValidPlan(t, compiled)
}

func TestFromFlagsMapsModeAndInvestigation(t *testing.T) {
	tests := []struct {
		name          string
		flags         Flags
		mode          string
		investigation string
	}{
		{
			name:          "defaults",
			flags:         Flags{Task: "use the ordinary defaults"},
			mode:          session.ModeAdversarial,
			investigation: session.InvestigationAuto,
		},
		{
			name: "overrides",
			flags: Flags{
				Task:          "use explicit modes",
				Mode:          session.ModeCooperative,
				Investigation: session.InvestigationNormal,
			},
			mode:          session.ModeCooperative,
			investigation: session.InvestigationNormal,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, err := FromFlags(test.flags)
			if err != nil {
				t.Fatalf("FromFlags: %v", err)
			}
			if compiled.Mode != test.mode || compiled.Investigation != test.investigation {
				t.Fatalf("mode/investigation = %q/%q, want %q/%q", compiled.Mode, compiled.Investigation, test.mode, test.investigation)
			}
			requireValidPlan(t, compiled)
		})
	}
}

func TestFromFlagsQuickOverridesRoundsAndAppliesSlotEffort(t *testing.T) {
	compiled, err := FromFlags(Flags{
		SessionID: "flags-quick",
		Task:      "quick review",
		Agents:    "codex",
		Rounds:    19,
		Quick:     true,
		EffortA:   "low",
		EffortB:   "max",
	})
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if compiled.Schedule.Turns != 3 || compiled.Schedule.StopOnConvergence {
		t.Fatalf("quick schedule = %#v", compiled.Schedule)
	}
	if got := actor(t, compiled, "slot_0").Effort; got != "low" {
		t.Fatalf("slot_0 effort = %q, want low", got)
	}
	if got := actor(t, compiled, "slot_1").Effort; got != "max" {
		t.Fatalf("slot_1 effort = %q, want max", got)
	}
	requireValidPlan(t, compiled)
}

func TestFromRecipeCompilesNamedSequenceAndInlineDialogue(t *testing.T) {
	fixed := recipeFixture("fixed")
	fixed.Schedule = session.Schedule{Kind: "sequence", Turns: 3, Order: []string{"alpha", "beta", "alpha"}}
	fixed.Actors = append(fixed.Actors, session.Actor{ID: "reducer", Backend: "codex", Model: "gpt-5.5", Effort: "high"})
	fixed.Reducer = &session.Reducer{Actor: "reducer"}
	fixed.ProviderRetry = session.ProviderRetry{Mode: "forbid"}
	fixed.Mode = session.ModeSteelman
	fixed.Investigation = session.InvestigationContextOnly
	rawCatalog, err := json.Marshal(Catalog{Recipes: []Recipe{fixed}})
	if err != nil {
		t.Fatalf("marshal raw recipe catalog: %v", err)
	}
	alternating := recipeFixture("alternating")
	alternating.Schedule = session.Schedule{Kind: "dialogue", Turns: 4, StopOnConvergence: true}
	alternating.Mode = session.ModeCooperative
	alternating.Investigation = session.InvestigationNormal

	tests := []struct {
		name          string
		input         RecipeInput
		kind          string
		reducer       bool
		mode          string
		investigation string
	}{
		{
			name: "named fixed sequence with reducer",
			input: RecipeInput{
				SessionID: "recipe-fixed",
				Task:      "produce a reduced answer",
				RecipeID:  "fixed",
				Raw:       rawCatalog,
			},
			kind:          "sequence",
			reducer:       true,
			mode:          session.ModeSteelman,
			investigation: session.InvestigationContextOnly,
		},
		{
			name: "inline alternating dialogue",
			input: RecipeInput{
				SessionID: "recipe-dialogue",
				Task:      "discuss the proposal",
				Inline:    &alternating,
			},
			kind:          "dialogue",
			mode:          session.ModeCooperative,
			investigation: session.InvestigationNormal,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, err := FromRecipe(test.input)
			if err != nil {
				t.Fatalf("FromRecipe: %v", err)
			}
			if compiled.Kind != session.PlanKind || compiled.Provenance != session.ProvenanceRecipe || compiled.RecipeID == "" {
				t.Fatalf("recipe identity = %#v", compiled)
			}
			if compiled.Schedule.Kind != test.kind {
				t.Fatalf("schedule kind = %q, want %q", compiled.Schedule.Kind, test.kind)
			}
			if (compiled.Reducer != nil) != test.reducer {
				t.Fatalf("reducer = %#v, want present=%t", compiled.Reducer, test.reducer)
			}
			if compiled.Mode != test.mode || compiled.Investigation != test.investigation {
				t.Fatalf("mode/investigation = %q/%q, want %q/%q", compiled.Mode, compiled.Investigation, test.mode, test.investigation)
			}
			if test.reducer && compiled.ProviderRetry != (session.ProviderRetry{Mode: "forbid", MaxAttempts: 1}) {
				t.Fatalf("sequence retry policy = %#v", compiled.ProviderRetry)
			}
			requireValidPlan(t, compiled)
		})
	}
}

func TestForChildBoundsAndDecrementsPolicy(t *testing.T) {
	parent, err := FromFlags(Flags{
		SessionID:     "parent-plan",
		Task:          "parent question",
		Agents:        "codex,claude",
		Rounds:        5,
		Dynamic:       "auto-safe",
		Mode:          session.ModeSteelman,
		Investigation: session.InvestigationContextOnly,
		ChildPolicy: session.ChildPolicy{
			MaxDepth:       2,
			MaxChildren:    3,
			MaxTurns:       2,
			AllowedRecipes: []string{"child-review"},
		},
	})
	if err != nil {
		t.Fatalf("FromFlags parent: %v", err)
	}
	child, err := ForChild(parent, ChildRequest{
		SessionID: "child-plan",
		RecipeID:  "child-review",
		Question:  "resolve the disputed design",
		Turns:     8,
	})
	if err != nil {
		t.Fatalf("ForChild: %v", err)
	}
	if child.Kind != session.PlanKind || child.Provenance != session.ProvenanceChild || child.RecipeID != "child-review" {
		t.Fatalf("child identity = %#v", child)
	}
	if child.Mode != parent.Mode || child.Investigation != parent.Investigation {
		t.Fatalf("child mode/investigation = %q/%q, parent = %q/%q", child.Mode, child.Investigation, parent.Mode, parent.Investigation)
	}
	if child.Schedule.Turns != 2 || child.Schedule.Turns > parent.ChildPolicy.MaxTurns {
		t.Fatalf("child turns = %d, parent max = %d", child.Schedule.Turns, parent.ChildPolicy.MaxTurns)
	}
	if child.ChildPolicy.MaxDepth != 1 || child.ChildPolicy.MaxChildren != 2 || child.ChildPolicy.MaxTurns != 1 {
		t.Fatalf("child policy = %#v", child.ChildPolicy)
	}
	requireValidPlan(t, child)
}

func TestCompilerRejectsInvalidInputsWithoutAPlan(t *testing.T) {
	tests := []struct {
		name string
		run  func() (session.Plan, error)
		want string
	}{
		{
			name: "unknown backend",
			run: func() (session.Plan, error) {
				return FromFlags(Flags{Task: "invalid backend", Agents: "unknown,codex"})
			},
			want: "unknown backend",
		},
		{
			name: "dialogue actor count does not match schedule",
			run: func() (session.Plan, error) {
				recipe := recipeFixture("bad-count")
				recipe.Participants = []string{"alpha"}
				return FromRecipe(RecipeInput{Task: "bad count", Inline: &recipe})
			},
			want: "exactly two participants",
		},
		{
			name: "facilitator names a nonexistent actor",
			run: func() (session.Plan, error) {
				recipe := recipeFixture("bad-facilitator")
				recipe.Facilitator = &session.Facilitator{Actor: "missing", Cadence: 1}
				return FromRecipe(RecipeInput{Task: "bad facilitator", Inline: &recipe})
			},
			want: "facilitator actor is not in actors",
		},
		{
			name: "reducer names a nonexistent actor",
			run: func() (session.Plan, error) {
				recipe := recipeFixture("bad-reducer")
				recipe.Reducer = &session.Reducer{Actor: "missing"}
				return FromRecipe(RecipeInput{Task: "bad reducer", Inline: &recipe})
			},
			want: "reducer actor is not in actors",
		},
		{
			name: "fixed order names an unknown actor",
			run: func() (session.Plan, error) {
				recipe := recipeFixture("bad-order")
				recipe.Schedule = session.Schedule{Kind: "sequence", Turns: 2, Order: []string{"alpha", "missing"}}
				return FromRecipe(RecipeInput{Task: "bad order", Inline: &recipe})
			},
			want: "schedule.order names unknown actor",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, err := test.run()
			if err == nil {
				t.Fatal("compiler accepted invalid input")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want fragment %q", err, test.want)
			}
			if compiled.Kind != "" || compiled.SchemaVersion != 0 || len(compiled.Actors) != 0 {
				t.Fatalf("invalid input returned a partial plan: %#v", compiled)
			}
		})
	}
}

func recipeFixture(id string) Recipe {
	return Recipe{
		ID: id,
		Actors: []session.Actor{
			{ID: "alpha", Backend: "codex", Model: "gpt-5.5", Effort: "high"},
			{ID: "beta", Backend: "claude", Model: "sonnet", Effort: "medium"},
			{ID: "facilitator", Backend: "codex", Model: "gpt-5.5", Effort: "medium"},
		},
		Participants: []string{"alpha", "beta"},
		Schedule:     session.Schedule{Kind: "dialogue", Turns: 2},
		Facilitator:  &session.Facilitator{Actor: "facilitator", Cadence: 1},
		ChildPolicy: session.ChildPolicy{
			Mode:           "deny",
			MaxDepth:       1,
			MaxChildren:    2,
			MaxTurns:       2,
			AllowedRecipes: []string{},
		},
	}
}

func actor(t *testing.T, compiled session.Plan, id string) session.Actor {
	t.Helper()
	value, found := findActor(compiled.Actors, id)
	if !found {
		t.Fatalf("actor %q not found in %#v", id, compiled.Actors)
	}
	return value
}

func requireValidPlan(t *testing.T, compiled session.Plan) {
	t.Helper()
	if err := session.ValidatePlan(compiled); err != nil {
		t.Fatalf("session.ValidatePlan: %v", err)
	}
}
