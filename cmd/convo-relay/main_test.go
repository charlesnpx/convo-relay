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
	"sort"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/v2/internal/blobstore"
	"github.com/charlesnpx/convo-relay/v2/internal/eventlog"
	"github.com/charlesnpx/convo-relay/v2/internal/format"
	"github.com/charlesnpx/convo-relay/v2/internal/relayv2"
	"github.com/charlesnpx/convo-relay/v2/internal/session"
)

const fakeCodexAppServerScript = `#!/usr/bin/env python3
import json
import os
import sys
from pathlib import Path

root_recipe_log = os.environ.get("ROOT_RECIPE_CLI_LOG", "")
if root_recipe_log:
    with open(root_recipe_log, "a", encoding="utf-8") as log:
        log.write(" ".join(sys.argv[1:]) + "\n")

def send(value):
    print(json.dumps(value), flush=True)

def response(request, result):
    send({"id": request.get("id"), "result": result})

def prompt_from(params):
    items = params.get("input", [])
    if items and isinstance(items[0], dict):
        return str(items[0].get("text", ""))
    return ""

def main():
    if "--version" in sys.argv:
        print("0.143.0")
        return 0
    if len(sys.argv) < 2 or sys.argv[1] != "app-server":
        print("expected codex app-server", file=sys.stderr)
        return 2
    thread_id = "root-recipe-codex"
    turn_number = 0
    for raw in sys.stdin:
        try:
            request = json.loads(raw)
        except json.JSONDecodeError:
            continue
        method = request.get("method", "")
        params = request.get("params", {}) or {}
        if method == "initialize":
            response(request, {"serverInfo": {"name": "root-recipe-codex"}})
        elif method in ("thread/start", "thread/resume"):
            thread_id = params.get("threadId") or thread_id
            response(request, {"thread": {"id": thread_id}})
        elif method == "model/list":
            response(request, {"data": [{"id": "root-recipe-codex", "supportedReasoningEfforts": ["low", "high"]}]})
        elif method == "turn/start":
            turn_number += 1
            turn_id = f"turn-{turn_number}"
            response(request, {"turn": {"id": turn_id}})
            prompt = prompt_from(params)
            if "Return the updated ledger as JSON" in prompt:
                text = '{"settled":["root"],"contested":[],"withdrawn":[]}'
            elif root_recipe_log and "Invalid structured result" in prompt:
                text = "not a JSON result"
            elif root_recipe_log and "Instructions for This Turn" in prompt:
                text = '{"value":"cli"}'
            else:
                text = "Fake Codex root recipe"
            send({"method": "item/completed", "params": {
                "item": {"id": f"item-{turn_number}", "type": "agentMessage", "text": text},
            }})
            send({"method": "turn/completed", "params": {
                "threadId": thread_id,
                "turn": {"id": turn_id, "status": "completed"},
            }})
        elif method == "turn/interrupt":
            response(request, {})
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
`

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
	for _, status := range []string{"all", "usable", "unavailable", "invalid", "skipped"} {
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
	if got := strings.Join(stringItemsLocal([]any{"codex", "gemini"}), ","); got != "codex,gemini" {
		t.Fatalf("stringItemsLocal = %q", got)
	}
}

func TestV2WorkspaceModeAcceptsOnlySurvivingModes(t *testing.T) {
	for _, want := range []string{"current", "head-copy"} {
		got, err := v2WorkspaceMode(want)
		if err != nil || got != want {
			t.Fatalf("workspace mode %q = %q, %v", want, got, err)
		}
	}
	for _, invalid := range []string{"ephemeral", " current "} {
		if _, err := v2WorkspaceMode(invalid); err == nil ||
			!strings.Contains(err.Error(), "current") ||
			!strings.Contains(err.Error(), "head-copy") {
			t.Fatalf("invalid workspace mode %q error = %v", invalid, err)
		}
	}
}

