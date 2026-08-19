package goldentrace_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests in this package deliberately use only the compiled CLI.  The
// provider driver below is a small process-level fake: the trace's only
// observations are the CLI's stdout JSON and exit status.

var (
	traceBuildOnce sync.Once
	traceBinary    string
	traceBuildDir  string
	traceBuildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if traceBuildDir != "" {
		_ = os.RemoveAll(traceBuildDir)
	}
	os.Exit(code)
}

type traceEnv struct {
	binary      string
	repoRoot    string
	root        string
	bin         string
	home        string
	relayHome   string
	workDir     string
	planPath    string
	statePath   string
	startedPath string
}

type commandResult struct {
	stdout   string
	stderr   string
	exitCode int
	duration time.Duration
}

func newTraceEnv(t *testing.T, plan map[string]any) *traceEnv {
	t.Helper()
	repoRoot := traceModuleRoot(t)
	root := t.TempDir()
	env := &traceEnv{
		binary:      traceBinaryFor(t),
		repoRoot:    repoRoot,
		root:        root,
		bin:         filepath.Join(root, "bin"),
		home:        filepath.Join(root, "home"),
		relayHome:   filepath.Join(root, "relay-home"),
		workDir:     filepath.Join(root, "work"),
		planPath:    filepath.Join(root, "provider-plan.json"),
		statePath:   filepath.Join(root, "provider-state.json"),
		startedPath: filepath.Join(root, "provider-started"),
	}
	for _, dir := range []string{env.bin, env.home, env.relayHome, env.workDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create trace directory %s: %v", dir, err)
		}
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("encode provider plan: %v", err)
	}
	if err := os.WriteFile(env.planPath, encoded, 0o600); err != nil {
		t.Fatalf("write provider plan: %v", err)
	}
	if err := os.WriteFile(env.statePath, []byte(`{"calls":{},"threads":{}}`), 0o600); err != nil {
		t.Fatalf("write provider state: %v", err)
	}
	installTraceProviders(t, env)
	return env
}

func (env *traceEnv) command(args ...string) *exec.Cmd {
	cmd := exec.Command(env.binary, args...)
	cmd.Dir = env.repoRoot
	cmd.Env = overlayEnv(os.Environ(), map[string]string{
		"PATH":                              env.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME":                              env.home,
		"XDG_CONFIG_HOME":                   filepath.Join(env.home, "xdg-config"),
		"XDG_CACHE_HOME":                    filepath.Join(env.home, "xdg-cache"),
		"XDG_STATE_HOME":                    filepath.Join(env.home, "xdg-state"),
		"CODEX_HOME":                        filepath.Join(env.home, "codex-default"),
		"CLAUDE_CONFIG_DIR":                 filepath.Join(env.home, "claude-config"),
		"GEMINI_CLI_HOME":                   filepath.Join(env.home, "gemini-default"),
		"CONVO_RELAY_TRACE_PROVIDER_DRIVER": filepath.Join(env.bin, "provider.py"),
		"CONVO_RELAY_TRACE_PLAN":            env.planPath,
		"CONVO_RELAY_TRACE_STATE":           env.statePath,
		"CONVO_RELAY_TRACE_STARTED":         env.startedPath,
	})
	return cmd
}

func (env *traceEnv) run(t *testing.T, args ...string) commandResult {
	t.Helper()
	cmd := env.command(args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	started := time.Now()
	err := cmd.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), duration: time.Since(started)}
	if err == nil {
		return result
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("start %q: %v", strings.Join(args, " "), err)
	}
	result.exitCode = exitErr.ExitCode()
	return result
}

func (env *traceEnv) runJSONArgs(args ...string) []string {
	command := make([]string, 0, len(args)+8)
	command = append(command, "run", "--home", env.relayHome)
	command = append(command, args...)
	command = append(command, "--timeout", "30", "--stall-timeout", "30", "--json")
	return command
}

func (env *traceEnv) runJSON(t *testing.T, args ...string) (commandResult, map[string]any) {
	t.Helper()
	result := env.run(t, env.runJSONArgs(args...)...)
	return result, mustJSON(t, result)
}

func (env *traceEnv) resumeJSON(t *testing.T, sessionID string, args ...string) (commandResult, map[string]any) {
	t.Helper()
	command := make([]string, 0, len(args)+9)
	command = append(command, "resume", "--home", env.relayHome)
	command = append(command, args...)
	command = append(command, "--timeout", "30", "--stall-timeout", "30", "--json", sessionID)
	result := env.run(t, command...)
	return result, mustJSON(t, result)
}

func (env *traceEnv) showJSON(t *testing.T, sessionID string) (commandResult, map[string]any) {
	t.Helper()
	result := env.run(t, "show", "--home", env.relayHome, "--json", sessionID)
	return result, mustJSON(t, result)
}

func traceBinaryFor(t *testing.T) string {
	t.Helper()
	traceBuildOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			traceBuildErr = err
			return
		}
		traceBuildDir, err = os.MkdirTemp("", "convo-relay-goldentrace-bin-")
		if err != nil {
			traceBuildErr = err
			return
		}
		traceBinary = filepath.Join(traceBuildDir, "convo-relay")
		cmd := exec.Command("go", "build", "-o", traceBinary, "./cmd/convo-relay")
		cmd.Dir = root
		// Inherit the ambient Go environment. Pinning build settings or forcing offline
		// module lookup here makes the build depend on one machine's pre-populated cache:
		// it passes locally and fails anywhere the cache is cold, CI included.
		cmd.Env = os.Environ()
		output, buildErr := cmd.CombinedOutput()
		if buildErr != nil {
			traceBuildErr = fmt.Errorf("build convo-relay trace binary: %w\n%s", buildErr, output)
		}
	})
	if traceBuildErr != nil {
		t.Fatal(traceBuildErr)
	}
	return traceBinary
}

func traceModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if info, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil && !info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("find module root from %s", dir)
		}
		dir = parent
	}
}

func overlayEnv(base []string, additions map[string]string) []string {
	result := make([]string, 0, len(base)+len(additions))
	for _, item := range base {
		key, _, found := strings.Cut(item, "=")
		if found {
			if _, overridden := additions[key]; overridden {
				continue
			}
		}
		result = append(result, item)
	}
	keys := make([]string, 0, len(additions))
	for key := range additions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+additions[key])
	}
	return result
}

