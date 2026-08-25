package readiness

import (
	"fmt"
	"sort"
	"strings"
)

type ClosureOptions struct {
	IncludeReducer        bool
	IncludeNestedReducers bool
}

// ResolveBackendClosure returns the concrete provider backends required by a
// recipe. A child participant is composition, not a provider: its recipe is
// traversed but child itself is never registered as a backend.
func ResolveBackendClosure(
	recipe map[string]any,
	profiles map[string]map[string]any,
	childRecipes map[string]map[string]any,
	options ClosureOptions,
) ([]string, error) {
	if recipe == nil {
		return nil, fmt.Errorf("recipe is required")
	}
	resolver := closureResolver{
		profiles: profiles, childRecipes: childRecipes, options: options,
		backends: map[string]bool{}, active: map[string]bool{},
	}
	rootID := strings.TrimSpace(valueString(recipe["id"]))
	if rootID == "" {
		rootID = "<root>"
	}
	if err := resolver.walkRecipe(rootID, recipe, options.IncludeReducer); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(resolver.backends))
	for backend := range resolver.backends {
		result = append(result, backend)
	}
	sort.Strings(result)
	return result, nil
}

type closureResolver struct {
	profiles     map[string]map[string]any
	childRecipes map[string]map[string]any
	options      ClosureOptions
	backends     map[string]bool
	active       map[string]bool
}

func (r *closureResolver) walkRecipe(recipeID string, recipe map[string]any, includeReducer bool) error {
	if r.active[recipeID] {
		return fmt.Errorf("recipe cycle includes %q", recipeID)
	}
	r.active[recipeID] = true
	defer delete(r.active, recipeID)

	participants, ok := stringValues(recipe["participants"])
	if !ok || len(participants) == 0 {
		return fmt.Errorf("recipe %q participants must be a list of backend or profile references", recipeID)
	}
	for index, ref := range participants {
		if err := r.walkReference(ref, fmt.Sprintf("recipe %q participant %d", recipeID, index)); err != nil {
			return err
		}
	}
	facilitator := strings.TrimSpace(valueString(recipe["facilitator"]))
	if facilitator == "" {
		facilitator = participants[0]
	}
	if err := r.walkReference(facilitator, fmt.Sprintf("recipe %q facilitator", recipeID)); err != nil {
		return err
	}
	if includeReducer {
		reducer := strings.TrimSpace(valueString(recipe["reducer"]))
		if reducer == "" {
			reducer = facilitator
		}
		if err := r.walkReference(reducer, fmt.Sprintf("recipe %q reducer", recipeID)); err != nil {
			return err
		}
	}
	return nil
}

func (r *closureResolver) walkReference(reference string, path string) error {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return fmt.Errorf("%s is empty", path)
	}
	backend := reference
	childRecipeID := ""
	if profile, exists := r.profiles[reference]; exists {
		backend = strings.TrimSpace(valueString(profile["backend"]))
		childRecipeID = strings.TrimSpace(valueString(profile["model"]))
	}
	if backend == "child" {
		if childRecipeID == "" {
			return fmt.Errorf("%s child participant has no child recipe", path)
		}
		child, exists := r.childRecipes[childRecipeID]
		if !exists || child == nil {
			return fmt.Errorf("%s references unknown child recipe %q", path, childRecipeID)
		}
		return r.walkRecipe(childRecipeID, child, r.options.IncludeNestedReducers)
	}
	if !isRegistered(backend) {
		return fmt.Errorf("%s references unknown backend or profile %q", path, reference)
	}
	r.backends[backend] = true
	return nil
}

func stringValues(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok {
		if typed, typedOK := value.([]string); typedOK {
			return append([]string(nil), typed...), true
		}
		return nil, false
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, false
		}
		result = append(result, strings.TrimSpace(text))
	}
	return result, true
}

func valueString(value any) string {
	text, _ := value.(string)
	return text
}
