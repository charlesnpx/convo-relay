package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/engine"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/format"
	"github.com/charlesnpx/convo-relay/internal/readiness"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/relayv2"
	"github.com/charlesnpx/convo-relay/internal/session"
	"github.com/charlesnpx/convo-relay/internal/sessionstore"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if os.Args[1] == "--help" || os.Args[1] == "-h" {
		usageTo(os.Stdout)
		return
	}

	switch os.Args[1] {
	case "version":
		runVersion(os.Args[2:])
	case "list":
		runList(os.Args[2:])
	case "show":
		runShow(os.Args[2:])
	case "export":
		runExport(os.Args[2:])
	case "doctor":
		runDoctor(os.Args[2:])
	case "recipes":
		runRecipes(os.Args[2:])
	case "run":
		runRelay(os.Args[2:])
	case "resume":
		runResume(os.Args[2:])
	case "control":
		runControl(os.Args[2:])
	case "clean":
		runClean(os.Args[2:])
	default:
		if replacement, retired := retiredCommandReplacements[os.Args[1]]; retired {
			fmt.Fprintf(os.Stderr, "error: command %q was removed; use %s\n", os.Args[1], replacement)
		} else {
			fmt.Fprintf(os.Stderr, "error: unknown command %q\n", os.Args[1])
		}
		usage()
		os.Exit(2)
	}
}

// Retired command names fail loudly rather than forwarding. The map is only
// operator guidance; every replacement enters through its surviving command.
var retiredCommandReplacements = map[string]string{
	"--version":      "version",
	"approve":        "control approve",
	"backends":       "doctor",
	"capabilities":   "version --json",
	"cleanup":        "clean --all",
	"compile-recipe": "recipes compile",
	"contracts":      "show --json",
	"create-session": "run",
	"diff":           "show --diff",
	"display":        "show --json",
	"health":         "doctor",
	"install-skills": "make install-assets",
	"kill":           "control cancel",
	"proposals":      "show --proposals",
	"reject":         "control reject",
	"show-graph":     "show --graph",
	"steer":          "control steer",
	"stop":           "control cancel",
	"verify-export":  "export verify",
}

func runVersion(args []string) {
	flags := flag.NewFlagSet("version", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "Emit machine-readable version and format JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if len(flags.Args()) > 0 {
		fmt.Fprintf(os.Stderr, "error: version does not accept positional arguments: %s\n", strings.Join(flags.Args(), " "))
		os.Exit(2)
	}
	if !*jsonOutput {
		fmt.Println(cliVersion)
		return
	}
	writeJSON(map[string]any{
		"version":        cliVersion,
		"formats":        format.PublicFormats(),
		"digest_classes": format.DigestClasses(),
	})
}