func installTraceProviders(t *testing.T, env *traceEnv) {
	t.Helper()
	driver := filepath.Join(env.bin, "provider.py")
	if err := os.WriteFile(driver, []byte(traceProviderDriver), 0o755); err != nil {
		t.Fatalf("write fake-provider driver: %v", err)
	}
	for _, provider := range []string{"codex", "claude", "gemini"} {
		wrapper := "#!/bin/sh\nexec python3 \"$CONVO_RELAY_TRACE_PROVIDER_DRIVER\" " + provider + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(env.bin, provider), []byte(wrapper), 0o755); err != nil {
			t.Fatalf("write fake provider %s: %v", provider, err)
		}
	}
}

func mustJSON(t *testing.T, result commandResult) map[string]any {
	t.Helper()
	value := map[string]any{}
	if err := json.Unmarshal([]byte(result.stdout), &value); err != nil {
		t.Fatalf("decode JSON stdout (exit %d): %v\nstdout:\n%s\nstderr:\n%s", result.exitCode, err, result.stdout, result.stderr)
	}
	return value
}

func requiredJSONField(t *testing.T, object map[string]any, field string, label string) any {
	t.Helper()
	value, found := object[field]
	if !found {
		t.Fatalf("%s is missing required field %q: %#v", label, field, object)
	}
	return value
}

func resultStatus(t *testing.T, result map[string]any) string {
	t.Helper()
	return jsonString(t, requiredJSONField(t, result, "status", "result"), "result.status")
}

func resultRoot(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	return jsonMap(t, requiredJSONField(t, result, "root", "result"), "result.root")
}

func resultSummary(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	return jsonMap(t, requiredJSONField(t, result, "summary", "result"), "result.summary")
}

func resultActualRounds(t *testing.T, result map[string]any) int {
	t.Helper()
	return jsonInt(t, requiredJSONField(t, result, "actual_rounds", "result"), "result.actual_rounds")
}

func resultSource(t *testing.T, result map[string]any) string {
	t.Helper()
	return jsonString(t, requiredJSONField(t, result, "result_source", "result"), "result.result_source")
}

func resultValidationStatus(t *testing.T, result map[string]any) string {
	t.Helper()
	return jsonString(t, requiredJSONField(t, result, "validation_status", "result"), "result.validation_status")
}

func requireExit(t *testing.T, result commandResult, want int) {
	t.Helper()
	if result.exitCode != want {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", result.exitCode, want, result.stdout, result.stderr)
	}
}

func jsonMap(t *testing.T, value any, label string) map[string]any {
	t.Helper()
	item, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object: %#v", label, value, value)
	}
	return item
}

func jsonSlice(t *testing.T, value any, label string) []any {
	t.Helper()
	item, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T, want array: %#v", label, value, value)
	}
	return item
}

func jsonString(t *testing.T, value any, label string) string {
	t.Helper()
	item, ok := value.(string)
	if !ok {
		t.Fatalf("%s is %T, want string: %#v", label, value, value)
	}
	return item
}

func jsonInt(t *testing.T, value any, label string) int {
	t.Helper()
	switch item := value.(type) {
	case float64:
		return int(item)
	case int:
		return item
	default:
		t.Fatalf("%s is %T, want number: %#v", label, value, value)
		return 0
	}
}

func transcript(t *testing.T, report map[string]any) []map[string]any {
	t.Helper()
	raw := jsonSlice(t, requiredJSONField(t, report, "transcript", "result"), "transcript")
	entries := make([]map[string]any, 0, len(raw))
	for index, item := range raw {
		entries = append(entries, jsonMap(t, item, fmt.Sprintf("transcript[%d]", index)))
	}
	return entries
}

func slotThreadID(t *testing.T, report map[string]any, slotID string) string {
	t.Helper()
	for _, raw := range jsonSlice(t, report["slots"], "slots") {
		slot := jsonMap(t, raw, "slot")
		if slot["slot_id"] != slotID {
			continue
		}
		state := jsonMap(t, slot["state"], "slot state")
		return jsonString(t, state["thread_id"], "thread_id")
	}
	t.Fatalf("slot %q missing from %#v", slotID, report["slots"])
	return ""
}

func listSessions(t *testing.T, env *traceEnv) []map[string]any {
	t.Helper()
	result := env.run(t, "list", "--home", env.relayHome, "--json")
	requireExit(t, result, 0)
	report := mustJSON(t, result)
	raw := jsonSlice(t, report["sessions"], "sessions")
	items := make([]map[string]any, 0, len(raw))
	for index, item := range raw {
		items = append(items, jsonMap(t, item, fmt.Sprintf("sessions[%d]", index)))
	}
	return items
}

func containsSession(items []map[string]any, sessionID string) bool {
	for _, item := range items {
		if item["session_id"] == sessionID {
			return true
		}
	}
	return false
}

func addedSessionID(t *testing.T, before []map[string]any, after []map[string]any) string {
	t.Helper()
	existing := make(map[string]bool, len(before))
	for index, item := range before {
		existing[jsonString(t, item["session_id"], fmt.Sprintf("before session[%d].session_id", index))] = true
	}
	added := []string{}
	for index, item := range after {
		sessionID := jsonString(t, item["session_id"], fmt.Sprintf("after session[%d].session_id", index))
		if !existing[sessionID] {
			added = append(added, sessionID)
		}
	}
	if len(added) != 1 {
		t.Fatalf("expected exactly one newly listed child session, before=%#v after=%#v", before, after)
	}
	return added[0]
}

func hasText(entries []map[string]any, token string) bool {
	for _, entry := range entries {
		if strings.Contains(fmt.Sprint(entry["content"]), token) {
			return true
		}
	}
	return false
}

func countText(entries []map[string]any, token string) int {
	count := 0
	for _, entry := range entries {
		if strings.Contains(fmt.Sprint(entry["content"]), token) {
			count++
		}
	}
	return count
}

