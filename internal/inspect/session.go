package inspect

import (
	"fmt"
	"html"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func LoadMeta(sessionDir string) (map[string]any, error) {
	meta, err := store.New(sessionDir).LoadMeta()
	if err != nil {
		return nil, err
	}
	return meta.ToMap(), nil
}

func LoadTranscript(sessionDir string) ([]map[string]any, error) {
	transcript, err := store.New(sessionDir).LoadTranscript()
	if err != nil {
		return nil, err
	}
	return transcript.ToMaps(), nil
}

func BuildShowTranscriptReport(sessionDir string, fromRound int, roundsSpec string) (map[string]any, error) {
	meta, err := LoadMeta(sessionDir)
	if err != nil {
		return nil, err
	}
	transcript, err := LoadTranscript(sessionDir)
	if err != nil {
		return nil, err
	}
	predicate, filterDesc, err := roundPredicate(fromRound, roundsSpec)
	if err != nil {
		return nil, err
	}
	filtered := annotateTranscriptModes(filterTranscript(transcript, predicate), meta)
	settled, contested, withdrawn := ledgerCountsLocal(meta["ledger"])
	actualRounds := len(transcript)
	if stored := intFromAny(meta["actual_rounds"], 0); stored >= actualRounds {
		actualRounds = stored
	}
	summary := map[string]any{
		"status":            valueOr(meta["status"], "unknown"),
		"mode":              valueOr(meta["mode"], "unknown"),
		"mode_history":      asSlice(meta["mode_history"]),
		"modes":             transcriptModes(filtered),
		"agents":            agentsForMeta(meta),
		"configured_rounds": meta["rounds"],
		"max_rounds":        meta["max_rounds"],
		"actual_rounds":     actualRounds,
		"filtered_rounds":   len(filtered),
		"ledger_counts": map[string]any{
			"settled":   settled,
			"contested": contested,
			"withdrawn": withdrawn,
		},
	}
	if filterDesc != "" {
		summary["round_filter"] = filterDesc
	}
	events, eventsErr := store.New(sessionDir).ReadEvents()
	diagnostics := sessionDiagnostics(meta, transcript, events)
	if eventsErr != nil {
		diagnostics["events_error"] = eventsErr.Error()
	}
	return model.NewShowReport(map[string]any{
		"session_id":   filepath.Base(filepath.Clean(sessionDir)),
		"session_dir":  sessionDir,
		"meta":         meta,
		"transcript":   transcriptAny(filtered),
		"summary":      summary,
		"diagnostics":  diagnostics,
		"incomplete":   sessionIncomplete(meta),
		"export_ready": true,
	}).ToMap(), nil
}

func BuildExportReport(sessionDir string, jsonMode bool) (map[string]any, error) {
	report, err := BuildShowTranscriptReport(sessionDir, 0, "")
	if err != nil {
		return nil, err
	}
	report["export"] = map[string]any{
		"format":     map[bool]string{true: "json", false: "markdown"}[jsonMode],
		"incomplete": report["incomplete"],
	}
	return model.NewExportReport(report).ToMap(), nil
}

func FormatTranscriptMarkdown(report map[string]any) string {
	meta, _ := report["meta"].(map[string]any)
	transcript := asSlice(report["transcript"])
	diagnostics, _ := report["diagnostics"].(map[string]any)
	settled, contested, withdrawn := ledgerCountsLocal(meta["ledger"])
	roundsLabel := fmt.Sprintf("%v/%v", valueOr(meta["actual_rounds"], len(transcript)), valueOr(meta["max_rounds"], "?"))
	switch meta["round_limit_mode"] {
	case "auto":
		roundsLabel += " (auto)"
	case "fixed":
		roundsLabel += " (fixed)"
	}
	lines := []string{
		"# Relay Dialogue",
		"",
		fmt.Sprintf("**Task**: %s", stringFromAny(meta["task"])),
		fmt.Sprintf("**Title**: %s", stringFromAny(meta["title"])),
		fmt.Sprintf("**Status**: %s", valueOr(meta["status"], "?")),
		fmt.Sprintf("**Attention required**: %v", valueOr(diagnostics["attention_required"], false)),
		fmt.Sprintf("**Mode**: %s", valueOr(meta["mode"], "?")),
		fmt.Sprintf("**Agents**: %s", strings.Join(agentsForMeta(meta), ",")),
		fmt.Sprintf("**Rounds**: %s", roundsLabel),
		fmt.Sprintf("**Duration**: %vs", valueOr(meta["elapsed_seconds"], "?")),
		fmt.Sprintf("**Session**: `%s`", truncate(filepath.Base(filepath.Clean(stringFromAny(report["session_dir"]))), 8)),
		fmt.Sprintf("**Ledger**: settled %d | contested %d | withdrawn %d", settled, contested, withdrawn),
	}
	if failure := lastFailureForMarkdown(diagnostics); failure != "" {
		lines = append(lines, fmt.Sprintf("**Last provider failure**: %s", failure))
	}
	if rejection := lastModeControlRejectionForMarkdown(diagnostics); rejection != "" {
		lines = append(lines, fmt.Sprintf("**Last mode control rejection**: %s", rejection))
	}
	lines = append(lines, "", "## Final Ledger")
	lines = append(lines, ledgerSummaryLines(meta["ledger"])...)
	lines = append(lines, slotReplacementMarkdownLines(meta["slot_replacement_history"])...)
	lines = append(lines, "", "---")
	for _, rawEntry := range transcript {
		entry, _ := rawEntry.(map[string]any)
		entrySettled, entryContested, entryWithdrawn := ledgerCountsLocal(entry["ledger"])
		lines = append(lines,
			"",
			fmt.Sprintf("## Round %v - %v", entry["round"], entry["from"]),
			"",
			fmt.Sprintf("**Mode**: %s", valueOr(entry["mode"], valueOr(meta["mode"], "?"))),
			"",
			fmt.Sprintf("**Ledger**: settled %d | contested %d | withdrawn %d", entrySettled, entryContested, entryWithdrawn),
			"",
			stringFromAny(entry["content"]),
			"",
			"---",
		)
	}
	return strings.Join(lines, "\n")
}

func FormatExportMarkdown(report map[string]any) string {
	meta, _ := report["meta"].(map[string]any)
	transcript := asSlice(report["transcript"])
	settled, contested, withdrawn := ledgerCountsLocal(meta["ledger"])
	lines := []string{
		"# Relay Export",
		"",
		fmt.Sprintf("**Status**: %s", valueOr(meta["status"], "?")),
		fmt.Sprintf("**Incomplete**: %v", valueOr(report["incomplete"], false)),
		fmt.Sprintf("**Mode**: %s", valueOr(meta["mode"], "?")),
		fmt.Sprintf("**Task**: %s", stringFromAny(meta["task"])),
		fmt.Sprintf("**Session**: `%s`", truncate(filepath.Base(filepath.Clean(stringFromAny(report["session_dir"]))), 8)),
		fmt.Sprintf("**Ledger**: settled %d | contested %d | withdrawn %d", settled, contested, withdrawn),
		"",
		"## Final Ledger",
	}
	lines = append(lines, ledgerSummaryLines(meta["ledger"])...)
	lines = append(lines, slotReplacementMarkdownLines(meta["slot_replacement_history"])...)
	lines = append(lines,
		"",
		"---",
	)
	for _, rawEntry := range transcript {
		entry, _ := rawEntry.(map[string]any)
		entrySettled, entryContested, entryWithdrawn := ledgerCountsLocal(entry["ledger"])
		lines = append(lines,
			"",
			fmt.Sprintf("## Round %v - %v", entry["round"], entry["from"]),
			"",
			fmt.Sprintf("**Mode**: %s", valueOr(entry["mode"], valueOr(meta["mode"], "?"))),
			"",
			fmt.Sprintf("**Ledger**: settled %d | contested %d | withdrawn %d", entrySettled, entryContested, entryWithdrawn),
			"",
			stringFromAny(entry["content"]),
			"",
			"---",
		)
	}
	return strings.Join(lines, "\n")
}

func BuildSessionHealthReport(sessionDir string) (map[string]any, error) {
	meta, err := LoadMeta(sessionDir)
	if err != nil {
		return nil, err
	}
	transcript, err := LoadTranscript(sessionDir)
	if err != nil {
		return nil, err
	}
	events, eventsErr := store.New(sessionDir).ReadEvents()
	diagnostics := sessionDiagnostics(meta, transcript, events)
	if eventsErr != nil {
		diagnostics["events_error"] = eventsErr.Error()
	}
	status := "ok"
	if attention, _ := diagnostics["attention_required"].(bool); attention {
		status = "attention_required"
	}
	return model.NewHealthReport(map[string]any{
		"scope":       "session",
		"status":      status,
		"session_id":  filepath.Base(filepath.Clean(sessionDir)),
		"session_dir": sessionDir,
		"summary": map[string]any{
			"session_status": valueOr(meta["status"], "unknown"),
			"actual_rounds":  valueOr(meta["actual_rounds"], len(transcript)),
			"max_rounds":     meta["max_rounds"],
		},
		"diagnostics": diagnostics,
	}).ToMap(), nil
}

func BuildGlobalHealthReport(settingsPath string) map[string]any {
	checks := []any{}
	status := "ok"
	config, err := recipes.LoadRuntimeConfig(settingsPath)
	if err != nil {
		status = "error"
		checks = append(checks, map[string]any{
			"name":   "runtime_config_load",
			"status": "error",
			"error":  err.Error(),
		})
		return map[string]any{
			"scope":  "global",
			"status": status,
			"checks": checks,
		}
	}
	checks = append(checks, map[string]any{
		"name":          "runtime_config_load",
		"status":        "ok",
		"settings_path": config.SettingsPath,
	})
	var issues []recipes.ChildRecipeIssue
	for _, recipe := range config.RelayRecipes {
		issues = append(issues, recipes.ExecutableIssues(recipe, config.BackendProfiles, config.RelayRecipes, recipes.DepthPolicy{}, "root")...)
	}
	if len(issues) > 0 {
		status = "degraded"
		checks = append(checks, map[string]any{
			"name":   "runtime_config_validation",
			"status": "degraded",
			"issues": recipeIssuesForReport(issues),
		})
	} else {
		checks = append(checks, map[string]any{
			"name":   "runtime_config_validation",
			"status": "ok",
		})
	}
	return map[string]any{
		"scope":  "global",
		"status": status,
		"checks": checks,
	}
}

func FormatHealthReport(report map[string]any) string {
	lines := []string{
		fmt.Sprintf("Health: %v", valueOr(report["status"], "unknown")),
		fmt.Sprintf("Scope: %v", valueOr(report["scope"], "unknown")),
	}
	if report["session_id"] != nil {
		lines = append(lines, fmt.Sprintf("Session: %.8s", fmt.Sprint(report["session_id"])))
	}
	if diagnostics, ok := report["diagnostics"].(map[string]any); ok {
		lines = append(lines, fmt.Sprintf("Attention required: %v", valueOr(diagnostics["attention_required"], false)))
		if failures := asSlice(diagnostics["recent_failures"]); len(failures) > 0 {
			lines = append(lines, fmt.Sprintf("Recent failures: %d", len(failures)))
			if failure, ok := failures[len(failures)-1].(map[string]any); ok {
				lines = append(lines, fmt.Sprintf("Last failure: %s", lastFailureSummary(failure)))
			}
		}
		if rejections := asSlice(diagnostics["mode_control_rejections"]); len(rejections) > 0 {
			lines = append(lines, fmt.Sprintf("Mode control rejections: %d", len(rejections)))
		}
	}
	if checks := asSlice(report["checks"]); len(checks) > 0 {
		for _, rawCheck := range checks {
			check, _ := rawCheck.(map[string]any)
			lines = append(lines, fmt.Sprintf("- %v: %v", check["name"], check["status"]))
		}
	}
	return strings.Join(lines, "\n")
}

func recipeIssuesForReport(issues []recipes.ChildRecipeIssue) []any {
	result := make([]any, 0, len(issues))
	for _, issue := range issues {
		item := map[string]any{
			"category": issue.Category,
			"code":     issue.Code,
			"message":  issue.Message,
		}
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

func sessionIncomplete(meta map[string]any) bool {
	switch strings.TrimSpace(stringFromAny(meta["status"])) {
	case "completed":
		return false
	default:
		return true
	}
}

func sessionDiagnostics(meta map[string]any, transcript []map[string]any, events []map[string]any) map[string]any {
	settled, contested, withdrawn := ledgerCountsLocal(meta["ledger"])
	failures := providerFailuresFromEvents(events)
	if len(failures) == 0 {
		failures = providerFailuresFromTranscript(transcript)
	}
	modeRejections := modeControlRejections(meta, events)
	status := strings.TrimSpace(stringFromAny(meta["status"]))
	attention := status != "" && status != "completed"
	if len(failures) > 0 {
		attention = true
	}
	if len(modeRejections) > 0 {
		attention = true
	}
	return map[string]any{
		"attention_required":      attention,
		"session_status":          valueOr(meta["status"], "unknown"),
		"recent_failures":         failures,
		"mode_control_rejections": modeRejections,
		"scan": map[string]any{
			"transcript_entries_scanned": len(transcript),
			"event_entries_scanned":      len(events),
			"truncated":                  false,
		},
		"ledger_counts": map[string]any{
			"settled":   settled,
			"contested": contested,
			"withdrawn": withdrawn,
		},
	}
}

func annotateTranscriptModes(transcript []map[string]any, meta map[string]any) []map[string]any {
	annotated := make([]map[string]any, 0, len(transcript))
	for _, entry := range transcript {
		item := map[string]any{}
		for key, value := range entry {
			item[key] = value
		}
		if strings.TrimSpace(stringFromAny(item["mode"])) == "" {
			item["mode"] = modeForRound(meta, intFromAny(item["round"], 0))
		}
		annotated = append(annotated, item)
	}
	return annotated
}

func modeForRound(meta map[string]any, round int) string {
	fallback := strings.TrimSpace(stringFromAny(meta["mode"]))
	if fallback == "" {
		fallback = "unknown"
	}
	mode := fallback
	for _, raw := range asSlice(meta["mode_history"]) {
		record, _ := raw.(map[string]any)
		if record == nil || intFromAny(record["applied_round"], 0) > round {
			continue
		}
		if candidate := strings.TrimSpace(firstNonEmpty(record["applied_mode"], record["mode"])); candidate != "" {
			mode = candidate
		}
	}
	return mode
}

func transcriptModes(transcript []map[string]any) []any {
	seen := map[string]bool{}
	modes := []any{}
	for _, entry := range transcript {
		mode := strings.TrimSpace(stringFromAny(entry["mode"]))
		if mode == "" || seen[mode] {
			continue
		}
		seen[mode] = true
		modes = append(modes, mode)
	}
	return modes
}

func modeControlRejections(meta map[string]any, events []map[string]any) []any {
	rejections := []any{}
	for _, event := range events {
		if event["event_type"] != "mode_control_rejected" {
			continue
		}
		payload, _ := event["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		item := map[string]any{}
		for key, value := range payload {
			item[key] = value
		}
		item["event_id"] = event["event_id"]
		item["timestamp"] = event["timestamp"]
		rejections = append(rejections, item)
	}
	if len(rejections) == 0 {
		rejections = append(rejections, asSlice(meta["mode_control_rejections"])...)
	}
	return rejections
}

func providerFailuresFromEvents(events []map[string]any) []any {
	failures := []any{}
	for _, event := range events {
		if event["event_type"] != "provider_failure" {
			continue
		}
		payload, _ := event["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		item := map[string]any{}
		for key, value := range payload {
			item[key] = value
		}
		item["event_id"] = event["event_id"]
		item["timestamp"] = event["timestamp"]
		failures = append(failures, item)
	}
	return failures
}

func providerFailuresFromTranscript(transcript []map[string]any) []any {
	failures := []any{}
	for _, entry := range transcript {
		providerResult, _ := entry["provider_result"].(map[string]any)
		if providerResult == nil {
			continue
		}
		if providerResult["retryable_error"] == nil && len(asSlice(providerResult["warnings"])) == 0 {
			continue
		}
		failures = append(failures, map[string]any{
			"phase":            "turn",
			"actor":            entry["from"],
			"backend":          providerResult["backend"],
			"category":         "provider_warning",
			"retryable":        providerResult["retryable_error"] != nil,
			"sanitized_detail": firstNonEmpty(providerResult["retryable_error"], firstStringInSlice(providerResult["warnings"]), "provider warning"),
			"round":            entry["round"],
		})
	}
	return failures
}

func lastFailureForMarkdown(diagnostics map[string]any) string {
	failures := asSlice(diagnostics["recent_failures"])
	if len(failures) == 0 {
		return ""
	}
	failure, _ := failures[len(failures)-1].(map[string]any)
	return lastFailureSummary(failure)
}

func lastFailureSummary(failure map[string]any) string {
	if failure == nil {
		return ""
	}
	detail := stringFromAny(failure["sanitized_detail"])
	if detail == "" {
		detail = stringFromAny(failure["detail"])
	}
	if detail == "" {
		detail = stringFromAny(failure["category"])
	}
	if actor := stringFromAny(failure["actor"]); actor != "" {
		return actor + ": " + detail
	}
	return detail
}

func lastModeControlRejectionForMarkdown(diagnostics map[string]any) string {
	rejections := asSlice(diagnostics["mode_control_rejections"])
	if len(rejections) == 0 {
		return ""
	}
	rejection, _ := rejections[len(rejections)-1].(map[string]any)
	if rejection == nil {
		return ""
	}
	reason := stringFromAny(rejection["reason"])
	if reason == "" {
		reason = "unsupported mode control"
	}
	return fmt.Sprintf("%s at round %v", reason, valueOr(rejection["queued_round"], "?"))
}

func firstStringInSlice(value any) string {
	for _, raw := range asSlice(value) {
		text := strings.TrimSpace(stringFromAny(raw))
		if text != "" {
			return text
		}
	}
	return ""
}

func BuildTraceReport(sessionDir string, nodeID string) (map[string]any, error) {
	graphReport, err := BuildShowGraphReport(sessionDir)
	if err != nil {
		return nil, err
	}
	graphData, _ := graphReport["graph"].(map[string]any)
	nodes, _ := graphData["nodes"].(map[string]any)
	node, _ := nodes[nodeID].(map[string]any)
	if node == nil {
		return nil, fmt.Errorf("no graph node %q", nodeID)
	}
	traceRef, _ := node["trace_ref"].(map[string]any)
	if traceRef == nil {
		return nil, fmt.Errorf("no trace artifact recorded for node %q", nodeID)
	}
	return store.New(sessionDir).LoadArtifact(traceRef)
}

func RenderDiff(sessionDir string) (string, error) {
	meta, err := LoadMeta(sessionDir)
	if err != nil {
		return "", err
	}
	transcript, err := LoadTranscript(sessionDir)
	if err != nil {
		return "", err
	}
	lines := []string{
		"# Relay Diff",
		"",
		fmt.Sprintf("**Task**: %s", stringFromAny(meta["task"])),
		fmt.Sprintf("**Session**: `%s`", truncate(filepath.Base(filepath.Clean(sessionDir)), 8)),
		"",
	}
	previous := emptyLedgerLocal()
	changesFound := false
	for _, entry := range transcript {
		current := normalizeLedgerLocal(entry["ledger"])
		contestedAdded := difference(current["contested"], previous["contested"])
		contestedRemoved := difference(previous["contested"], current["contested"])
		withdrawnAdded := difference(current["withdrawn"], previous["withdrawn"])
		withdrawnRemoved := difference(previous["withdrawn"], current["withdrawn"])
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
		lines = append(lines, "No contested or withdrawn changes recorded.")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n", nil
}

func BuildDisplayHTML(sessionDir string) (string, error) {
	meta, err := LoadMeta(sessionDir)
	if err != nil {
		return "", err
	}
	transcript, err := LoadTranscript(sessionDir)
	if err != nil {
		return "", err
	}
	if len(transcript) == 0 {
		return "", fmt.Errorf("session has no transcript entries")
	}
	title := firstNonEmpty(meta["title"], meta["task"], "Relay Session")
	var blocks []string
	for _, entry := range transcript {
		settled, contested, withdrawn := ledgerCountsLocal(entry["ledger"])
		blocks = append(blocks, fmt.Sprintf(
			`<section class="turn"><h2>Round %v - %s</h2><div class="ledger-mini">Ledger: settled %d | contested %d | withdrawn %d</div><pre>%s</pre></section>`,
			entry["round"],
			html.EscapeString(stringFromAny(entry["from"])),
			settled,
			contested,
			withdrawn,
			html.EscapeString(stringFromAny(entry["content"])),
		))
	}
	promptBlock := buildLaunchPromptBlock(meta)
	planBlock := buildLaunchPlanBlock(meta)
	ledgerBlock := buildLedgerHTMLBlock(meta["ledger"])
	replacementBlock := buildSlotReplacementHTMLBlock(meta["slot_replacement_history"])
	return "<!doctype html>\n<html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">" +
		"<title>" + html.EscapeString(title) + " - convo-relay</title>" +
		"<style>body{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;background:#111827;color:#e5e7eb;margin:0;padding:32px}.container{max-width:920px;margin:0 auto}.meta,.ledger-mini{color:#9ca3af}.ledger-mini{font-size:13px;margin:0 0 12px}.turn,.prompt-terminal,.plan-terminal,.ledger-terminal,.replacement-terminal{background:#1f2937;border:1px solid #374151;border-radius:8px;margin:18px 0;padding:18px}h1{margin:0 0 8px}h2{font-size:16px;color:#f9fafb;margin:0 0 12px}pre{white-space:pre-wrap;line-height:1.55;margin:0}.ledger-list,.replacement-list{display:grid;gap:8px;margin:0;padding-left:20px}.plan-steps{display:grid;gap:10px;margin:0;padding:0;list-style:none}.plan-step{display:flex;gap:10px}.plan-status{min-width:92px;text-transform:uppercase;font-size:12px}.status-completed{color:#86efac}.status-in_progress{color:#fde68a}.status-pending{color:#cbd5e1}.status-other{color:#93c5fd}</style>" +
		"</head><body><main class=\"container\"><h1>" + html.EscapeString(title) + "</h1><div class=\"meta\">convo-relay transcript &middot; " +
		html.EscapeString(fmt.Sprint(valueOr(meta["actual_rounds"], len(transcript)))) + " rounds</div>" +
		promptBlock +
		planBlock +
		ledgerBlock +
		replacementBlock +
		strings.Join(blocks, "") + "</main></body></html>", nil
}

func ledgerSummaryLines(ledger any) []string {
	current := normalizeLedgerLocal(ledger)
	lines := []string{}
	for _, key := range []string{"settled", "contested", "withdrawn"} {
		items := current[key]
		if len(items) == 0 {
			lines = append(lines, fmt.Sprintf("- %s: none", ledgerLabel(key)))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s (%d): %s", ledgerLabel(key), len(items), compactLedgerItems(items)))
	}
	return lines
}

func slotReplacementMarkdownLines(history any) []string {
	replacements := asSlice(history)
	if len(replacements) == 0 {
		return nil
	}
	lines := []string{"", "## Slot Replacements"}
	for _, rawReplacement := range replacements {
		replacement, _ := rawReplacement.(map[string]any)
		lines = append(lines, "- "+formatSlotReplacementSummary(replacement))
	}
	return lines
}

func formatSlotReplacementSummary(replacement map[string]any) string {
	logicalSlotID := firstNonEmpty(stringFromAny(replacement["logical_slot_id"]), "slot")
	previousBackend := firstNonEmpty(stringFromAny(replacement["previous_backend"]), "?")
	newBackend := firstNonEmpty(stringFromAny(replacement["new_backend"]), "?")
	previousProfile := stringFromAny(replacement["previous_profile_id"])
	newProfile := stringFromAny(replacement["new_profile_id"])
	if previousProfile != "" {
		previousBackend += "/" + previousProfile
	}
	if newProfile != "" {
		newBackend += "/" + newProfile
	}
	return fmt.Sprintf(
		"Round %v: %s generation %v -> %v, %s -> %s (%s -> %s)",
		valueOr(replacement["applies_from_round"], "?"),
		logicalSlotID,
		valueOr(replacement["previous_generation"], "?"),
		valueOr(replacement["new_generation"], "?"),
		previousBackend,
		newBackend,
		valueOr(replacement["previous_slot_id"], "?"),
		valueOr(replacement["new_slot_id"], "?"),
	)
}

func buildSlotReplacementHTMLBlock(history any) string {
	replacements := asSlice(history)
	if len(replacements) == 0 {
		return ""
	}
	var body strings.Builder
	body.WriteString(`<section class="replacement-terminal"><h2>Slot Replacements</h2><ul class="replacement-list">`)
	for _, rawReplacement := range replacements {
		replacement, _ := rawReplacement.(map[string]any)
		body.WriteString(`<li>`)
		body.WriteString(html.EscapeString(formatSlotReplacementSummary(replacement)))
		body.WriteString(`</li>`)
	}
	body.WriteString(`</ul></section>`)
	return body.String()
}

func ledgerLabel(key string) string {
	switch key {
	case "settled":
		return "Settled"
	case "contested":
		return "Contested"
	case "withdrawn":
		return "Withdrawn"
	default:
		return key
	}
}

func compactLedgerItems(items []string) string {
	const limit = 5
	if len(items) <= limit {
		return strings.Join(items, "; ")
	}
	return strings.Join(items[:limit], "; ") + fmt.Sprintf("; +%d more", len(items)-limit)
}

func buildLedgerHTMLBlock(ledger any) string {
	lines := ledgerSummaryLines(ledger)
	if len(lines) == 0 {
		return ""
	}
	var body strings.Builder
	body.WriteString(`<section class="ledger-terminal"><h2>Final Ledger</h2><ul class="ledger-list">`)
	for _, line := range lines {
		body.WriteString(`<li>`)
		body.WriteString(html.EscapeString(strings.TrimPrefix(line, "- ")))
		body.WriteString(`</li>`)
	}
	body.WriteString(`</ul></section>`)
	return body.String()
}

func buildLaunchPromptBlock(meta map[string]any) string {
	prompt := strings.TrimSpace(firstNonEmpty(meta["initial_prompt"], meta["task"]))
	if prompt == "" {
		return ""
	}
	return `<section class="prompt-terminal"><h2>Launch Prompt</h2><pre>` + html.EscapeString(prompt) + `</pre></section>`
}

func buildLaunchPlanBlock(meta map[string]any) string {
	plan := displayLaunchPlan(meta)
	if plan == nil {
		return ""
	}
	var body strings.Builder
	switch value := plan.(type) {
	case string:
		body.WriteString(`<pre>`)
		body.WriteString(html.EscapeString(value))
		body.WriteString(`</pre>`)
	case map[string]any:
		if explanation := strings.TrimSpace(stringFromAny(value["explanation"])); explanation != "" {
			body.WriteString(`<p>`)
			body.WriteString(html.EscapeString(explanation))
			body.WriteString(`</p>`)
		}
		if steps, ok := value["plan"].([]any); ok && len(steps) > 0 {
			body.WriteString(`<ol class="plan-steps">`)
			for _, rawStep := range steps {
				step, _ := rawStep.(map[string]any)
				text := strings.TrimSpace(stringFromAny(step["step"]))
				if text == "" {
					continue
				}
				status := strings.TrimSpace(stringFromAny(step["status"]))
				if status == "" {
					status = "pending"
				}
				body.WriteString(`<li class="plan-step"><span class="plan-status `)
				body.WriteString(planStatusClass(status))
				body.WriteString(`">`)
				body.WriteString(html.EscapeString(strings.ReplaceAll(status, "_", " ")))
				body.WriteString(`</span><span class="plan-step-text">`)
				body.WriteString(html.EscapeString(text))
				body.WriteString(`</span></li>`)
			}
			body.WriteString(`</ol>`)
		}
	}
	if body.Len() == 0 {
		return ""
	}
	return `<section class="plan-terminal"><h2>Launch Plan</h2>` + body.String() + `</section>`
}

func displayLaunchPlan(meta map[string]any) any {
	for _, key := range []string{"launch_plan", "task_plan", "relay_plan"} {
		if plan := normalizeDisplayLaunchPlan(meta[key]); plan != nil {
			return plan
		}
	}
	return nil
}

func normalizeDisplayLaunchPlan(plan any) any {
	switch value := plan.(type) {
	case nil:
		return nil
	case string:
		if text := strings.TrimSpace(value); text != "" {
			return text
		}
		return nil
	case []any:
		return normalizeDisplayLaunchPlan(map[string]any{"plan": value})
	case map[string]any:
		normalized := map[string]any{}
		if explanation := strings.TrimSpace(stringFromAny(value["explanation"])); explanation != "" {
			normalized["explanation"] = explanation
		}
		rawSteps := value["plan"]
		if rawSteps == nil {
			rawSteps = value["steps"]
		}
		steps := []any{}
		if rawItems, ok := rawSteps.([]any); ok {
			for _, rawItem := range rawItems {
				step := ""
				status := "pending"
				if item, ok := rawItem.(map[string]any); ok {
					step = strings.TrimSpace(stringFromAny(item["step"]))
					if rawStatus := strings.TrimSpace(stringFromAny(item["status"])); rawStatus != "" {
						status = rawStatus
					}
				} else {
					step = strings.TrimSpace(fmt.Sprint(rawItem))
				}
				if step != "" {
					steps = append(steps, map[string]any{"step": step, "status": status})
				}
			}
		}
		if len(steps) > 0 {
			normalized["plan"] = steps
		}
		if len(normalized) == 0 {
			return nil
		}
		return normalized
	default:
		if text := strings.TrimSpace(fmt.Sprint(value)); text != "" {
			return text
		}
		return nil
	}
}

func planStatusClass(status string) string {
	slug := regexp.MustCompile(`[^a-z0-9_]+`).ReplaceAllString(strings.ToLower(status), "_")
	slug = strings.Trim(slug, "_")
	switch slug {
	case "completed", "in_progress", "pending":
		return "status-" + slug
	default:
		return "status-other"
	}
}

func roundPredicate(fromRound int, roundsSpec string) (func(int) bool, string, error) {
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

func filterTranscript(transcript []map[string]any, predicate func(int) bool) []map[string]any {
	if predicate == nil {
		return transcript
	}
	filtered := []map[string]any{}
	for _, entry := range transcript {
		round := intFromAny(entry["round"], 0)
		if predicate(round) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func transcriptAny(transcript []map[string]any) []any {
	items := make([]any, 0, len(transcript))
	for _, entry := range transcript {
		items = append(items, entry)
	}
	return items
}

func agentsForMeta(meta map[string]any) []string {
	rawSlots, _ := meta["slots"].([]any)
	if len(rawSlots) > 0 {
		agents := make([]string, 0, len(rawSlots))
		for _, rawSlot := range rawSlots {
			slot, _ := rawSlot.(map[string]any)
			agents = append(agents, firstNonEmpty(slot["backend"], "?"))
		}
		return agents
	}
	first := firstNonEmpty(meta["first"], "claude")
	second := "claude"
	if first == "claude" {
		second = "codex"
	}
	return []string{first, second}
}

func ledgerCountsLocal(value any) (int, int, int) {
	ledger := normalizeLedgerLocal(value)
	return len(ledger["settled"]), len(ledger["contested"]), len(ledger["withdrawn"])
}

func normalizeLedgerLocal(value any) map[string][]string {
	object, ok := value.(map[string]any)
	if !ok {
		return emptyLedgerLocal()
	}
	ledger := emptyLedgerLocal()
	for _, key := range []string{"settled", "contested", "withdrawn"} {
		rawItems, ok := object[key].([]any)
		if !ok {
			return emptyLedgerLocal()
		}
		for _, rawItem := range rawItems {
			text := strings.TrimSpace(stringFromAny(rawItem))
			if text != "" {
				ledger[key] = append(ledger[key], text)
			}
		}
	}
	return ledger
}

func emptyLedgerLocal() map[string][]string {
	return map[string][]string{"settled": []string{}, "contested": []string{}, "withdrawn": []string{}}
}

func difference(current []string, previous []string) []string {
	seen := map[string]bool{}
	for _, item := range previous {
		seen[item] = true
	}
	items := []string{}
	for _, item := range current {
		if !seen[item] {
			items = append(items, item)
		}
	}
	return items
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func stringFromAny(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}
