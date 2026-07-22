package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestParseFlagsAllowsFlagsAfterPositionals(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "json")
	home := flags.String("home", "", "home")
	rounds := flags.String("rounds", "", "rounds")

	if err := parseFlags(flags, []string{"phase7-session", "--json", "--home", "/tmp/relay", "--rounds=2-3"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if !*jsonOutput || *home != "/tmp/relay" || *rounds != "2-3" {
		t.Fatalf("parsed values json=%v home=%q rounds=%q", *jsonOutput, *home, *rounds)
	}
	if args := flags.Args(); len(args) != 1 || args[0] != "phase7-session" {
		t.Fatalf("positionals = %#v", args)
	}
}

func TestParseFlagsSupportsShortValueAfterPositionals(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	output := ""
	htmlOnly := flags.Bool("html-only", false, "html")
	flags.StringVar(&output, "o", "", "output")
	if err := parseFlags(flags, []string{"phase7-session", "--html-only", "-o", "out.html"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if !*htmlOnly || output != "out.html" {
		t.Fatalf("parsed values htmlOnly=%v output=%q", *htmlOnly, output)
	}
}

func TestRecipeCLIValidators(t *testing.T) {
	for _, status := range []string{"all", "usable", "requires_integration", "unavailable", "invalid", "skipped"} {
		if err := validateRecipeStatusFilter(status); err != nil {
			t.Fatalf("status %q unexpectedly invalid: %v", status, err)
		}
	}
	if err := validateRecipeStatusFilter("broken"); err == nil {
		t.Fatalf("invalid status accepted")
	}
	for _, view := range []string{"all", "declared", "resolved"} {
		if err := validateRecipeView(view); err != nil {
			t.Fatalf("view %q unexpectedly invalid: %v", view, err)
		}
	}
	if err := validateRecipeView("raw"); err == nil {
		t.Fatalf("invalid view accepted")
	}
	for input, want := range map[string]recipes.CompileTarget{
		"":      recipes.CompileTargetChild,
		"child": recipes.CompileTargetChild,
		"root":  recipes.CompileTargetRoot,
	} {
		if got, err := parseCompileTarget(input); err != nil || got != want {
			t.Fatalf("target %q = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := parseCompileTarget("automatic"); err == nil {
		t.Fatal("invalid compile target accepted")
	}
	if got := strings.Join(stringItemsLocal([]any{"codex", "gemini"}), ","); got != "codex,gemini" {
		t.Fatalf("stringItemsLocal = %q", got)
	}
}

func TestBackendsStatusJSONRunsOnlyVersionProbesByDefault(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "backends.log")
	t.Setenv("BACKENDS_TEST_LOG", logPath)
	writeBackendProbeExecutable(t, dir, "claude", `
case "$*" in
  "--version") printf 'claude 1.0\n' ;;
  *) printf 'unexpected:%s\n' "$*" >&2; exit 90 ;;
esac`)
	writeBackendProbeExecutable(t, dir, "codex", `
case "$*" in
  "--version") printf 'codex 1.0\n' ;;
  *) printf 'unexpected:%s\n' "$*" >&2; exit 90 ;;
esac`)
	writeBackendProbeExecutable(t, dir, "gemini", `
case "$*" in
  "--version") printf 'gemini 1.0\n' ;;
  *) printf 'unexpected:%s\n' "$*" >&2; exit 90 ;;
esac`)
	t.Setenv("PATH", dir)

	oldArgs := os.Args
	os.Args = []string{"convo-relay", "backends", "status", "--json"}
	defer func() { os.Args = oldArgs }()
	output := captureStdout(t, main)
	report := decodeJSONObject(t, output)
	if report["scope"] != "backends" || report["probe_auth"] != false {
		t.Fatalf("backend report metadata = %#v", report)
	}
	backends, ok := report["backends"].([]any)
	if !ok || len(backends) != 4 {
		t.Fatalf("backend records = %#v", report["backends"])
	}
	for _, raw := range backends[:3] {
		record := raw.(map[string]any)
		if record["status"] != "installed_auth_unknown" || record["authentication_status"] != "unknown" {
			t.Fatalf("default backend record = %#v", record)
		}
		auth := record["probe_detail"].(map[string]any)["authentication"].(map[string]any)
		if auth["attempted"] != false || auth["status"] != "not_run" {
			t.Fatalf("default authentication probe = %#v", auth)
		}
	}
	relay := backends[3].(map[string]any)
	if relay["backend"] != "relay" || relay["status"] != "ready" || relay["executable_path"] != "built-in" {
		t.Fatalf("relay record = %#v", relay)
	}
	assertBackendProbeLog(t, logPath, []string{"claude:--version", "codex:--version", "gemini:--version"})
}

func TestBackendsStatusProbeAuthHumanOutput(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "backends.log")
	t.Setenv("BACKENDS_TEST_LOG", logPath)
	writeBackendProbeExecutable(t, dir, "claude", `
case "$*" in
  "--version") printf 'claude 1.0\n' ;;
  "auth status --json") printf '{"loggedIn":true}\n' ;;
  *) exit 90 ;;
esac`)
	writeBackendProbeExecutable(t, dir, "codex", `
case "$*" in
  "--version") printf 'codex 1.0\n' ;;
  "login status") printf 'Not logged in\n' >&2; exit 1 ;;
  *) exit 90 ;;
esac`)
	writeBackendProbeExecutable(t, dir, "gemini", `
case "$*" in
  "--version") printf 'gemini 1.0\n' ;;
  *) exit 90 ;;
esac`)
	t.Setenv("PATH", dir)

	output := captureStdout(t, func() {
		runBackends([]string{"status", "--probe-auth"})
	})
	for _, expected := range []string{"Backend readiness:", "claude  ready", "codex   auth_failed", "gemini  unsupported_probe", "relay   ready", "auth=unauthenticated", "auth=unsupported"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("human backend output missing %q:\n%s", expected, output)
		}
	}
	assertBackendProbeLog(t, logPath, []string{
		"claude:--version",
		"claude:auth status --json",
		"codex:--version",
		"codex:login status",
		"gemini:--version",
	})
}

func writeBackendProbeExecutable(t *testing.T, dir string, name string, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\nprintf '" + name + ":%s\\n' \"$*\" >> \"$BACKENDS_TEST_LOG\"\n" + strings.TrimSpace(body) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write backend probe %s: %v", name, err)
	}
}

func assertBackendProbeLog(t *testing.T, path string, want []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backend probe log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("backend probe log = %#v, want %#v", got, want)
	}
}