func fixturePath(t *testing.T, env *traceEnv, name string) string {
	t.Helper()
	path := filepath.Join(env.repoRoot, "testdata", "goldentrace", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	return path
}

func writeTraceFixture(t *testing.T, env *traceEnv, name string, contents string) string {
	t.Helper()
	path := filepath.Join(env.root, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write trace fixture %s: %v", name, err)
	}
	return path
}

func reply(text string) map[string]any {
	return map[string]any{"text": text}
}

func failure(detail string) map[string]any {
	return map[string]any{"mode": "failure", "detail": detail}
}

func ledger(items ...string) map[string]any {
	body, err := json.Marshal(map[string]any{
		"settled":   items,
		"contested": []string{},
		"withdrawn": []string{},
	})
	if err != nil {
		panic(err)
	}
	return reply(string(body))
}

func contestedLedger(item string) map[string]any {
	body, err := json.Marshal(map[string]any{
		"settled":   []string{},
		"contested": []string{item},
		"withdrawn": []string{},
	})
	if err != nil {
		panic(err)
	}
	return reply(string(body))
}

func scriptedPlan(roles map[string][]map[string]any) map[string]any {
	return map[string]any{
		"roles":   roles,
		"default": ledger(),
	}
}

func TestTwoActorDialogue(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0":      {reply("TRACE_ACTOR_A_TURN1")},
		"claude":      {reply("TRACE_ACTOR_B_TURN2")},
		"facilitator": {ledger("TRACE_DIALOGUE_SETTLED")},
	}))

	run, report := env.runJSON(t,
		"--session-id", "two-actors",
		"--task", "TRACE_TWO_ACTOR_TASK", "--agents", "codex,claude", "--rounds", "2",
		"--facilitator-backend", "codex",
	)
	requireExit(t, run, 0)
	if got := resultStatus(t, report); got != "completed" {
		t.Fatalf("run status = %#v", got)
	}

	show, showReport := env.showJSON(t, "two-actors")
	requireExit(t, show, 0)
	entries := transcript(t, showReport)
	if len(entries) != 2 || entries[0]["content"] != "TRACE_ACTOR_A_TURN1" || entries[1]["content"] != "TRACE_ACTOR_B_TURN2" || entries[0]["from"] == entries[1]["from"] {
		t.Fatalf("public dialogue transcript = %#v", entries)
	}
}

func TestFacilitatorTurn(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0":      {reply("TRACE_FACILITATED_PARTICIPANT")},
		"facilitator": {ledger("TRACE_FACILITATOR_CONTRIBUTION")},
	}))

	run, _ := env.runJSON(t,
		"--session-id", "facilitated",
		"--task", "TRACE_FACILITATOR_TASK", "--recipe", "trace-facilitated",
		"--settings", fixturePath(t, env, "root-recipes.toml"), "--launch-cwd", env.workDir,
	)
	requireExit(t, run, 0)

	show, showReport := env.showJSON(t, "facilitated")
	requireExit(t, show, 0)
	entries := transcript(t, showReport)
	if len(entries) != 1 {
		t.Fatalf("facilitated transcript = %#v", entries)
	}
	settled := jsonSlice(t, jsonMap(t, entries[0]["ledger"], "facilitator ledger")["settled"], "facilitator settled")
	if len(settled) != 1 || settled[0] != "TRACE_FACILITATOR_CONTRIBUTION" {
		t.Fatalf("facilitator contribution missing from public ledger: %#v", entries[0])
	}
	facilitatorResult := jsonMap(t, entries[0]["facilitator_provider_result"], "facilitator provider result")
	if facilitatorResult["backend"] != "codex" {
		t.Fatalf("facilitator attribution = %#v", facilitatorResult)
	}
}

func TestConvergenceStop(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0":      {reply("TRACE_CONVERGENCE_A_TURN1"), reply("TRACE_CONVERGENCE_A_TURN3")},
		"slot_1":      {reply("TRACE_CONVERGENCE_B_TURN2"), reply("TRACE_CONVERGENCE_B_TURN4")},
		"facilitator": {ledger("TRACE_CONVERGENCE_SETTLED")},
	}))

	run, report := env.runJSON(t,
		"--session-id", "convergence",
		"--task", "TRACE_CONVERGENCE_TASK", "--agents", "gemini", "--max-rounds", "8",
	)
	requireExit(t, run, 0)
	if resultStatus(t, report) != "completed" || requiredJSONField(t, report, "stop_reason", "result") != "converged" || resultActualRounds(t, report) >= 8 {
		t.Fatalf("convergence result = %#v", report)
	}
}

func TestDeclarativeRecipeRun(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0":      {reply("TRACE_DECLARATIVE_ALPHA")},
		"slot_1":      {reply("TRACE_DECLARATIVE_BETA")},
		"facilitator": {ledger("TRACE_DECLARATIVE_LEDGER")},
	}))

	run, report := env.runJSON(t,
		"--session-id", "declarative",
		"--task", "TRACE_DECLARATIVE_TASK", "--recipe", "trace-declarative",
		"--settings", fixturePath(t, env, "root-recipes.toml"), "--launch-cwd", env.workDir,
	)
	requireExit(t, run, 0)
	if resultStatus(t, report) != "completed" || jsonInt(t, requiredJSONField(t, report, "actual_participant_turns", "result"), "result.actual_participant_turns") != 2 {
		t.Fatalf("recipe run result = %#v", report)
	}
	slots := jsonSlice(t, report["slots"], "recipe slots")
	if len(slots) != 2 || jsonMap(t, slots[0], "slot 0")["profile_id"] != "trace-alpha" || jsonMap(t, slots[1], "slot 1")["profile_id"] != "trace-beta" {
		t.Fatalf("recipe actors did not come from fixture: %#v", slots)
	}

	show, showReport := env.showJSON(t, "declarative")
	requireExit(t, show, 0)
	root := resultRoot(t, showReport)
	if jsonMap(t, requiredJSONField(t, root, "recipe", "result.root"), "recipe report")["id"] != "trace-declarative" || resultActualRounds(t, resultSummary(t, showReport)) != 2 {
		t.Fatalf("recipe public report = %#v", showReport)
	}
}

