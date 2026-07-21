package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const catalogCLIBundleJSON = `{
  "schema_version": "relay-integration-bundle-v1",
  "id": "catalog-cli-bundle",
  "contracts": {
    "test/contract-v1": {
      "turns": [
        {"participant_turn": 1, "slot": "slot_0", "instructions": "Present."},
        {"participant_turn": 2, "slot": "slot_1", "instructions": "Challenge."}
      ],
      "reducer": {"instructions": "Reduce."},
      "result": {"transport": "json", "schema": {"type": "object"}}
    }
  }
}`

func TestRecipeCatalogAndCompileTargetCLIContracts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake readiness executables use POSIX shell")
	}
	env := setupCatalogCLIEnv(t)

	list := env.run(t, "recipes", "list", "--settings", env.settingsPath)
	list.requireExit(t, 0)
	if !strings.Contains(list.stdout, "bound-review") || !strings.Contains(list.stdout, "requires_integration") {
		t.Fatalf("default human list omitted unresolved integration:\n%s", list.stdout)
	}

	doctor := env.run(t, "recipes", "doctor", "--settings", env.settingsPath, "--json")
	doctor.requireExit(t, 0)
	doctorReport := decodeCLIJSON(t, doctor.stdout)
	if doctorReport["status"] != "ok" {
		t.Fatalf("missing integration degraded doctor: %#v", doctorReport)
	}
	requireRecipeCLIStatus(t, doctorReport, "bound-review", "requires_integration")

	filtered := env.run(t, "recipes", "list", "--settings", env.settingsPath, "--status", "requires_integration", "--json")
	filtered.requireExit(t, 0)
	filteredReport := decodeCLIJSON(t, filtered.stdout)
	recipes := filteredReport["recipes"].([]any)
	if len(recipes) != 1 || recipes[0].(map[string]any)["id"] != "bound-review" {
		t.Fatalf("requires_integration filter = %#v", recipes)
	}

	show := env.run(t, "recipes", "show", "bound-review", "--settings", env.settingsPath, "--integration-bundle", env.bundlePath, "--json")
	show.requireExit(t, 0)
	showRecord := decodeCLIJSON(t, show.stdout)
	if showRecord["status"] != "usable" || showRecord["integration"].(map[string]any)["status"] != "bound" {
		t.Fatalf("bundle-aware show = %#v", showRecord)
	}

	boundDoctor := env.run(t, "recipes", "doctor", "--settings", env.settingsPath, "--integration-bundle", env.bundlePath, "--json")
	boundDoctor.requireExit(t, 0)
	requireRecipeCLIStatus(t, decodeCLIJSON(t, boundDoctor.stdout), "bound-review", "usable")

	malformedDoctor := env.run(t, "recipes", "doctor", "--settings", env.settingsPath, "--integration-bundle", env.malformedBundlePath, "--json")
	malformedDoctor.requireExit(t, 1)
	if decodeCLIJSON(t, malformedDoctor.stdout)["status"] != "error" {
		t.Fatalf("malformed bundle doctor output = %s", malformedDoctor.stdout)
	}

	omittedChild := env.run(t, "compile-recipe", "--recipe", "review-panel", "--settings", env.settingsPath, "--json")
	omittedChild.requireExit(t, 0)
	omittedReport := decodeCLIJSON(t, omittedChild.stdout)
	explicitChild := env.run(t, "compile-recipe", "--recipe", "review-panel", "--target", "child", "--settings", env.settingsPath, "--json")
	explicitChild.requireExit(t, 0)
	explicitReport := decodeCLIJSON(t, explicitChild.stdout)
	if omittedReport["target"] != "child" || explicitReport["target"] != "child" {
		t.Fatalf("child targets = omitted %#v explicit %#v", omittedReport["target"], explicitReport["target"])
	}
	if omittedReport["compiled_plan_digest"] != explicitReport["compiled_plan_digest"] || omittedReport["recipe_digest"] != explicitReport["recipe_digest"] {
		t.Fatalf("omitted child changed compatibility digests: omitted=%#v explicit=%#v", omittedReport, explicitReport)
	}
	if omittedReport["compiled_plan"].(map[string]any)["kind"] != "compiled_plan" || omittedReport["launch"] == nil {
		t.Fatalf("omitted child report = %#v", omittedReport)
	}

	root := env.run(t, "compile-recipe", "--recipe", "review-panel", "--target", "root", "--settings", env.settingsPath, "--json")
	root.requireExit(t, 0)
	rootReport := decodeCLIJSON(t, root.stdout)
	if rootReport["target"] != "root" || rootReport["compiled_plan"].(map[string]any)["kind"] != "root_recipe_plan" {
		t.Fatalf("root compile report = %#v", rootReport)
	}
	if _, exists := rootReport["launch"]; exists {
		t.Fatalf("root compile derived child launch: %#v", rootReport)
	}

	boundRoot := env.run(t, "compile-recipe", "--recipe", "bound-review", "--target", "root", "--settings", env.settingsPath, "--integration-bundle", env.bundlePath, "--json")
	boundRoot.requireExit(t, 0)
	boundRootPlan := decodeCLIJSON(t, boundRoot.stdout)["compiled_plan"].(map[string]any)
	if boundRootPlan["kind"] != "root_recipe_plan" || boundRootPlan["integration_contract_id"] != "test/contract-v1" || boundRootPlan["integration_bundle_digest"] == "" {
		t.Fatalf("bound root plan = %#v", boundRootPlan)
	}

	boundChild := env.run(t, "compile-recipe", "--recipe", "bound-review", "--target", "child", "--settings", env.settingsPath, "--integration-bundle", env.bundlePath, "--json")
	boundChild.requireExit(t, 1)
	boundChildError := decodeCLIJSON(t, boundChild.stdout)
	if boundChildError["code"] != "root_only_recipe" || boundChildError["integration_contract"] != "test/contract-v1" {
		t.Fatalf("bound child error = %#v", boundChildError)
	}

	missingRootBundle := env.run(t, "compile-recipe", "--recipe", "bound-review", "--target", "root", "--settings", env.settingsPath, "--json")
	missingRootBundle.requireExit(t, 1)
	if len(decodeCLIJSON(t, missingRootBundle.stdout)["diagnostics"].([]any)) == 0 {
		t.Fatalf("missing root bundle error = %s", missingRootBundle.stdout)
	}
	malformedRootBundle := env.run(t, "compile-recipe", "--recipe", "bound-review", "--target", "root", "--settings", env.settingsPath, "--integration-bundle", env.malformedBundlePath, "--json")
	malformedRootBundle.requireExit(t, 1)
	if len(decodeCLIJSON(t, malformedRootBundle.stdout)["diagnostics"].([]any)) == 0 {
		t.Fatalf("malformed root bundle error = %s", malformedRootBundle.stdout)
	}

	invalidTarget := env.run(t, "compile-recipe", "--recipe", "review-panel", "--target", "automatic", "--settings", env.settingsPath, "--json")
	invalidTarget.requireExit(t, 2)
	if !strings.Contains(invalidTarget.stderr, "--target must be one of root or child") {
		t.Fatalf("invalid target stderr = %q", invalidTarget.stderr)
	}
}