func TestCompileRecipeJSONReportsTransientDigestPairs(t *testing.T) {
	generatedTOML := `
[relay_recipes.gen-cli-review]
participants = ["codex-fast", "codex-deep"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
auto_approval = "auto-safe"
`
	withStdin(t, generatedTOML, func() {
		output := captureStdout(t, func() {
			runCompileRecipe([]string{
				"--recipe", "gen-cli-review",
				"--settings", filepath.Join(t.TempDir(), "missing.toml"),
				"--generated-recipe-file", "-",
				"--json",
			})
		})
		report := decodeJSONObject(t, output)
		if report["source_digest"] == "" || report["recipe_digest"] == "" {
			t.Fatalf("generated compile report missing digest pair: %#v", report)
		}
		recipe := report["recipe"].(map[string]any)
		if recipe["auto_approval"] != "never" || recipe["origin"] != "generated" {
			t.Fatalf("generated recipe normalization = %#v", recipe)
		}
		compiledPlan := report["compiled_plan"].(map[string]any)
		recipeRef := compiledPlan["recipe_ref"].(map[string]any)
		if recipeRef["id"] != "recipe:gen-cli-review" || recipeRef["digest"] != report["recipe_digest"] {
			t.Fatalf("compiled recipe ref = %#v, report digest = %v", recipeRef, report["recipe_digest"])
		}
	})

	ordinaryTOML := `
[relay_recipes.cli-review]
participants = ["codex-fast", "codex-deep"]
facilitator = "codex-fast"
reducer = "codex-deep"
max_rounds = 1
max_depth = 1
auto_approval = "auto-safe"
`
	recipePath := filepath.Join(t.TempDir(), "recipes.toml")
	if err := os.WriteFile(recipePath, []byte(ordinaryTOML), 0o644); err != nil {
		t.Fatalf("write ordinary recipe file: %v", err)
	}
	output := captureStdout(t, func() {
		runCompileRecipe([]string{
			"--recipe", "cli-review",
			"--settings", filepath.Join(t.TempDir(), "missing.toml"),
			"--recipe-file", recipePath,
			"--json",
		})
	})
	report := decodeJSONObject(t, output)
	if report["source_digest"] == "" || report["recipe_digest"] == "" {
		t.Fatalf("ordinary compile report missing digest pair: %#v", report)
	}
	recipe := report["recipe"].(map[string]any)
	if recipe["auto_approval"] != "auto-safe" || recipe["origin"] != nil {
		t.Fatalf("ordinary recipe normalization = %#v", recipe)
	}
}