func TestFixedSequenceWithReducer(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0":      {reply("TRACE_FIXED_SEQUENCE_FIRST")},
		"slot_1":      {reply("TRACE_FIXED_SEQUENCE_SECOND")},
		"facilitator": {ledger("TRACE_FIXED_SEQUENCE_LEDGER")},
		"reducer": {{
			"text":           `{"result":"TRACE_REDUCER_RESULT"}`,
			"require_prompt": "TRACE_FIXED_SEQUENCE_SECOND",
		}},
	}))
	bundle := writeTraceFixture(t, env, "reducer-bundle.json", `{
  "schema_version": "relay-integration-bundle-v2",
  "id": "goldentrace/reducer-bundle",
  "contracts": {
    "goldentrace/reducer-v1": {
      "turns": [
        {"participant_turn": 1, "slot": "slot_0", "instructions": "Provide the first fixed-sequence response."},
        {"participant_turn": 2, "slot": "slot_1", "instructions": "Provide the second fixed-sequence response."}
      ],
      "reducer": {"instructions": "Return the final reducer JSON object."},
      "inputs": {},
      "result": {
        "transport": "json",
        "schema": {
          "type": "object",
          "required": ["result"],
          "properties": {"result": {"type": "string", "enum": ["TRACE_REDUCER_RESULT"]}}
        }
      },
      "prompt_context": {"participant_transcript": "complete", "facilitator_ledger": "include"}
    }
  }
}`)

	run, report := env.runJSON(t,
		"--session-id", "fixed-reducer",
		"--task", "TRACE_FIXED_REDUCER_TASK", "--recipe", "trace-reducer",
		"--settings", fixturePath(t, env, "root-recipes.toml"), "--integration-bundle", bundle,
		"--launch-cwd", env.workDir,
	)
	requireExit(t, run, 0)
	if resultStatus(t, report) != "completed" || resultSource(t, report) != "reducer" || resultValidationStatus(t, report) != "validated" {
		t.Fatalf("fixed reducer run result = %#v", report)
	}

	show, showReport := env.showJSON(t, "fixed-reducer")
	requireExit(t, show, 0)
	entries := transcript(t, showReport)
	if len(entries) != 2 || entries[0]["content"] != "TRACE_FIXED_SEQUENCE_FIRST" || entries[1]["content"] != "TRACE_FIXED_SEQUENCE_SECOND" {
		t.Fatalf("fixed participant sequence = %#v", entries)
	}
	root := resultRoot(t, showReport)
	result := jsonMap(t, requiredJSONField(t, root, "result", "result.root"), "root result")
	reducerAttempts := jsonMap(t, requiredJSONField(t, root, "reducer_attempts", "result.root"), "reducer attempts")
	if jsonString(t, requiredJSONField(t, result, "source", "root result"), "root result.source") != "reducer" || resultValidationStatus(t, result) != "validated" || jsonInt(t, requiredJSONField(t, reducerAttempts, "count", "reducer attempts"), "reducer attempt count") != 1 {
		t.Fatalf("public reducer result projection = %#v", root)
	}
}

func TestProviderSessionContinuation(t *testing.T) {
	plan := scriptedPlan(map[string][]map[string]any{
		"slot_0":      {reply("TRACE_CONTINUATION_FIRST"), reply("TRACE_CONTINUATION_SECOND")},
		"slot_1":      {reply("TRACE_CONTINUATION_OTHER")},
		"facilitator": {ledger("TRACE_CONTINUATION_LEDGER")},
	})
	plan["strict_resume_roles"] = []string{"slot_0"}
	env := newTraceEnv(t, plan)

	first, firstReport := env.runJSON(t,
		"--session-id", "provider-continuation",
		"--task", "TRACE_PROVIDER_CONTINUATION", "--agents", "codex", "--rounds", "1",
	)
	requireExit(t, first, 0)
	firstThread := slotThreadID(t, firstReport, "slot_0")

	resumed, resumedReport := env.resumeJSON(t, "provider-continuation",
		"--rounds", "2",
	)
	requireExit(t, resumed, 0)
	if got := slotThreadID(t, resumedReport, "slot_0"); got != firstThread {
		t.Fatalf("public codex thread identity changed: first=%q resumed=%q", firstThread, got)
	}
	if !hasText(transcript(t, resumedReport), "TRACE_CONTINUATION_SECOND") {
		t.Fatalf("resumed conversation omitted second provider turn: %#v", resumedReport["transcript"])
	}
}

func TestProviderRetryThenSuccess(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0":      {failure("API Error: TRACE_TRANSIENT_RETRY"), reply("TRACE_RETRY_SUCCESS_AFTER_ATTEMPT_TWO")},
		"facilitator": {ledger("TRACE_RETRY_LEDGER")},
	}))

	run, report := env.runJSON(t,
		"--session-id", "retry-success",
		"--task", "TRACE_RETRY_TASK", "--recipe", "trace-retry",
		"--settings", fixturePath(t, env, "root-recipes.toml"), "--launch-cwd", env.workDir,
	)
	requireExit(t, run, 0)
	if resultStatus(t, report) != "completed" || !hasText(transcript(t, report), "TRACE_RETRY_SUCCESS_AFTER_ATTEMPT_TWO") {
		t.Fatalf("retry success is not visible through the public transcript: %#v", report)
	}
	show, showReport := env.showJSON(t, "retry-success")
	requireExit(t, show, 0)
	root := resultRoot(t, showReport)
	invocations := jsonMap(t, requiredJSONField(t, root, "invocations", "result.root"), "provider invocations")
	if requiredJSONField(t, root, "provider_retry", "result.root") != "allow" || jsonInt(t, requiredJSONField(t, invocations, "count", "provider invocations"), "provider invocation count") != 3 {
		t.Fatalf("public provider retry report = %#v", root)
	}
}

func TestAuthFailureIsNotRetried(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0": {failure("Authentication error: TRACE_AUTH_FAILURE"), reply("TRACE_AUTH_FAILURE_WAS_RETRIED")},
	}))

	run, report := env.runJSON(t,
		"--session-id", "auth-failure",
		"--task", "TRACE_AUTH_TASK", "--agents", "gemini", "--rounds", "1",
	)
	if run.exitCode == 0 {
		t.Fatalf("auth failure unexpectedly succeeded\nstdout:\n%s\nstderr:\n%s", run.stdout, run.stderr)
	}
	if resultStatus(t, report) != "failed" {
		t.Fatalf("auth failure status = %#v", report)
	}
	failures := jsonSlice(t, requiredJSONField(t, report, "provider_failures", "result"), "provider_failures")
	if len(failures) != 1 {
		t.Fatalf("auth provider failures = %#v", failures)
	}
	failureReport := jsonMap(t, failures[0], "auth failure")
	if requiredJSONField(t, failureReport, "category", "auth failure") != "auth" || requiredJSONField(t, failureReport, "retryable", "auth failure") != false || jsonInt(t, requiredJSONField(t, failureReport, "attempts", "auth failure"), "auth attempts") != 1 {
		t.Fatalf("auth failure public report = %#v", failureReport)
	}
}

