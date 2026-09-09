package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/v2/internal/blobstore"
	"github.com/charlesnpx/convo-relay/v2/internal/eventlog"
	"github.com/charlesnpx/convo-relay/v2/internal/format"
	"github.com/charlesnpx/convo-relay/v2/internal/recipes"
	"github.com/charlesnpx/convo-relay/v2/internal/relayv2"
	"github.com/charlesnpx/convo-relay/v2/internal/session"
)

func v2ShowTranscriptReport(sess *session.Session, fromRound int, roundsSpec string) (map[string]any, error) {
	report, err := v2BuildReportMap(sess)
	if err != nil {
		return nil, err
	}
	return v2ProjectShowTranscriptReport(report, fromRound, roundsSpec)
}

func v2BuildReportMap(sess *session.Session) (map[string]any, error) {
	report, err := relayv2.BuildReport(sess, relayv2.ProjectionOptions{})
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// v2ProjectShowTranscriptReport keeps the public show envelope stable while
// deriving it solely from the v2 plan, event log, and blobs.
func v2ProjectShowTranscriptReport(report map[string]any, fromRound int, roundsSpec string) (map[string]any, error) {
	predicate, filterDesc, err := v2RoundPredicate(fromRound, roundsSpec)
	if err != nil {
		return nil, err
	}
	entries := v2TranscriptMaps(report["transcript"])
	filtered := v2FilterTranscript(entries, predicate)
	report["transcript"] = v2TranscriptAny(filtered)
	report["transcript_payload"] = v2TranscriptAny(filtered)
	summary, _ := report["summary"].(map[string]any)
	if summary == nil {
		summary = map[string]any{}
		report["summary"] = summary
	}
	summary["filtered_rounds"] = len(filtered)
	if filterDesc != "" {
		summary["round_filter"] = filterDesc
	}
	ledger := v2EmptyLedger()
	if len(entries) > 0 {
		ledger = v2NormalizeLedger(entries[len(entries)-1]["ledger"])
	}
	status := v2String(report["status"])
	report["meta"] = map[string]any{
		"task":             report["task"],
		"title":            report["title"],
		"status":           report["status"],
		"stop_reason":      report["stop_reason"],
		"mode":             report["mode"],
		"rounds":           report["participant_turns"],
		"max_rounds":       report["max_rounds"],
		"actual_rounds":    report["actual_rounds"],
		"round_limit_mode": report["round_limit_mode"],
		"ledger":           ledger,
		"slots":            report["slots"],
	}
	report["incomplete"] = status == "awaiting_decision" || status == "running"
	report["export_ready"] = status == "completed" || status == "failed"
	return report, nil
}

func v2ExportReport(sess *session.Session, jsonMode bool) (map[string]any, error) {
	report, err := v2ShowTranscriptReport(sess, 0, "")
	if err != nil {
		return nil, err
	}
	report["export"] = map[string]any{
		"format":     map[bool]string{true: "json", false: "markdown"}[jsonMode],
		"incomplete": report["incomplete"],
	}
	return report, nil
}

func v2FormatTranscriptMarkdown(report map[string]any) string {
	meta, _ := report["meta"].(map[string]any)
	transcript := v2AsSlice(report["transcript"])
	diagnostics, _ := report["diagnostics"].(map[string]any)
	settled, contested, withdrawn := v2LedgerCounts(meta["ledger"])
	roundsLabel := fmt.Sprintf("%v/%v", v2ValueOr(meta["actual_rounds"], len(transcript)), v2ValueOr(meta["max_rounds"], "?"))
	switch meta["round_limit_mode"] {
	case "auto":
		roundsLabel += " (auto)"
	case "fixed":
		roundsLabel += " (fixed)"
	}
	attention := v2String(meta["status"]) != "completed"
	if value, ok := diagnostics["attention_required"].(bool); ok {
		attention = value
	}
	lines := []string{
		"# Relay Dialogue",
		"",
		fmt.Sprintf("**Task**: %s", v2String(meta["task"])),
		fmt.Sprintf("**Title**: %s", v2String(v2ValueOr(meta["title"], meta["task"]))),
		fmt.Sprintf("**Status**: %s", v2ValueOr(meta["status"], "?")),
		fmt.Sprintf("**Attention required**: %v", attention),
		fmt.Sprintf("**Mode**: %s", v2ValueOr(meta["mode"], "?")),
		fmt.Sprintf("**Agents**: %s", strings.Join(v2AgentsForMeta(meta), ",")),
		fmt.Sprintf("**Rounds**: %s", roundsLabel),
		fmt.Sprintf("**Session**: `%s`", v2Truncate(filepath.Base(filepath.Clean(v2String(report["session_dir"]))), 8)),
		fmt.Sprintf("**Ledger**: settled %d | contested %d | withdrawn %d", settled, contested, withdrawn),
	}
	if root, ok := report["root"].(map[string]any); ok {
		lines = append(lines, "", "## Root Execution", "")
		for _, line := range strings.Split(v2FormatRootSummary(root), "\n") {
			lines = append(lines, "- "+line)
		}
	}
	lines = append(lines, "", "## Final Ledger")
	lines = append(lines, v2LedgerSummaryLines(meta["ledger"])...)
	lines = append(lines, "", "---")
	for _, rawEntry := range transcript {
		entry, _ := rawEntry.(map[string]any)
		entrySettled, entryContested, entryWithdrawn := v2LedgerCounts(entry["ledger"])
		lines = append(lines,
			"",
			fmt.Sprintf("## Round %v - %v", entry["round"], entry["from"]),
			"",
			fmt.Sprintf("**Mode**: %s", v2ValueOr(entry["mode"], v2ValueOr(meta["mode"], "?"))),
			"",
			fmt.Sprintf("**Ledger**: settled %d | contested %d | withdrawn %d", entrySettled, entryContested, entryWithdrawn),
			"",
			v2String(entry["content"]),
			"",
			"---",
		)
	}
	return strings.Join(lines, "\n")
}

func v2FormatExportMarkdown(report map[string]any) string {
	meta, _ := report["meta"].(map[string]any)
	transcript := v2AsSlice(report["transcript"])
	settled, contested, withdrawn := v2LedgerCounts(meta["ledger"])
	lines := []string{
		"# Relay Export",
		"",
		fmt.Sprintf("**Status**: %s", v2ValueOr(meta["status"], "?")),
		fmt.Sprintf("**Incomplete**: %v", v2ValueOr(report["incomplete"], false)),
		fmt.Sprintf("**Mode**: %s", v2ValueOr(meta["mode"], "?")),
		fmt.Sprintf("**Task**: %s", v2String(meta["task"])),
		fmt.Sprintf("**Session**: `%s`", v2Truncate(filepath.Base(filepath.Clean(v2String(report["session_dir"]))), 8)),
		fmt.Sprintf("**Ledger**: settled %d | contested %d | withdrawn %d", settled, contested, withdrawn),
		"",
	}
	if root, ok := report["root"].(map[string]any); ok {
		lines = append(lines, "## Root Execution", "")
		for _, line := range strings.Split(v2FormatRootSummary(root), "\n") {
			lines = append(lines, "- "+line)
		}
		lines = append(lines, "", "## Final Ledger")
	} else {
		lines = append(lines, "## Final Ledger")
	}
	lines = append(lines, v2LedgerSummaryLines(meta["ledger"])...)
	lines = append(lines, "", "---")
	for _, rawEntry := range transcript {
		entry, _ := rawEntry.(map[string]any)
		entrySettled, entryContested, entryWithdrawn := v2LedgerCounts(entry["ledger"])
		lines = append(lines,
			"",
			fmt.Sprintf("## Round %v - %v", entry["round"], entry["from"]),
			"",
			fmt.Sprintf("**Mode**: %s", v2ValueOr(entry["mode"], v2ValueOr(meta["mode"], "?"))),
			"",
			fmt.Sprintf("**Ledger**: settled %d | contested %d | withdrawn %d", entrySettled, entryContested, entryWithdrawn),
			"",
			v2String(entry["content"]),
			"",
			"---",
		)
	}
	return strings.Join(lines, "\n")
}

func v2FormatGraphSummary(report map[string]any) string {
	graphData, _ := report["graph"].(map[string]any)
	nodes, _ := graphData["nodes"].(map[string]any)
	proposals, _ := graphData["proposals"].(map[string]any)
	lines := []string{fmt.Sprintf("Graph v2: %d node(s), %d proposal(s)", len(nodes), len(proposals))}
	nodeIDs := make([]string, 0, len(nodes))
	for nodeID := range nodes {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	for _, nodeID := range nodeIDs {
		node, _ := nodes[nodeID].(map[string]any)
		recipe := v2String(node["recipe_id"])
		if recipe == "" {
			recipe = "root"
		}
		lines = append(lines, fmt.Sprintf("  %s  status=%v  recipe=%s", nodeID, v2ValueOr(node["status"], "?"), recipe))
	}
	if len(proposals) > 0 {
		lines = append(lines, "", "Proposals:")
		proposalIDs := make([]string, 0, len(proposals))
		for proposalID := range proposals {
			proposalIDs = append(proposalIDs, proposalID)
		}
		sort.Strings(proposalIDs)
		for _, proposalID := range proposalIDs {
			proposal, _ := proposals[proposalID].(map[string]any)
			lines = append(lines, fmt.Sprintf("  %s  status=%v  recipe=%v", proposalID, v2ValueOr(proposal["status"], "?"), v2ValueOr(proposal["selected_recipe_id"], "?")))
		}
	}
	return strings.Join(lines, "\n")
}

func v2BuildTraceReport(sess *session.Session, nodeID string) (map[string]any, error) {
	graphReport, err := relayv2.BuildGraphReport(sess)
	if err != nil {
		return nil, err
	}
	graphData, _ := graphReport["graph"].(map[string]any)
	nodes, _ := graphData["nodes"].(map[string]any)
	if _, found := nodes[nodeID]; !found {
		return nil, fmt.Errorf("no graph node %q", nodeID)
	}
	return nil, fmt.Errorf("no trace artifact recorded for node %q", nodeID)
}

func v2RenderDiff(sess *session.Session) (string, error) {
	report, err := v2ShowTranscriptReport(sess, 0, "")
	if err != nil {
		return "", err
	}
	meta, _ := report["meta"].(map[string]any)
	lines := []string{
		"# Relay Diff",
		"",
		fmt.Sprintf("**Task**: %s", v2String(meta["task"])),
		fmt.Sprintf("**Session**: `%s`", v2Truncate(filepath.Base(filepath.Clean(v2String(report["session_dir"]))), 8)),
		"",
	}
	previous := v2EmptyLedger()
	changesFound := false
	for _, entry := range v2TranscriptMaps(report["transcript"]) {
		current := v2NormalizeLedger(entry["ledger"])
		contestedAdded := v2Difference(current["contested"], previous["contested"])
		contestedRemoved := v2Difference(previous["contested"], current["contested"])
		withdrawnAdded := v2Difference(current["withdrawn"], previous["withdrawn"])
		withdrawnRemoved := v2Difference(previous["withdrawn"], current["withdrawn"])
		if len(contestedAdded)+len(contestedRemoved)+len(withdrawnAdded)+len(withdrawnRemoved) > 0 {
			changesFound = true
			lines = append(lines, fmt.Sprintf("## Round %v - %v", entry["round"], entry["from"]), "")
			for _, item := range contestedAdded {
				lines = append(lines, "+ contested: "+item)
			}
			for _, item := range contestedRemoved {
				lines = append(lines, "- contested: "+item)
			}
			for _, item := range withdrawnAdded {
				lines = append(lines, "+ withdrawn: "+item)
			}
			for _, item := range withdrawnRemoved {
				lines = append(lines, "- withdrawn: "+item)
			}
			lines = append(lines, "")
		}
		previous = current
	}
	if !changesFound {
		lines = append(lines, "No contested or withdrawn ledger changes.")
	}
	return strings.Join(lines, "\n"), nil
}

func v2SessionHealthReport(sess *session.Session) (map[string]any, error) {
	report, err := v2ShowTranscriptReport(sess, 0, "")
	if err != nil {
		return nil, err
	}
	status := v2String(report["status"])
	healthStatus := "ok"
	if status != "completed" {
		healthStatus = "attention_required"
	}
	diagnostics, _ := report["diagnostics"].(map[string]any)
	if diagnostics == nil {
		diagnostics = map[string]any{}
	}
	diagnostics["attention_required"] = healthStatus != "ok"
	result := map[string]any{
		"scope":       "session",
		"status":      healthStatus,
		"session_id":  report["session_id"],
		"session_dir": report["session_dir"],
		"summary": map[string]any{
			"session_status": status,
			"actual_rounds":  report["actual_rounds"],
			"max_rounds":     report["max_rounds"],
		},
		"diagnostics": diagnostics,
	}
	if root, ok := report["root"].(map[string]any); ok {
		result["root"] = root
	}
	return result, nil
}

func v2GlobalHealthReport(settingsPath string) map[string]any {
	checks := []any{}
	status := "ok"
	config, err := recipes.LoadRuntimeConfig(settingsPath)
	if err != nil {
		return map[string]any{
			"scope":  "global",
			"status": "error",
			"checks": []any{map[string]any{"name": "runtime_config_load", "status": "error", "error": err.Error()}},
		}
	}
	checks = append(checks, map[string]any{"name": "runtime_config_load", "status": "ok", "settings_path": config.SettingsPath})
	var issues []recipes.ChildRecipeIssue
	for _, recipe := range config.RelayRecipes {
		issues = append(issues, recipes.ExecutableIssues(recipe, config.BackendProfiles, config.RelayRecipes, recipes.DepthPolicy{}, "root")...)
	}
	if len(issues) > 0 {
		status = "degraded"
		checks = append(checks, map[string]any{"name": "runtime_config_validation", "status": "degraded", "issues": v2RecipeIssuesForReport(issues)})
	} else {
		checks = append(checks, map[string]any{"name": "runtime_config_validation", "status": "ok"})
	}
	return map[string]any{"scope": "global", "status": status, "checks": checks}
}

func v2FormatHealthReport(report map[string]any) string {
	lines := []string{
		fmt.Sprintf("Health: %v", v2ValueOr(report["status"], "unknown")),
		fmt.Sprintf("Scope: %v", v2ValueOr(report["scope"], "unknown")),
	}
	if report["session_id"] != nil {
		lines = append(lines, fmt.Sprintf("Session: %.8s", fmt.Sprint(report["session_id"])))
	}
	if diagnostics, ok := report["diagnostics"].(map[string]any); ok {
		lines = append(lines, fmt.Sprintf("Attention required: %v", v2ValueOr(diagnostics["attention_required"], false)))
	}
	for _, rawCheck := range v2AsSlice(report["checks"]) {
		check, _ := rawCheck.(map[string]any)
		lines = append(lines, fmt.Sprintf("- %v: %v", check["name"], check["status"]))
	}
	if root, ok := report["root"].(map[string]any); ok {
		lines = append(lines, strings.Split(v2FormatRootSummary(root), "\n")...)
	}
	return strings.Join(lines, "\n")
}

func v2FormatRootSummary(root map[string]any) string {
	if root == nil {
		return ""
	}
	recipe, _ := root["recipe"].(map[string]any)
	turns, _ := root["turns"].(map[string]any)
	result, _ := root["result"].(map[string]any)
	workspaceState, _ := root["workspace"].(map[string]any)
	providers, _ := root["providers"].(map[string]string)
	return strings.Join([]string{
		fmt.Sprintf("Root recipe: %v", v2ValueOr(recipe["id"], "unknown")),
		fmt.Sprintf("Root status: %v (v2)", v2ValueOr(root["status"], "unknown")),
		fmt.Sprintf("Participant turns: %v/%v", v2ValueOr(turns["completed"], 0), v2ValueOr(turns["configured"], "?")),
		fmt.Sprintf("Result: %v (%v)", v2ValueOr(result["source"], "pending"), v2ValueOr(result["validation_status"], "pending")),
		fmt.Sprintf("Workspace: source %v", v2ValueOr(workspaceState["workspace_content_source"], "unknown")),
		fmt.Sprintf("Providers: %d projected sessions", len(providers)),
	}, "\n")
}

func v2RecipeIssuesForReport(issues []recipes.ChildRecipeIssue) []any {
	result := make([]any, 0, len(issues))
	for _, issue := range issues {
		item := map[string]any{"category": issue.Category, "code": issue.Code, "message": issue.Message}
		if issue.Path != "" {
			item["path"] = issue.Path
		}
		if len(issue.Detail) > 0 {
			item["detail"] = issue.Detail
		}
		result = append(result, item)
	}
	return result
}

func v2RoundPredicate(fromRound int, roundsSpec string) (func(int) bool, string, error) {
	if fromRound > 0 && strings.TrimSpace(roundsSpec) != "" {
		return nil, "", fmt.Errorf("--from-round and --rounds are mutually exclusive")
	}
	if fromRound > 0 {
		return func(round int) bool { return round >= fromRound }, fmt.Sprintf("%d+", fromRound), nil
	}
	spec := strings.TrimSpace(roundsSpec)
	if spec == "" {
		return nil, "", nil
	}
	if match := regexp.MustCompile(`^(\d+)\+$`).FindStringSubmatch(spec); match != nil {
		start, _ := strconv.Atoi(match[1])
		return func(round int) bool { return round >= start }, spec, nil
	}
	if match := regexp.MustCompile(`^(\d+)-(\d+)$`).FindStringSubmatch(spec); match != nil {
		start, _ := strconv.Atoi(match[1])
		end, _ := strconv.Atoi(match[2])
		if start > end {
			return nil, "", fmt.Errorf("invalid range %d-%d", start, end)
		}
		return func(round int) bool { return round >= start && round <= end }, spec, nil
	}
	selected := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, "", fmt.Errorf("invalid --rounds value: %q", spec)
		}
		selected[value] = true
	}
	return func(round int) bool { return selected[round] }, spec, nil
}