func TestRunCompatibilityFlagsBuildContextSkillsPlanAndQuickMode(t *testing.T) {
	tempDir := t.TempDir()
	contextPath := filepath.Join(tempDir, "context.md")
	skillPath := filepath.Join(tempDir, "skill.md")
	recipePath := filepath.Join(tempDir, "recipes.toml")
	generatedRecipePath := filepath.Join(tempDir, "generated-recipes.toml")
	planPath := filepath.Join(tempDir, "plan.json")
	if err := os.WriteFile(contextPath, []byte("context body"), 0o644); err != nil {
		t.Fatalf("write context: %v", err)
	}
	if err := os.WriteFile(skillPath, []byte("skill body"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	if err := os.WriteFile(recipePath, []byte("[relay_recipes.example]\nparticipants = [\"codex\", \"codex\"]\n"), 0o644); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	if err := os.WriteFile(generatedRecipePath, []byte("[relay_recipes.generated]\nparticipants = [\"codex\", \"codex\"]\n"), 0o644); err != nil {
		t.Fatalf("write generated recipe: %v", err)
	}
	if err := os.WriteFile(planPath, []byte(`{"explanation":"Prepare","plan":[{"step":"Run quick relay","status":"completed"}]}`), 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	args := []string{
		"Pressure-test this",
		"--context", contextPath,
		"--skill", skillPath,
		"--recipe-file", recipePath,
		"--generated-recipe-file", generatedRecipePath,
		"--task-plan", planPath,
		"--quick",
		"--verbose",
		"--stream",
		"-o", filepath.Join(tempDir, "transcript.md"),
	}
	extracted, cleaned, err := extractMultiValueFlags(args, "context", "skill", "recipe-file", "generated-recipe-file")
	if err != nil {
		t.Fatalf("extract flags: %v", err)
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	taskPlan := flags.String("task-plan", "", "plan")
	quick := flags.Bool("quick", false, "quick")
	verbose := false
	stream := false
	output := ""
	flags.BoolVar(&verbose, "verbose", false, "verbose")
	flags.BoolVar(&stream, "stream", false, "stream")
	flags.StringVar(&output, "o", "", "output")
	if err := parseFlags(flags, cleaned); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	taskWithContext, skillsText, err := buildTaskWithContext(flags.Arg(0), extracted["context"], extracted["skill"])
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	launchPlan, err := loadLaunchPlanFile(*taskPlan)
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}

	if !*quick || !verbose || !stream || output == "" {
		t.Fatalf("compat flags quick=%v verbose=%v stream=%v output=%q", *quick, verbose, stream, output)
	}
	if !strings.Contains(taskWithContext, "--- Launch Context ---") || !strings.Contains(taskWithContext, "ctx1") || !strings.Contains(taskWithContext, "context body") {
		t.Fatalf("task context not injected:\n%s", taskWithContext)
	}
	if !strings.Contains(skillsText, "skill body") {
		t.Fatalf("skills text not injected:\n%s", skillsText)
	}
	if len(extracted["recipe-file"]) != 1 || extracted["recipe-file"][0] != recipePath {
		t.Fatalf("recipe files = %#v", extracted["recipe-file"])
	}
	if len(extracted["generated-recipe-file"]) != 1 || extracted["generated-recipe-file"][0] != generatedRecipePath {
		t.Fatalf("generated recipe files = %#v", extracted["generated-recipe-file"])
	}
	plan, _ := launchPlan.(map[string]any)
	steps, _ := plan["plan"].([]any)
	if plan["explanation"] != "Prepare" || len(steps) != 1 {
		t.Fatalf("launch plan = %#v", launchPlan)
	}
}

func TestRecipeRunStructuralOverridesUseOnlyVisitedFlags(t *testing.T) {
	newFlags := func() *flag.FlagSet {
		flags := flag.NewFlagSet("run", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		_ = flags.String("recipe", "", "recipe")
		_ = flags.String("agents", "codex,codex", "agents")
		_ = flags.String("model-a", "", "model a")
		_ = flags.String("effort-a", "", "effort a")
		_ = flags.String("model-b", "", "model b")
		_ = flags.String("effort-b", "", "effort b")
		_ = flags.String("facilitator-backend", "", "facilitator backend")
		_ = flags.String("facilitator-model", "", "facilitator model")
		_ = flags.String("facilitator-effort", "", "facilitator effort")
		_ = flags.String("mode", "adversarial", "mode")
		_ = flags.Int("rounds", 0, "rounds")
		_ = flags.Int("max-rounds", 50, "max rounds")
		_ = flags.Bool("quick", false, "quick")
		_ = flags.String("dynamic", "off", "dynamic")
		return flags
	}

	flags := newFlags()
	if err := parseFlags(flags, []string{"ordinary task", "--recipe", "neutral-root"}); err != nil {
		t.Fatalf("parse default recipe flags: %v", err)
	}
	visited := visitedFlagNames(flags)
	if err := validateRecipeRunStructuralOverrides(visited); err != nil {
		t.Fatalf("ordinary defaults created a structural conflict: %v (visited %#v)", err, visited)
	}

	structural := []struct {
		name  string
		value string
	}{
		{name: "agents", value: "codex"},
		{name: "model-a", value: "model-a"},
		{name: "effort-a", value: "high"},
		{name: "model-b", value: "model-b"},
		{name: "effort-b", value: "low"},
		{name: "facilitator-backend", value: "codex"},
		{name: "facilitator-model", value: "facilitator-model"},
		{name: "facilitator-effort", value: "medium"},
		{name: "mode", value: "cooperative"},
		{name: "rounds", value: "1"},
		{name: "max-rounds", value: "1"},
		{name: "quick"},
		{name: "dynamic", value: "ask"},
	}
	for _, override := range structural {
		t.Run(override.name, func(t *testing.T) {
			flags := newFlags()
			args := []string{"ordinary task", "--recipe", "neutral-root", "--" + override.name}
			if override.value != "" {
				args = append(args, override.value)
			}
			if err := parseFlags(flags, args); err != nil {
				t.Fatalf("parse explicit override: %v", err)
			}
			visited := visitedFlagNames(flags)
			if !visited[override.name] {
				t.Fatalf("explicit override %s was not visited: %#v", override.name, visited)
			}
			err := validateRecipeRunStructuralOverrides(visited)
			if err == nil || !strings.Contains(err.Error(), "--"+override.name) {
				t.Fatalf("override %s error = %v", override.name, err)
			}
		})
	}
}

func TestRunRecipeCLIDispatchesDirectRootExecution(t *testing.T) {
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "convo-relay")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	launchCWD := filepath.Join(tempDir, "launch")
	if err := os.MkdirAll(launchCWD, 0o755); err != nil {
		t.Fatalf("mkdir launch CWD: %v", err)
	}
	settingsPath := filepath.Join(launchCWD, "settings.toml")
	settings := `
[backend_profiles.cli-a]
backend = "codex"
model = "fake-a"
effort = "medium"
capabilities = []

[backend_profiles.cli-b]
backend = "codex"
model = "fake-b"
effort = "medium"
capabilities = []

[backend_profiles.cli-f]
backend = "codex"
model = "fake-f"
effort = "medium"
capabilities = []

[backend_profiles.cli-r]
backend = "codex"
model = "fake-r"
effort = "medium"
capabilities = []
`
	if err := os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	recipePath := filepath.Join(launchCWD, "root-recipes.toml")
	recipeSource := `
[relay_recipes.neutral-root]
purpose = "Neutral CLI root recipe"
participants = ["cli-a", "cli-b"]
facilitator = "cli-f"
reducer = "cli-r"
mode = "cooperative"
max_rounds = 2
participant_turns = 2
result_source = "last_turn"
max_depth = 1
required_capabilities = []
auto_approval = "never"
match_keywords = []

[relay_recipes.neutral-root.lifecycle]
resume = "allow"
steering = "allow"
dynamic = "forbid"
workspace_isolation = "inherited"

[relay_recipes.bound-root]
purpose = "Neutral integration-bound CLI root recipe"
participants = ["cli-a", "cli-b"]
facilitator = "cli-f"
reducer = "cli-r"
mode = "cooperative"
max_rounds = 2
participant_turns = 2
result_source = "last_turn"
integration_contract = "neutral/contract-v1"
max_depth = 1
required_capabilities = []
auto_approval = "never"
match_keywords = []

[relay_recipes.bound-root.lifecycle]
resume = "allow"
steering = "forbid"
dynamic = "forbid"
workspace_isolation = "inherited"
`
	if err := os.WriteFile(recipePath, []byte(recipeSource), 0o644); err != nil {
		t.Fatalf("write recipe source: %v", err)
	}
	bundlePath := filepath.Join(launchCWD, "bundle.json")
	bundleSource := `{
  "schema_version": "relay-integration-bundle-v1",
  "id": "neutral/integration-v1",
  "contracts": {
    "neutral/contract-v1": {
      "turns": [
        {"participant_turn": 1, "slot": "slot_0", "instructions": "Present the payload."},
        {"participant_turn": 2, "slot": "slot_1", "instructions": "Challenge the payload."}
      ],
      "inputs": {
        "payload": {
          "required": true,
          "cardinality": "one",
          "media_type": "application/json",
          "max_bytes": 1024,
          "schema": {"type": "object"}
        }
      },
      "result": {"transport": "json", "schema": {"type": "object"}}
    }
  }
}`
	if err := os.WriteFile(bundlePath, []byte(bundleSource), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(launchCWD, "payload.json"), []byte(`{"value":"cli"}`), 0o644); err != nil {
		t.Fatalf("write named input: %v", err)
	}
	generatedRecipePath := filepath.Join(launchCWD, "generated-recipes.toml")
	generatedRecipeSource := `
[relay_recipes.generated-helper]
purpose = "Generated helper recipe"
participants = ["cli-a", "cli-b"]
facilitator = "cli-f"
reducer = "cli-r"
mode = "cooperative"
max_rounds = 2
participant_turns = 2
result_source = "last_turn"
max_depth = 1
required_capabilities = []
auto_approval = "never"
match_keywords = []

[relay_recipes.generated-helper.lifecycle]
resume = "allow"
steering = "allow"
dynamic = "forbid"
workspace_isolation = "inherited"
`
	if err := os.WriteFile(generatedRecipePath, []byte(generatedRecipeSource), 0o644); err != nil {
		t.Fatalf("write generated recipe source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(launchCWD, "context.md"), []byte("compatibility context\n"), 0o644); err != nil {
		t.Fatalf("write context: %v", err)
	}
	if err := os.WriteFile(filepath.Join(launchCWD, "skill.md"), []byte("compatibility skill\n"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	taskPlanSource := `{"explanation":"Compatibility plan","plan":[{"step":"Inspect","status":"pending"}]}`
	if err := os.WriteFile(filepath.Join(launchCWD, "task-plan.json"), []byte(taskPlanSource), 0o644); err != nil {
		t.Fatalf("write task plan: %v", err)
	}
	fakeBin := filepath.Join(tempDir, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	providerLog := filepath.Join(tempDir, "provider.log")
	fakeCodex := filepath.Join(fakeBin, "codex")
	fakeScript := `#!/bin/sh
printf '%s\n' "$*" >> "$ROOT_RECIPE_CLI_LOG"
if [ "$1" = "--version" ]; then
  printf 'codex test 1.0\n'
  exit 0
fi
prompt=$(cat)
suffix=$(basename "${CODEX_HOME:-codex}")
case " $* " in
  *" resume "*) ;;
  *) printf '{"type":"thread.started","thread_id":"root-cli-%s"}\n' "$suffix" ;;
esac
case "$prompt" in
  *"Return the updated ledger as JSON"*)
    printf '%s\n' '{"type":"item.completed","item":{"text":"{\"settled\":[\"cli\"],\"contested\":[],\"withdrawn\":[]}"}}'
    ;;
  *"Invalid structured result"*)
    printf '%s\n' '{"type":"item.completed","item":{"text":"not a JSON result"}}'
    ;;
  *"Integration Contract Instructions for This Turn"*)
    printf '%s\n' '{"type":"item.completed","item":{"text":"{\"value\":\"cli\"}"}}'
    ;;
  *)
    printf '{"type":"item.completed","item":{"text":"CLI participant %s"}}\n' "$suffix"
    ;;
esac
`
	if err := os.WriteFile(fakeCodex, []byte(fakeScript), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	sessionDir := filepath.Join(tempDir, "session")
	command := exec.Command(binary,
		"run", "CLI positional task",
		"--recipe", "neutral-root",
		"--settings", "settings.toml",
		"--recipe-file", "root-recipes.toml",
		"--session-dir", sessionDir,
		"--launch-cwd", launchCWD,
		"--json",
	)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ROOT_RECIPE_CLI_LOG="+providerLog,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run recipe CLI: %v\n%s", err, output)
	}
	result := decodeJSONObject(t, string(output))
	if result["execution_kind"] != "recipe" || result["status"] != "completed" || result["recipe_id"] != "neutral-root" || intValue(result["actual_participant_turns"]) != 2 {
		t.Fatalf("root CLI result = %#v", result)
	}
	if result["root_recipe_plan_ref"] == nil || result["latest_root_checkpoint_ref"] == nil {
		t.Fatalf("root CLI result refs = %#v", result)
	}
	if len(result["transient_recipe_refs"].([]any)) != 1 || len(result["transient_recipe_contract_refs"].([]any)) != 2 {
		t.Fatalf("root CLI transient recipe refs = %#v / %#v", result["transient_recipe_refs"], result["transient_recipe_contract_refs"])
	}
	logData, err := os.ReadFile(providerLog)
	if err != nil {
		t.Fatalf("read provider log: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(string(logData)), "\n"); len(lines) != 5 {
		t.Fatalf("unexpected provider invocation log:\n%s", logData)
	}
	graphData, err := os.ReadFile(filepath.Join(sessionDir, "graph.json"))
	if err != nil {
		t.Fatalf("read graph: %v", err)
	}
	graphPayload := decodeJSONObject(t, string(graphData))
	nodes := graphPayload["nodes"].(map[string]any)
	if len(nodes) != 1 || nodes["root"] == nil || len(graphPayload["edges"].([]any)) != 0 {
		t.Fatalf("root CLI graph = %#v", graphPayload)
	}

	boundSessionDir := filepath.Join(tempDir, "bound-session")
	boundCommand := exec.Command(binary,
		"run", "Bound CLI task",
		"--recipe", "bound-root",
		"--settings", "settings.toml",
		"--recipe-file", "root-recipes.toml",
		"--integration-bundle", "bundle.json",
		"--input", "payload=payload.json",
		"--session-dir", boundSessionDir,
		"--launch-cwd", launchCWD,
		"--json",
	)
	boundCommand.Env = command.Env
	boundOutput, err := boundCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("run bound recipe CLI: %v\n%s", err, boundOutput)
	}
	boundResult := decodeJSONObject(t, string(boundOutput))
	for _, field := range []string{"integration_bundle_ref", "integration_contract_ref", "named_input_manifest_ref", "execution_workspace_ref"} {
		if boundResult[field] == nil {
			t.Fatalf("bound root CLI result missing %s: %#v", field, boundResult)
		}
	}
	boundInputs := boundResult["provider_inputs"].(map[string]any)["inputs"].([]any)
	if len(boundInputs) != 1 || boundInputs[0].(map[string]any)["name"] != "payload" {
		t.Fatalf("bound root provider inputs = %#v", boundInputs)
	}

	logData, err = os.ReadFile(providerLog)
	if err != nil {
		t.Fatalf("read provider log after bound run: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(string(logData)), "\n"); len(lines) != 10 {
		t.Fatalf("unexpected provider invocation log after bound run:\n%s", logData)
	}

	compatibilityHome := filepath.Join(tempDir, "compatibility-home")
	compatibilityOutput := filepath.Join(tempDir, "compatibility-output.json")
	compatibilityCommand := exec.Command(binary,
		"run",
		"--task", "Compatible CLI task",
		"--recipe", "neutral-root",
		"--context", "context.md",
		"--skill", "skill.md",
		"--task-plan", "task-plan.json",
		"--settings", "settings.toml",
		"--recipe-file", "root-recipes.toml",
		"--generated-recipe-file", "generated-recipes.toml",
		"--timeout", "77",
		"--stall-timeout", "33",
		"--investigation", "context_only",
		"--launch-cwd", launchCWD,
		"--session-id", "compat-session",
		"--home", compatibilityHome,
		"--verbose",
		"--stream",
		"--output", compatibilityOutput,
		"--json",
	)
	compatibilityCommand.Env = command.Env
	compatibilityRaw, err := compatibilityCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("run recipe with compatible flags: %v\n%s", err, compatibilityRaw)
	}
	compatibilityResult := decodeJSONObject(t, string(compatibilityRaw))
	if compatibilityResult["session_id"] != "compat-session" || compatibilityResult["task"] != "Compatible CLI task" {
		t.Fatalf("compatible run identity = %#v", compatibilityResult)
	}
	if intValue(compatibilityResult["timeout_seconds"]) != 77 || intValue(compatibilityResult["stall_timeout_seconds"]) != 33 {
		t.Fatalf("compatible run timeouts = %#v / %#v", compatibilityResult["timeout_seconds"], compatibilityResult["stall_timeout_seconds"])
	}
	canonicalLaunchCWD, err := filepath.EvalSymlinks(launchCWD)
	if err != nil {
		t.Fatalf("canonical launch CWD: %v", err)
	}
	if compatibilityResult["investigation_mode"] != "context_only" || compatibilityResult["source_launch_cwd"] != canonicalLaunchCWD {
		t.Fatalf("compatible run launch policy = %#v", compatibilityResult)
	}
	if len(compatibilityResult["launch_context_refs"].([]any)) != 1 || len(compatibilityResult["input_bundle_refs"].([]any)) != 2 {
		t.Fatalf("compatible context and skill refs = %#v / %#v", compatibilityResult["launch_context_refs"], compatibilityResult["input_bundle_refs"])
	}
	if compatibilityResult["launch_plan"] == nil || len(compatibilityResult["transient_recipe_refs"].([]any)) != 2 {
		t.Fatalf("compatible task plan or recipe sources missing = %#v", compatibilityResult)
	}
	outputData, err := os.ReadFile(compatibilityOutput)
	if err != nil {
		t.Fatalf("read compatible JSON output: %v", err)
	}
	outputResult := decodeJSONObject(t, string(outputData))
	if outputResult["session_id"] != "compat-session" {
		t.Fatalf("compatible JSON output = %#v", outputResult)
	}
	logData, err = os.ReadFile(providerLog)
	if err != nil {
		t.Fatalf("read provider log after compatible run: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(string(logData)), "\n"); len(lines) != 15 {
		t.Fatalf("unexpected provider invocation log after compatible run:\n%s", logData)
	}

	for _, mode := range []struct {
		name       string
		jsonOutput bool
		withOutput bool
	}{
		{name: "plain stdout"},
		{name: "plain output file", withOutput: true},
		{name: "json stdout", jsonOutput: true},
		{name: "json output file", jsonOutput: true, withOutput: true},
	} {
		t.Run("invalid result "+mode.name, func(t *testing.T) {
			sessionDir := filepath.Join(tempDir, strings.ReplaceAll("invalid-"+mode.name, " ", "-"))
			args := []string{
				"run", "Invalid structured result",
				"--recipe", "bound-root",
				"--settings", "settings.toml",
				"--recipe-file", "root-recipes.toml",
				"--integration-bundle", "bundle.json",
				"--input", "payload=payload.json",
				"--session-dir", sessionDir,
				"--launch-cwd", launchCWD,
			}
			if mode.jsonOutput {
				args = append(args, "--json")
			}
			outputPath := ""
			if mode.withOutput {
				extension := ".md"
				if mode.jsonOutput {
					extension = ".json"
				}
				outputPath = filepath.Join(tempDir, strings.ReplaceAll(mode.name, " ", "-")+extension)
				args = append(args, "-o", outputPath)
			}
			invalid := exec.Command(binary, args...)
			invalid.Env = command.Env
			var stdout strings.Builder
			var stderr strings.Builder
			invalid.Stdout = &stdout
			invalid.Stderr = &stderr
			runErr := invalid.Run()
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("invalid result exit = %v, stdout=%s, stderr=%s", runErr, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "JSON input is not syntactically valid") {
				t.Fatalf("invalid result stderr = %s", stderr.String())
			}
			if mode.jsonOutput {
				stdoutResult := decodeJSONObject(t, stdout.String())
				if stdoutResult["status"] != "invalid_result" || stdoutResult["result_validation_failed"] != true {
					t.Fatalf("invalid JSON stdout = %#v", stdoutResult)
				}
			} else if !strings.Contains(stdout.String(), " invalid_result at ") || !strings.Contains(stdout.String(), "Participant turns: 2/2") {
				t.Fatalf("invalid plain stdout = %s", stdout.String())
			}
			metaData, readErr := os.ReadFile(filepath.Join(sessionDir, "meta.json"))
			if readErr != nil {
				t.Fatalf("read invalid result metadata: %v", readErr)
			}
			meta := decodeJSONObject(t, string(metaData))
			if meta["status"] != "invalid_result" || meta["raw_result_ref"] == nil || meta["result_validation_ref"] == nil || meta["canonical_result_ref"] != nil {
				t.Fatalf("persisted invalid result = %#v", meta)
			}
			validation, loadErr := store.New(sessionDir).LoadArtifact(meta["result_validation_ref"].(map[string]any))
			if loadErr != nil || validation["status"] != "failed" || len(validation["diagnostics"].([]any)) == 0 {
				t.Fatalf("persisted invalid diagnostics = %#v, %v", validation, loadErr)
			}
			if mode.withOutput {
				outputData, readErr := os.ReadFile(outputPath)
				if readErr != nil {
					t.Fatalf("read invalid output file: %v", readErr)
				}
				if mode.jsonOutput {
					fileResult := decodeJSONObject(t, string(outputData))
					if fileResult["status"] != "invalid_result" || fileResult["session_id"] != meta["session_id"] {
						t.Fatalf("invalid JSON output file = %#v", fileResult)
					}
				} else if !strings.Contains(string(outputData), "not a JSON result") {
					t.Fatalf("invalid Markdown output file = %s", outputData)
				}
			}
		})
	}

	for _, rejection := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no target flag", args: []string{"--target", "root"}, want: "flag provided but not defined"},
		{name: "explicit structural override", args: []string{"--agents", "codex"}, want: "does not accept structural overrides"},
	} {
		t.Run(rejection.name, func(t *testing.T) {
			rejectedSession := filepath.Join(tempDir, strings.ReplaceAll(rejection.name, " ", "-"))
			args := []string{
				"run", "Rejected task",
				"--recipe", "neutral-root",
				"--settings", settingsPath,
				"--session-dir", rejectedSession,
				"--launch-cwd", launchCWD,
			}
			args = append(args, rejection.args...)
			rejected := exec.Command(binary, args...)
			rejected.Env = command.Env
			rejectedOutput, rejectedErr := rejected.CombinedOutput()
			var exitError *exec.ExitError
			if !errors.As(rejectedErr, &exitError) || exitError.ExitCode() != 2 {
				t.Fatalf("rejected command error = %v, output = %s", rejectedErr, rejectedOutput)
			}
			if !strings.Contains(string(rejectedOutput), rejection.want) {
				t.Fatalf("rejected output missing %q:\n%s", rejection.want, rejectedOutput)
			}
			if _, statErr := os.Stat(rejectedSession); !os.IsNotExist(statErr) {
				t.Fatalf("rejected command created session, err = %v", statErr)
			}
		})
	}
}

func TestSaveRunnerOutputWritesMarkdownAndJSON(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session: %v", err)
	}
	meta := `{"task":"Output task","title":"Output title","status":"completed","mode":"adversarial","actual_rounds":1,"max_rounds":1,"ledger":{"settled":[],"contested":[],"withdrawn":[]},"slots":[{"backend":"codex"}]}`
	transcript := `[{"round":1,"from":"Codex","content":"Output body","ledger":{"settled":[],"contested":[],"withdrawn":[]}}]`
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "transcript.json"), []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	markdownPath := filepath.Join(t.TempDir(), "relay.md")
	if saved, err := saveRunnerOutput(map[string]any{"session_dir": sessionDir}, markdownPath, false); err != nil || saved != markdownPath {
		t.Fatalf("save markdown = %q, %v", saved, err)
	}
	markdown, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatalf("read markdown: %v", err)
	}
	if !strings.Contains(string(markdown), "Output body") || !strings.Contains(string(markdown), "# Relay Dialogue") {
		t.Fatalf("markdown output:\n%s", markdown)
	}

	jsonPath := filepath.Join(t.TempDir(), "relay.json")
	if saved, err := saveRunnerOutput(map[string]any{"session_id": "abc123", "session_dir": sessionDir}, jsonPath, true); err != nil || saved != jsonPath {
		t.Fatalf("save json = %q, %v", saved, err)
	}
	jsonData, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	if !strings.Contains(string(jsonData), `"session_id": "abc123"`) {
		t.Fatalf("json output:\n%s", jsonData)
	}
}

func TestEmitRunnerResultStillWritesStdoutWhenOutputSaveFails(t *testing.T) {
	runErr := errors.New("invalid root result")
	result := map[string]any{
		"execution_kind":           "recipe",
		"session_id":               "invalid123",
		"session_dir":              "/tmp/invalid123",
		"status":                   "invalid_result",
		"actual_participant_turns": 2,
		"participant_turns":        2,
	}
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block child creation"), 0o644); err != nil {
		t.Fatalf("write blocking parent: %v", err)
	}

	for _, jsonOutput := range []bool{false, true} {
		name := "plain"
		if jsonOutput {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := emitRunnerResult(&stdout, result, runErr, jsonOutput, filepath.Join(blockedParent, "result.out"))
			if !errors.Is(err, runErr) {
				t.Fatalf("emit error does not retain run error: %v", err)
			}
			if jsonOutput {
				payload := decodeJSONObject(t, stdout.String())
				if payload["status"] != "invalid_result" || payload["session_id"] != "invalid123" {
					t.Fatalf("JSON stdout result = %#v", payload)
				}
			} else if !strings.Contains(stdout.String(), "Session invalid123 invalid_result") {
				t.Fatalf("plain stdout result = %q", stdout.String())
			}
		})
	}
}

func TestEmitRunnerResultWithRunErrorPersistsSelectedOutputRepresentation(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "root-session")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir root session: %v", err)
	}
	meta := `{"execution_kind":"recipe","recipe_id":"neutral-root","task":"Output task","title":"Output title","status":"invalid_result","mode":"cooperative","participant_turns":2,"participant_turns_completed":2,"actual_participant_turns":2,"actual_rounds":2,"max_rounds":2,"result_source":"reducer","validation_status":"failed","ledger":{"settled":[],"contested":[],"withdrawn":[]},"slots":[{"backend":"codex"},{"backend":"codex"}]}`
	transcript := `[{"round":1,"from":"Participant A","content":"First participant body","ledger":{"settled":[],"contested":[],"withdrawn":[]}},{"round":2,"from":"Participant B","content":"Second participant body","ledger":{"settled":[],"contested":[],"withdrawn":[]}}]`
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write root meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "transcript.json"), []byte(transcript), 0o644); err != nil {
		t.Fatalf("write root transcript: %v", err)
	}
	canonicalRef := map[string]any{
		"kind": "artifact_ref", "schema_version": 1, "id": "canonical_result:selected",
		"digest": contracts.DigestPrefix + strings.Repeat("0", 64),
	}
	result := map[string]any{
		"execution_kind": "recipe", "session_id": "invalid123", "session_dir": sessionDir,
		"status": "invalid_result", "actual_participant_turns": 2, "participant_turns": 2,
		"canonical_result_ref": canonicalRef, "transcript": []any{map[string]any{"content": "machine envelope body"}},
	}
	runErr := errors.New("invalid root result")

	markdownPath := filepath.Join(t.TempDir(), "result.md")
	var plainStdout bytes.Buffer
	if err := emitRunnerResult(&plainStdout, result, runErr, false, markdownPath); !errors.Is(err, runErr) {
		t.Fatalf("plain emit error = %v", err)
	}
	markdown, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatalf("read markdown output: %v", err)
	}
	if !strings.Contains(string(markdown), "# Relay Dialogue") || !strings.Contains(string(markdown), "Second participant body") || strings.Contains(string(markdown), `"canonical_result_ref"`) {
		t.Fatalf("plain run output =\n%s", markdown)
	}
	if !strings.Contains(plainStdout.String(), "invalid_result") || !strings.Contains(plainStdout.String(), markdownPath) {
		t.Fatalf("plain stdout = %q", plainStdout.String())
	}

	jsonPath := filepath.Join(t.TempDir(), "result.json")
	var jsonStdout bytes.Buffer
	if err := emitRunnerResult(&jsonStdout, result, runErr, true, jsonPath); !errors.Is(err, runErr) {
		t.Fatalf("JSON emit error = %v", err)
	}
	jsonBody, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read JSON output: %v", err)
	}
	jsonFile := decodeJSONObject(t, string(jsonBody))
	jsonConsole := decodeJSONObject(t, jsonStdout.String())
	for label, payload := range map[string]map[string]any{"file": jsonFile, "stdout": jsonConsole} {
		if payload["status"] != "invalid_result" || payload["canonical_result_ref"] == nil || payload["transcript"] == nil {
			t.Fatalf("JSON %s envelope = %#v", label, payload)
		}
	}
}