func TestResumeContinuesSession(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0": {reply("TRACE_RESUME_INITIAL")},
		"slot_1": {{
			"text":           "TRACE_RESUME_FURTHER_PROMPT",
			"require_prompt": "TRACE_RESUME_DIRECTION",
		}},
		"facilitator": {ledger("TRACE_RESUME_LEDGER")},
	}))

	first, firstReport := env.runJSON(t,
		"--session-id", "resume-session",
		"--task", "TRACE_RESUME_TASK", "--agents", "codex", "--rounds", "1",
	)
	requireExit(t, first, 0)

	resumed, resumedReport := env.resumeJSON(t, "resume-session",
		"--prompt", "TRACE_RESUME_DIRECTION", "--rounds", "1",
	)
	requireExit(t, resumed, 0)
	if jsonString(t, requiredJSONField(t, firstReport, "session_id", "initial result"), "initial result.session_id") != jsonString(t, requiredJSONField(t, resumedReport, "session_id", "resumed result"), "resumed result.session_id") {
		t.Fatalf("resume public result = %#v", resumedReport)
	}
	entries := transcript(t, resumedReport)
	initialIndex, furtherIndex := -1, -1
	for index, entry := range entries {
		if strings.Contains(fmt.Sprint(entry["content"]), "TRACE_RESUME_INITIAL") {
			initialIndex = index
		}
		if strings.Contains(fmt.Sprint(entry["content"]), "TRACE_RESUME_FURTHER_PROMPT") {
			furtherIndex = index
		}
	}
	if initialIndex < 0 || furtherIndex < 0 || initialIndex >= furtherIndex || resultActualRounds(t, resumedReport) != 2 {
		t.Fatalf("resume history or round count = entries=%#v actual_rounds=%d", entries, resultActualRounds(t, resumedReport))
	}
}

func TestSteering(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0": {reply("TRACE_STEERING_BEFORE")},
		"slot_1": {{
			"text":           "TRACE_STEERING_DELIVERED",
			"require_prompt": "TRACE_STEERING_PROMPT",
		}},
		"facilitator": {ledger("TRACE_STEERING_LEDGER")},
	}))

	seed, _ := env.runJSON(t,
		"--session-id", "steering-session",
		"--task", "TRACE_STEERING_TASK", "--agents", "codex", "--rounds", "1",
	)
	requireExit(t, seed, 0)
	queued := env.run(t, "steer", "--home", env.relayHome, "--json", "steering-session", "TRACE_STEERING_PROMPT")
	requireExit(t, queued, 0)
	resumed, resumedReport := env.resumeJSON(t, "steering-session",
		"--rounds", "1",
	)
	requireExit(t, resumed, 0)
	if !hasText(transcript(t, resumedReport), "TRACE_STEERING_DELIVERED") {
		t.Fatalf("queued steering was not delivered to the next public conversation turn")
	}
}

func TestCancellation(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0": {{"mode": "wait"}},
	}))
	cmd := env.command(env.runJSONArgs(
		"--session-id", "cancellation",
		"--task", "TRACE_CANCELLATION_TASK", "--agents", "gemini", "--rounds", "1",
	)...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cancellation run: %v", err)
	}
	if !waitForTraceProvider(env.startedPath) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		state, _ := os.ReadFile(env.statePath)
		t.Fatalf("fake provider did not reach its cancellable turn; state=%s\nstdout:\n%s\nstderr:\n%s", state, stdout.String(), stderr.String())
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt relay: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled relay did not terminate")
	}
	if runErr == nil {
		t.Fatalf("cancelled relay exited successfully\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: 1}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.exitCode = exitErr.ExitCode()
	}
	report := mustJSON(t, result)
	if resultStatus(t, report) != "interrupted" {
		t.Fatalf("cancelled terminal result = %#v", report)
	}
}

func TestBoundedChildRelayWithAdmission(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0":      {reply("TRACE_PARENT_A"), reply("TRACE_CHILD_REPLY")},
		"slot_1":      {reply("TRACE_PARENT_B")},
		"facilitator": {contestedLedger("TRACE_CHILD_CONTESTED")},
	}))
	settings := fixturePath(t, env, "root-recipes.toml")
	parent, _ := env.runJSON(t,
		"--session-id", "child-parent",
		"--task", "TRACE_CHILD_PARENT_TASK", "--agents", "codex", "--rounds", "2", "--dynamic", "ask",
		"--settings", settings,
	)
	requireExit(t, parent, 0)
	proposalID := graphProposalID(t, env, "child-parent")
	beforeApproval := listSessions(t, env)
	approval := env.run(t,
		"approve", "--home", env.relayHome, "--proposal", proposalID, "--rounds", "1",
		"--timeout", "30", "--stall-timeout", "30", "child-parent",
	)
	requireExit(t, approval, 0)
	afterApproval := listSessions(t, env)
	childSessionID := addedSessionID(t, beforeApproval, afterApproval)
	if proposal := graphProposal(t, env, "child-parent", proposalID); jsonString(t, requiredJSONField(t, proposal, "status", "graph proposal"), "graph proposal.status") != "collapsed" {
		t.Fatalf("approved child proposal was not collapsed: %#v", proposal)
	}

	parentShow, parentShowReport := env.showJSON(t, "child-parent")
	requireExit(t, parentShow, 0)
	parentEntries := transcript(t, parentShowReport)
	if countText(parentEntries, "TRACE_CHILD_REPLY") != 1 {
		t.Fatalf("child result did not appear exactly once in parent conversation: %#v", parentEntries)
	}
	if !containsSession(afterApproval, childSessionID) {
		t.Fatalf("admitted child %q is not separately listed", childSessionID)
	}
	childShow, childReport := env.showJSON(t, childSessionID)
	requireExit(t, childShow, 0)
	if resultActualRounds(t, resultSummary(t, childReport)) != 1 || !hasText(transcript(t, childReport), "TRACE_CHILD_REPLY") {
		t.Fatalf("separately inspected child omitted its response")
	}

	denied := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0":      {reply("TRACE_DENIED_PARENT_A"), reply("TRACE_DENIED_CHILD_MUST_NOT_RUN")},
		"slot_1":      {reply("TRACE_DENIED_PARENT_B")},
		"facilitator": {contestedLedger("TRACE_CHILD_CONTESTED")},
	}))
	deniedSettings := fixturePath(t, denied, "root-recipes.toml")
	seedDenied, _ := denied.runJSON(t,
		"--session-id", "denied-parent",
		"--task", "TRACE_DENIED_CHILD_TASK", "--agents", "codex", "--rounds", "2", "--dynamic", "ask",
		"--settings", deniedSettings,
	)
	requireExit(t, seedDenied, 0)
	deniedProposalID := graphProposalID(t, denied, "denied-parent")
	before := listSessions(t, denied)
	rejected := denied.run(t, "reject", "--home", denied.relayHome, "--proposal", deniedProposalID, "denied-parent")
	requireExit(t, rejected, 0)
	after := listSessions(t, denied)
	if len(after) != len(before) || !containsSession(after, "denied-parent") {
		t.Fatalf("denied child changed public session list: before=%#v after=%#v", before, after)
	}
	if proposal := graphProposal(t, denied, "denied-parent", deniedProposalID); jsonString(t, requiredJSONField(t, proposal, "status", "graph proposal"), "graph proposal.status") != "rejected" {
		t.Fatalf("denied child proposal was not rejected: %#v", proposal)
	}
	deniedShow, deniedShowReport := denied.showJSON(t, "denied-parent")
	requireExit(t, deniedShow, 0)
	if hasText(transcript(t, deniedShowReport), "TRACE_DENIED_CHILD_MUST_NOT_RUN") {
		t.Fatalf("denied child executed despite rejection")
	}
}

