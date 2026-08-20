package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type phase17SmokeEnv struct {
	binary     string
	repoRoot   string
	fakeBinDir string
	homeDir    string
	relayHome  string
	pidDir     string
	projectDir string
}

type phase17SmokeCase struct {
	name             string
	agents           string
	expectedBackends []string
	rounds           int
	extraRunArgs     []string
}

const fakeCodexAppServerScript = `#!/usr/bin/env python3
import atexit
import json
import os
import sys
from pathlib import Path

root_recipe_log = os.environ.get("ROOT_RECIPE_CLI_LOG", "")
if root_recipe_log:
    with open(root_recipe_log, "a", encoding="utf-8") as log:
        log.write(" ".join(sys.argv[1:]) + "\n")

marker = None
pids = os.environ.get("CONVO_RELAY_FAKE_PROVIDER_PIDS", "")
if pids:
    Path(pids).mkdir(parents=True, exist_ok=True)
    marker = Path(pids) / f"codex-{os.getpid()}.pid"
    marker.write_text(str(os.getpid()) + "\n", encoding="utf-8")
    atexit.register(lambda: marker.unlink(missing_ok=True))

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
    suffix = Path(os.environ.get("CODEX_HOME", "default")).name or "default"
    thread_id = f"phase17-{suffix}"
    turn_number = 0
    for raw in sys.stdin:
        try:
            request = json.loads(raw)
        except json.JSONDecodeError:
            continue
        method = request.get("method", "")
        params = request.get("params", {}) or {}
        if method == "initialize":
            response(request, {"serverInfo": {"name": "phase17-codex"}})
        elif method in ("thread/start", "thread/resume"):
            thread_id = params.get("threadId") or thread_id
            response(request, {"thread": {"id": thread_id}})
        elif method == "model/list":
            response(request, {"data": [{"id": "phase17-codex", "supportedReasoningEfforts": ["low", "high"]}]})
        elif method == "turn/start":
            turn_number += 1
            turn_id = f"turn-{turn_number}"
            response(request, {"turn": {"id": turn_id}})
            prompt = prompt_from(params)
            if "Return the updated ledger as JSON" in prompt:
                text = '{"settled":["phase17"],"contested":[],"withdrawn":[]}'
            elif root_recipe_log and "Invalid structured result" in prompt:
                text = "not a JSON result"
            elif root_recipe_log and "Integration Contract Instructions for This Turn" in prompt:
                text = '{"value":"cli"}'
            else:
                text = f"Fake Codex {suffix}"
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

func TestGoOnlySmokeMatrix(t *testing.T) {
	if os.Getenv("CONVO_RELAY_RUN_SMOKE_MATRIX") != "1" {
		t.Skip("set CONVO_RELAY_RUN_SMOKE_MATRIX=1 or run make smoke-fake-providers")
	}
	env := setupPhase17SmokeEnv(t)

	matrix := []phase17SmokeCase{
		{name: "codex-codex", agents: "codex,codex", expectedBackends: []string{"codex", "codex"}, rounds: 2},
		{name: "codex-gemini", agents: "codex,gemini", expectedBackends: []string{"codex", "gemini"}, rounds: 2},
		{name: "gemini-codex", agents: "gemini,codex", expectedBackends: []string{"gemini", "codex"}, rounds: 2},
		{name: "gemini-gemini", agents: "gemini,gemini", expectedBackends: []string{"gemini", "gemini"}, rounds: 2},
		{name: "claude-codex", agents: "claude,codex", expectedBackends: []string{"claude", "codex"}, rounds: 2},
		{name: "codex-claude", agents: "codex,claude", expectedBackends: []string{"codex", "claude"}, rounds: 2},
		{name: "claude-claude", agents: "claude,claude", expectedBackends: []string{"claude", "claude"}, rounds: 2},
		{name: "profile-claude-code", agents: "claude-code,codex", expectedBackends: []string{"claude", "codex"}, rounds: 2},
		{name: "profile-gemini-vision", agents: "gemini-vision,codex", expectedBackends: []string{"gemini", "codex"}, rounds: 2},
		{name: "codex-relay", agents: "codex,relay", expectedBackends: []string{"codex", "relay"}, rounds: 2, extraRunArgs: []string{"--effort-b", "1"}},
		{name: "relay-codex", agents: "relay,codex", expectedBackends: []string{"relay", "codex"}, rounds: 2, extraRunArgs: []string{"--effort-a", "1"}},
		{name: "relay-relay", agents: "relay,relay", expectedBackends: []string{"relay", "relay"}, rounds: 2, extraRunArgs: []string{"--effort-a", "1", "--effort-b", "1"}},
	}

	for _, tc := range matrix {
		t.Run(tc.name, func(t *testing.T) {
			result := env.runRelayJSON(t, tc)
			env.verifySessionArtifacts(t, phase17SessionID(tc.name), tc.expectedBackends, tc.rounds, result)
		})
	}

	richSessionID := phase17SessionID("relay-relay")
	graphReport := env.runJSON(t, "show", richSessionID, "--home", env.relayHome, "--graph", "--json")
	env.requireValidationOK(t, graphReport, "show --graph")
	graphData := phase17Map(graphReport["graph"])
	if len(phase17Map(graphData["nodes"])) == 0 {
		t.Fatalf("show --graph report has no graph nodes: %#v", graphReport)
	}

	for _, provider := range []string{"codex", "claude", "gemini", "relay"} {
		t.Run("resume-cleanup-"+provider, func(t *testing.T) {
			env.verifyResumeCleanupAndClean(t, provider)
		})
	}

	env.requireNoRelayPIDFiles(t)
	env.requireNoActiveFakeProviders(t)
}

func setupPhase17SmokeEnv(t *testing.T) phase17SmokeEnv {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("resolve repo root from %s: %v", cwd, err)
	}

	root := t.TempDir()
	env := phase17SmokeEnv{
		binary:     filepath.Join(root, "convo-relay"),
		repoRoot:   repoRoot,
		fakeBinDir: filepath.Join(root, "bin"),
		homeDir:    filepath.Join(root, "home"),
		relayHome:  filepath.Join(root, "relay-home"),
		pidDir:     filepath.Join(root, "provider-pids"),
		projectDir: filepath.Join(root, "project"),
	}
	for _, dir := range []string{env.fakeBinDir, env.homeDir, env.relayHome, env.pidDir, env.projectDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	build := exec.Command("go", "build", "-o", env.binary, ".")
	build.Dir = cwd
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build smoke binary: %v\n%s", err, output)
	}
	if output, err := exec.Command("git", "init", env.projectDir).CombinedOutput(); err != nil {
		t.Fatalf("git init project: %v\n%s", err, output)
	}
	env.writeFakeProviders(t)
	return env
}

func (env phase17SmokeEnv) writeFakeProviders(t *testing.T) {
	t.Helper()
	fakes := map[string]string{
		"codex": fakeCodexAppServerScript,
		"claude": `#!/usr/bin/env python3