func TestWriteExportOutputWritesMarkdownAndJSON(t *testing.T) {
	report := map[string]any{
		"session_id":  "export123",
		"session_dir": "/tmp/export123",
		"incomplete":  true,
		"meta": map[string]any{
			"task":   "Export task",
			"status": "interrupted",
			"ledger": map[string]any{"settled": []any{"one"}, "contested": []any{}, "withdrawn": []any{}},
		},
		"transcript": []any{map[string]any{"round": 1, "from": "Codex", "content": "Partial body"}},
	}
	markdownPath := filepath.Join(t.TempDir(), "export.md")
	if saved, err := writeExportOutput(report, markdownPath, false); err != nil || saved != markdownPath {
		t.Fatalf("write export markdown = %q, %v", saved, err)
	}
	markdown, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatalf("read export markdown: %v", err)
	}
	if !strings.Contains(string(markdown), "# Relay Export") || !strings.Contains(string(markdown), "**Incomplete**: true") {
		t.Fatalf("export markdown:\n%s", markdown)
	}

	jsonPath := filepath.Join(t.TempDir(), "export.json")
	if saved, err := writeExportOutput(report, jsonPath, true); err != nil || saved != jsonPath {
		t.Fatalf("write export json = %q, %v", saved, err)
	}
	jsonData, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read export json: %v", err)
	}
	if !strings.Contains(string(jsonData), `"session_id": "export123"`) || !strings.Contains(string(jsonData), `"incomplete": true`) {
		t.Fatalf("export json:\n%s", jsonData)
	}
}