func TestCommittedHeadExecution(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0": {{
			"mode":          "source",
			"source_file":   "trace-source.txt",
			"expect":        "TRACE_COMMITTED_BYTES\n",
			"text":          "TRACE_COMMITTED_HEAD_SEEN",
			"mismatch_text": "TRACE_DIRTY_WORKTREE_SEEN",
		}},
		"facilitator": {ledger("TRACE_COMMITTED_LEDGER")},
	}))
	source := filepath.Join(env.root, "source-repository")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("create fixture repository: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "trace-source.txt"), []byte("TRACE_COMMITTED_BYTES\n"), 0o644); err != nil {
		t.Fatalf("write committed fixture: %v", err)
	}
	traceGit(t, source, "init", "-q")
	traceGit(t, source, "add", "trace-source.txt")
	traceGit(t, source, "commit", "--no-verify", "-q", "-m", "trace committed source")
	if err := os.WriteFile(filepath.Join(source, "trace-source.txt"), []byte("TRACE_DIRTY_BYTES\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted fixture edit: %v", err)
	}

	run, report := env.runJSON(t,
		"--session-id", "committed-head",
		"--task", "TRACE_COMMITTED_HEAD_TASK", "--recipe", "trace-isolated",
		"--settings", fixturePath(t, env, "root-recipes.toml"), "--launch-cwd", source,
		"--workspace-isolation", "ephemeral", "--allow-dirty-source",
	)
	requireExit(t, run, 0)
	if resultStatus(t, report) != "completed" {
		t.Fatalf("committed-head run = %#v", report)
	}
	show, showReport := env.showJSON(t, "committed-head")
	requireExit(t, show, 0)
	workspace := jsonMap(t, requiredJSONField(t, resultRoot(t, showReport), "workspace", "result.root"), "workspace report")
	if requiredJSONField(t, workspace, "workspace_content_source", "workspace report") != "committed_head" || requiredJSONField(t, workspace, "working_tree_changes_included", "workspace report") != false {
		t.Fatalf("public workspace provenance = %#v", workspace)
	}
	entries := transcript(t, report)
	if !hasText(entries, "TRACE_COMMITTED_HEAD_SEEN") || hasText(entries, "TRACE_DIRTY_WORKTREE_SEEN") {
		t.Fatalf("provider did not observe committed HEAD bytes: execution_cwd=%#v source=%q workspace=%#v transcript=%#v", report["execution_cwd"], source, workspace, entries)
	}
}

func TestPortableExportRoundTripAndTamperDetection(t *testing.T) {
	env := newTraceEnv(t, scriptedPlan(map[string][]map[string]any{
		"slot_0":      {reply("TRACE_EXPORT_SOURCE")},
		"facilitator": {ledger("TRACE_EXPORT_LEDGER")},
	}))
	run, _ := env.runJSON(t,
		"--session-id", "portable-export",
		"--task", "TRACE_PORTABLE_EXPORT_TASK", "--recipe", "trace-facilitated",
		"--settings", fixturePath(t, env, "root-recipes.toml"), "--launch-cwd", env.workDir,
	)
	requireExit(t, run, 0)

	bundle := filepath.Join(env.root, "portable-bundle")
	exported := env.run(t, "export", "--home", env.relayHome, "--portable", "-o", bundle, "--json", "portable-export")
	requireExit(t, exported, 0)
	exportedReport := mustJSON(t, exported)
	if got := requiredJSONField(t, exportedReport, "output", "portable export result"); got != bundle {
		t.Fatalf("portable export output = %#v, want %q", got, bundle)
	}
	verified := env.run(t, "verify-export", "--json", bundle)
	requireExit(t, verified, 0)
	verifiedReport := mustJSON(t, verified)
	if resultStatus(t, verifiedReport) != "valid" {
		t.Fatalf("portable verification = %#v", verifiedReport)
	}

	target := portableInventoryPayload(t, bundle, "participant_transcript")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read exported file %s: %v", target, err)
	}
	if !json.Valid(data) {
		t.Fatalf("selected payload %s is not valid JSON", target)
	}
	tamperedData := bytes.Replace(data, []byte("TRACE_EXPORT_SOURCE"), []byte("TRACE_EXPORT_SOURCe"), 1)
	if bytes.Count(data, []byte("TRACE_EXPORT_SOURCE")) != 1 || len(tamperedData) != len(data) || !json.Valid(tamperedData) {
		t.Fatalf("payload tamper must replace exactly one same-length JSON string: %s", target)
	}
	if err := os.WriteFile(target, tamperedData, 0o644); err != nil {
		t.Fatalf("tamper exported file %s: %v", target, err)
	}
	if info, statErr := os.Stat(target); statErr != nil || info.Size() != int64(len(data)) {
		t.Fatalf("tampered payload size changed or cannot be read: info=%#v err=%v", info, statErr)
	}
	tampered := env.run(t, "verify-export", "--json", bundle)
	if tampered.exitCode == 0 {
		t.Fatalf("tampered portable export unexpectedly verified: %s", tampered.stdout)
	}
	tamperedReport := mustJSON(t, tampered)
	if resultStatus(t, tamperedReport) != "invalid" {
		t.Fatalf("tampered verification result = %#v", tamperedReport)
	}
	integrityError := jsonString(t, requiredJSONField(t, tamperedReport, "error", "tampered verify-export result"), "tampered verify-export result.error")
	if !strings.Contains(integrityError, "portable export payload") || !strings.Contains(integrityError, "size or digest mismatch") || strings.Contains(strings.ToLower(integrityError), "decode") {
		t.Fatalf("tampered payload failed for the wrong reason: %q", integrityError)
	}
}