func v2FilterTranscript(transcript []map[string]any, predicate func(int) bool) []map[string]any {
	if predicate == nil {
		return transcript
	}
	filtered := make([]map[string]any, 0, len(transcript))
	for _, entry := range transcript {
		if predicate(v2Int(entry["round"], 0)) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func v2TranscriptMaps(value any) []map[string]any {
	items := make([]map[string]any, 0)
	for _, raw := range v2AsSlice(value) {
		if item, ok := raw.(map[string]any); ok {
			items = append(items, item)
		}
	}
	return items
}

func v2TranscriptAny(transcript []map[string]any) []any {
	items := make([]any, 0, len(transcript))
	for _, entry := range transcript {
		items = append(items, entry)
	}
	return items
}

func v2AgentsForMeta(meta map[string]any) []string {
	agents := make([]string, 0)
	for _, rawSlot := range v2AsSlice(meta["slots"]) {
		slot, _ := rawSlot.(map[string]any)
		agents = append(agents, v2FirstNonEmpty(v2String(slot["backend"]), "?"))
	}
	return agents
}

func v2LedgerSummaryLines(value any) []string {
	ledger := v2NormalizeLedger(value)
	lines := make([]string, 0)
	for _, key := range []string{"settled", "contested", "withdrawn"} {
		items := ledger[key]
		if len(items) == 0 {
			lines = append(lines, fmt.Sprintf("%s (0): none", strings.ToUpper(key[:1])+key[1:]))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s (%d): %s", strings.ToUpper(key[:1])+key[1:], len(items), strings.Join(items, "; ")))
	}
	return lines
}

func v2LedgerCounts(value any) (int, int, int) {
	ledger := v2NormalizeLedger(value)
	return len(ledger["settled"]), len(ledger["contested"]), len(ledger["withdrawn"])
}

func v2NormalizeLedger(value any) map[string][]string {
	ledger := v2EmptyLedger()
	object, ok := value.(map[string]any)
	if !ok {
		return ledger
	}
	for _, key := range []string{"settled", "contested", "withdrawn"} {
		for _, item := range v2AsSlice(object[key]) {
			if text := strings.TrimSpace(v2String(item)); text != "" {
				ledger[key] = append(ledger[key], text)
			}
		}
	}
	return ledger
}

func v2EmptyLedger() map[string][]string {
	return map[string][]string{"settled": {}, "contested": {}, "withdrawn": {}}
}

func v2Difference(current []string, previous []string) []string {
	seen := map[string]bool{}
	for _, item := range previous {
		seen[item] = true
	}
	items := make([]string, 0)
	for _, item := range current {
		if !seen[item] {
			items = append(items, item)
		}
	}
	return items
}

func v2AsSlice(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return []any{}
}

func v2Int(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case v2JSONNumber:
		if result, err := typed.Int64(); err == nil {
			return int(result)
		}
	}
	return fallback
}

type v2JSONNumber interface {
	Int64() (int64, error)
}

func v2String(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func v2ValueOr(value any, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}

func v2FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func v2Truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

type v2PortableResult struct {
	Directory string
	Manifest  map[string]any
}

type v2PortablePayload struct {
	entry map[string]any
	body  []byte
}

func v2ExportPortable(sess *session.Session, targetDir string, version string) (*v2PortableResult, error) {
	target, err := filepath.Abs(strings.TrimSpace(targetDir))
	if err != nil || strings.TrimSpace(targetDir) == "" {
		return nil, format.NewValidationError("portable export requires an output directory")
	}
	target = filepath.Clean(target)
	if _, err := os.Lstat(target); err == nil {
		return nil, format.NewValidationError("portable export target already exists")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if sess.Plan.Provenance != session.ProvenanceRecipe && sess.Plan.Provenance != session.ProvenanceSupplied {
		return nil, format.NewValidationError("portable export requires a direct root session")
	}
	report, err := v2BuildReportMap(sess)
	if err != nil {
		return nil, err
	}
	status := strings.TrimSpace(v2String(report["status"]))
	switch status {
	case "completed", "failed":
	default:
		return nil, format.NewValidationError("portable export requires a terminal root session, got %q", status)
	}
	root, _ := report["root"].(map[string]any)
	sessionPayload := map[string]any{
		"plan":                          sess.Plan,
		"terminal_status":               status,
		"stop_reason":                   report["stop_reason"],
		"result_source":                 report["result_source"],
		"validation_status":             report["validation_status"],
		"workspace_content_source":      report["workspace_content_source"],
		"working_tree_changes_included": report["working_tree_changes_included"],
		"root":                          root,
	}
	diagnostics, _ := report["diagnostics"].(map[string]any)
	inputPayloads, err := v2PortableInputPayloads(sess)
	if err != nil {
		return nil, err
	}
	payloads := make([]v2PortablePayload, 0, 3+len(inputPayloads))
	for _, item := range []struct {
		kind string
		id   string
		data any
	}{
		{kind: "root_session", id: "session", data: sessionPayload},
		{kind: "participant_transcript", id: "transcript", data: report["transcript_payload"]},
		{kind: "diagnostics", id: "diagnostics", data: diagnostics},
	} {
		payload, err := v2NewPortablePayload(item.kind, item.id, item.data)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	payloads = append(payloads, inputPayloads...)
	sort.Slice(payloads, func(left, right int) bool {
		return v2String(payloads[left].entry["path"]) < v2String(payloads[right].entry["path"])
	})
	inventory := make([]any, 0, len(payloads))
	for _, payload := range payloads {
		inventory = append(inventory, payload.entry)
	}
	manifest, err := format.BundleManifest(map[string]any{
		"convo_relay_version": strings.TrimSpace(version),
		"terminal_status":     status,
		"stop_reason":         v2EmptyStringAsNil(v2String(report["stop_reason"])),
		"session_payload":     "payloads/root_session/session.json",
		"transcript_payload":  "payloads/participant_transcript/transcript.json",
		"diagnostics_payload": "payloads/diagnostics/diagnostics.json",
		"payload_inventory":   inventory,
	})
	if err != nil {
		return nil, err
	}
	if err := v2PublishPortableDirectory(target, payloads, manifest); err != nil {
		return nil, err
	}
	return &v2PortableResult{Directory: target, Manifest: manifest}, nil
}

func v2NewPortablePayload(kind string, id string, value any) (v2PortablePayload, error) {
	body, err := format.CanonicalJSONBytes(value)
	if err != nil {
		return v2PortablePayload{}, err
	}
	return v2PortablePayload{
		entry: map[string]any{
			"kind":        kind,
			"portable_id": id,
			"path":        path.Join("payloads", kind, id+".json"),
			"blob":        blobstore.RefForBytes(body, "application/json"),
		},
		body: body,
	}, nil
}

// v2PortableInputPayloads carries the raw plan-input blobs into the display
// bundle. The plan keeps the references; the input entries keep the bytes
// available to a consumer that receives only the bundle.
func v2PortableInputPayloads(sess *session.Session) ([]v2PortablePayload, error) {
	if sess == nil {
		return nil, errors.New("portable export requires a session")
	}
	refs := session.BlobRefs(sess.Plan)
	if len(refs) == 0 {
		return nil, nil
	}
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		return nil, fmt.Errorf("open input blobs for portable export: %w", err)
	}
	byDigest := make(map[string]blobstore.BlobRef, len(refs))
	for _, ref := range refs {
		if previous, exists := byDigest[ref.SHA256]; exists {
			if !previous.Equal(ref) {
				return nil, format.NewValidationError("portable export cannot represent blob %s with conflicting metadata", ref.SHA256)
			}
			continue
		}
		byDigest[ref.SHA256] = ref
	}
	digests := make([]string, 0, len(byDigest))
	for digest := range byDigest {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	payloads := make([]v2PortablePayload, 0, len(digests))
	for _, digest := range digests {
		ref := byDigest[digest]
		reader, err := blobs.Open(ref)
		if err != nil {
			return nil, fmt.Errorf("open input blob %s for portable export: %w", digest, err)
		}
		body, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read input blob %s for portable export: %w", digest, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("verify input blob %s for portable export: %w", digest, closeErr)
		}
		payloads = append(payloads, v2NewPortableBlobPayload("input", digest, ref, body))
	}
	return payloads, nil
}

func v2NewPortableBlobPayload(kind string, id string, ref blobstore.BlobRef, body []byte) v2PortablePayload {
	return v2PortablePayload{
		entry: map[string]any{
			"kind":        kind,
			"portable_id": id,
			"path":        path.Join("payloads", kind, id+".json"),
			"blob":        ref,
		},
		body: body,
	}
}

func v2PortableBlobRef(value any) (blobstore.BlobRef, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return blobstore.BlobRef{}, errors.New("blob must be an object")
	}
	ref := blobstore.BlobRef{
		SHA256:    v2String(object["sha256"]),
		Size:      int64(v2Int(object["size"], -1)),
		MediaType: v2String(object["media_type"]),
	}
	if err := blobstore.ValidateRef(ref); err != nil {
		return blobstore.BlobRef{}, err
	}
	return ref, nil
}

func v2PublishPortableDirectory(target string, payloads []v2PortablePayload, manifest map[string]any) (returnErr error) {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(parent, filepath.Base(target)+"-tmp-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			returnErr = errors.Join(returnErr, os.RemoveAll(temporary))
		}
	}()
	for _, payload := range payloads {
		if err := v2WriteSyncedFile(filepath.Join(temporary, filepath.FromSlash(v2String(payload.entry["path"]))), payload.body); err != nil {
			return err
		}
	}
	manifestBody, err := format.CanonicalJSONBytes(manifest)
	if err != nil {
		return err
	}
	if err := v2WriteSyncedFile(filepath.Join(temporary, "manifest.json"), manifestBody); err != nil {
		return err
	}
	if _, err := v2VerifyPortableDirectory(temporary); err != nil {
		return err
	}
	if err := v2SyncDirectory(temporary); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return format.NewValidationError("portable export target appeared during publication")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	published = true
	return v2SyncDirectory(parent)
}

func v2WriteSyncedFile(filename string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(body)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func v2SyncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if errors.Is(err, fs.ErrInvalid) {
		err = nil
	}
	return errors.Join(err, closeErr)
}

func v2VerifyPortableDirectory(directory string) (map[string]any, error) {
	root, err := v2CanonicalDirectory(directory)
	if err != nil {
		return nil, err
	}
	manifestBody, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return nil, err
	}
	manifestValue, err := format.DecodeStrictJSONObjectBytes(manifestBody)
	if err != nil {
		return nil, err
	}
	manifest, err := format.ValidateBundleManifest(manifestValue)
	if err != nil {
		return nil, err
	}
	expectedFiles := map[string]bool{"manifest.json": true}
	required := map[string]bool{
		"root_session:session":              false,
		"participant_transcript:transcript": false,
		"diagnostics:diagnostics":           false,
	}
	inputEntries := map[string]blobstore.BlobRef{}
	var rootSessionValue any
	payloadCount := 0
	for _, raw := range manifest["payload_inventory"].([]any) {
		entry := raw.(map[string]any)
		kind := v2String(entry["kind"])
		portableID := v2String(entry["portable_id"])
		key := kind + ":" + portableID
		isInput := kind == "input"
		if _, accepted := required[key]; !accepted && !isInput {
			return nil, format.NewValidationError("portable export contains unsupported payload %s", key)
		}
		if isInput && portableID == "" {
			return nil, format.NewValidationError("portable export input payload has an empty identity")
		}
		if !isInput {
			required[key] = true
		}
		relative := v2String(entry["path"])
		expectedFiles[relative] = true
		filename := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(filename)
		if err != nil || !info.Mode().IsRegular() {
			return nil, format.NewValidationError("portable export payload %s is missing or not a regular file", relative)
		}
		blob, err := v2PortableBlobRef(entry["blob"])
		if err != nil {
			return nil, format.NewValidationError("portable export payload %s has invalid blob metadata", relative)
		}
		if info.Size() != blob.Size {
			return nil, format.NewValidationError("portable export payload %s size or digest mismatch", relative)
		}
		body, err := os.ReadFile(filename)
		if err != nil {
			return nil, err
		}
		if got := blobstore.RefForBytes(body, blob.MediaType); !got.Equal(blob) {
			return nil, format.NewValidationError("portable export payload %s size or digest mismatch", relative)
		}
		if isInput {
			if blob.SHA256 != portableID {
				return nil, format.NewValidationError("portable export input payload %s identity does not match its blob", relative)
			}
			inputEntries[portableID] = blob
		} else {
			value, err := format.DecodeStrictJSONBytes(body)
			if err != nil {
				return nil, fmt.Errorf("decode portable export payload %s: %w", relative, err)
			}
			if key == "root_session:session" {
				if err := v2VerifyPortableRootSessionPayload(value); err != nil {
					return nil, err
				}
				rootSessionValue = value
			}
		}
		payloadCount++
	}
	for key, found := range required {
		if !found {
			return nil, format.NewValidationError("portable export is missing required payload %s", key)
		}
	}
	expectedInputs, err := v2PortableInputRefs(rootSessionValue)
	if err != nil {
		return nil, err
	}
	for digest, ref := range expectedInputs {
		actual, found := inputEntries[digest]
		if !found {
			return nil, format.NewValidationError("portable export is missing input payload %s", digest)
		}
		if !actual.Equal(ref) {
			return nil, format.NewValidationError("portable export input payload %s does not match the root plan", digest)
		}
	}
	for digest := range inputEntries {
		if _, expected := expectedInputs[digest]; !expected {
			return nil, format.NewValidationError("portable export contains input payload %s not referenced by the root plan", digest)
		}
	}
	if err := v2VerifyClosedPortableFileSet(root, expectedFiles); err != nil {
		return nil, err
	}
	return map[string]any{
		"format":          manifest["kind"],
		"status":          "valid",
		"terminal_status": manifest["terminal_status"],
		"payload_count":   payloadCount,
		"manifest_digest": manifest["manifest_digest"],
	}, nil
}

func v2VerifyPortableRootSessionPayload(value any) error {
	payload, ok := value.(map[string]any)
	if !ok {
		return format.NewValidationError("portable root session payload must be an object")
	}
	if _, found := payload["kind"]; found {
		return format.NewValidationError("portable root session payload must omit kind; relay.bundle/v1 identifies it")
	}
	return nil
}

func v2PortableInputRefs(value any) (map[string]blobstore.BlobRef, error) {
	refs := map[string]blobstore.BlobRef{}
	payload, ok := value.(map[string]any)
	if !ok {
		return refs, nil
	}
	rawPlan, found := payload["plan"]
	if !found {
		return refs, nil
	}
	body, err := format.CanonicalJSONBytes(rawPlan)
	if err != nil {
		return nil, fmt.Errorf("encode portable root plan: %w", err)
	}
	var planValue session.Plan
	if err := eventlog.DecodeCanonicalJSON(body, &planValue); err != nil {
		return nil, fmt.Errorf("decode portable root plan: %w", err)
	}
	if err := session.ValidatePlan(planValue); err != nil {
		return nil, fmt.Errorf("validate portable root plan: %w", err)
	}
	for _, ref := range session.BlobRefs(planValue) {
		if previous, exists := refs[ref.SHA256]; exists && !previous.Equal(ref) {
			return nil, format.NewValidationError("portable root plan references blob %s with conflicting metadata", ref.SHA256)
		}
		refs[ref.SHA256] = ref
	}
	return refs, nil
}

func v2VerifyClosedPortableFileSet(root string, expected map[string]bool) error {
	expectedDirectories := map[string]bool{}
	for filename := range expected {
		for directory := path.Dir(filename); directory != "."; directory = path.Dir(directory) {
			expectedDirectories[directory] = true
		}
	}
	return filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return format.NewValidationError("portable export contains a symlink at %s", relative)
		}
		if entry.IsDir() {
			if expectedDirectories[relative] {
				return nil
			}
			return format.NewValidationError("portable export contains an unexpected directory %s", relative)
		}
		if !expected[relative] {
			return format.NewValidationError("portable export contains an unexpected file %s", relative)
		}
		return nil
	})
}

func v2CanonicalDirectory(directory string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(directory))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", format.NewValidationError("path must resolve to a directory")
	}
	return filepath.Clean(resolved), nil
}

func v2EmptyStringAsNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