func TestDisplayOutputPaths(t *testing.T) {
	htmlPath, pdfPath, openPath := displayOutputPaths("/tmp/session", "", true)
	if htmlPath != "/tmp/session/transcript.html" || pdfPath != "" || openPath != htmlPath {
		t.Fatalf("html paths = %q, %q, %q", htmlPath, pdfPath, openPath)
	}

	htmlPath, pdfPath, openPath = displayOutputPaths("/tmp/session", "/tmp/out.pdf", false)
	if htmlPath != "/tmp/session/transcript.html" || pdfPath != "/tmp/out.pdf" || openPath != pdfPath {
		t.Fatalf("pdf paths = %q, %q, %q", htmlPath, pdfPath, openPath)
	}
}

func TestWriteDisplayPDFUsesHelperAndCleansFailedOutput(t *testing.T) {
	tempDir := t.TempDir()
	successHelper := filepath.Join(tempDir, "pdf-helper")
	successScript := "#!/bin/sh\n" +
		"test -f \"$1\" || exit 3\n" +
		"mkdir -p \"$(dirname \"$2\")\"\n" +
		"printf 'pdf:%s' \"$(cat \"$1\")\" > \"$2\"\n"
	if err := os.WriteFile(successHelper, []byte(successScript), 0o755); err != nil {
		t.Fatalf("write success helper: %v", err)
	}
	htmlPath := filepath.Join(tempDir, "session", "transcript.html")
	pdfPath := filepath.Join(tempDir, "session", "transcript.pdf")
	if err := writeDisplayPDF("<html>ok</html>", htmlPath, pdfPath, successHelper); err != nil {
		t.Fatalf("write display pdf: %v", err)
	}
	if data, err := os.ReadFile(htmlPath); err != nil || string(data) != "<html>ok</html>" {
		t.Fatalf("html data = %q, err = %v", data, err)
	}
	if data, err := os.ReadFile(pdfPath); err != nil || string(data) != "pdf:<html>ok</html>" {
		t.Fatalf("pdf data = %q, err = %v", data, err)
	}

	failHelper := filepath.Join(tempDir, "failing-helper")
	failScript := "#!/bin/sh\nprintf partial > \"$2\"\necho helper unavailable >&2\nexit 7\n"
	if err := os.WriteFile(failHelper, []byte(failScript), 0o755); err != nil {
		t.Fatalf("write failing helper: %v", err)
	}
	failedHTML := filepath.Join(tempDir, "failed", "transcript.html")
	failedPDF := filepath.Join(tempDir, "failed", "transcript.pdf")
	err := writeDisplayPDF("<html>fail</html>", failedHTML, failedPDF, failHelper)
	if err == nil || !strings.Contains(err.Error(), "helper unavailable") {
		t.Fatalf("failing helper err = %v", err)
	}
	if _, err := os.Stat(failedHTML); !os.IsNotExist(err) {
		t.Fatalf("failed html should not exist, err = %v", err)
	}
	if _, err := os.Stat(failedPDF); !os.IsNotExist(err) {
		t.Fatalf("failed pdf should be removed, err = %v", err)
	}
}

