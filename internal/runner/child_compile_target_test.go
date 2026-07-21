package runner

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/recipes"
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