func runRecipesCompile(args []string) {
	flags := flag.NewFlagSet("recipes compile", flag.ExitOnError)
	settingsPath := flags.String("settings", "", "Optional settings.toml path")
	integrationBundlePath := flags.String("integration-bundle", "", "Optional integration bundle JSON path")
	compositionPath := flags.String("composition-path", "root", "Composition path for compiled profile slots")
	relayDepth := flags.Int("relay-backend-depth", 0, "Current relay-backend nesting depth")
	maxRelayDepth := flags.Int("max-relay-backend-depth", 0, "Maximum relay-backend nesting depth; defaults to recipe max_depth")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable compile JSON")
	_ = flags.String("recipe-file", "", "Attach a transient recipe TOML source; may be repeated")
	_ = flags.String("generated-recipe-file", "", "Attach a generated transient recipe TOML source; may be repeated")
	extracted, cleanedArgs, err := extractMultiValueFlags(args, "recipe-file", "generated-recipe-file")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	if err := parseFlags(flags, cleanedArgs); err != nil {
		os.Exit(2)
	}
	if len(flags.Args()) == 0 {
		fmt.Fprintln(os.Stderr, "error: recipes compile requires a recipe id")
		os.Exit(2)
	}
	if len(flags.Args()) > 1 {
		fmt.Fprintf(os.Stderr, "error: recipes compile accepts one recipe id, got: %s\n", strings.Join(flags.Args(), " "))
		os.Exit(2)
	}
	recipeID := flags.Args()[0]

	sources := readTransientRecipeSourcesOrExit(extracted["recipe-file"], extracted["generated-recipe-file"])
	config, transientSources, err := recipes.LoadRuntimeConfigWithTransientSources(*settingsPath, sources)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	bundle, loadErr := recipes.LoadIntegrationBundle(*settingsPath, *integrationBundlePath)
	if loadErr != nil {
		failCompileRecipe(loadErr, *jsonOutput)
	}
	compileOptions := recipes.CompileOptions{
		CompositionPath:      *compositionPath,
		RelayBackendDepth:    *relayDepth,
		MaxRelayBackendDepth: *maxRelayDepth,
		ValidateExecutable:   true,
		TransientSources:     transientSources,
	}
	compileOptions.IntegrationBundle = bundle
	report, err := recipes.BuildCompileReport(recipeID, config, recipes.CompileTargetRoot, compileOptions)
	if err != nil {
		failCompileRecipe(err, *jsonOutput)
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Printf("Target: %s\n", report["target"])
	fmt.Printf("Recipe: %s\n", report["recipe_id"])
	if sourceDigest := stringValue(report["source_digest"]); sourceDigest != "" {
		fmt.Printf("Source digest: %s\n", sourceDigest)
	}
	fmt.Printf("Recipe digest: %s\n", report["recipe_digest"])
	fmt.Printf("Compiled plan digest: %s\n", report["compiled_plan_digest"])
	if launch, ok := report["launch"].(map[string]any); ok {
		fmt.Printf("Resolved backends: %s\n", strings.Join(stringItemsLocal(launch["agents"]), ","))
	}
	if compiled, ok := report["compiled_plan"].(map[string]any); ok {
		fmt.Printf("Plan preview: %s participant turn(s)\n", stringValue(compiled["participant_turns"]))
		if participants, ok := compiled["participants"].([]any); ok {
			fmt.Println("Participants:")
			for _, rawParticipant := range participants {
				participant, _ := rawParticipant.(map[string]any)
				fmt.Printf("  %s: profile=%s backend=%s model=%v effort=%v\n",
					stringValue(participant["slot_id"]),
					stringValue(participant["profile_id"]),
					stringValue(participant["backend"]),
					participant["model"],
					participant["effort"],
				)
			}
		}
	}
}

func runRecipes(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: recipes requires a subcommand: list, show, doctor, or compile")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		runRecipesList(args[1:])
	case "show":
		runRecipesShow(args[1:])
	case "doctor":
		runRecipesDoctor(args[1:])
	case "compile":
		runRecipesCompile(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown recipes subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func runRecipesList(args []string) {
	flags := flag.NewFlagSet("recipes list", flag.ExitOnError)
	settingsPath := flags.String("settings", "", "Optional settings.toml path")
	integrationBundlePath := flags.String("integration-bundle", "", "Optional integration bundle JSON path")
	statusFilter := flags.String("status", "", "Filter by status: usable, requires_integration, unavailable, invalid, skipped, or all")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable recipe list JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	filter := strings.TrimSpace(*statusFilter)
	if filter == "" && *jsonOutput {
		filter = "all"
	}
	if filter != "" {
		if err := validateRecipeStatusFilter(filter); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(2)
		}
	}
	bundle, err := recipes.LoadIntegrationBundle(*settingsPath, *integrationBundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	report, err := recipes.BuildRecipeCatalogReportWithOptions(*settingsPath, recipes.RecipeCatalogOptions{IntegrationBundle: bundle})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if filter == "" {
		records := recipes.FilterRecipeRecordsByStatuses(report.Recipes, recipes.RecipeStatusUsable, recipes.RecipeStatusRequiresIntegration)
		fmt.Println(recipes.FormatRecipeList(records))
		return
	}
	records := recipes.FilterRecipeRecords(report.Recipes, filter)
	if *jsonOutput {
		report.Recipes = records
		writeJSON(report)
		return
	}
	fmt.Println(recipes.FormatRecipeList(records))
}

func runRecipesShow(args []string) {
	flags := flag.NewFlagSet("recipes show", flag.ExitOnError)
	settingsPath := flags.String("settings", "", "Optional settings.toml path")
	integrationBundlePath := flags.String("integration-bundle", "", "Optional integration bundle JSON path")
	view := flags.String("view", "all", "View: all, declared, or resolved")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable recipe JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	recipeID := ""
	if len(flags.Args()) > 0 {
		recipeID = flags.Args()[0]
	}
	if strings.TrimSpace(recipeID) == "" {
		fmt.Fprintln(os.Stderr, "error: recipes show requires a recipe id")
		os.Exit(2)
	}
	if err := validateRecipeView(*view); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	bundle, err := recipes.LoadIntegrationBundle(*settingsPath, *integrationBundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	report, err := recipes.BuildRecipeCatalogReportWithOptions(*settingsPath, recipes.RecipeCatalogOptions{IntegrationBundle: bundle})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	record, ok := recipes.FindRecipeRecord(report.Recipes, recipeID)
	if !ok {
		fmt.Fprintf(os.Stderr, "error: unknown recipe %q\n", recipeID)
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(record)
		return
	}
	fmt.Println(recipes.FormatRecipeShow(record, *view))
}

func runRecipesDoctor(args []string) {
	flags := flag.NewFlagSet("recipes doctor", flag.ExitOnError)
	settingsPath := flags.String("settings", "", "Optional settings.toml path")
	integrationBundlePath := flags.String("integration-bundle", "", "Optional integration bundle JSON path")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable doctor JSON")
	_ = flags.String("recipe-file", "", "Attach a transient recipe TOML source; may be repeated")
	_ = flags.String("generated-recipe-file", "", "Attach a generated transient recipe TOML source; may be repeated")
	extracted, cleanedArgs, err := extractMultiValueFlags(args, "recipe-file", "generated-recipe-file")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	if err := parseFlags(flags, cleanedArgs); err != nil {
		os.Exit(2)
	}
	sources := readTransientRecipeSourcesOrExit(extracted["recipe-file"], extracted["generated-recipe-file"])
	bundle, err := recipes.LoadIntegrationBundle(*settingsPath, *integrationBundlePath)
	if err != nil {
		if *jsonOutput {
			writeJSON(map[string]any{"scope": "recipes", "status": "error", "error": err.Error()})
		} else {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
		}
		os.Exit(1)
	}
	report, err := recipes.BuildRecipeCatalogReportWithOptions(*settingsPath, recipes.RecipeCatalogOptions{
		TransientSources:  sources,
		IntegrationBundle: bundle,
	})
	if err != nil {
		if *jsonOutput {
			writeJSON(map[string]any{"scope": "recipes", "status": "error", "error": err.Error()})
		} else {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
		}
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(report)
	} else {
		fmt.Println(recipes.FormatRecipeDoctor(report))
	}
	if report.Status != "ok" {
		os.Exit(1)
	}
}

func validateRecipeStatusFilter(status string) error {
	switch strings.TrimSpace(status) {
	case "all", recipes.RecipeStatusUsable, recipes.RecipeStatusRequiresIntegration, recipes.RecipeStatusUnavailable, recipes.RecipeStatusInvalid, recipes.RecipeStatusSkipped:
		return nil
	default:
		return fmt.Errorf("--status must be one of usable, requires_integration, unavailable, invalid, skipped, or all")
	}
}

func failCompileRecipe(err error, jsonOutput bool) {
	if jsonOutput {
		var configErr recipes.ChildRelayConfigError
		var rootOnly *recipes.RootOnlyRecipeError
		var diagnosticErr *format.DiagnosticError
		var validationErr format.ValidationError
		switch {
		case errors.As(err, &configErr):
			writeJSON(configErr.ToMap())
		case errors.As(err, &rootOnly):
			writeJSON(rootOnly.ToMap())
		case errors.As(err, &diagnosticErr):
			writeJSON(diagnosticErr.ToMap())
		case errors.As(err, &validationErr):
			writeJSON(map[string]any{"code": "validation_error", "message": validationErr.Error()})
		default:
			writeJSON(map[string]any{"message": err.Error()})
		}
	} else {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
	}
	os.Exit(1)
}

func readTransientRecipeSourcesOrExit(recipeFiles []string, generatedRecipeFiles []string) []recipes.TransientRecipeSource {
	sources, err := recipes.ReadTransientRecipeSources(recipeFiles, generatedRecipeFiles, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	return sources
}

func validateRecipeView(view string) error {
	switch strings.TrimSpace(view) {
	case "all", "declared", "resolved":
		return nil
	default:
		return fmt.Errorf("--view must be one of all, declared, or resolved")
	}
}

func runList(args []string) {
	flags := flag.NewFlagSet("list", flag.ExitOnError)
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	limit := flags.Int("limit", 20, "Maximum sessions to list")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable session list")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	sessions, err := sessionstore.ListSessions(*relayHome, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(map[string]any{"sessions": sessions})
		return
	}
	if len(sessions) == 0 {
		fmt.Println("No relay sessions found.")
		return
	}
	for _, session := range sessions {
		title := firstNonEmptyString(stringValue(session["title"]), stringValue(session["task"]))
		if len(title) > 60 {
			title = strings.TrimSpace(title[:57]) + "..."
		}
		fmt.Printf(
			"  %.8s  %-10v  %-11v  %-13s  %2v rounds  %.19s  %s\n",
			stringValue(session["session_id"]),
			session["status"],
			session["mode"],
			strings.Join(agentsForSummary(session), ","),
			session["actual_rounds"],
			stringValue(session["created_at"]),
			title,
		)
		if root, ok := session["root"].(map[string]any); ok {
			for _, line := range strings.Split(v2FormatRootSummary(root), "\n") {
				fmt.Printf("             %s\n", line)
			}
		}
	}
}

func runShow(args []string) {
	flags := flag.NewFlagSet("show", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to inspect")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable JSON")
	graphOutput := flags.Bool("graph", false, "Show durable relay graph instead of transcript")
	diffOutput := flags.Bool("diff", false, "Show the durable-session diff")
	proposalsOutput := flags.Bool("proposals", false, "Show spawn proposals")
	traceNodeID := flags.String("trace", "", "Show raw child trace artifact for a graph node")
	fromRound := flags.Int("from-round", 0, "Show only rounds from N onward")
	roundsSpec := flags.String("rounds", "", "Filter rounds: '5+', '3-7', or '5,6'")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	viewCount := 0
	for _, selected := range []bool{*graphOutput, *diffOutput, *proposalsOutput, *traceNodeID != ""} {
		if selected {
			viewCount++
		}
	}
	if viewCount > 1 {
		fmt.Fprintln(os.Stderr, "error: show accepts only one of --graph, --diff, --proposals, or --trace")
		os.Exit(2)
	}
	resolvedSessionDir, _ := resolveSessionDirAndArgs(*sessionDir, *sessionID, *relayHome, flags.Args())
	sess, err := session.Open(resolvedSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *traceNodeID != "" {
		trace, err := v2BuildTraceReport(sess, *traceNodeID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		writeJSON(trace)
		return
	}
	if *graphOutput {
		report, err := relayv2.BuildGraphReport(sess)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		if *jsonOutput {
			writeJSON(report)
			return
		}
		fmt.Println(v2FormatGraphSummary(report))
		return
	}
	if *diffOutput {
		text, err := v2RenderDiff(sess)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		fmt.Print(text)
		return
	}
	if *proposalsOutput {
		report, err := v2ProposalReport(sess)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		if *jsonOutput {
			writeJSON(report)
			return
		}
		proposals, _ := report["proposals"].([]any)
		if len(proposals) == 0 {
			fmt.Printf("No spawn proposals for %s.\n", report["session_id"])
			return
		}
		for _, rawProposal := range proposals {
			proposal, _ := rawProposal.(map[string]any)
			fmt.Printf("%v  %-10v  %v\n", proposal["proposal_id"], proposal["status"], proposal["selected_recipe_id"])
			fmt.Printf("  reason: %v\n", proposal["reason"])
			fmt.Printf("  question: %v\n", proposal["delegated_question"])
		}
		return
	}
	report, err := v2ShowTranscriptReport(sess, *fromRound, *roundsSpec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Println(v2FormatTranscriptMarkdown(report))
}

func runExport(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: export requires a subcommand: create or verify")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		runExportCreate(args[1:])
	case "verify":
		runExportVerify(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown export subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func runExportCreate(args []string) {
	flags := flag.NewFlagSet("export create", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to export")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	jsonOutput := flags.Bool("json", false, "Write structured JSON instead of markdown")
	portableOutput := flags.Bool("portable", false, "Write a complete portable root-session directory")
	output := ""
	flags.StringVar(&output, "output", "", "Output path")
	flags.StringVar(&output, "o", "", "Alias for --output")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if strings.TrimSpace(output) == "" {
		fmt.Fprintln(os.Stderr, "error: export requires -o/--output")
		os.Exit(2)
	}
	resolvedSessionDir, _ := resolveSessionDirAndArgs(*sessionDir, *sessionID, *relayHome, flags.Args())
	sess, err := session.Open(resolvedSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *portableOutput {
		result, err := v2ExportPortable(sess, output, cliVersion)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		if *jsonOutput {
			writeJSON(map[string]any{
				"output":          result.Directory,
				"format":          result.Manifest["kind"],
				"manifest_digest": result.Manifest["manifest_digest"],
				"terminal_status": result.Manifest["terminal_status"],
			})
			return
		}
		fmt.Printf("Exported portable root session to %s\n", result.Directory)
		return
	}
	report, err := v2ExportReport(sess, *jsonOutput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	outputPath, err := writeExportOutput(report, output, *jsonOutput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		fmt.Printf("{\"output\":%q,\"session_id\":%q,\"incomplete\":%v}\n", outputPath, stringValue(report["session_id"]), report["incomplete"])
		return
	}
	fmt.Printf("Exported %s to %s\n", stringValue(report["session_id"]), outputPath)
}

func runExportVerify(args []string) {
	flags := flag.NewFlagSet("export verify", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "Write structured JSON instead of markdown")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if len(flags.Args()) != 1 {
		fmt.Fprintln(os.Stderr, "error: export verify requires a portable export directory")
		os.Exit(2)
	}
	report, err := v2VerifyPortableDirectory(flags.Args()[0])
	if err != nil {
		if *jsonOutput {
			writeJSON(map[string]any{
				"format": format.BundleV1,
				"status": "invalid",
				"error":  err.Error(),
			})
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Printf("Portable export %s: %s\n", flags.Args()[0], report["status"])
}

func runDoctor(args []string) {
	flags := flag.NewFlagSet("doctor", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to inspect")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	settingsPath := flags.String("settings", "", "Optional settings.toml path for global config validation")
	probeAuth := flags.Bool("probe-auth", false, "Run supported non-model authentication probes")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable doctor JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	var health map[string]any
	if *sessionDir != "" || *sessionID != "" {
		resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
		sess, err := session.Open(resolvedSessionDir)
		if err == nil {
			health, err = v2SessionHealthReport(sess)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
	} else {
		health = v2GlobalHealthReport(*settingsPath)
	}
	backends := readiness.CheckRegistered(context.Background(), readiness.Options{ProbeAuth: *probeAuth})
	if *jsonOutput {
		writeJSON(map[string]any{"health": health, "backends": backends})
		return
	}
	fmt.Println(v2FormatHealthReport(health))
	fmt.Println()
	fmt.Println(readiness.FormatReport(backends))
}

func runRelay(args []string) {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	task := flags.String("task", "", "Task text for the relay")
	recipeID := flags.String("recipe", "", "Run a configured recipe as the direct root execution")
	integrationBundlePath := flags.String("integration-bundle", "", "Integration bundle JSON for an integration-bound root recipe")
	workspaceMode := flags.String("workspace", "current", "Root recipe workspace mode: current or head-copy")
	allowDirtySource := flags.Bool("allow-dirty-source", false, "Use committed HEAD for isolated root execution when the source is dirty")
	var inputBindings repeatableFlagValue
	flags.Var(&inputBindings, "input", "Bind a named root recipe input as name=path; may be repeated")
	sessionDir := flags.String("session-dir", "", "Optional explicit session directory")
	sessionID := flags.String("session-id", "", "Optional session id when --session-dir is omitted")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	agentsRaw := flags.String("agents", "codex,codex", "One backend shorthand or two comma-separated backends")
	rounds := flags.Int("rounds", 0, "Run exactly N rounds; omit for auto-stop up to --max-rounds")
	maxRounds := flags.Int("max-rounds", 50, "Auto-stop safety cap")
	timeout := flags.Int("timeout", 600, "Per-turn timeout in seconds")
	stallTimeout := flags.Int("stall-timeout", 300, "Claude JSONL stall timeout in seconds")
	mode := flags.String("mode", "adversarial", "Relay mode: adversarial, cooperative, or steelman")
	dynamicMode := flags.String("dynamic", "off", "Dynamic expansion mode: off, ask, or auto-safe")
	investigationMode := flags.String("investigation", "auto", "Investigation mode: auto, normal, or context_only")
	settingsPath := flags.String("settings", "", "Optional settings.toml path")
	launchCWD := flags.String("launch-cwd", "", "Working directory for backend subprocesses; defaults to the current directory")
	facilitatorBackend := flags.String("facilitator-backend", "", "Facilitator backend: codex, claude, or gemini")
	facilitatorModel := flags.String("facilitator-model", "", "Facilitator model override")
	facilitatorEffort := flags.String("facilitator-effort", "", "Facilitator effort override")
	taskPlanPath := flags.String("task-plan", "", "Attach a launch task plan from a JSON or text file")
	quick := flags.Bool("quick", false, "Force exactly 3 rounds")
	output := ""
	_ = flags.String("context", "", "Attach context text files; may be repeated. Limits: 1 MiB per file, 2 MiB total")
	_ = flags.String("skill", "", "Attach capability text files; may be repeated")
	_ = flags.String("recipe-file", "", "Attach a session-scoped transient recipe TOML file; may be repeated")
	_ = flags.String("generated-recipe-file", "", "Attach a generated session-scoped transient recipe TOML file; may be repeated")
	flags.StringVar(&output, "output", "", "Write transcript or JSON export to file")
	flags.StringVar(&output, "o", "", "Alias for --output")
	modelA := flags.String("model-a", "", "Model for slot_0")
	effortA := flags.String("effort-a", "", "Effort for slot_0")
	modelB := flags.String("model-b", "", "Model for slot_1")
	effortB := flags.String("effort-b", "", "Effort for slot_1")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable run JSON")
	extracted, cleanedArgs, err := extractMultiValueFlags(args, "context", "skill", "recipe-file", "generated-recipe-file")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	if err := parseFlags(flags, cleanedArgs); err != nil {
		os.Exit(2)
	}
	if _, err := v2WorkspaceMode(*workspaceMode); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	if *task == "" && len(flags.Args()) > 0 {
		*task = strings.Join(flags.Args(), " ")
	}
	visited := visitedFlagNames(flags)
	recipeRequested := strings.TrimSpace(*recipeID) != "" || visited["recipe"] || visited["integration-bundle"] || visited["workspace"] || visited["allow-dirty-source"] || visited["input"]
	if recipeRequested {
		if strings.TrimSpace(*recipeID) == "" {
			fmt.Fprintln(os.Stderr, "error: --recipe is required when root recipe run options are used")
			os.Exit(2)
		}
		if err := validateRecipeRunStructuralOverrides(visited); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(2)
		}
		sourceAnchor, err := resolveRunRecipeSourceAnchor(*launchCWD)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: launch CWD: %s\n", err)
			os.Exit(2)
		}
		contextFiles := anchorRecipeCLIPaths(sourceAnchor, extracted["context"])
		skillFiles := anchorRecipeCLIPaths(sourceAnchor, extracted["skill"])
		recipeFiles := anchorRecipeCLIPaths(sourceAnchor, extracted["recipe-file"])
		generatedRecipeFiles := anchorRecipeCLIPaths(sourceAnchor, extracted["generated-recipe-file"])
		launchPlan, err := loadLaunchPlanFile(anchorRecipeCLIPath(sourceAnchor, *taskPlanPath))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		transientRecipeSources := readTransientRecipeSourcesOrExit(recipeFiles, generatedRecipeFiles)
		ctx, stop := signal.NotifyContext(context.Background(), commandInterruptSignals()...)
		defer stop()
		result, err := v2RunRecipe(ctx, v2RecipeRunOptions{
			SessionDir:            *sessionDir,
			SessionID:             *sessionID,
			RelayHome:             *relayHome,
			Task:                  *task,
			RecipeID:              *recipeID,
			ContextFiles:          contextFiles,
			SkillFiles:            skillFiles,
			TransientSources:      transientRecipeSources,
			IntegrationBundlePath: anchorRecipeCLIPath(sourceAnchor, *integrationBundlePath),
			InputBindings:         append([]string{}, inputBindings...),
			WorkspaceMode:         *workspaceMode,
			AllowDirtySource:      *allowDirtySource,
			SettingsPath:          anchorRecipeCLIPath(sourceAnchor, *settingsPath),
			LaunchCWD:             sourceAnchor,
			TimeoutSeconds:        *timeout,
			StallTimeoutSeconds:   *stallTimeout,
			Investigation:         *investigationMode,
			LaunchPlan:            launchPlan,
			WorkspaceWarning: func(warning v2WorkspaceWarning) {
				fmt.Fprintf(
					os.Stderr,
					"warning: %s (staged=%d unstaged=%d untracked=%d)\n",
					warning.Message,
					warning.StagedChanges,
					warning.UnstagedChanges,
					warning.UntrackedChanges,
				)
			},
		})
		writeRunnerResult(result, err, *jsonOutput, output)
		return
	}
	launchPlan, err := loadLaunchPlanFile(*taskPlanPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	transientRecipeSources := readTransientRecipeSourcesOrExit(extracted["recipe-file"], extracted["generated-recipe-file"])
	ctx, stop := signal.NotifyContext(context.Background(), commandInterruptSignals()...)
	defer stop()
	result, err := v2RunOrdinary(ctx, v2OrdinaryRunOptions{
		SessionDir:          *sessionDir,
		SessionID:           *sessionID,
		RelayHome:           *relayHome,
		Task:                *task,
		Agents:              *agentsRaw,
		Rounds:              *rounds,
		MaxRounds:           *maxRounds,
		TimeoutSeconds:      *timeout,
		StallTimeoutSeconds: *stallTimeout,
		Mode:                *mode,
		Dynamic:             *dynamicMode,
		Investigation:       *investigationMode,
		SettingsPath:        *settingsPath,
		LaunchCWD:           *launchCWD,
		FacilitatorBackend:  *facilitatorBackend,
		FacilitatorModel:    *facilitatorModel,
		FacilitatorEffort:   *facilitatorEffort,
		ModelA:              *modelA,
		EffortA:             *effortA,
		ModelB:              *modelB,
		EffortB:             *effortB,
		Quick:               *quick,
		ContextFiles:        extracted["context"],
		SkillFiles:          extracted["skill"],
		TransientSources:    transientRecipeSources,
		LaunchPlan:          launchPlan,
	})
	writeRunnerResult(result, err, *jsonOutput, output)
}

type repeatableFlagValue []string

func (v *repeatableFlagValue) String() string {
	if v == nil {
		return ""
	}
	return strings.Join(*v, ",")
}

func (v *repeatableFlagValue) Set(value string) error {
	if value == "" {
		return fmt.Errorf("value must not be empty")
	}
	*v = append(*v, value)
	return nil
}

func visitedFlagNames(flags *flag.FlagSet) map[string]bool {
	visited := map[string]bool{}
	flags.Visit(func(flagValue *flag.Flag) {
		visited[flagValue.Name] = true
	})
	return visited
}

func validateRecipeRunStructuralOverrides(visited map[string]bool) error {
	structural := []string{
		"agents",
		"model-a", "effort-a", "model-b", "effort-b",
		"facilitator-backend", "facilitator-model", "facilitator-effort",
		"mode", "rounds", "max-rounds", "quick", "dynamic",
	}
	conflicts := []string{}
	for _, name := range structural {
		if visited[name] {
			conflicts = append(conflicts, "--"+name)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return fmt.Errorf("run --recipe does not accept structural overrides: %s", strings.Join(conflicts, ", "))
}

func resolveRunRecipeSourceAnchor(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		value = "."
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func anchorRecipeCLIPaths(sourceAnchor string, values []string) []string {
	anchored := make([]string, len(values))
	for index, value := range values {
		anchored[index] = anchorRecipeCLIPath(sourceAnchor, value)
	}
	return anchored
}

func anchorRecipeCLIPath(sourceAnchor string, value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "-" || filepath.IsAbs(value) {
		return value
	}
	if value == "~" || strings.HasPrefix(value, "~"+string(filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil {
			if value == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(value, "~"+string(filepath.Separator)))
		}
	}
	return filepath.Join(sourceAnchor, value)
}

func runControl(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: control requires a subcommand: steer, approve, reject, or cancel")
		os.Exit(2)
	}
	switch args[0] {
	case "steer":
		runControlSteer(args[1:])
	case "approve":
		runControlApprove(args[1:])
	case "reject":
		runControlReject(args[1:])
	case "cancel":
		runControlCancel(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown control subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func runControlApprove(args []string) {
	flags := flag.NewFlagSet("control approve", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	proposalID := flags.String("proposal", "", "Proposal id to approve")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable approval JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	resolvedSessionDir, remaining := resolveSessionDirAndArgs(*sessionDir, *sessionID, *relayHome, flags.Args())
	if *proposalID == "" && len(remaining) > 0 {
		*proposalID = remaining[0]
	}
	if *proposalID == "" {
		fmt.Fprintln(os.Stderr, "error: control approve requires a session and proposal id")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), commandInterruptSignals()...)
	defer stop()
	sess, err := session.Open(resolvedSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	runtime, err := relayv2.LoadRuntime(sess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if err := engine.ApproveChild(ctx, sess, *proposalID, runtime.Recipes); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
	executionCWD, err := relayv2.ExecutionCWD(ctx, sess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	outcome, err := engine.Resume(ctx, sess, relayv2.NewDeps(runtime, executionCWD), "", 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
	proposals, err := v2ProposalReport(sess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	report := map[string]any{
		"session_id":  sess.Plan.SessionID,
		"proposal_id": *proposalID,
		"status":      outcome.Status,
	}
	for _, rawProposal := range proposals["proposals"].([]any) {
		proposal, _ := rawProposal.(map[string]any)
		if stringValue(proposal["proposal_id"]) != *proposalID {
			continue
		}
		if status := stringValue(proposal["status"]); status != "" {
			report["status"] = status
		}
		if childID := stringValue(proposal["child_session_id"]); childID != "" {
			report["child_session_id"] = childID
			report["child_node_id"] = childID
		}
		break
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Printf("Approved %v and %v child %v into %v.\n", report["proposal_id"], report["status"], report["child_node_id"], report["session_id"])
}

func runControlReject(args []string) {
	flags := flag.NewFlagSet("control reject", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	proposalID := flags.String("proposal", "", "Proposal id to reject")
	reason := flags.String("reason", "", "Rejection reason")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable rejection JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	resolvedSessionDir, remaining := resolveSessionDirAndArgs(*sessionDir, *sessionID, *relayHome, flags.Args())
	if *proposalID == "" && len(remaining) > 0 {
		*proposalID = remaining[0]
	}
	if *proposalID == "" {
		fmt.Fprintln(os.Stderr, "error: control reject requires a session and proposal id")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), commandInterruptSignals()...)
	defer stop()
	sess, err := session.Open(resolvedSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if err := engine.RejectChild(ctx, sess, *proposalID, *reason); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
	report := map[string]any{"session_id": sess.Plan.SessionID, "proposal_id": *proposalID, "status": "rejected"}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Printf("Rejected %v for %v.\n", report["proposal_id"], report["session_id"])
}

func runResume(args []string) {
	flags := flag.NewFlagSet("resume", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to resume")
	sessionID := flags.String("session-id", "", "Session id under --home when --session-dir is omitted")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	prompt := flags.String("prompt", "", "Optional new direction for the next turn")
	_ = flags.String("mode", "", "Typed mode control for resumed rounds: adversarial, cooperative, or steelman")
	rounds := flags.Int("rounds", 0, "Resume for exactly N additional rounds; omit for auto-stop")
	_ = flags.Int("max-rounds", 50, "Additional-round safety cap when --rounds is omitted")
	_ = flags.Int("timeout", 600, "Per-turn timeout in seconds")
	_ = flags.Int("stall-timeout", 300, "Claude JSONL stall timeout in seconds")
	_ = flags.String("settings", "", "Optional settings.toml path")
	_ = flags.String("facilitator-model", "", "Facilitator model override")
	_ = flags.String("facilitator-effort", "", "Facilitator effort override")
	quick := flags.Bool("quick", false, "Force exactly 3 additional rounds")
	output := ""
	flags.StringVar(&output, "output", "", "Write transcript or JSON export to file")
	flags.StringVar(&output, "o", "", "Alias for --output")
	_ = flags.String("model-a", "", "Model override for slot_0")
	_ = flags.String("effort-a", "", "Effort override for slot_0")
	_ = flags.String("model-b", "", "Model override for slot_1")
	_ = flags.String("effort-b", "", "Effort override for slot_1")
	_ = flags.String("replace-a", "", "Advanced: replace slot_0 backend/profile for resumed turns")
	_ = flags.String("replace-b", "", "Advanced: replace slot_1 backend/profile for resumed turns")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable run JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	resolvedSessionDir, remaining := resolveSessionDirAndArgs(*sessionDir, *sessionID, *relayHome, flags.Args())
	if *prompt == "" && len(remaining) > 0 {
		*prompt = strings.Join(remaining, " ")
	}
	effectiveRounds := *rounds
	if *quick {
		effectiveRounds = 3
	}
	ctx, stop := signal.NotifyContext(context.Background(), commandInterruptSignals()...)
	defer stop()
	result, err := v2RunResume(ctx, resolvedSessionDir, v2ResumeOptions{Prompt: *prompt, RequestedTurns: effectiveRounds})
	writeRunnerResult(result, err, *jsonOutput, output)
}

func runControlCancel(args []string) {
	flags := flag.NewFlagSet("control cancel", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to cancel")
	sessionID := flags.String("session-id", "", "Session id under --home when --session-dir is omitted")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable cancellation JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	resolvedSessionDir, _ := resolveSessionDirAndArgs(*sessionDir, *sessionID, *relayHome, flags.Args())
	sess, err := session.Open(resolvedSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	report, err := v2CancelReport(sess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Printf("Session %s: %s\n", report["session_id"], report["status"])
}

func runControlSteer(args []string) {
	flags := flag.NewFlagSet("control steer", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	prompt := flags.String("prompt", "", "Prompt to inject before the next relay turn")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable steering JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	resolvedSessionDir, remaining := resolveSessionDirAndArgs(*sessionDir, *sessionID, *relayHome, flags.Args())
	if *prompt == "" && len(remaining) > 0 {
		*prompt = strings.Join(remaining, " ")
	}
	promptText := strings.TrimSpace(*prompt)
	if promptText == "" {
		fmt.Fprintln(os.Stderr, "error: steering prompt cannot be empty")
		os.Exit(1)
	}
	sess, err := session.Open(resolvedSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if err := engine.QueueSteering(sess, promptText); err != nil {
		var locked *eventlog.WriterLockedError
		if errors.As(err, &locked) {
			err = fmt.Errorf("session %s is running; interrupt its relay process directly, then queue steering before its next resume", sess.Plan.SessionID)
		}
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	item := map[string]any{
		"id":         "steering-queued",
		"session_id": sess.Plan.SessionID,
		"prompt":     promptText,
		"status":     "queued",
	}
	report := map[string]any{"session_id": filepath.Base(filepath.Clean(resolvedSessionDir)), "steering": item}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Printf("Queued steering for %.8s: %.8s\n", report["session_id"], item["id"])
}

func runClean(args []string) {
	flags := flag.NewFlagSet("clean", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	all := flags.Bool("all", false, "Mark orphaned sessions under --home")
	limit := flags.Int("limit", 500, "Maximum sessions to scan with --all")
	force := flags.Bool("force", false, "Bypass the active-writer check")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable clean JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *all {
		if *sessionID != "" || *sessionDir != "" || len(flags.Args()) > 0 {
			fmt.Fprintln(os.Stderr, "error: clean --all does not accept a session argument")
			os.Exit(2)
		}
		report, err := sessionstore.CleanupSessions(*relayHome, *limit, *force)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		if *jsonOutput {
			writeJSON(report)
			return
		}
		count := intValue(report["orphaned_count"])
		if count == 0 {
			fmt.Println("No orphaned sessions found.")
			return
		}
		for _, rawItem := range asSlice(report["orphaned"]) {
			item, _ := rawItem.(map[string]any)
			fmt.Printf("  Marked orphaned: %.8s  %s\n", item["session_id"], item["title"])
		}
		fmt.Printf("\n%d session(s) marked as orphaned.\n", count)
		return
	}
	resolvedSessionDir, _ := resolveSessionDirAndArgs(*sessionDir, *sessionID, *relayHome, flags.Args())
	report, err := sessionstore.CleanSession(resolvedSessionDir, *force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Printf("Deleted session %.8s: %s\n", report["session_id"], report["title"])
}

func loadLaunchPlanFile(path string) (any, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("unable to read task plan file %q: %w", path, err)
	}
	stripped := strings.TrimSpace(string(data))
	if stripped == "" {
		return nil, nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(stripped), &parsed); err != nil {
		return stripped, nil
	}
	return normalizeLaunchPlan(parsed), nil
}

func normalizeLaunchPlan(plan any) any {
	switch value := plan.(type) {
	case nil:
		return nil
	case string:
		if text := strings.TrimSpace(value); text != "" {
			return text
		}
		return nil
	case []any:
		return normalizeLaunchPlan(map[string]any{"plan": value})
	case map[string]any:
		normalized := map[string]any{}
		if explanation := strings.TrimSpace(stringValue(value["explanation"])); explanation != "" {
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
					step = strings.TrimSpace(stringValue(item["step"]))
					if rawStatus := strings.TrimSpace(stringValue(item["status"])); rawStatus != "" {
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

func extractMultiValueFlags(args []string, names ...string) (map[string][]string, []string, error) {
	nameSet := map[string]bool{}
	for _, name := range names {
		nameSet[name] = true
	}
	values := map[string][]string{}
	cleaned := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !isFlagToken(arg) {
			cleaned = append(cleaned, arg)
			continue
		}
		name, hasInlineValue := flagName(arg)
		if !nameSet[name] {
			cleaned = append(cleaned, arg)
			continue
		}
		if hasInlineValue {
			_, rawValue, _ := strings.Cut(arg, "=")
			for _, value := range strings.Split(rawValue, ",") {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					values[name] = append(values[name], trimmed)
				}
			}
		} else {
			for index+1 < len(args) && !isFlagToken(args[index+1]) {
				index++
				values[name] = append(values[name], args[index])
			}
		}
		if len(values[name]) == 0 {
			return nil, nil, fmt.Errorf("--%s requires at least one file", name)
		}
	}
	return values, cleaned, nil
}

func parseFlags(flags *flag.FlagSet, args []string) error {
	normalized := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if isFlagToken(arg) {
			normalized = append(normalized, arg)
			name, hasInlineValue := flagName(arg)
			if flagValue := flags.Lookup(name); flagValue != nil && !hasInlineValue && !isBoolFlag(flagValue) && index+1 < len(args) {
				index++
				normalized = append(normalized, args[index])
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	return flags.Parse(append(normalized, positionals...))
}

func isFlagToken(arg string) bool {
	return strings.HasPrefix(arg, "-") && arg != "-"
}

func flagName(arg string) (string, bool) {
	name := strings.TrimLeft(arg, "-")
	if before, _, found := strings.Cut(name, "="); found {
		return before, true
	}
	return name, false
}

type boolFlag interface {
	IsBoolFlag() bool
}

func isBoolFlag(flagValue *flag.Flag) bool {
	boolean, ok := flagValue.Value.(boolFlag)
	return ok && boolean.IsBoolFlag()
}

func resolveSessionDirAndArgs(sessionDir string, sessionID string, relayHome string, args []string) (string, []string) {
	if sessionID == "" && sessionDir == "" && len(args) > 0 {
		sessionID, args = args[0], args[1:]
	}
	return resolveSessionDirOrExit(sessionDir, sessionID, relayHome), args
}

func resolveSessionDirOrExit(sessionDir string, sessionID string, relayHome string) string {
	resolved, err := sessionstore.ResolveSessionDir(relayHome, sessionDir, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	return resolved
}

func writeRunnerResult(result map[string]any, err error, jsonOutput bool, outputPath string) {
	emitErr := emitRunnerResult(os.Stdout, result, err, jsonOutput, outputPath)
	if emitErr != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", emitErr)
		if errors.Is(emitErr, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
}

func emitRunnerResult(writer io.Writer, result map[string]any, runErr error, jsonOutput bool, outputPath string) error {
	if result == nil {
		if runErr != nil {
			return runErr
		}
		return errors.New("runner returned no session result")
	}
	savedOutput := ""
	var saveErr error
	if strings.TrimSpace(outputPath) != "" {
		savedOutput, saveErr = saveRunnerOutput(result, outputPath, jsonOutput)
	}
	if jsonOutput {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return errors.Join(runErr, saveErr, encoder.Encode(result))
	}
	var emitErr error
	write := func(format string, args ...any) {
		_, err := fmt.Fprintf(writer, format, args...)
		emitErr = errors.Join(emitErr, err)
	}
	if result["execution_kind"] == "recipe" {
		status := firstNonEmptyString(stringValue(result["status"]), "completed")
		write("Session %s %s at %s\n", result["session_id"], status, result["session_dir"])
		write("Participant turns: %v/%v\n", result["actual_participant_turns"], result["participant_turns"])
		if savedOutput != "" {
			write("Output: %s\n", savedOutput)
		}
		return errors.Join(runErr, saveErr, emitErr)
	}
	status := firstNonEmptyString(stringValue(result["status"]), "completed")
	write("Session %s %s at %s\n", result["session_id"], status, result["session_dir"])
	write("Rounds: %v/%v\n", result["actual_rounds"], result["max_rounds"])
	if savedOutput != "" {
		write("Output: %s\n", savedOutput)
	}
	return errors.Join(runErr, saveErr, emitErr)
}

func saveRunnerOutput(result map[string]any, outputPath string, jsonOutput bool) (string, error) {
	outPath, err := filepath.Abs(outputPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return "", err
	}
	var body []byte
	if jsonOutput {
		body, err = json.MarshalIndent(result, "", "  ")
		if err != nil {
			return "", err
		}
		body = append(body, '\n')
	} else {
		sessionDir := stringValue(result["session_dir"])
		if sessionDir == "" {
			return "", fmt.Errorf("cannot write transcript output without session_dir")
		}
		reportInput := make(map[string]any, len(result))
		for key, value := range result {
			reportInput[key] = value
		}
		if summary, ok := result["summary"].(map[string]any); ok {
			copiedSummary := make(map[string]any, len(summary))
			for key, value := range summary {
				copiedSummary[key] = value
			}
			reportInput["summary"] = copiedSummary
		}
		report, err := v2ProjectShowTranscriptReport(reportInput, 0, "")
		if err != nil {
			return "", err
		}
		body = []byte(v2FormatTranscriptMarkdown(report))
	}
	if err := os.WriteFile(outPath, body, 0o644); err != nil {
		return "", err
	}
	return outPath, nil
}

func writeExportOutput(report map[string]any, outputPath string, jsonOutput bool) (string, error) {
	outPath, err := filepath.Abs(outputPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return "", err
	}
	var body []byte
	if jsonOutput {
		body, err = json.MarshalIndent(report, "", "  ")
		if err != nil {
			return "", err
		}
		body = append(body, '\n')
	} else {
		body = []byte(v2FormatExportMarkdown(report))
	}
	if err := os.WriteFile(outPath, body, 0o644); err != nil {
		return "", err
	}
	return outPath, nil
}

func writeJSON(value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}

func usage() {
	usageTo(os.Stderr)
}

func usageTo(writer io.Writer) {
	fmt.Fprintln(writer, "usage:")
	fmt.Fprintln(writer, "  convo-relay run --task <task> --agents codex,codex --rounds 2 --json")
	fmt.Fprintln(writer, "  convo-relay run --task <task> --recipe <id> [--integration-bundle <path>] [--input name=path] [--workspace current|head-copy] --json")
	fmt.Fprintln(writer, "  convo-relay resume <session-id-prefix> --mode steelman --rounds 1 --json")
	fmt.Fprintln(writer, "  convo-relay list --home <relay-home> --json")
	fmt.Fprintln(writer, "  convo-relay show <session-id-prefix> --json")
	fmt.Fprintln(writer, "  convo-relay show <session-id-prefix> --graph --json")
	fmt.Fprintln(writer, "  convo-relay show <session-id-prefix> --trace <node-id>")
	fmt.Fprintln(writer, "  convo-relay control steer <session-id-prefix> <prompt>")
	fmt.Fprintln(writer, "  convo-relay control approve <session-id-prefix> <proposal-id> --json")
	fmt.Fprintln(writer, "  convo-relay control cancel <session-id-prefix>")
	fmt.Fprintln(writer, "  convo-relay export create <session-id-prefix> -o transcript.md")
	fmt.Fprintln(writer, "  convo-relay export create <session-id-prefix> --portable -o bundle-directory --json")
	fmt.Fprintln(writer, "  convo-relay export verify <bundle-directory> --json")
	fmt.Fprintln(writer, "  convo-relay recipes list --json")
	fmt.Fprintln(writer, "  convo-relay recipes show review-panel")
	fmt.Fprintln(writer, "  convo-relay recipes doctor")
	fmt.Fprintln(writer, "  convo-relay recipes compile <id> [--integration-bundle <path>] --json")
	fmt.Fprintln(writer, "  convo-relay clean <session-id-prefix>")
	fmt.Fprintln(writer, "  convo-relay clean --all --home <relay-home> --json")
	fmt.Fprintln(writer, "  convo-relay doctor [session-id-prefix] [--probe-auth] --json")
	fmt.Fprintln(writer, "  convo-relay version [--json]")
}

var cliVersion = "1.0.0"

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func stringItemsLocal(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return []string{}
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text := strings.TrimSpace(stringValue(item))
		if text != "" {
			result = append(result, text)
		}
	}
	return result
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func agentsForSummary(session map[string]any) []string {
	if slots, ok := session["slots"].([]any); ok && len(slots) > 0 {
		agents := make([]string, 0, len(slots))
		for _, rawSlot := range slots {
			slot, _ := rawSlot.(map[string]any)
			agents = append(agents, firstNonEmptyString(stringValue(slot["backend"]), stringValue(slot["slot_id"]), "?"))
		}
		return agents
	}
	if agents, ok := session["agents"].([]any); ok && len(agents) > 0 {
		values := make([]string, 0, len(agents))
		for _, agent := range agents {
			values = append(values, stringValue(agent))
		}
		return values
	}
	first := firstNonEmptyString(stringValue(session["first"]), "claude")
	second := "claude"
	if first == "claude" {
		second = "codex"
	}
	return []string{first, second}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func asSlice(value any) []any {
	items, _ := value.([]any)
	return items
}
