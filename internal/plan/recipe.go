package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/session"
)

// FromRecipe compiles either a catalog-named or inline typed recipe into the
// same target document as FromFlags.
func FromRecipe(input RecipeInput) (session.Plan, error) {
	recipe, err := selectRecipe(input)
	if err != nil {
		return session.Plan{}, err
	}
	schedule := copySchedule(recipe.Schedule)
	if strings.TrimSpace(schedule.Kind) == "" {
		schedule.Kind = "dialogue"
	}
	if schedule.Turns == 0 {
		schedule.Turns = 1
	}
	return compile(planSpec{
		sessionID:     input.SessionID,
		provenance:    session.ProvenanceRecipe,
		recipeID:      recipe.ID,
		task:          input.Task,
		timeouts:      input.Timeouts,
		mode:          recipe.Mode,
		investigation: recipe.Investigation,
		actors:        recipe.Actors,
		participants:  append([]string{}, recipe.Participants...),
		schedule:      schedule,
		facilitator:   recipe.Facilitator,
		reducer:       recipe.Reducer,
		retry:         recipe.ProviderRetry,
		workspace:     recipe.Workspace,
		inputs:        recipe.Inputs,
		childPolicy:   recipe.ChildPolicy,
		result:        recipe.Result,
	})
}

func selectRecipe(input RecipeInput) (Recipe, error) {
	requestedID := strings.TrimSpace(input.RecipeID)
	if input.Inline != nil {
		if len(input.Catalog.Recipes) != 0 || len(input.Raw) != 0 {
			return Recipe{}, fmt.Errorf("inline recipe cannot be combined with a catalog or raw recipe document")
		}
		recipe := copyRecipe(*input.Inline)
		if strings.TrimSpace(recipe.ID) == "" {
			return Recipe{}, fmt.Errorf("inline recipe id is required")
		}
		if requestedID != "" && requestedID != recipe.ID {
			return Recipe{}, fmt.Errorf("requested recipe %q does not match inline recipe %q", requestedID, recipe.ID)
		}
		return recipe, nil
	}
	if len(input.Raw) != 0 {
		if len(input.Catalog.Recipes) != 0 {
			return Recipe{}, fmt.Errorf("catalog and raw recipe document cannot both be supplied")
		}
		recipes, err := decodeRawRecipes(input.Raw)
		if err != nil {
			return Recipe{}, err
		}
		if requestedID == "" && len(recipes) == 1 {
			return copyRecipe(recipes[0]), nil
		}
		return selectNamedRecipe(requestedID, recipes)
	}
	return selectNamedRecipe(requestedID, input.Catalog.Recipes)
}

func selectNamedRecipe(requestedID string, recipes []Recipe) (Recipe, error) {
	if requestedID == "" {
		return Recipe{}, fmt.Errorf("recipe id is required when no inline recipe is supplied")
	}
	for _, recipe := range recipes {
		if recipe.ID == requestedID {
			return copyRecipe(recipe), nil
		}
	}
	return Recipe{}, fmt.Errorf("recipe %q was not found in the catalog", requestedID)
}

func decodeRawRecipes(raw json.RawMessage) ([]Recipe, error) {
	var envelope struct {
		Recipes json.RawMessage `json:"recipes"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode raw recipe document: %w", err)
	}
	if len(envelope.Recipes) != 0 {
		var catalog Catalog
		if err := decodeStrictJSON(raw, &catalog); err != nil {
			return nil, fmt.Errorf("decode raw recipe catalog: %w", err)
		}
		if len(catalog.Recipes) == 0 {
			return nil, fmt.Errorf("raw recipe catalog contains no recipes")
		}
		return catalog.Recipes, nil
	}
	var recipe Recipe
	if err := decodeStrictJSON(raw, &recipe); err != nil {
		return nil, fmt.Errorf("decode raw inline recipe: %w", err)
	}
	if strings.TrimSpace(recipe.ID) == "" {
		return nil, fmt.Errorf("raw inline recipe id is required")
	}
	return []Recipe{recipe}, nil
}

func decodeStrictJSON(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func copyRecipe(value Recipe) Recipe {
	value.Actors = copyActors(value.Actors)
	value.Participants = append([]string{}, value.Participants...)
	if value.Participants == nil {
		value.Participants = []string{}
	}
	value.Schedule = copySchedule(value.Schedule)
	value.Facilitator = copyFacilitator(value.Facilitator)
	value.Reducer = copyReducer(value.Reducer)
	value.Inputs = copyInputs(value.Inputs)
	value.ChildPolicy = normalizeChildPolicy(value.ChildPolicy)
	value.Result = normalizeResult(value.Result)
	return value
}
