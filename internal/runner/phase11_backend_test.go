package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/recipes"
)

type phase11Env struct {
	relayHome  string
	projectDir string
	homeDir    string
}

func TestPhase11RunSupportsClaudeAndGeminiFacilitators(t *testing.T) {
	env := setupPhase11FakeProviders(t)

	claudeSessionDir := filepath.Join(env.relayHome, "sessions", "phase11-claude-facilitator")
	if _, err := Run(context.Background(), Options{
		SessionDir:          claudeSessionDir,
		Task:                "PHASE11_CLAUDE_FACILITATOR",
		Agents:              []string{"codex", "gemini"},
		Rounds:              1,
		TimeoutSeconds:      5,
		StallTimeoutSeconds: 5,
		LaunchCWD:           env.projectDir,
		FacilitatorBackend:  "claude",
		FacilitatorModel:    "claude-test-model",
		FacilitatorEffort:   "max",
	}); err != nil {
		t.Fatalf("run claude facilitator: %v", err)
	}
	claudeMeta := mustLoadMeta(t, claudeSessionDir)
	if claudeMeta["facilitator_backend"] != "claude" ||
		claudeMeta["facilitator_model"] != "claude-test-model" ||
		claudeMeta["facilitator_effort"] != "max" {
		t.Fatalf("claude facilitator meta = %#v", claudeMeta)
	}
	claudeLedger := claudeMeta["ledger"].(map[string]any)
	if settled := claudeLedger["settled"].([]any); len(settled) != 1 || settled[0] != "claude facilitator" {
		t.Fatalf("claude facilitator ledger = %#v", claudeLedger)
	}
	claudeCommands := readClaudeCommands(t, env.homeDir)
	lastClaudeCommand := claudeCommands[len(claudeCommands)-1]
	if !contains(lastClaudeCommand, "--model") || !contains(lastClaudeCommand, "claude-test-model") ||
		!contains(lastClaudeCommand, "--effort") || !contains(lastClaudeCommand, "max") {
		t.Fatalf("claude facilitator command = %#v", lastClaudeCommand)
	}

	geminiSessionDir := filepath.Join(env.relayHome, "sessions", "phase11-gemini-facilitator")
	if _, err := Run(context.Background(), Options{
		SessionDir:          geminiSessionDir,
		Task:                "PHASE11_GEMINI_FACILITATOR",
		Agents:              []string{"claude", "codex"},
		Rounds:              1,
		TimeoutSeconds:      5,
		StallTimeoutSeconds: 5,
		LaunchCWD:           env.projectDir,
		FacilitatorBackend:  "gemini",
		FacilitatorModel:    "gemini-test-model",
		FacilitatorEffort:   "low",
	}); err != nil {
		t.Fatalf("run gemini facilitator: %v", err)
	}
	geminiMeta := mustLoadMeta(t, geminiSessionDir)
	if geminiMeta["facilitator_backend"] != "gemini" ||
		geminiMeta["facilitator_model"] != "gemini-test-model" ||
		geminiMeta["facilitator_effort"] != "low" {
		t.Fatalf("gemini facilitator meta = %#v", geminiMeta)
	}
	geminiLedger := geminiMeta["ledger"].(map[string]any)
	if settled := geminiLedger["settled"].([]any); len(settled) != 1 || settled[0] != "gemini facilitator" {
		t.Fatalf("gemini facilitator ledger = %#v", geminiLedger)
	}
	geminiCommands := readGeminiCommands(t, env.homeDir)
	lastGeminiCommand := geminiCommands[len(geminiCommands)-1]
	if !contains(lastGeminiCommand, "--model") || !contains(lastGeminiCommand, "gemini-test-model") {
		t.Fatalf("gemini facilitator command = %#v", lastGeminiCommand)
	}
}

func TestPhase11RejectsRelayFacilitatorAndDefaultsClaudeConfig(t *testing.T) {
	env := setupFakeCodex(t)
	_, err := Run(context.Background(), Options{
		SessionDir:         filepath.Join(env.relayHome, "sessions", "phase11-relay-facilitator"),
		Task:               "Relay cannot facilitate",
		Agents:             []string{"codex", "codex"},
		Rounds:             1,
		TimeoutSeconds:     5,
		LaunchCWD:          env.projectDir,
		FacilitatorBackend: "relay",
	})
	if err == nil || !strings.Contains(err.Error(), `backend "relay" cannot be used as a facilitator`) {
		t.Fatalf("relay facilitator error = %v", err)
	}

	config := resolveFacilitatorConfig("claude", SlotConfig{})
	if config.Model != defaultClaudeFacilitatorModel || config.Effort != "" {
		t.Fatalf("claude facilitator defaults = %#v", config)
	}
}