type catalogCLIEnv struct {
	binary              string
	repoRoot            string
	settingsPath        string
	bundlePath          string
	malformedBundlePath string
	commandEnv          []string
}

type catalogCLIResult struct {
	stdout   string
	stderr   string
	exitCode int
}

func setupCatalogCLIEnv(t *testing.T) catalogCLIEnv {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))
	root := t.TempDir()
	binary := filepath.Join(root, "convo-relay")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = cwd
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	for _, backend := range []string{"claude", "codex", "gemini"} {
		path := filepath.Join(fakeBin, backend)
		content := "#!/bin/sh\nprintf '" + backend + " 1.0\\n'\n"
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", backend, err)
		}
	}
	settingsPath := filepath.Join(root, "settings.toml")
	settings := `
[relay_recipes.bound-review]
participants = ["codex", "codex"]
facilitator = "codex"
reducer = "codex"
participant_turns = 2
result_source = "reducer"
integration_contract = "test/contract-v1"
max_depth = 1
`
	if err := os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	bundlePath := filepath.Join(root, "bundle.json")
	if err := os.WriteFile(bundlePath, []byte(catalogCLIBundleJSON), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	malformedBundlePath := filepath.Join(root, "malformed-bundle.json")
	if err := os.WriteFile(malformedBundlePath, []byte(`{"schema_version":`), 0o644); err != nil {
		t.Fatalf("write malformed bundle: %v", err)
	}
	commandEnv := append(os.Environ(),
		"PATH="+fakeBin,
		"HOME="+filepath.Join(root, "home"),
		"CONVO_RELAY_SETTINGS=",
	)
	return catalogCLIEnv{
		binary:              binary,
		repoRoot:            repoRoot,
		settingsPath:        settingsPath,
		bundlePath:          bundlePath,
		malformedBundlePath: malformedBundlePath,
		commandEnv:          commandEnv,
	}
}

func (env catalogCLIEnv) run(t *testing.T, args ...string) catalogCLIResult {
	t.Helper()
	cmd := exec.Command(env.binary, args...)
	cmd.Dir = env.repoRoot
	cmd.Env = env.commandEnv
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run %s: %v", strings.Join(args, " "), err)
		}
		exitCode = exitError.ExitCode()
	}
	return catalogCLIResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: exitCode}
}

func (result catalogCLIResult) requireExit(t *testing.T, want int) {
	t.Helper()
	if result.exitCode != want {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", result.exitCode, want, result.stdout, result.stderr)
	}
}

func decodeCLIJSON(t *testing.T, output string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatalf("decode CLI JSON: %v\n%s", err, output)
	}
	return value
}

func requireRecipeCLIStatus(t *testing.T, report map[string]any, recipeID string, want string) {
	t.Helper()
	for _, raw := range report["recipes"].([]any) {
		record := raw.(map[string]any)
		if record["id"] == recipeID {
			if record["status"] != want {
				t.Fatalf("recipe %s status = %v, want %s", recipeID, record["status"], want)
			}
			return
		}
	}
	t.Fatalf("recipe %s missing from %#v", recipeID, report)
}