func TestDoctorJSONRetainsBackendReadinessByDefault(t *testing.T) {
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
	os.Args = []string{"convo-relay", "doctor", "--json"}
	defer func() { os.Args = oldArgs }()
	output := captureStdout(t, main)
	doctor := decodeJSONObject(t, output)
	health, ok := doctor["health"].(map[string]any)
	if !ok || health["scope"] != "global" {
		t.Fatalf("doctor health report = %#v", doctor["health"])
	}
	report, ok := doctor["backends"].(map[string]any)
	if !ok || report["scope"] != "backends" || report["probe_auth"] != false {
		t.Fatalf("doctor backend report metadata = %#v", doctor["backends"])
	}
	backends, ok := report["backends"].([]any)
	if !ok || len(backends) != 3 {
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
	assertBackendProbeLog(t, logPath, []string{"claude:--version", "codex:--version", "gemini:--version"})
}

func TestVersionJSONReportsFlatFormatsWithoutProviderProbes(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "provider.log")
	t.Setenv("BACKENDS_TEST_LOG", logPath)
	for _, backend := range []string{"claude", "codex", "gemini"} {
		writeBackendProbeExecutable(t, dir, backend, "exit 99")
	}
	t.Setenv("PATH", dir)

	oldArgs := os.Args
	os.Args = []string{"convo-relay", "version", "--json"}
	defer func() { os.Args = oldArgs }()
	output := captureStdout(t, main)
	want := map[string]any{
		"version":        cliVersion,
		"formats":        format.PublicFormats(),
		"digest_classes": format.DigestClasses(),
	}
	wantBody, err := json.MarshalIndent(want, "", "  ")
	if err != nil || output != string(wantBody)+"\n" {
		t.Fatalf("version output = %s, want %s, marshal error %v", output, wantBody, err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("version probed a provider: %v", err)
	}
}

func TestExportVerifyJSONReportsPortableBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "portable")
	payloads := []struct {
		kind  string
		id    string
		value any
	}{
		{kind: "root_session", id: "session", value: map[string]any{"terminal_status": "completed"}},
		{kind: "participant_transcript", id: "transcript", value: []any{}},
		{kind: "diagnostics", id: "diagnostics", value: map[string]any{"execution_kind": "recipe", "status": "completed"}},
	}
	inventory := make([]any, 0, len(payloads))
	for _, payload := range payloads {
		body, err := format.CanonicalJSONBytes(payload.value)
		if err != nil {
			t.Fatalf("encode payload: %v", err)
		}
		relative := filepath.ToSlash(filepath.Join("payloads", payload.kind, payload.id+".json"))
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, relative)), 0o755); err != nil {
			t.Fatalf("mkdir payload: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, relative), body, 0o644); err != nil {
			t.Fatalf("write payload: %v", err)
		}
		inventory = append(inventory, map[string]any{
			"kind":        payload.kind,
			"portable_id": payload.id,
			"path":        relative,
			"blob":        blobstore.RefForBytes(body, "application/json"),
		})
	}
	sort.Slice(inventory, func(left int, right int) bool {
		return inventory[left].(map[string]any)["path"].(string) < inventory[right].(map[string]any)["path"].(string)
	})
	manifest, err := format.BundleManifest(map[string]any{
		"convo_relay_version": "test",
		"terminal_status":     "completed",
		"stop_reason":         nil,
		"session_payload":     "payloads/root_session/session.json",
		"transcript_payload":  "payloads/participant_transcript/transcript.json",
		"diagnostics_payload": "payloads/diagnostics/diagnostics.json",
		"payload_inventory":   inventory,
	})
	if err != nil {
		t.Fatalf("portable manifest: %v", err)
	}
	body, err := format.CanonicalJSONBytes(manifest)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), body, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	oldArgs := os.Args
	os.Args = []string{"convo-relay", "export", "verify", dir, "--json"}
	defer func() { os.Args = oldArgs }()
	output := captureStdout(t, main)
	var report map[string]any
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("decode export verify output %q: %v", output, err)
	}
	if report["status"] != "valid" || report["format"] != format.BundleV1 {
		t.Fatalf("export verify report = %#v", report)
	}
	binary := filepath.Join(t.TempDir(), "convo-relay")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build export verifier: %v\n%s", err, output)
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "manifest", mutate: func(value map[string]any) {
			value["unexpected_manifest_field"] = true
		}},
		{name: "inventory", mutate: func(value map[string]any) {
			value["payload_inventory"].([]any)[0].(map[string]any)["unexpected_inventory_field"] = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidateBody, err := json.Marshal(manifest)
			if err != nil {
				t.Fatalf("clone manifest: %v", err)
			}
			var candidate map[string]any
			if err := json.Unmarshal(candidateBody, &candidate); err != nil {
				t.Fatalf("decode cloned manifest: %v", err)
			}
			test.mutate(candidate)
			invalidBody, err := format.CanonicalJSONBytes(candidate)
			if err != nil {
				t.Fatalf("encode invalid manifest: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), invalidBody, 0o644); err != nil {
				t.Fatalf("write invalid manifest: %v", err)
			}
			command := exec.Command(binary, "export", "verify", dir, "--json")
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			err = command.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("export verify unknown %s field exit = %v, stdout=%q, stderr=%q", test.name, err, stdout.String(), stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("export verify unknown %s field stderr = %q", test.name, stderr.String())
			}
			invalidReport := decodeJSONObject(t, stdout.String())
			if invalidReport["format"] != format.BundleV1 || invalidReport["status"] != "invalid" {
				t.Fatalf("export verify unknown %s field report = %#v", test.name, invalidReport)
			}
		})
	}

	legacyRoot := map[string]any{
		"kind":            strings.Join([]string{"portable", "v2", "root", "session"}, "_"),
		"terminal_status": "completed",
	}
	legacyBody, err := format.CanonicalJSONBytes(legacyRoot)
	if err != nil {
		t.Fatalf("encode legacy root payload: %v", err)
	}
	rootPath := filepath.Join(dir, "payloads", "root_session", "session.json")
	if err := os.WriteFile(rootPath, legacyBody, 0o644); err != nil {
		t.Fatalf("write legacy root payload: %v", err)
	}
	for _, raw := range inventory {
		entry := raw.(map[string]any)
		if entry["path"] == filepath.ToSlash(filepath.Join("payloads", "root_session", "session.json")) {
			entry["blob"] = blobstore.RefForBytes(legacyBody, "application/json")
		}
	}
	legacyManifest, err := format.BundleManifest(map[string]any{
		"convo_relay_version": "test",
		"terminal_status":     "completed",
		"stop_reason":         nil,
		"session_payload":     "payloads/root_session/session.json",
		"transcript_payload":  "payloads/participant_transcript/transcript.json",
		"diagnostics_payload": "payloads/diagnostics/diagnostics.json",
		"payload_inventory":   inventory,
	})
	if err != nil {
		t.Fatalf("legacy portable manifest: %v", err)
	}
	legacyManifestBody, err := format.CanonicalJSONBytes(legacyManifest)
	if err != nil {
		t.Fatalf("encode legacy manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), legacyManifestBody, 0o644); err != nil {
		t.Fatalf("write legacy manifest: %v", err)
	}
	if _, err := v2VerifyPortableDirectory(dir); err == nil || !strings.Contains(err.Error(), "must omit kind") {
		t.Fatalf("legacy root marker verification error = %v", err)
	}
}

