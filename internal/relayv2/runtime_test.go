package relayv2

import (
	"testing"

	"github.com/charlesnpx/convo-relay/v2/internal/eventlog"
	"github.com/charlesnpx/convo-relay/v2/internal/plan"
	"github.com/charlesnpx/convo-relay/v2/internal/provider"
	"github.com/charlesnpx/convo-relay/v2/internal/session"
)

func TestNewDepsSuppliesChildRequestExtractor(t *testing.T) {
	deps := NewDeps(Runtime{Recipes: []plan.Recipe{{ID: "child"}}}, ".")
	if deps.ChildRequestExtractor == nil {
		t.Fatal("NewDeps left ChildRequestExtractor nil")
	}
}

func TestChildRequestExtractorSelectionOrder(t *testing.T) {
	catalog := []plan.Recipe{{ID: "zeta"}, {ID: "review-panel"}, {ID: "alpha"}, {ID: "child-beta"}}
	extract := NewChildRequestExtractor(catalog)
	ledger := provider.TurnResult{Content: `{"settled":[],"contested":["Need focused review"],"withdrawn":[]}`}

	tests := []struct {
		name   string
		policy session.ChildPolicy
		want   string
	}{
		{
			name:   "first present allowed recipe",
			policy: session.ChildPolicy{AllowedRecipes: []string{"missing", "child-beta", "alpha"}},
			want:   "child-beta",
		},
		{
			name: "review panel convention",
			want: "review-panel",
		},
		{
			name: "sorted catalog fallback",
			want: "alpha",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selectedCatalog := catalog
			if test.name == "sorted catalog fallback" {
				selectedCatalog = []plan.Recipe{{ID: "zeta"}, {ID: "alpha"}}
				extract = NewChildRequestExtractor(selectedCatalog)
			}
			requests := extract(session.Plan{ChildPolicy: test.policy}, session.Actor{}, eventlog.FacilitatorRole, ledger)
			if len(requests) != 1 || requests[0].Request.RecipeID != test.want {
				t.Fatalf("selection = %#v, want %q", requests, test.want)
			}
		})
	}
}

func TestActorFromProfileMapsStaticChildEffort(t *testing.T) {
	for _, test := range []struct {
		name   string
		effort any
		want   int
	}{
		{name: "numeric string", effort: "2", want: 2},
		{name: "TOML integer", effort: int64(3), want: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			actor, err := actorFromProfile(
				"slot_1",
				"nested",
				map[string]map[string]any{
					"nested": {
						"id": "nested", "backend": "child", "model": "child-recipe", "effort": test.effort,
					},
				},
				map[string]map[string]any{"child-recipe": {"max_rounds": int64(5)}},
			)
			if err != nil {
				t.Fatalf("map child profile: %v", err)
			}
			if actor.Backend != "child" || actor.ChildRecipeID != "child-recipe" || actor.ChildTurns != test.want || actor.Model != "" || actor.Effort != "" {
				t.Fatalf("static child actor = %#v", actor)
			}
		})
	}
}