func TestPhase11RelayBackendProfileRunsProviderChildAndFacilitator(t *testing.T) {
	env := setupPhase11FakeProviders(t)
	settingsPath := writePhase11ProviderRelaySettings(t, env)
	sessionDir := filepath.Join(env.relayHome, "sessions", "phase11-relay-provider-profile")

	if _, err := Run(context.Background(), Options{
		SessionDir:          sessionDir,
		Task:                "PHASE11_RELAY_BACKEND_PROFILE",
		Agents:              []string{"phase11-relay-profile", "codex"},
		Rounds:              1,
		TimeoutSeconds:      5,
		StallTimeoutSeconds: 5,
		SettingsPath:        settingsPath,
		LaunchCWD:           env.projectDir,
	}); err != nil {
		t.Fatalf("run relay backend profile: %v", err)
	}

	meta := mustLoadMeta(t, sessionDir)
	slots := meta["slots"].([]any)
	firstSlot := slots[0].(map[string]any)
	if firstSlot["backend"] != "relay" {
		t.Fatalf("first slot = %#v", firstSlot)
	}
	relayState := firstSlot["state"].(map[string]any)
	if relayState["recipe_id"] != "phase11-provider-child" || intFromAny(relayState["rounds"], 0) != 1 {
		t.Fatalf("relay profile state = %#v", relayState)
	}
	childIDs := relayState["child_session_ids"].([]any)
	if len(childIDs) != 1 {
		t.Fatalf("child session ids = %#v", childIDs)
	}
	childDir := filepath.Join(env.relayHome, "sessions", childIDs[0].(string))
	childMeta := mustLoadMeta(t, childDir)
	childSlots := childMeta["slots"].([]any)
	if childSlots[0].(map[string]any)["backend"] != "gemini" ||
		childSlots[1].(map[string]any)["backend"] != "claude" ||
		childMeta["facilitator_backend"] != "gemini" ||
		childMeta["facilitator_model"] != "gemini-child-model" ||
		childMeta["facilitator_effort"] != "low" {
		t.Fatalf("child meta = %#v", childMeta)
	}
	if _, err := inspect.BuildContractsReport(sessionDir, false, "", ""); err != nil {
		t.Fatalf("contracts report: %v", err)
	}
	if _, err := inspect.BuildShowTranscriptReport(sessionDir, 0, ""); err != nil {
		t.Fatalf("show transcript report: %v", err)
	}
	if _, err := inspect.RenderDiff(sessionDir); err != nil {
		t.Fatalf("diff report: %v", err)
	}
	if _, err := inspect.BuildDisplayHTML(sessionDir); err != nil {
		t.Fatalf("display html: %v", err)
	}
	graphReport, err := inspect.BuildShowGraphReport(sessionDir)
	if err != nil {
		t.Fatalf("graph report: %v", err)
	}
	if graphReport["validation"].(map[string]any)["ok"] != true {
		t.Fatalf("graph validation = %#v", graphReport["validation"])
	}
	graphPayload := graphReport["graph"].(map[string]any)
	nodes := graphPayload["nodes"].(map[string]any)
	traceNodeID := ""
	for nodeID, rawNode := range nodes {
		node, _ := rawNode.(map[string]any)
		if node["kind"] == "relay_backend_child" {
			traceNodeID = nodeID
			break
		}
	}
	if traceNodeID == "" {
		t.Fatalf("graph missing relay backend child node: %#v", nodes)
	}
	if _, err := inspect.BuildTraceReport(sessionDir, traceNodeID); err != nil {
		t.Fatalf("trace report: %v", err)
	}
	if cleanReport, err := CleanSession(sessionDir); err != nil || cleanReport["status"] != "deleted" {
		t.Fatalf("clean report = %#v, err = %v", cleanReport, err)
	}
}