func TestExportVerifyJSONFailureIsMachineReadable(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "convo-relay")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	command := exec.Command(binary, "export", "verify", t.TempDir(), "--json")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("export verify invalid exit = %v, stdout=%q, stderr=%q", err, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("export verify invalid stderr = %q", stderr.String())
	}
	report := decodeJSONObject(t, stdout.String())
	if report["format"] != format.BundleV1 || report["status"] != "invalid" || strings.TrimSpace(stringValue(report["error"])) == "" {
		t.Fatalf("export verify invalid report = %#v", report)
	}
}

func TestDoctorProbeAuthHumanOutput(t *testing.T) {
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
		runDoctor([]string{"--probe-auth"})
	})
	for _, expected := range []string{"Health:", "Backend readiness:", "claude  ready", "codex   auth_failed", "gemini  unsupported_probe", "auth=unauthenticated", "auth=unsupported"} {
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
			runRecipesCompile([]string{
				"gen-cli-review",
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
		if compiledPlan["recipe_id"] != "gen-cli-review" || report["compiled_plan_digest"] == "" {
			t.Fatalf("compiled plan preview = %#v", compiledPlan)
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
		runRecipesCompile([]string{
			"cli-review",
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

[backend_profiles.cli-b]
backend = "codex"
model = "fake-b"
effort = "medium"

[backend_profiles.cli-f]
backend = "codex"
model = "fake-f"
effort = "medium"

[backend_profiles.cli-r]
backend = "codex"
model = "fake-r"
effort = "medium"
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
auto_approval = "never"

[relay_recipes.neutral-root.lifecycle]
resume = "allow"
steering = "allow"
dynamic = "forbid"
workspace_isolation = "inherited"

`
	if err := os.WriteFile(recipePath, []byte(recipeSource), 0o644); err != nil {
		t.Fatalf("write recipe source: %v", err)
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
auto_approval = "never"

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
	if err := os.WriteFile(fakeCodex, []byte(fakeCodexAppServerScript), 0o755); err != nil {
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
	v2Session, err := session.Open(sessionDir)
	if err != nil {
		t.Fatalf("open v2 root session: %v", err)
	}
	if v2Session.Plan.RecipeID != "neutral-root" || v2Session.Plan.Provenance != session.ProvenanceRecipe {
		t.Fatalf("v2 root plan = %#v", v2Session.Plan)
	}
	events, err := relayv2.Events(v2Session)
	if err != nil || len(events) == 0 {
		t.Fatalf("v2 root events = %#v, %v", events, err)
	}
	logData, err := os.ReadFile(providerLog)
	if err != nil {
		t.Fatalf("read provider log: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(string(logData)), "\n"); len(lines) != 4 {
		t.Fatalf("unexpected provider invocation log:\n%s", logData)
	}
	if _, statErr := os.Stat(filepath.Join(sessionDir, "meta.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("v2 root unexpectedly wrote legacy metadata: %v", statErr)
	}
	graphPayload, err := relayv2.BuildGraphReport(v2Session)
	if err != nil {
		t.Fatalf("build v2 graph: %v", err)
	}
	if graphPayload["graph"] == nil {
		t.Fatalf("v2 root graph = %#v", graphPayload)
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
	compatibilitySession, err := session.Open(stringValue(compatibilityResult["session_dir"]))
	if err != nil {
		t.Fatalf("open compatible v2 session: %v", err)
	}
	if compatibilitySession.Plan.Investigation != "context_only" ||
		len(compatibilitySession.Plan.Context) != 1 ||
		len(compatibilitySession.Plan.Skills) != 1 ||
		len(compatibilitySession.Plan.TaskPlan) == 0 {
		t.Fatalf("compatible v2 plan = %#v", compatibilitySession.Plan)
	}
	compatibilityRuntime, err := relayv2.LoadRuntime(compatibilitySession)
	if err != nil || len(compatibilityRuntime.Recipes) < 3 {
		t.Fatalf("compatible v2 runtime = %#v, %v", compatibilityRuntime, err)
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
	if lines := strings.Split(strings.TrimSpace(string(logData)), "\n"); len(lines) != 8 {
		t.Fatalf("unexpected provider invocation log after compatible run:\n%s", logData)
	}

	runCLITestGit(t, launchCWD, "init", "-q")
	runCLITestGit(t, launchCWD, "config", "user.name", "CLI Test")
	runCLITestGit(t, launchCWD, "config", "user.email", "cli@example.invalid")
	runCLITestGit(t, launchCWD, "add", "--all")
	runCLITestGit(t, launchCWD, "commit", "-q", "-m", "committed CLI fixture")
	if err := os.WriteFile(filepath.Join(launchCWD, "context.md"), []byte("dirty compatibility context\n"), 0o644); err != nil {
		t.Fatalf("dirty committed CLI source: %v", err)
	}
	dirtySessionDir := filepath.Join(tempDir, "dirty-source-session")
	dirtyCommand := exec.Command(binary,
		"run", "Dirty source JSON stream",
		"--recipe", "neutral-root",
		"--settings", "settings.toml",
		"--recipe-file", "root-recipes.toml",
		"--session-dir", dirtySessionDir,
		"--launch-cwd", launchCWD,
		// U3b §2 (mode collapse): exercise the surviving head-copy operator mode.
		"--workspace", "head-copy",
		"--json",
	)
	dirtyCommand.Env = command.Env
	var dirtyStdout strings.Builder
	var dirtyStderr strings.Builder
	dirtyCommand.Stdout = &dirtyStdout
	dirtyCommand.Stderr = &dirtyStderr
	if err := dirtyCommand.Run(); err != nil {
		t.Fatalf("dirty-source CLI run: %v\nstdout:\n%s\nstderr:\n%s", err, dirtyStdout.String(), dirtyStderr.String())
	}
	dirtyResult := decodeJSONObject(t, dirtyStdout.String())
	if dirtyResult["status"] != "completed" || dirtyResult["session_id"] == nil {
		t.Fatalf("dirty-source JSON stdout = %#v", dirtyResult)
	}
	if warning := dirtyStderr.String(); warning != "" {
		t.Fatalf("head-copy run wrote unexpected stderr: %q", warning)
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

func TestRunRecipeCLIExecutesStaticChildParticipant(t *testing.T) {
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
[backend_profiles.parent]
backend = "codex"
model = "fake-parent"
effort = "medium"

[backend_profiles.facilitator]
backend = "codex"
model = "fake-facilitator"
effort = "medium"

[backend_profiles.static-child]
backend = "child"
model = "child-review"
effort = 1
`
	if err := os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	recipePath := filepath.Join(launchCWD, "static-recipes.toml")
	recipes := `
[relay_recipes.static-parent]
purpose = "Root recipe with a declared static child participant."
participants = ["parent", "static-child"]
facilitator = "facilitator"
mode = "cooperative"
max_rounds = 2
participant_turns = 2
result_source = "last_turn"
max_depth = 1
auto_approval = "never"

[relay_recipes.static-parent.lifecycle]
resume = "allow"
steering = "allow"
dynamic = "forbid"
workspace_isolation = "inherited"

[relay_recipes.child-review]
purpose = "Child review used by the static participant."
participants = ["parent", "parent"]
facilitator = "facilitator"
mode = "cooperative"
max_rounds = 1
participant_turns = 1
result_source = "last_turn"
max_depth = 1
auto_approval = "never"

[relay_recipes.child-review.lifecycle]
resume = "allow"
steering = "allow"
dynamic = "forbid"
workspace_isolation = "inherited"
`
	if err := os.WriteFile(recipePath, []byte(recipes), 0o644); err != nil {
		t.Fatalf("write recipes: %v", err)
	}

	compile := exec.Command(binary,
		"recipes", "compile", "static-parent",
		"--settings", settingsPath,
		"--recipe-file", recipePath,
		"--json",
	)
	compiledOutput, err := compile.CombinedOutput()
	if err != nil {
		t.Fatalf("compile static child recipe: %v\n%s", err, compiledOutput)
	}
	compiled := decodeJSONObject(t, string(compiledOutput))
	compiledPlan, _ := compiled["compiled_plan"].(map[string]any)
	participants, _ := compiledPlan["participants"].([]any)
	if len(participants) != 2 {
		t.Fatalf("compiled static child participants = %#v", compiledPlan["participants"])
	}
	childStep, _ := participants[1].(map[string]any)
	if childStep["kind"] != "child_step" || childStep["recipe_id"] != "child-review" || intValue(childStep["turns"]) != 1 {
		t.Fatalf("compiled child step = %#v", childStep)
	}

	fakeBin := filepath.Join(tempDir, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "codex"), []byte(fakeCodexAppServerScript), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	sessionDir := filepath.Join(tempDir, "session")
	command := exec.Command(binary,
		"run", "Execute the declared static child.",
		"--recipe", "static-parent",
		"--settings", settingsPath,
		"--recipe-file", recipePath,
		"--session-dir", sessionDir,
		"--launch-cwd", launchCWD,
		"--json",
	)
	command.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	rawResult, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run static child recipe: %v\n%s", err, rawResult)
	}
	result := decodeJSONObject(t, string(rawResult))
	if result["status"] != "completed" || result["execution_kind"] != "recipe" || result["recipe_id"] != "static-parent" {
		t.Fatalf("static child recipe result = %#v", result)
	}

	sess, err := session.Open(sessionDir)
	if err != nil {
		t.Fatalf("open static child parent session: %v", err)
	}
	events, err := relayv2.Events(sess)
	if err != nil {
		t.Fatalf("read static child events: %v", err)
	}
	var requested eventlog.ChildRequestedPayload
	var decided eventlog.ChildDecidedPayload
	var completed eventlog.ChildCompletedPayload
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.ChildRequestedPayload:
			requested = payload
		case eventlog.ChildDecidedPayload:
			decided = payload
		case eventlog.ChildCompletedPayload:
			completed = payload
		}
	}
	if requested.RequesterActorID != "slot_1" || requested.RecipeID != "child-review" {
		t.Fatalf("static child request = %#v", requested)
	}
	if !decided.Admitted || decided.Reason != "admitted by static child step" || decided.Plan == nil {
		t.Fatalf("static child decision = %#v", decided)
	}
	if completed.RequestID != requested.RequestID || completed.Status != "completed" || completed.ChildSessionID == "" {
		t.Fatalf("static child completion = %#v", completed)
	}
}

func TestSaveRunnerOutputWritesMarkdownAndJSON(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	report := map[string]any{
		"session_id":    "abc123",
		"session_dir":   sessionDir,
		"task":          "Output task",
		"title":         "Output title",
		"status":        "completed",
		"mode":          "adversarial",
		"actual_rounds": 1,
		"max_rounds":    1,
		"slots":         []any{map[string]any{"backend": "codex"}},
		"transcript": []any{map[string]any{
			"round": 1, "from": "Codex", "content": "Output body",
			"ledger": map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}},
		}},
	}

	markdownPath := filepath.Join(t.TempDir(), "relay.md")
	if saved, err := saveRunnerOutput(report, markdownPath, false); err != nil || saved != markdownPath {
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
	if saved, err := saveRunnerOutput(report, jsonPath, true); err != nil || saved != jsonPath {
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
	result := map[string]any{
		"execution_kind": "recipe", "session_id": "invalid123", "session_dir": sessionDir,
		"task": "Output task", "title": "Output title", "mode": "cooperative", "status": "invalid_result",
		"actual_participant_turns": 2, "participant_turns": 2, "actual_rounds": 2, "max_rounds": 2,
		"result": "canonical result",
		"slots":  []any{map[string]any{"backend": "codex"}, map[string]any{"backend": "codex"}},
		"transcript": []any{
			map[string]any{"round": 1, "from": "Participant A", "content": "First participant body", "ledger": map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}}},
			map[string]any{"round": 2, "from": "Participant B", "content": "Second participant body", "ledger": map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}}},
		},
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
	if !strings.Contains(string(markdown), "# Relay Dialogue") || !strings.Contains(string(markdown), "Second participant body") || strings.Contains(string(markdown), `"result"`) {
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
		if payload["status"] != "invalid_result" || payload["result"] != "canonical result" || payload["transcript"] == nil {
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

func runCLITestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
