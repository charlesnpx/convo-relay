package readiness

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveBackendClosureIncludesRolesAndTransitiveRelayChildren(t *testing.T) {
	profiles := map[string]map[string]any{
		"root-child":   {"backend": "relay", "model": "child"},
		"nested-child": {"backend": "relay", "model": "grandchild"},
		"root-reducer": {"backend": "gemini"},
	}
	recipes := map[string]map[string]any{
		"child": {
			"id":           "child",
			"participants": []any{"nested-child", "codex"},
			"facilitator":  "codex",
			"reducer":      "root-reducer",
		},
		"grandchild": {
			"id":           "grandchild",
			"participants": []any{"claude", "codex"},
			"facilitator":  "claude",
			"reducer":      "root-reducer",
		},
	}
	root := map[string]any{
		"id":           "root",
		"participants": []any{"root-child", "codex"},
		"facilitator":  "codex",
		"reducer":      "root-reducer",
	}

	withoutReducer, err := ResolveBackendClosure(root, profiles, recipes, ClosureOptions{})
	if err != nil {
		t.Fatalf("closure without reducer: %v", err)
	}
	if want := []string{"claude", "codex", "relay"}; !reflect.DeepEqual(withoutReducer, want) {
		t.Fatalf("closure without reducer = %#v, want %#v", withoutReducer, want)
	}

	withRootReducer, err := ResolveBackendClosure(root, profiles, recipes, ClosureOptions{IncludeReducer: true})
	if err != nil {
		t.Fatalf("closure with root reducer: %v", err)
	}
	if want := []string{"claude", "codex", "gemini", "relay"}; !reflect.DeepEqual(withRootReducer, want) {
		t.Fatalf("closure with root reducer = %#v, want %#v", withRootReducer, want)
	}

	withNestedReducers, err := ResolveBackendClosure(root, profiles, recipes, ClosureOptions{IncludeNestedReducers: true})
	if err != nil {
		t.Fatalf("closure with nested reducers: %v", err)
	}
	if want := []string{"claude", "codex", "gemini", "relay"}; !reflect.DeepEqual(withNestedReducers, want) {
		t.Fatalf("closure with nested reducers = %#v, want %#v", withNestedReducers, want)
	}
}

func TestResolveBackendClosureUsesDirectRelayDefaultAndRejectsCycles(t *testing.T) {
	root := map[string]any{"id": "root", "participants": []any{"relay", "codex"}, "facilitator": "codex"}
	recipes := map[string]map[string]any{
		DefaultRelayRecipeID: {"id": DefaultRelayRecipeID, "participants": []any{"claude", "codex"}, "facilitator": "claude"},
	}
	closure, err := ResolveBackendClosure(root, nil, recipes, ClosureOptions{})
	if err != nil {
		t.Fatalf("default relay closure: %v", err)
	}
	if want := []string{"claude", "codex", "relay"}; !reflect.DeepEqual(closure, want) {
		t.Fatalf("default relay closure = %#v, want %#v", closure, want)
	}

	cycleProfiles := map[string]map[string]any{"self": {"backend": "relay", "model": "loop"}}
	cycleRecipes := map[string]map[string]any{
		"loop": {"id": "loop", "participants": []any{"self", "codex"}, "facilitator": "codex"},
	}
	_, err = ResolveBackendClosure(cycleRecipes["loop"], cycleProfiles, cycleRecipes, ClosureOptions{})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestResolveBackendClosureRejectsUnknownProfilesWithoutNormalizing(t *testing.T) {
	recipe := map[string]any{"id": "raw", "participants": []any{"missing-profile", "codex"}}
	_, err := ResolveBackendClosure(recipe, nil, nil, ClosureOptions{})
	if err == nil || !strings.Contains(err.Error(), "missing-profile") {
		t.Fatalf("unknown profile error = %v", err)
	}
}
