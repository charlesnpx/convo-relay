package recipes

import (
	"fmt"
	"strings"
)

type DepthPolicy struct {
	RelayBackendDepth    int
	MaxRelayBackendDepth int
}

type ChildRecipeIssue struct {
	Category string         `json:"category"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Path     string         `json:"path,omitempty"`
	Detail   map[string]any `json:"detail,omitempty"`
}

type ChildRelayConfigError struct {
	Message string             `json:"message"`
	Issues  []ChildRecipeIssue `json:"issues"`
}

func (e ChildRelayConfigError) Error() string {
	return e.Message
}

func (e ChildRelayConfigError) ToMap() map[string]any {
	issues := make([]any, 0, len(e.Issues))
	for _, issue := range e.Issues {
		payload := map[string]any{
			"category": issue.Category,
			"code":     issue.Code,
			"message":  issue.Message,
		}
		if issue.Path != "" {
			payload["path"] = issue.Path
		}
		if len(issue.Detail) > 0 {
			payload["detail"] = issue.Detail
		}
		issues = append(issues, payload)
	}
	return map[string]any{
		"message": e.Message,
		"issues":  issues,
	}
}

func ExecutableIssues(
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	depthPolicy DepthPolicy,
	compositionPath string,
) []ChildRecipeIssue {
	return executableIssues(recipe, profiles, relayRecipes, depthPolicy, compositionPath, true)
}

func rootExecutableIssues(
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	depthPolicy DepthPolicy,
	compositionPath string,
) []ChildRecipeIssue {
	includeReducer := normalizeResultSource(recipe["result_source"]) == "reducer"
	return executableIssues(recipe, profiles, relayRecipes, depthPolicy, compositionPath, includeReducer)
}

func executableIssues(
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	depthPolicy DepthPolicy,
	compositionPath string,
	includeReducer bool,
) []ChildRecipeIssue {
	issues := []ChildRecipeIssue{}
	recipeID := strings.TrimSpace(stringValue(recipe["id"]))
	if recipeID == "" {
		issues = append(issues, ChildRecipeIssue{
			Category: "invalid_config",
			Code:     "missing_recipe_id",
			Message:  "Child relay recipe is missing id.",
			Path:     "recipe.id",
		})
	}

	participants := stringSlice(recipe["participants"])
	if len(participants) != 2 {
		issues = append(issues, ChildRecipeIssue{
			Category: "invalid_config",
			Code:     "invalid_participants",
			Message:  "Child relay recipe must declare exactly two participants.",
			Path:     "recipe.participants",
		})
		participants = []string{}
	}
	for index, ref := range participants {
		appendProfileIssues(&issues, ref, profiles, fmt.Sprintf("recipe.participants[%d]", index), "participant", relayRecipes, true)
	}

	facilitatorRef := strings.TrimSpace(stringValue(recipe["facilitator"]))
	if facilitatorRef == "" && len(participants) > 0 {
		facilitatorRef = participants[0]
	}
	appendProfileIssues(&issues, facilitatorRef, profiles, "recipe.facilitator", "facilitator", relayRecipes, false)

	if includeReducer {
		reducerRef := strings.TrimSpace(stringValue(recipe["reducer"]))
		if reducerRef == "" {
			reducerRef = facilitatorRef
		}
		appendProfileIssues(&issues, reducerRef, profiles, "recipe.reducer", "reducer", relayRecipes, false)
	}

	if len(issues) == 0 {
		maxDepth := depthPolicy.MaxRelayBackendDepth
		if maxDepth <= 0 {
			maxDepth = intFromAny(recipe["max_depth"], 1)
		}
		appendNestedRelayRecipeIssues(
			&issues,
			recipe,
			profiles,
			relayRecipes,
			depthPolicy.RelayBackendDepth,
			maxDepth,
			"recipe",
			defaultCompositionPath(compositionPath),
			[]string{recipeID},
		)
	}
	return issues
}

func appendProfileIssues(
	issues *[]ChildRecipeIssue,
	ref string,
	profiles map[string]map[string]any,
	path string,
	role string,
	relayRecipes map[string]map[string]any,
	allowRelay bool,
) {
	profileRef := strings.TrimSpace(ref)
	if profileRef == "" {
		*issues = append(*issues, ChildRecipeIssue{
			Category: "invalid_config",
			Code:     "missing_" + role + "_profile",
			Message:  "Child relay recipe is missing a " + role + " profile reference.",
			Path:     path,
		})
		return
	}
	profile, err := ResolveProfileRef(profileRef, profiles)
	if err != nil {
		*issues = append(*issues, ChildRecipeIssue{
			Category: "invalid_config",
			Code:     "unknown_" + role + "_profile",
			Message:  err.Error(),
			Path:     path,
			Detail:   map[string]any{"profile_ref": profileRef},
		})
		return
	}
	if stringValue(profile["backend"]) != "relay" {
		return
	}
	if !allowRelay {
		*issues = append(*issues, ChildRecipeIssue{
			Category: "unsupported_capability",
			Code:     "relay_backend_role_unsupported",
			Message:  "Relay backend profiles are not supported for the " + role + " role.",
			Path:     path,
			Detail:   map[string]any{"profile_ref": profileRef, "role": role},
		})
		return
	}
	appendRelayProfileReferenceIssues(issues, profile, path, profileRef, relayRecipes)
}

func appendRelayProfileReferenceIssues(
	issues *[]ChildRecipeIssue,
	profile map[string]any,
	path string,
	profileRef string,
	relayRecipes map[string]map[string]any,
) {
	childRecipeID := strings.TrimSpace(stringValue(profile["model"]))
	if childRecipeID == "" {
		*issues = append(*issues, ChildRecipeIssue{
			Category: "invalid_config",
			Code:     "missing_relay_profile_recipe",
			Message:  "Relay participant profiles must set model to a child recipe id.",
			Path:     path,
			Detail:   map[string]any{"profile_ref": profileRef},
		})
		return
	}
	if _, ok := relayRecipes[childRecipeID]; !ok {
		*issues = append(*issues, ChildRecipeIssue{
			Category: "invalid_config",
			Code:     "unknown_relay_profile_recipe",
			Message:  fmt.Sprintf("Relay participant profile references unknown child recipe '%s'.", childRecipeID),
			Path:     path,
			Detail:   map[string]any{"profile_ref": profileRef, "child_recipe_id": childRecipeID},
		})
	}
	if profile["effort"] != nil {
		parsedEffort, ok := parseInt(profile["effort"])
		if !ok || parsedEffort < 1 {
			*issues = append(*issues, ChildRecipeIssue{
				Category: "invalid_config",
				Code:     "invalid_relay_profile_effort",
				Message:  "Relay participant profile effort must be a positive child round count.",
				Path:     path,
				Detail:   map[string]any{"profile_ref": profileRef, "effort": profile["effort"]},
			})
		}
	}
}

func appendNestedRelayRecipeIssues(
	issues *[]ChildRecipeIssue,
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	currentDepth int,
	maxDepth int,
	path string,
	compositionPath string,
	stack []string,
) {
	for index, ref := range stringSlice(recipe["participants"]) {
		profile, err := ResolveProfileRef(ref, profiles)
		if err != nil || stringValue(profile["backend"]) != "relay" {
			continue
		}
		childRecipeID := strings.TrimSpace(stringValue(profile["model"]))
		childPath := fmt.Sprintf("%s.participants[%d]", path, index)
		childCompositionPath := fmt.Sprintf("%s.slot_%d", compositionPath, index)
		if containsString(stack, childRecipeID) {
			cycle := strings.Join(append(append([]string{}, stack...), childRecipeID), " -> ")
			*issues = append(*issues, ChildRecipeIssue{
				Category: "invalid_config",
				Code:     "relay_recipe_cycle",
				Message:  fmt.Sprintf("Relay recipe cycle detected: %s.", cycle),
				Path:     childPath,
				Detail: map[string]any{
					"profile_ref":      ref,
					"child_recipe_id":  childRecipeID,
					"composition_path": childCompositionPath,
				},
			})
			continue
		}
		childRecipe, ok := relayRecipes[childRecipeID]
		if !ok {
			continue
		}
		nextDepth := currentDepth + 1
		if nextDepth >= maxDepth {
			*issues = append(*issues, ChildRecipeIssue{
				Category: "runtime_guard",
				Code:     "relay_backend_depth_exceeded",
				Message: fmt.Sprintf(
					"Relay participant at %s would run at depth %d, but max relay backend depth is %d.",
					childCompositionPath,
					nextDepth,
					maxDepth,
				),
				Path: childPath,
				Detail: map[string]any{
					"profile_ref":             ref,
					"child_recipe_id":         childRecipeID,
					"composition_path":        childCompositionPath,
					"relay_backend_depth":     nextDepth,
					"max_relay_backend_depth": maxDepth,
				},
			})
			continue
		}
		appendNestedRelayRecipeIssues(
			issues,
			childRecipe,
			profiles,
			relayRecipes,
			nextDepth,
			maxDepth,
			fmt.Sprintf("%s<%s>", childPath, childRecipeID),
			childCompositionPath,
			append(append([]string{}, stack...), childRecipeID),
		)
	}
}