func TestResolveDisplayPDFHelperEnvMissingIsClear(t *testing.T) {
	t.Setenv(displayPDFHelperEnv, filepath.Join(t.TempDir(), "missing-helper.py"))
	_, err := resolveDisplayPDFHelper()
	if err == nil || !strings.Contains(err.Error(), displayPDFHelperEnv) {
		t.Fatalf("helper error = %v", err)
	}
}

func TestResolveDisplayPDFHelperDefaultMissingIsClear(t *testing.T) {
	_, err := resolveDisplayPDFHelperFromCandidates([]string{filepath.Join(t.TempDir(), "scripts", "render_display_pdf.py")})
	if err == nil ||
		!strings.Contains(err.Error(), "scripts/render_display_pdf.py") ||
		!strings.Contains(err.Error(), "Playwright") ||
		!strings.Contains(err.Error(), "--html-only") {
		t.Fatalf("helper error = %v", err)
	}
}

func TestResolveDisplayPDFHelperUsesSourceAndInstalledCandidates(t *testing.T) {
	tempDir := t.TempDir()
	sourceHelper := filepath.Join(tempDir, "scripts", "render_display_pdf.py")
	if err := os.MkdirAll(filepath.Dir(sourceHelper), 0o755); err != nil {
		t.Fatalf("mkdir source helper: %v", err)
	}
	if err := os.WriteFile(sourceHelper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write source helper: %v", err)
	}
	resolved, err := resolveDisplayPDFHelperFromCandidates([]string{sourceHelper})
	if err != nil || resolved != sourceHelper {
		t.Fatalf("source helper resolved = %q, err = %v", resolved, err)
	}

	executable := filepath.Join(tempDir, "pkg", "bin", "convo-relay")
	installedHelper := filepath.Join(filepath.Dir(executable), "..", "share", installedShareDir, "scripts", "render_display_pdf.py")
	if err := os.MkdirAll(filepath.Dir(installedHelper), 0o755); err != nil {
		t.Fatalf("mkdir installed helper: %v", err)
	}
	if err := os.WriteFile(installedHelper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write installed helper: %v", err)
	}
	resolved, err = resolveDisplayPDFHelperFromCandidates(displayPDFHelperCandidatesForExecutable(executable))
	if err != nil || resolved != installedHelper {
		t.Fatalf("installed helper resolved = %q, err = %v", resolved, err)
	}
}

