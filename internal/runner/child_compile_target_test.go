package runner

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestNestedAndDynamicChildRoutesRejectIntegrationBoundRecipes(t *testing.T) {
	recipe := map[string]any{
		"id":                   "consumer-owned-arbitrary-root-recipe",
		"participants":         []any{"codex", "codex"},
		"facilitator":          "codex",
		"reducer":              "codex",
		"max_rounds":           1,
		"participant_turns":    2,
		"result_source":        "reducer",
		"integration_contract": "consumer/opaque-contract-v1",
		"max_depth":            2,
	}
	profiles := map[string]map[string]any{}
	relayRecipes := map[string]map[string]any{stringFromAny(recipe["id"]): recipe}
	childSpec := childRelaySpec{
		Recipe:   recipe,
		Profiles: profiles,
		Recipes:  relayRecipes,
		DepthPolicy: map[string]any{
			"relay_backend_depth":     0,
			"max_relay_backend_depth": 2,
		},
	}
	routes := map[string]func() error{
		"nested relay execution": func() error {
			_, err := compileChildRelayPlan(childSpec, "root.slot_0")
			return err
		},
		"relay-backed profile": func() error {
			_, err := compileChildRelayPlan(childSpec, "root.slot_1")
			return err
		},
		"proposal child": func() error {
			_, err := compileDynamicChildPlan(recipe, profiles, relayRecipes, "root.proposal", 2)
			return err
		},
		"automatic dynamic child": func() error {
			_, err := compileDynamicChildPlan(recipe, profiles, relayRecipes, "root.dynamic", 2)
			return err
		},
	}
	for name, invoke := range routes {
		t.Run(name, func(t *testing.T) {
			err := invoke()
			var rootOnly *recipes.RootOnlyRecipeError
			if !errors.As(err, &rootOnly) {
				t.Fatalf("error = %T %[1]v, want *recipes.RootOnlyRecipeError", err)
			}
			if rootOnly.RecipeID != "consumer-owned-arbitrary-root-recipe" || rootOnly.IntegrationContract != "consumer/opaque-contract-v1" {
				t.Fatalf("root-only error = %#v", rootOnly)
			}
		})
	}
}

func TestApproveProposalRejectsRootOnlyRecipeWithoutDurableAdmission(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "sessions", "root-only-proposal")
	st := store.New(sessionDir)
	config, err := recipes.LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if err != nil {
		t.Fatalf("load runtime config: %v", err)
	}
	recipe := map[string]any{
		"id":                   "consumer-owned-arbitrary-root-recipe",
		"participants":         []any{"codex", "codex"},
		"facilitator":          "codex",
		"reducer":              "codex",
		"max_rounds":           1,
		"participant_turns":    2,
		"result_source":        "reducer",
		"integration_contract": "consumer/opaque-contract-v1",
		"max_depth":            2,
	}
	config.RelayRecipes[stringFromAny(recipe["id"])] = recipe
	runtimeConfigRef, err := persistRuntimeConfigSnapshot(st, config)
	if err != nil {
		t.Fatalf("persist runtime config: %v", err)
	}
	if err := st.SaveMetaMap(map[string]any{
		"status":             "completed",
		"task":               "Reject root-only dynamic child",
		"runtime_config_ref": runtimeConfigRef,
	}); err != nil {
		t.Fatalf("save meta: %v", err)
	}
	if err := st.SaveTranscriptItems([]any{}); err != nil {
		t.Fatalf("save transcript: %v", err)
	}
	proposalID := "sp_root_only"
	if err := st.SaveProposalMap(map[string]any{
		"proposal_id":          proposalID,
		"parent_node_id":       "root",
		"contested_lineage_id": "ln_root_only",
		"delegated_question":   "Attempt a root-only child",
		"selected_recipe_id":   recipe["id"],
		"requested_rounds":     1,
		"status":               "proposed",
		"created_at":           utcNow(),
		"updated_at":           utcNow(),
	}); err != nil {
		t.Fatalf("save proposal: %v", err)
	}
	beforeProposal, err := st.LoadProposalMap(proposalID)
	if err != nil {
		t.Fatalf("load proposal before approval: %v", err)
	}
	beforeGraph := st.LoadGraph()
	beforeEvents, err := st.ReadEvents()
	if err != nil {
		t.Fatalf("read events before approval: %v", err)
	}

	_, err = ApproveProposal(context.Background(), sessionDir, ApproveOptions{
		ProposalID:     proposalID,
		Rounds:         1,
		TimeoutSeconds: 5,
	})
	var rootOnly *recipes.RootOnlyRecipeError
	if !errors.As(err, &rootOnly) {
		t.Fatalf("approval error = %T %[1]v, want *recipes.RootOnlyRecipeError", err)
	}
	afterProposal, loadErr := st.LoadProposalMap(proposalID)
	if loadErr != nil {
		t.Fatalf("load proposal after approval: %v", loadErr)
	}
	if !reflect.DeepEqual(afterProposal, beforeProposal) {
		t.Fatalf("rejected approval mutated proposal:\nbefore=%#v\nafter=%#v", beforeProposal, afterProposal)
	}
	if afterGraph := st.LoadGraph(); !reflect.DeepEqual(afterGraph, beforeGraph) {
		t.Fatalf("rejected approval mutated graph:\nbefore=%#v\nafter=%#v", beforeGraph, afterGraph)
	}
	afterEvents, readErr := st.ReadEvents()
	if readErr != nil {
		t.Fatalf("read events after approval: %v", readErr)
	}
	if !reflect.DeepEqual(afterEvents, beforeEvents) {
		t.Fatalf("rejected approval appended events:\nbefore=%#v\nafter=%#v", beforeEvents, afterEvents)
	}
}

func TestEveryRunnerRecipeCompilerCallSelectsChildTargetExplicitly(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate runner package")
	}
	directory := filepath.Dir(sourceFile)
	packages, err := parser.ParseDir(token.NewFileSet(), directory, func(info fs.FileInfo) bool {
		return filepath.Ext(info.Name()) == ".go" && !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse runner package: %v", err)
	}
	callCount := 0
	for _, parsed := range packages {
		for _, file := range parsed.Files {
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "CompileRecipe" {
					return true
				}
				callCount++
				if len(call.Args) < 4 {
					t.Fatalf("CompileRecipe call has %d arguments", len(call.Args))
				}
				target, ok := call.Args[3].(*ast.SelectorExpr)
				if !ok || target.Sel.Name != "CompileTargetChild" {
					t.Fatalf("runner CompileRecipe call does not explicitly select CompileTargetChild: %#v", call.Args[3])
				}
				return true
			})
		}
	}
	if callCount != 2 {
		t.Fatalf("runner CompileRecipe call count = %d, want child relay and dynamic child routes", callCount)
	}
}
