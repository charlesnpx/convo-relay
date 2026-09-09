package plan

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/session"
)

// FromRecipe compiles a typed canonical normalized recipe into the same target
// document as FromFlags. It deliberately accepts no raw configuration data.
func FromRecipe(input RecipeInput) (session.Plan, error) {
	recipe, err := selectRecipe(input)
	if err != nil {
		return session.Plan{}, err
	}
	if err := validateRecipeProjection(recipe); err != nil {
		return session.Plan{}, err
	}
	return compile(planFromRecipe(input, recipe, session.ProvenanceRecipe))
}

func selectRecipe(input RecipeInput) (Recipe, error) {
	requestedID := strings.TrimSpace(input.RecipeID)
	if input.Inline != nil {
		if len(input.Recipes) != 0 {
			return Recipe{}, fmt.Errorf("inline recipe cannot be combined with named recipes")
		}
		if strings.TrimSpace(input.Inline.ID) == "" {
			return Recipe{}, fmt.Errorf("inline recipe id is required")
		}
		if requestedID != "" && requestedID != input.Inline.ID {
			return Recipe{}, fmt.Errorf("requested recipe %q does not match inline recipe %q", requestedID, input.Inline.ID)
		}
		return *input.Inline, nil
	}
	return selectNamedRecipe(requestedID, input.Recipes)
}

func selectNamedRecipe(requestedID string, recipes []Recipe) (Recipe, error) {
	if requestedID == "" {
		return Recipe{}, fmt.Errorf("recipe id is required when no inline recipe is supplied")
	}
	for _, recipe := range recipes {
		if recipe.ID == requestedID {
			return recipe, nil
		}
	}
	return Recipe{}, fmt.Errorf("recipe %q was not found in the supplied recipes", requestedID)
}

func validateRecipeProjection(recipe Recipe) error {
	if strings.TrimSpace(recipe.ID) == "" {
		return fmt.Errorf("recipe id is required")
	}
	return nil
}

func planFromRecipe(input RecipeInput, recipe Recipe, provenance string) session.Plan {
	schedule := recipe.Schedule
	if strings.TrimSpace(schedule.Kind) == "" {
		schedule.Kind = "dialogue"
	}
	if recipe.ParticipantTurns != 0 {
		schedule.Turns = recipe.ParticipantTurns
	} else if schedule.Turns == 0 && recipe.MaxRounds != 0 {
		schedule.Turns = recipe.MaxRounds
	}
	if schedule.Turns == 0 {
		schedule.Turns = 1
	}

	result := recipe.Result
	if strings.TrimSpace(recipe.ResultSource) != "" {
		result.Source = strings.TrimSpace(recipe.ResultSource)
	}
	policy := recipeChildPolicy(recipe)
	workspace := recipe.Workspace
	lifecycle := recipeLifecycle(recipe.Lifecycle)

	return session.Plan{
		Provenance:    provenance,
		SessionID:     input.SessionID,
		RecipeID:      recipe.ID,
		Task:          input.Task,
		Timeouts:      input.Timeouts,
		Mode:          recipe.Mode,
		Investigation: recipe.Investigation,
		Actors:        recipe.Actors,
		Schedule:      schedule,
		Facilitator:   recipe.Facilitator,
		Reducer:       recipe.Reducer,
		ProviderRetry: recipe.ProviderRetry,
		Workspace:     workspace,
		Inputs:        recipe.Inputs,
		Context:       input.Context,
		Skills:        input.Skills,
		TaskPlan:      input.TaskPlan,
		ChildPolicy:   policy,
		Result:        result,
		Lifecycle:     lifecycle,
	}
}

func recipeChildPolicy(recipe Recipe) session.ChildPolicy {
	policy := recipe.ChildPolicy
	unconfigured := strings.TrimSpace(policy.Mode) == "" && policy.MaxDepth == 0 && policy.MaxChildren == 0 && policy.MaxTurns == 0 && len(policy.AllowedRecipes) == 0
	if unconfigured {
		policy = normalizeChildPolicy(policy)
		policy.Mode = childPolicyMode(recipe.AutoApproval)
	}
	if strings.TrimSpace(policy.Mode) == "" {
		policy.Mode = childPolicyMode(recipe.AutoApproval)
	}
	if recipe.MaxDepth != 0 {
		policy.MaxDepth = recipe.MaxDepth
	}
	if recipe.Lifecycle.Dynamic == "forbid" {
		policy.Mode = "deny"
	}
	return policy
}

func childPolicyMode(autoApproval string) string {
	switch strings.TrimSpace(autoApproval) {
	case "", "never":
		return "deny"
	case "ask":
		return "ask"
	case "auto-safe":
		return "allow"
	default:
		return strings.TrimSpace(autoApproval)
	}
}

func recipeLifecycle(value session.Lifecycle) *session.Lifecycle {
	if value.Resume == "" && value.Steering == "" && value.Dynamic == "" {
		return nil
	}
	copy := value
	return &copy
}