func TestInstalledAssetDiscoveryCandidates(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "bin", "convo-relay")

	pdfCandidates := displayPDFHelperCandidatesForExecutable(executable)
	expectedPDF := filepath.Join(filepath.Dir(executable), "..", "share", installedShareDir, "scripts", "render_display_pdf.py")
	if !containsPath(pdfCandidates, expectedPDF) {
		t.Fatalf("PDF helper candidates = %#v, want %q", pdfCandidates, expectedPDF)
	}

	skillCandidates := skillBundleCandidatesForExecutable(executable)
	expectedSkill := filepath.Join(filepath.Dir(executable), "..", "share", installedShareDir, "skill")
	if !containsPath(skillCandidates, expectedSkill) {
		t.Fatalf("skill candidates = %#v, want %q", skillCandidates, expectedSkill)
	}
}

func containsPath(paths []string, target string) bool {
	for _, path := range paths {
		if path == target {
			return true
		}
	}
	return false
}

func TestPDFHelperDoesNotImportRelayOrchestration(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "render_display_pdf.py"))
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	text := string(data)
	for _, forbidden := range []string{"\nimport relay", "\nfrom relay", "\nimport convo_relay", "\nfrom convo_relay", "\nimport display", "\nfrom display"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("pdf helper imports orchestration boundary %q", forbidden)
		}
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	done := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		done <- data
	}()
	os.Stdout = writer
	defer func() {
		os.Stdout = oldStdout
		_ = reader.Close()
	}()
	fn()
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	return string(<-done)
}