func TestPhase11RestorePythonEraProviderStateAndBackendCWD(t *testing.T) {
	env := setupPhase11FakeProviders(t)

	claudeMeta := map[string]any{
		"slots": []any{
			map[string]any{
				"backend": "claude",
				"slot_id": "slot_0",
				"state": map[string]any{
					"session_id": "python-claude-session",
					"cwd":        env.projectDir,
					"model":      "python-claude-model",
				},
			},
			map[string]any{
				"backend": "codex",
				"slot_id": "slot_1",
				"state": map[string]any{
					"thread_id": "python-codex-thread",
					"cwd":       env.projectDir,
				},
			},
		},
	}
	claudeSlots, err := restoreSlots(claudeMeta, env.relayHome, []SlotConfig{{}, {}}, recipes.RuntimeConfig{}, "", 0, 1)
	if err != nil {
		t.Fatalf("restore claude python-era slots: %v", err)
	}
	claudeState := claudeSlots[0].SessionState()
	if claudeSlots[0].Label() != backendLabel("claude") ||
		claudeState["session_id"] != "python-claude-session" ||
		claudeState["started"] != true ||
		claudeState["model"] != "python-claude-model" {
		t.Fatalf("restored claude state = %#v", claudeState)
	}

	geminiMeta := map[string]any{
		"slots": []any{
			map[string]any{
				"backend": "gemini",
				"slot_id": "slot_0",
				"state": map[string]any{
					"session_ref": "python-gemini-session",
					"cwd":         env.projectDir,
					"model":       "python-gemini-model",
				},
			},
			map[string]any{
				"backend": "codex",
				"slot_id": "slot_1",
				"state": map[string]any{
					"thread_id": "python-codex-thread",
					"cwd":       env.projectDir,
				},
			},
		},
	}
	geminiSlots, err := restoreSlots(geminiMeta, env.relayHome, []SlotConfig{{Effort: "medium"}, {}}, recipes.RuntimeConfig{}, "", 0, 1)
	if err != nil {
		t.Fatalf("restore gemini python-era slots: %v", err)
	}
	geminiState := geminiSlots[0].SessionState()
	if geminiSlots[0].Label() != backendLabel("gemini") ||
		geminiState["session_ref"] != "python-gemini-session" ||
		geminiState["started"] != true ||
		geminiState["model"] != "python-gemini-model" ||
		geminiState["effort"] != "medium" {
		t.Fatalf("restored gemini state = %#v", geminiState)
	}

	nonGitDir := filepath.Join(filepath.Dir(env.relayHome), "non-git")
	if err := os.MkdirAll(nonGitDir, 0o755); err != nil {
		t.Fatalf("mkdir non-git: %v", err)
	}
	if got := resolveBackendCWD(nonGitDir, env.relayHome, "codex"); got != env.relayHome {
		t.Fatalf("codex cwd = %s, want session root", got)
	}
	if got := resolveBackendCWD(nonGitDir, env.relayHome, "claude"); got != nonGitDir {
		t.Fatalf("claude cwd = %s, want launch cwd", got)
	}
	if got := resolveBackendCWD(nonGitDir, env.relayHome, "gemini"); got != nonGitDir {
		t.Fatalf("gemini cwd = %s, want launch cwd", got)
	}
}

func setupPhase11FakeProviders(t *testing.T) phase11Env {
	t.Helper()
	base := setupFakeCodex(t)
	root := filepath.Dir(base.relayHome)
	binDir := filepath.Join(root, "bin")
	homeDir := filepath.Join(root, "home")
	fakeClaude := fakeClaudeStreamJSONScript
	fakeGemini := `#!/usr/bin/env python3
import json
import os
import sys
from pathlib import Path

def arg_value(flag):
    if flag in sys.argv:
        idx = sys.argv.index(flag)
        if idx + 1 < len(sys.argv):
            return sys.argv[idx + 1]
    return ""

def log_command():
    command_home = Path(os.environ.get("CONVO_RELAY_COMMAND_HOME") or str(Path.home()))
    path = command_home / "gemini_commands.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(sys.argv[1:]) + "\n")

def main():
    prompt = sys.stdin.read()
    log_command()
    gemini_home = Path(os.environ.get("GEMINI_CLI_HOME") or os.environ.get("HOME", ""))
    suffix = gemini_home.name or "default"
    model = arg_value("--model")
    if "Return the updated ledger as JSON" in prompt:
        text = '{"settled":["gemini facilitator"],"contested":[],"withdrawn":[]}'
    else:
        text = f"Fake Gemini {suffix} model={model}"
    print(json.dumps({"response": text, "session_id": f"gemini-{suffix}"}), flush=True)
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gemini"), []byte(fakeGemini), 0o755); err != nil {
		t.Fatalf("write fake gemini: %v", err)
	}
	t.Setenv("CONVO_RELAY_COMMAND_HOME", homeDir)
	return phase11Env{relayHome: base.relayHome, projectDir: base.projectDir, homeDir: homeDir}
}

func writePhase11ProviderRelaySettings(t *testing.T, env phase11Env) string {
	t.Helper()
	settingsPath := filepath.Join(env.relayHome, "phase11-settings.toml")
	settings := `
[backend_profiles.phase11-relay-profile]
backend = "relay"
model = "phase11-provider-child"
effort = "1"
capabilities = ["composite"]

[backend_profiles.phase11-gemini]
backend = "gemini"
model = "gemini-child-model"
effort = "low"
capabilities = ["vision"]

[backend_profiles.phase11-claude]
backend = "claude"
model = "claude-child-model"
effort = "medium"
capabilities = ["code"]

[relay_recipes.phase11-provider-child]
participants = ["phase11-gemini", "phase11-claude"]
facilitator = "phase11-gemini"
reducer = "phase11-claude"
mode = "adversarial"
max_rounds = 1
max_depth = 1
`
	if err := os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return settingsPath
}

func readGeminiCommands(t *testing.T, homeDir string) [][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(homeDir, "gemini_commands.jsonl"))
	if err != nil {
		t.Fatalf("read gemini commands: %v", err)
	}
	var commands [][]string
	for _, rawLine := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(rawLine) == "" {
			continue
		}
		var command []string
		if err := json.Unmarshal([]byte(rawLine), &command); err != nil {
			t.Fatalf("decode command %q: %v", rawLine, err)
		}
		commands = append(commands, command)
	}
	return commands
}