import atexit
import json
import os
import sys
from pathlib import Path

marker = None
pids = os.environ.get("CONVO_RELAY_FAKE_PROVIDER_PIDS", "")
if pids:
    Path(pids).mkdir(parents=True, exist_ok=True)
    marker = Path(pids) / f"claude-{os.getpid()}.pid"
    marker.write_text(str(os.getpid()) + "\n", encoding="utf-8")
    atexit.register(lambda: marker.unlink(missing_ok=True))

def arg_value(flag):
    if flag in sys.argv:
        index = sys.argv.index(flag)
        if index + 1 < len(sys.argv):
            return sys.argv[index + 1]
    return ""

def send(value):
    print(json.dumps(value), flush=True)

def main():
    if "--version" in sys.argv:
        print("2.1.205")
        return 0
    if "--help" in sys.argv:
        print("--effort values (low, medium, high, max)\n--model examples (phase17-claude)")
        return 0
    if arg_value("--input-format") != "stream-json" or arg_value("--output-format") != "stream-json":
        print("expected Claude stream-json flags", file=sys.stderr)
        return 2
    initialize_line = sys.stdin.readline()
    if not initialize_line:
        return 0
    initialize = json.loads(initialize_line)
    send({"type": "control_response", "response": {
        "subtype": "success",
        "request_id": initialize.get("request_id", ""),
        "response": {},
    }})
    user_line = sys.stdin.readline()
    if not user_line:
        return 0
    user = json.loads(user_line)
    prompt = str((user.get("message") or {}).get("content", ""))
    session_id = arg_value("--resume") or f"phase17-claude-{os.getpid()}"
    if "Return the updated ledger as JSON" in prompt:
        text = '{"settled":["phase17"],"contested":[],"withdrawn":[]}'
    else:
        text = "Fake Claude response"
    send({"type": "system", "session_id": session_id, "model": arg_value("--model") or "phase17-claude"})
    send({"type": "assistant", "session_id": session_id, "message": {"content": [{"type": "text", "text": text}]}})
    send({"type": "result", "session_id": session_id, "subtype": "success", "is_error": False, "result": text})
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
`,
		"gemini": `#!/bin/sh
