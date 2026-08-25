package recipes

import (
	"fmt"
	"strings"
)

// DepthPolicy retains the public compile-option shape while child composition
// is represented directly as child steps rather than a provider backend.
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
	Message string
	Issues  []ChildRecipeIssue
}

func (e ChildRelayConfigError) Error() string { return e.Message }

func (e ChildRelayConfigError) ToMap() map[string]any {
	issues := make([]any, 0, len(e.Issues))
	for _, issue := range e.Issues {
		issues = append(issues, map[string]any{
			"category": issue.Category,
			"code":     issue.Code,
			"message":  issue.Message,
			"path":     issue.Path,
			"detail":   issue.Detail,
		})
	}
	return map[string]any{"message": e.Message, "issues": issues}
}

func ExecutableIssues(
	recipe map[string]any,
	profiles map[string]map[string]any,
	childRecipes map[string]map[string]any,
	depthPolicy DepthPolicy,
	compositionPath string,
) []ChildRecipeIssue {
	return executableIssues(recipe, profiles, childRecipes, depthPolicy, compositionPath, true)
}

func rootExecutableIssues(
	recipe map[string]any,
	profiles map[string]map[string]any,
	childRecipes map[string]map[string]any,
	depthPolicy DepthPolicy,
	compositionPath string,
) []ChildRecipeIssue {
	return executableIssues(recipe, profiles, childRecipes, depthPolicy, compositionPath, true)
}

func executableIssues(
	recipe map[string]any,
	profiles map[string]map[string]any,
	childRecipes map[string]map[string]any,
	depthPolicy DepthPolicy,
	compositionPath string,
	includeReducer bool,
) []ChildRecipeIssue {
	issues := []ChildRecipeIssue{}
	if recipe == nil {
		return []ChildRecipeIssue{{Category: "invalid_config", Code: "missing_recipe", Message: "Recipe is required.", Path: "recipe"}}
	}
	path := defaultCompositionPath(compositionPath)
	stack := []string{strings.TrimSpace(stringValue(recipe["id"]))}
	if stack[0] == "" {
		stack[0] = "<root>"
	}
	validateRecipeComposition(&issues, recipe, profiles, childRecipes, depthPolicy, path, stack, includeReducer)
	return issues
}

func validateRecipeComposition(
	issues *[]ChildRecipeIssue,
	recipe map[string]any,
	profiles map[string]map[string]any,
	childRecipes map[string]map[string]any,
	depthPolicy DepthPolicy,
	compositionPath string,
	stack []string,
	includeReducer bool,
) {
	participants := stringSlice(recipe["participants"])
	if len(participants) != 2 {
		*issues = append(*issues, ChildRecipeIssue{Category: "invalid_config", Code: "invalid_participants", Message: "Recipe must declare exactly two participants.", Path: "recipe.participants"})
		return
	}
	for index, reference := range participants {
		validateReference(issues, reference, profiles, childRecipes, depthPolicy, fmt.Sprintf("recipe.participants[%d]", index), "participant", true, compositionPath, stack)
	}
	facilitator := strings.TrimSpace(stringValue(recipe["facilitator"]))
	if facilitator == "" {
		facilitator = participants[0]
	}
	validateReference(issues, facilitator, profiles, childRecipes, depthPolicy, "recipe.facilitator", "facilitator", false, compositionPath, stack)
	if includeReducer && normalizeResultSource(recipe["result_source"]) == "reducer" {
		reducer := strings.TrimSpace(stringValue(recipe["reducer"]))
		if reducer == "" {
			reducer = facilitator
		}
		validateReference(issues, reducer, profiles, childRecipes, depthPolicy, "recipe.reducer", "reducer", false, compositionPath, stack)
	}
}

func validateReference(
	issues *[]ChildRecipeIssue,
	reference string,
	profiles map[string]map[string]any,
	childRecipes map[string]map[string]any,
	depthPolicy DepthPolicy,
	path string,
	role string,
	allowChild bool,
	compositionPath string,
	stack []string,
) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		*issues = append(*issues, ChildRecipeIssue{Category: "invalid_config", Code: "missing_" + role + "_profile", Message: "Recipe is missing a " + role + " profile reference.", Path: path})
		return
	}
	profile, err := ResolveProfileRef(reference, profiles)
	if err != nil {
		*issues = append(*issues, ChildRecipeIssue{Category: "invalid_config", Code: "unknown_" + role + "_profile", Message: err.Error(), Path: path, Detail: map[string]any{"profile_ref": reference}})
		return
	}
	backend := strings.TrimSpace(stringValue(profile["backend"]))
	if backend != "child" {
		if !backendRegistry[backend] {
			*issues = append(*issues, ChildRecipeIssue{Category: "invalid_config", Code: "unknown_provider_backend", Message: fmt.Sprintf("Profile %q has unknown provider backend %q.", reference, backend), Path: path})
		}
		return
	}
	if !allowChild {
		*issues = append(*issues, ChildRecipeIssue{Category: "invalid_config", Code: "child_step_role_unsupported", Message: "A child step is only valid for a participant role.", Path: path, Detail: map[string]any{"profile_ref": reference, "role": role}})
		return
	}
	childID := strings.TrimSpace(stringValue(profile["model"]))
	if childID == "" {
		*issues = append(*issues, ChildRecipeIssue{Category: "invalid_config", Code: "missing_child_recipe", Message: "Child participant profiles must name a child recipe in model.", Path: path})
		return
	}
	for _, ancestor := range stack {
		if ancestor == childID {
			*issues = append(*issues, ChildRecipeIssue{Category: "invalid_config", Code: "child_recipe_cycle", Message: fmt.Sprintf("Child recipe cycle detected at %q.", childID), Path: path})
			return
		}
	}
	child, found := childRecipes[childID]
	if !found || child == nil {
		*issues = append(*issues, ChildRecipeIssue{Category: "invalid_config", Code: "unknown_child_recipe", Message: fmt.Sprintf("Child participant references unknown recipe %q.", childID), Path: path})
		return
	}
	nextDepth := depthPolicy.RelayBackendDepth + 1
	maxDepth := depthPolicy.MaxRelayBackendDepth
	if maxDepth <= 0 {
		maxDepth = intFromAny(child["max_depth"], 1)
	}
	if nextDepth > maxDepth {
		*issues = append(*issues, ChildRecipeIssue{Category: "runtime_guard", Code: "child_depth_exceeded", Message: fmt.Sprintf("Child participant at %s exceeds the configured child depth.", compositionPath), Path: path})
		return
	}
	nextPolicy := depthPolicy
	nextPolicy.RelayBackendDepth = nextDepth
	validateRecipeComposition(issues, child, profiles, childRecipes, nextPolicy, compositionPath+".child", append(stack, childID), true)
}