func waitForTraceProvider(path string) bool {
	// Generous deadline on purpose: this is a poll loop, so a high ceiling costs
	// nothing when the provider starts promptly, and a tight one turns ordinary
	// machine load into a spurious failure. A 2s ceiling flaked under -race.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func graphProposalID(t *testing.T, env *traceEnv, sessionID string) string {
	t.Helper()
	proposals := graphProposals(t, env, sessionID)
	if len(proposals) != 1 {
		t.Fatalf("expected exactly one public child proposal, got %#v", proposals)
	}
	for id := range proposals {
		return id
	}
	t.Fatal("missing graph proposal")
	return ""
}

func graphProposal(t *testing.T, env *traceEnv, sessionID string, proposalID string) map[string]any {
	t.Helper()
	proposal, found := graphProposals(t, env, sessionID)[proposalID]
	if !found {
		t.Fatalf("public child proposal %q missing", proposalID)
	}
	return jsonMap(t, proposal, "graph proposal")
}

func graphProposals(t *testing.T, env *traceEnv, sessionID string) map[string]any {
	t.Helper()
	result := env.run(t, "show-graph", "--home", env.relayHome, "--json", sessionID)
	requireExit(t, result, 0)
	graph := jsonMap(t, mustJSON(t, result)["graph"], "graph")
	proposals := jsonMap(t, graph["proposals"], "graph proposals")
	return proposals
}

func traceGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	fixtureRoot := filepath.Dir(dir)
	home := filepath.Join(fixtureRoot, "fixture-git-home")
	xdgConfig := filepath.Join(fixtureRoot, "fixture-git-xdg-config")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create isolated git HOME: %v", err)
	}
	if err := os.MkdirAll(xdgConfig, 0o755); err != nil {
		t.Fatalf("create isolated git XDG config: %v", err)
	}
	command := make([]string, 0, len(args)+4)
	command = append(command,
		"-c", "commit.gpgSign=false",
		"-c", "core.hooksPath="+filepath.Join(fixtureRoot, "fixture-git-hooks-disabled"),
	)
	command = append(command, args...)
	cmd := exec.Command("git", command...)
	cmd.Dir = dir
	cmd.Env = overlayEnv(os.Environ(), map[string]string{
		"HOME":                home,
		"XDG_CONFIG_HOME":     xdgConfig,
		"GIT_CONFIG_GLOBAL":   filepath.Join(fixtureRoot, "fixture-git-global-config"),
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_COUNT":    "0",
		"GIT_AUTHOR_NAME":     "goldentrace",
		"GIT_AUTHOR_EMAIL":    "goldentrace@example.invalid",
		"GIT_COMMITTER_NAME":  "goldentrace",
		"GIT_COMMITTER_EMAIL": "goldentrace@example.invalid",
	})
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func portableInventoryPayload(t *testing.T, root string, wantKind string) string {
	t.Helper()
	manifestBody, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatalf("read portable manifest: %v", err)
	}
	manifest := map[string]any{}
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatalf("decode portable manifest: %v", err)
	}
	transcriptPath := jsonString(t, requiredJSONField(t, manifest, "transcript_payload", "portable manifest"), "portable manifest.transcript_payload")
	inventory := jsonSlice(t, requiredJSONField(t, manifest, "payload_inventory", "portable manifest"), "portable manifest.payload_inventory")
	for index, raw := range inventory {
		entry := jsonMap(t, raw, fmt.Sprintf("portable manifest.payload_inventory[%d]", index))
		if jsonString(t, requiredJSONField(t, entry, "kind", "portable inventory entry"), "portable inventory entry.kind") != wantKind {
			continue
		}
		relative := jsonString(t, requiredJSONField(t, entry, "path", "portable inventory entry"), "portable inventory entry.path")
		if wantKind == "participant_transcript" && relative != transcriptPath {
			t.Fatalf("portable transcript inventory path = %q, want manifest path %q", relative, transcriptPath)
		}
		return filepath.Join(root, filepath.FromSlash(relative))
	}
	t.Fatalf("portable manifest inventory lacks %q payload", wantKind)
	return ""
}