set -eu
if [ -n "${CONVO_RELAY_FAKE_PROVIDER_PIDS:-}" ]; then
  mkdir -p "$CONVO_RELAY_FAKE_PROVIDER_PIDS"
  marker="$CONVO_RELAY_FAKE_PROVIDER_PIDS/gemini-$$.pid"
  printf '%s\n' "$$" > "$marker"
  trap 'rm -f "$marker"' EXIT INT TERM
fi
prompt=$(cat)
suffix=$(basename "${GEMINI_CLI_HOME:-${HOME:-default}}")
case "$prompt" in
  *"Return the updated ledger as JSON"*)
    printf '{"response":"{\"settled\":[\"phase17\"],\"contested\":[],\"withdrawn\":[]}","session_id":"gemini-%s"}\n' "$suffix"
    ;;
  *)
    printf '{"response":"Fake Gemini %s","session_id":"gemini-%s"}\n' "$suffix" "$suffix"
    ;;
esac
`,
	}
	for name, body := range fakes {
		path := filepath.Join(env.fakeBinDir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write fake provider %s: %v", name, err)
		}
	}
}

func (env phase17SmokeEnv) runRelayJSON(t *testing.T, tc phase17SmokeCase) map[string]any {
	t.Helper()
	args := []string{
		"run",
		"--home", env.relayHome,
		"--session-id", phase17SessionID(tc.name),
		"--task", "PHASE17_SMOKE " + tc.name,
		"--agents", tc.agents,
		"--rounds", fmt.Sprint(tc.rounds),
		"--timeout", "10",
		"--stall-timeout", "10",
		"--launch-cwd", env.projectDir,
		"--json",
	}
	args = append(args, tc.extraRunArgs...)
	return env.runJSON(t, args...)
}

func (env phase17SmokeEnv) verifyResumeCleanupAndClean(t *testing.T, provider string) {
	t.Helper()
	sessionID := phase17SessionID("resume-cleanup-" + provider)
	extraArgs := []string{}
	if provider == "relay" {
		extraArgs = []string{"--effort-a", "1"}
	}
	seed := phase17SmokeCase{
		name:             "resume-cleanup-" + provider,
		agents:           provider + ",codex",
		expectedBackends: []string{provider, "codex"},
		rounds:           1,
		extraRunArgs:     extraArgs,
	}
	result := env.runRelayJSON(t, seed)
	env.verifySessionArtifacts(t, sessionID, seed.expectedBackends, 1, result)

	resumed := env.runJSON(t,
		"resume",
		sessionID,
		"--home", env.relayHome,
		"--rounds", "2",
		"--timeout", "10",
		"--stall-timeout", "10",
		"--json",
	)
	env.verifySessionArtifacts(t, sessionID, seed.expectedBackends, 3, resumed)

	sessionDir := filepath.Join(env.relayHome, "sessions", sessionID)
	meta := phase17ReadJSONObject(t, filepath.Join(sessionDir, "meta.json"))

	meta["status"] = "running"
	phase17WriteJSONObject(t, filepath.Join(sessionDir, "meta.json"), meta)
	if err := os.WriteFile(filepath.Join(sessionDir, "relay.pid"), []byte("99999999"), 0o644); err != nil {
		t.Fatalf("write dead relay pid: %v", err)
	}
	cleanup := env.runJSON(t, "clean", "--all", "--home", env.relayHome, "--limit", "1000", "--json")
	if phase17Int(cleanup["orphaned_count"]) < 1 {
		t.Fatalf("cleanup did not mark orphaned session: %#v", cleanup)
	}
	meta = phase17ReadJSONObject(t, filepath.Join(sessionDir, "meta.json"))
	if meta["status"] != "orphaned" {
		t.Fatalf("cleanup meta status = %#v", meta["status"])
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "relay.pid")); !os.IsNotExist(err) {
		t.Fatalf("cleanup should remove dead relay.pid, err = %v", err)
	}

	clean := env.runJSON(t, "clean", sessionID, "--home", env.relayHome, "--json")
	if clean["status"] != "deleted" {
		t.Fatalf("clean report = %#v", clean)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("cleaned session still exists, err = %v", err)
	}
}

func (env phase17SmokeEnv) verifySessionArtifacts(t *testing.T, sessionID string, expectedBackends []string, expectedRounds int, result map[string]any) {
	t.Helper()
	sessionDir := filepath.Join(env.relayHome, "sessions", sessionID)
	if result["status"] != "completed" || phase17Int(result["actual_rounds"]) != expectedRounds {
		t.Fatalf("run result status/rounds = %#v", result)
	}
	if !strings.HasPrefix(phase17String(result["session_dir"]), env.relayHome) {
		t.Fatalf("session did not use temp relay home: %#v", result["session_dir"])
	}
	meta := phase17ReadJSONObject(t, filepath.Join(sessionDir, "meta.json"))
	if meta["status"] != "completed" || phase17Int(meta["actual_rounds"]) != expectedRounds {
		t.Fatalf("meta status/rounds = %#v", meta)
	}
	if task := phase17String(meta["task"]); !strings.Contains(task, "PHASE17_SMOKE") {
		t.Fatalf("meta task missing smoke marker: %#v", task)
	}
	slots := phase17Slice(meta["slots"])
	if len(slots) != len(expectedBackends) {
		t.Fatalf("slots = %#v", slots)
	}
	for index, rawSlot := range slots {
		slot := phase17Map(rawSlot)
		if slot["backend"] != expectedBackends[index] {
			t.Fatalf("slot %d backend = %#v, want %s", index, slot, expectedBackends[index])
		}
		if expectedRounds > index {
			env.verifyProviderState(t, expectedBackends[index], phase17Map(slot["state"]))
		}
	}

	transcript := phase17ReadJSONArray(t, filepath.Join(sessionDir, "transcript.json"))
	if len(transcript) != expectedRounds {
		t.Fatalf("transcript length = %d, want %d: %#v", len(transcript), expectedRounds, transcript)
	}
	for index, rawEntry := range transcript {
		entry := phase17Map(rawEntry)
		providerResult := phase17Map(entry["provider_result"])
		expectedBackend := expectedBackends[index%len(expectedBackends)]
		if providerResult["backend"] != expectedBackend {
			t.Fatalf("turn %d provider_result = %#v, want backend %s", index+1, providerResult, expectedBackend)
		}
		if expectedBackend != "relay" && phase17Int(providerResult["return_code"]) != 0 {
			t.Fatalf("turn %d provider_result return code = %#v", index+1, providerResult)
		}
	}

	events := phase17ReadJSONLines(t, filepath.Join(sessionDir, "events.jsonl"))
	if len(events) < expectedRounds+2 {
		t.Fatalf("event count = %d, want at least %d", len(events), expectedRounds+2)
	}
	if last := events[len(events)-1]; last["event_type"] != "node_completed" {
		t.Fatalf("last event = %#v, want node_completed", last)
	}
	graph := phase17ReadJSONObject(t, filepath.Join(sessionDir, "graph.json"))
	if len(phase17Map(graph["nodes"])) == 0 {
		t.Fatalf("graph has no nodes: %#v", graph)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "relay.pid")); !os.IsNotExist(err) {
		t.Fatalf("relay.pid should not remain after command, err = %v", err)
	}
	env.requireNoActiveFakeProviders(t)
}

func (env phase17SmokeEnv) verifyProviderState(t *testing.T, backend string, state map[string]any) {
	t.Helper()
	switch backend {
	case "codex":
		if state["started"] != true || phase17String(state["thread_id"]) == "" {
			t.Fatalf("codex state = %#v", state)
		}
	case "claude":
		if state["started"] != true || phase17String(state["session_id"]) == "" || phase17String(state["cwd"]) == "" {
			t.Fatalf("claude state = %#v", state)
		}
	case "gemini":
		if state["started"] != true || phase17String(state["session_ref"]) == "" {
			t.Fatalf("gemini state = %#v", state)
		}
	case "relay":
		if state["started"] != true || len(phase17Slice(state["child_session_ids"])) == 0 || len(phase17Slice(state["child_contract_refs"])) == 0 {
			t.Fatalf("relay state = %#v", state)
		}
	default:
		t.Fatalf("unexpected backend %q", backend)
	}
}

func (env phase17SmokeEnv) requireValidationOK(t *testing.T, report map[string]any, label string) {
	t.Helper()
	validation := phase17Map(report["validation"])
	if validation["ok"] != true {
		t.Fatalf("%s validation = %#v", label, validation)
	}
}

func (env phase17SmokeEnv) runJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	output := env.runText(t, args...)
	var value map[string]any
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatalf("%s did not emit JSON: %v\n%s", strings.Join(args, " "), err, output)
	}
	return value
}

func (env phase17SmokeEnv) runText(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(env.binary, args...)
	cmd.Dir = env.repoRoot
	cmd.Env = env.commandEnv()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	if stderr.Len() > 0 {
		t.Fatalf("%s wrote unexpected stderr:\n%s", strings.Join(args, " "), stderr.String())
	}
	env.requireNoActiveFakeProviders(t)
	return stdout.String()
}

func (env phase17SmokeEnv) commandEnv() []string {
	path := env.fakeBinDir + string(os.PathListSeparator) + phase17SystemPath()
	return append(os.Environ(),
		"PATH="+path,
		"HOME="+env.homeDir,
		"CODEX_CLAUDE_HOME="+env.relayHome,
		"CONVO_RELAY_FAKE_CWD="+env.projectDir,
		"CONVO_RELAY_FAKE_PROVIDER_PIDS="+env.pidDir,
	)
}

func phase17SystemPath() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("PATH")
	}
	return "/usr/bin:/bin:/usr/sbin:/sbin"
}

func (env phase17SmokeEnv) requireNoActiveFakeProviders(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(env.pidDir)
	if err != nil {
		t.Fatalf("read fake provider pid dir: %v", err)
	}
	if len(entries) == 0 {
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	t.Fatalf("fake provider pid markers still active: %s", strings.Join(names, ", "))
}

func (env phase17SmokeEnv) requireNoRelayPIDFiles(t *testing.T) {
	t.Helper()
	sessionsDir := filepath.Join(env.relayHome, "sessions")
	err := filepath.WalkDir(sessionsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == "relay.pid" {
			return fmt.Errorf("unexpected relay pid file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("relay pid scan: %v", err)
	}
}

func phase17SessionID(name string) string {
	return "phase17-" + strings.ReplaceAll(name, "_", "-")
}

func phase17ReadJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}

func phase17WriteJSONObject(t *testing.T, path string, value map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func phase17ReadJSONArray(t *testing.T, path string) []any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var value []any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}

func phase17ReadJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var events []map[string]any
	for lineNumber, rawLine := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode %s line %d: %v", path, lineNumber+1, err)
		}
		events = append(events, event)
	}
	return events
}

func phase17Map(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

func phase17Slice(value any) []any {
	if typed, ok := value.([]any); ok {
		return typed
	}
	return []any{}
}

func phase17String(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func phase17Int(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		asInt, _ := typed.Int64()
		return int(asInt)
	default:
		return 0
	}
}