func withStdin(t *testing.T, body string, fn func()) {
	t.Helper()
	oldStdin := os.Stdin
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdin: %v", err)
	}
	if _, err := writer.WriteString(body); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdin writer: %v", err)
	}
	os.Stdin = reader
	defer func() {
		os.Stdin = oldStdin
		_ = reader.Close()
	}()
	fn()
}

func decodeJSONObject(t *testing.T, body string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("decode JSON output %q: %v", body, err)
	}
	return result
}

func TestDelegatedInstallSkillsPlanUsesInstallRoot(t *testing.T) {
	stage := filepath.Join(t.TempDir(), "stage")
	result, err := delegatedInstallSkillsResult("plan", "all", stage, false)
	if err != nil {
		t.Fatalf("install skills plan: %v", err)
	}
	if result.Operation != "plan" || result.Name != "convo-relay" || result.Kind != "delegated" {
		t.Fatalf("unexpected result metadata: %#v", result)
	}
	claude := result.Targets["claude"].Files
	codex := result.Targets["codex"].Files
	tools := result.Targets["tools"].Files
	if len(claude) != 2 || len(codex) != 2 {
		t.Fatalf("target files claude=%#v codex=%#v", claude, codex)
	}
	if len(tools) < 6 {
		t.Fatalf("tools target should include binary and bundled assets: %#v", tools)
	}
	if !strings.Contains(claude[0].Path, filepath.Join("stage", ".claude", "skills")) {
		t.Fatalf("claude install path does not use stage root: %#v", claude)
	}
	if !strings.Contains(codex[0].Path, filepath.Join("stage", ".codex", "skills")) {
		t.Fatalf("codex install path does not use stage root: %#v", codex)
	}
	if !strings.Contains(tools[0].Path, filepath.Join("stage", ".local", "bin", "convo-relay")) {
		t.Fatalf("tools binary path does not use stage root: %#v", tools)
	}
	if claude[0].SHA256 != "" || codex[0].SHA256 != "" || tools[0].SHA256 != "" {
		t.Fatalf("plan should not hash missing destination files: claude=%#v codex=%#v tools=%#v", claude, codex, tools)
	}
}

func TestDelegatedInstallSkillsCopiesAndHashes(t *testing.T) {
	stage := filepath.Join(t.TempDir(), "stage")
	result, err := delegatedInstallSkillsResult("install", "codex", stage, true)
	if err != nil {
		t.Fatalf("install codex skills: %v", err)
	}
	toolFiles := result.Targets["tools"].Files
	if len(toolFiles) < 6 {
		t.Fatalf("tools files = %#v", toolFiles)
	}
	if _, err := os.Stat(filepath.Join(stage, ".local", "bin", "convo-relay")); err != nil {
		t.Fatalf("installed convo-relay tool: %v", err)
	}
	for _, file := range toolFiles {
		if len(file.SHA256) != 64 || strings.HasPrefix(file.SHA256, "sha256:") {
			t.Fatalf("missing tool sha for %s: %#v", file.Path, file)
		}
	}
	files := result.Targets["codex"].Files
	if len(files) != 2 {
		t.Fatalf("codex files = %#v", files)
	}
	for _, file := range files {
		if _, err := os.Stat(file.Path); err != nil {
			t.Fatalf("installed file %s: %v", file.Path, err)
		}
		if len(file.SHA256) != 64 || strings.HasPrefix(file.SHA256, "sha256:") {
			t.Fatalf("missing sha for %s: %#v", file.Path, file)
		}
	}
}