// traceProviderDriver is installed into each test's temporary PATH. It is
// intentionally small and protocol-specific rather than a reusable provider
// abstraction: the trace plan is the only input it needs.
const traceProviderDriver = `#!/usr/bin/env python3
import json
import os
import sys
import time
from pathlib import Path


def send(value):
    print(json.dumps(value), flush=True)


def arg_value(flag):
    if flag in sys.argv:
        index = sys.argv.index(flag)
        if index + 1 < len(sys.argv):
            return sys.argv[index + 1]
    return ""


def load_json(path, fallback):
    try:
        return json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return fallback


def plan():
    return load_json(os.environ.get("CONVO_RELAY_TRACE_PLAN", ""), {})


def state_path():
    return os.environ.get("CONVO_RELAY_TRACE_STATE", "")


def state():
    return load_json(state_path(), {"calls": {}, "threads": {}, "resumed": {}})


def save_state(value):
    path = state_path()
    if not path:
        return
    temp = Path(path + ".tmp")
    temp.write_text(json.dumps(value, sort_keys=True), encoding="utf-8")
    temp.replace(path)


def roles():
    raw = plan().get("roles", {})
    return raw if isinstance(raw, dict) else {}


def role_for(provider):
    configured = roles()
    if provider == "codex":
        candidate = Path(os.environ.get("CODEX_HOME", "")).name
        if candidate in configured:
            return candidate
    if provider == "gemini":
        candidate = Path(os.environ.get("GEMINI_CLI_HOME", "")).name
        if candidate in configured:
            return candidate
    if provider in configured:
        return provider
    return provider


def next_action(provider, role, prompt):
    current = state()
    calls = current.setdefault("calls", {})
    index = int(calls.get(role, 0))
    candidates = roles().get(role)
    if not isinstance(candidates, list) or not candidates:
        candidates = [plan().get("default", {"text": "TRACE_DEFAULT_PROVIDER_REPLY"})]
    action = candidates[min(index, len(candidates) - 1)]
    if not isinstance(action, dict):
        action = {"text": str(action)}
    calls[role] = index + 1
    current.setdefault("last_prompt", {})[role] = prompt
    save_state(current)

    required = action.get("require_prompt", "")
    if required and required not in prompt:
        return {
            "mode": "failure",
            "detail": "TRACE_REQUIRED_PROMPT_MISSING " + str(required),
        }
    if action.get("mode") == "source":
        source = Path.cwd() / str(action.get("source_file", ""))
        try:
            observed = source.read_text(encoding="utf-8")
        except OSError as err:
            return {"mode": "failure", "detail": "TRACE_SOURCE_READ_FAILED " + str(err)}
        expected = str(action.get("expect", ""))
        if observed == expected:
            return {"text": str(action.get("text", "TRACE_SOURCE_MATCHED"))}
        return {"text": str(action.get("mismatch_text", "TRACE_SOURCE_MISMATCH"))}
    return action


def mark_started():
    path = os.environ.get("CONVO_RELAY_TRACE_STARTED", "")
    if path:
        Path(path).write_text("started\n", encoding="utf-8")


def wait_forever():
    mark_started()
    while True:
        time.sleep(0.1)


def wait_for_codex_interrupt(thread_id, turn_id):
    mark_started()
    for raw in sys.stdin:
        try:
            request = json.loads(raw)
        except json.JSONDecodeError:
            continue
        if request.get("method") != "turn/interrupt":
            continue
        send({"id": request.get("id"), "result": {}})
        send({"method": "turn/completed", "params": {
            "threadId": thread_id,
            "turn": {"id": turn_id, "status": "interrupted"},
        }})
        return


def codex_prompt(params):
    items = params.get("input", []) if isinstance(params, dict) else []
    if items and isinstance(items[0], dict):
        return str(items[0].get("text", ""))
    return ""


def strict_resume_violation(role):
    strict = plan().get("strict_resume_roles", [])
    if role not in strict:
        return False
    current = state()
    return int(current.get("calls", {}).get(role, 0)) > 0 and not current.get("resumed", {}).get(role, False)


def record_codex_start(role, resumed, thread_id):
    current = state()
    threads = current.setdefault("threads", {})
    threads[role] = thread_id
    if resumed:
        current.setdefault("resumed", {})[role] = True
    save_state(current)


def codex_main():
    if "--version" in sys.argv:
        print("0.143.0")
        return 0
    if len(sys.argv) < 3 or sys.argv[2] != "app-server":
        print("expected codex app-server", file=sys.stderr)
        return 2
    role = role_for("codex")
    thread_id = "trace-codex-" + role
    turn_number = 0
    for raw in sys.stdin:
        try:
            request = json.loads(raw)
        except json.JSONDecodeError:
            continue
        method = request.get("method", "")
        params = request.get("params", {}) or {}
        if method == "initialize":
            send({"id": request.get("id"), "result": {"serverInfo": {"name": "goldentrace-codex"}}})
        elif method in ("thread/start", "thread/resume"):
            thread_id = params.get("threadId") or thread_id
            record_codex_start(role, method == "thread/resume", thread_id)
            send({"id": request.get("id"), "result": {"thread": {"id": thread_id}}})
        elif method == "model/list":
            send({"id": request.get("id"), "result": {"data": [{"id": "goldentrace-codex", "supportedReasoningEfforts": ["low", "medium", "high"]}]}})
        elif method == "turn/start":
            turn_number += 1
            turn_id = "trace-turn-" + str(turn_number)
            send({"id": request.get("id"), "result": {"turn": {"id": turn_id}}})
            if strict_resume_violation(role):
                action = {"mode": "failure", "detail": "TRACE_CODEX_THREAD_WAS_NOT_RESUMED"}
            else:
                action = next_action("codex", role, codex_prompt(params))
            if action.get("mode") == "wait":
                wait_for_codex_interrupt(thread_id, turn_id)
                continue
            if action.get("mode") == "failure":
                send({"method": "turn/completed", "params": {
                    "threadId": thread_id,
                    "turn": {"id": turn_id, "status": "failed", "error": {"message": str(action.get("detail", "TRACE_PROVIDER_FAILURE"))}},
                }})
                continue
            send({"method": "item/completed", "params": {
                "item": {"id": "trace-item-" + str(turn_number), "type": "agentMessage", "text": str(action.get("text", "TRACE_EMPTY_REPLY"))},
            }})
            send({"method": "turn/completed", "params": {
                "threadId": thread_id,
                "turn": {"id": turn_id, "status": "completed"},
            }})
        elif method == "turn/interrupt":
            send({"id": request.get("id"), "result": {}})
    return 0


def claude_main():
    if "--version" in sys.argv:
        print("2.1.205")
        return 0
    if "--help" in sys.argv:
        print("--effort values (low, medium, high, max)\n--model examples (goldentrace-claude)")
        return 0
    init_line = sys.stdin.readline()
    if not init_line:
        return 0
    try:
        initialize = json.loads(init_line)
    except json.JSONDecodeError:
        return 2
    send({"type": "control_response", "response": {
        "subtype": "success", "request_id": initialize.get("request_id", ""), "response": {},
    }})
    user_line = sys.stdin.readline()
    if not user_line:
        return 0
    try:
        user = json.loads(user_line)
    except json.JSONDecodeError:
        return 2
    prompt = str((user.get("message") or {}).get("content", ""))
    role = role_for("claude")
    action = next_action("claude", role, prompt)
    if action.get("mode") == "wait":
        wait_forever()
    session_id = arg_value("--resume") or "trace-claude-" + role
    send({"type": "system", "session_id": session_id, "model": arg_value("--model") or "goldentrace-claude"})
    if action.get("mode") == "failure":
        detail = str(action.get("detail", "TRACE_PROVIDER_FAILURE"))
        send({"type": "result", "session_id": session_id, "subtype": "error", "is_error": True, "error": detail})
        return 0
    text = str(action.get("text", "TRACE_EMPTY_REPLY"))
    send({"type": "assistant", "session_id": session_id, "message": {"content": [{"type": "text", "text": text}]}})
    send({"type": "result", "session_id": session_id, "subtype": "success", "is_error": False, "result": text})
    return 0


def gemini_main():
    prompt = sys.stdin.read()
    role = role_for("gemini")
    action = next_action("gemini", role, prompt)
    if action.get("mode") == "wait":
        wait_forever()
    session_id = "trace-gemini-" + role
    if action.get("mode") == "failure":
        detail = str(action.get("detail", "TRACE_PROVIDER_FAILURE"))
        print(json.dumps({"error": {"message": detail}, "session_id": session_id}), flush=True)
        print(detail, file=sys.stderr, flush=True)
        return 1
    print(json.dumps({"response": str(action.get("text", "TRACE_EMPTY_REPLY")), "session_id": session_id}), flush=True)
    return 0


def main():
    if len(sys.argv) < 2:
        return 2
    provider = sys.argv[1]
    if provider == "codex":
        return codex_main()
    if provider == "claude":
        return claude_main()
    if provider == "gemini":
        return gemini_main()
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
`
