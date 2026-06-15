package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/runner"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version", "--version", "-version":
		fmt.Println(cliVersion)
	case "install-skills":
		runInstallSkills(os.Args[2:])
	case "list":
		runList(os.Args[2:])
	case "show":
		runShow(os.Args[2:])
	case "export":
		runExport(os.Args[2:])
	case "health":
		runHealth(os.Args[2:])
	case "contracts":
		runContracts(os.Args[2:])
	case "show-graph":
		runShowGraph(os.Args[2:])
	case "recipes":
		runRecipes(os.Args[2:])
	case "compile-recipe":
		runCompileRecipe(os.Args[2:])
	case "create-session":
		runCreateSession(os.Args[2:])
	case "run":
		runRelay(os.Args[2:])
	case "resume":
		runResume(os.Args[2:])
	case "steer":
		runSteer(os.Args[2:])
	case "proposals":
		runProposals(os.Args[2:])
	case "approve":
		runApprove(os.Args[2:])
	case "reject":
		runReject(os.Args[2:])
	case "stop":
		runStop(os.Args[2:], false)
	case "kill":
		runStop(os.Args[2:], true)
	case "diff":
		runDiff(os.Args[2:])
	case "clean":
		runClean(os.Args[2:])
	case "cleanup":
		runCleanup(os.Args[2:])
	case "display":
		runDisplay(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func runContracts(args []string) {
	flags := flag.NewFlagSet("contracts", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to inspect")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable inspection JSON")
	includeRaw := flags.Bool("raw", false, "Include loaded artifact payloads")
	refID := flags.String("ref", "", "Resolve one artifact ref id from the index")
	digest := flags.String("digest", "", "Digest to disambiguate --ref")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)

	report, err := inspect.BuildContractsReport(resolvedSessionDir, *includeRaw, *refID, *digest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}
	fmt.Println(inspect.FormatContractsReport(report))
}

func runShowGraph(args []string) {
	flags := flag.NewFlagSet("show-graph", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to inspect")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable graph JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)

	report, err := inspect.BuildShowGraphReport(resolvedSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}
	fmt.Println(inspect.FormatGraphSummary(report))
}

func runCompileRecipe(args []string) {
	flags := flag.NewFlagSet("compile-recipe", flag.ExitOnError)
	recipeID := flags.String("recipe", "", "Relay recipe id to compile")
	settingsPath := flags.String("settings", "", "Optional settings.toml path")
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
	if *recipeID == "" {
		fmt.Fprintln(os.Stderr, "error: --recipe is required")
		os.Exit(2)
	}

	sources := readTransientRecipeSourcesOrExit(extracted["recipe-file"], extracted["generated-recipe-file"])
	config, transientSources, err := recipes.LoadRuntimeConfigWithTransientSources(*settingsPath, sources)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	report, err := recipes.BuildCompileReport(*recipeID, config, recipes.CompileOptions{
		CompositionPath:      *compositionPath,
		RelayBackendDepth:    *relayDepth,
		MaxRelayBackendDepth: *maxRelayDepth,
		ValidateExecutable:   true,
		TransientSources:     transientSources,
	})
	if err != nil {
		var configErr recipes.ChildRelayConfigError
		if *jsonOutput && errors.As(err, &configErr) {
			writeJSON(configErr.ToMap())
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
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
		fmt.Fprintln(os.Stderr, "error: recipes requires a subcommand: list, show, or doctor")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		runRecipesList(args[1:])
	case "show":
		runRecipesShow(args[1:])
	case "doctor":
		runRecipesDoctor(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown recipes subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func runRecipesList(args []string) {
	flags := flag.NewFlagSet("recipes list", flag.ExitOnError)
	settingsPath := flags.String("settings", "", "Optional settings.toml path")
	statusFilter := flags.String("status", "", "Filter by status: usable, unavailable, invalid, skipped, or all")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable recipe list JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	report, err := recipes.BuildRecipeCatalogReport(*settingsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	filter := strings.TrimSpace(*statusFilter)
	if filter == "" {
		if *jsonOutput {
			filter = "all"
		} else {
			filter = recipes.RecipeStatusUsable
		}
	}
	if err := validateRecipeStatusFilter(filter); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
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
	report, err := recipes.BuildRecipeCatalogReport(*settingsPath)
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
	report, err := recipes.BuildRecipeCatalogReportWithTransientSources(*settingsPath, sources)
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
	case "all", recipes.RecipeStatusUsable, recipes.RecipeStatusUnavailable, recipes.RecipeStatusInvalid, recipes.RecipeStatusSkipped:
		return nil
	default:
		return fmt.Errorf("--status must be one of usable, unavailable, invalid, skipped, or all")
	}
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

func runCreateSession(args []string) {
	flags := flag.NewFlagSet("create-session", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to create")
	task := flags.String("task", "Go-created compatibility session", "Session task text")
	settingsPath := flags.String("settings", "", "Optional settings.toml path")
	recipeID := flags.String("recipe", "review-panel", "Recipe id for sample child contract artifacts")
	withChildContracts := flags.Bool("with-child-contracts", true, "Write sample child contract artifacts and completion event")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable creation JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionDir == "" {
		fmt.Fprintln(os.Stderr, "error: --session-dir is required")
		os.Exit(2)
	}

	report, err := createCompatibilitySession(*sessionDir, *task, *settingsPath, *recipeID, *withChildContracts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Printf("Created session %s at %s\n", report["session_id"], report["session_dir"])
}

func runList(args []string) {
	flags := flag.NewFlagSet("list", flag.ExitOnError)
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	limit := flags.Int("limit", 20, "Maximum sessions to list")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable session list")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	sessions, err := runner.ListSessions(*relayHome, *limit)
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
	}
}

func runShow(args []string) {
	flags := flag.NewFlagSet("show", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to inspect")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable JSON")
	graphOutput := flags.Bool("graph", false, "Show durable relay graph instead of transcript")
	traceNodeID := flags.String("trace", "", "Show raw child trace artifact for a graph node")
	fromRound := flags.Int("from-round", 0, "Show only rounds from N onward")
	roundsSpec := flags.String("rounds", "", "Filter rounds: '5+', '3-7', or '5,6'")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	if *traceNodeID != "" {
		trace, err := inspect.BuildTraceReport(resolvedSessionDir, *traceNodeID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		writeJSON(trace)
		return
	}
	if *graphOutput {
		report, err := inspect.BuildShowGraphReport(resolvedSessionDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		if *jsonOutput {
			writeJSON(report)
			return
		}
		fmt.Println(inspect.FormatGraphSummary(report))
		return
	}
	report, err := inspect.BuildShowTranscriptReport(resolvedSessionDir, *fromRound, *roundsSpec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Println(inspect.FormatTranscriptMarkdown(report))
}

func runExport(args []string) {
	flags := flag.NewFlagSet("export", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to export")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	jsonOutput := flags.Bool("json", false, "Write structured JSON instead of markdown")
	output := ""
	flags.StringVar(&output, "output", "", "Output path")
	flags.StringVar(&output, "o", "", "Alias for --output")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	if strings.TrimSpace(output) == "" {
		fmt.Fprintln(os.Stderr, "error: export requires -o/--output")
		os.Exit(2)
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	report, err := inspect.BuildExportReport(resolvedSessionDir, *jsonOutput)
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

func runHealth(args []string) {
	flags := flag.NewFlagSet("health", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to inspect")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	settingsPath := flags.String("settings", "", "Optional settings.toml path for global config validation")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable health JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	var report map[string]any
	if *sessionDir != "" || *sessionID != "" {
		resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
		var err error
		report, err = inspect.BuildSessionHealthReport(resolvedSessionDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
	} else {
		report = inspect.BuildGlobalHealthReport(*settingsPath)
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Println(inspect.FormatHealthReport(report))
}

func runRelay(args []string) {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	task := flags.String("task", "", "Task text for the relay")
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
	verbose := false
	stream := false
	output := ""
	_ = flags.String("context", "", "Attach context text files; may be repeated. Limits: 1 MiB per file, 2 MiB total")
	_ = flags.String("skill", "", "Attach capability text files; may be repeated")
	_ = flags.String("recipe-file", "", "Attach a session-scoped transient recipe TOML file; may be repeated")
	_ = flags.String("generated-recipe-file", "", "Attach a generated session-scoped transient recipe TOML file; may be repeated")
	flags.BoolVar(&verbose, "verbose", false, "Accepted for Python CLI compatibility")
	flags.BoolVar(&verbose, "v", false, "Accepted for Python CLI compatibility")
	flags.BoolVar(&stream, "stream", false, "Accepted for Python CLI compatibility")
	flags.BoolVar(&stream, "s", false, "Accepted for Python CLI compatibility")
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
	if *task == "" && len(flags.Args()) > 0 {
		*task = strings.Join(flags.Args(), " ")
	}
	agents, usedShorthand, err := runner.ParseAgents(*agentsRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	if *facilitatorBackend == "" {
		if usedShorthand && agents[0] != "relay" {
			*facilitatorBackend = agents[0]
		} else {
			*facilitatorBackend = "codex"
		}
	}
	launchPlan, err := loadLaunchPlanFile(*taskPlanPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	transientRecipeSources := readTransientRecipeSourcesOrExit(extracted["recipe-file"], extracted["generated-recipe-file"])
	effectiveRounds := *rounds
	if *quick {
		effectiveRounds = 3
	}
	_ = verbose
	_ = stream
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := runner.Run(ctx, runner.Options{
		SessionDir:             *sessionDir,
		SessionID:              *sessionID,
		RelayHome:              *relayHome,
		Task:                   *task,
		ContextFiles:           extracted["context"],
		SkillFiles:             extracted["skill"],
		TransientRecipeSources: transientRecipeSources,
		Agents:                 agents,
		SlotConfigs:            []runner.SlotConfig{{Model: *modelA, Effort: *effortA}, {Model: *modelB, Effort: *effortB}},
		Rounds:                 effectiveRounds,
		MaxRounds:              *maxRounds,
		TimeoutSeconds:         *timeout,
		StallTimeoutSeconds:    *stallTimeout,
		Mode:                   *mode,
		DynamicMode:            *dynamicMode,
		SettingsPath:           *settingsPath,
		LaunchCWD:              *launchCWD,
		FacilitatorBackend:     *facilitatorBackend,
		FacilitatorModel:       *facilitatorModel,
		FacilitatorEffort:      *facilitatorEffort,
		LaunchPlan:             launchPlan,
		InvestigationMode:      *investigationMode,
	})
	writeRunnerResult(result, err, *jsonOutput, output)
}

func runProposals(args []string) {
	flags := flag.NewFlagSet("proposals", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable proposal JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	report, err := runner.Proposals(resolvedSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if jsonOutput != nil && *jsonOutput {
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
}

func runApprove(args []string) {
	flags := flag.NewFlagSet("approve", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	proposalID := flags.String("proposal", "", "Proposal id to approve")
	rounds := flags.Int("rounds", 0, "Override admitted child rounds")
	timeout := flags.Int("timeout", 600, "Per-turn timeout in seconds")
	stallTimeout := flags.Int("stall-timeout", 300, "Stall timeout recorded in the child invocation contract")
	settingsPath := flags.String("settings", "", "Optional settings.toml path")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable approval JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	remaining := flags.Args()
	if *sessionID == "" && *sessionDir == "" && len(remaining) > 0 {
		*sessionID = remaining[0]
		remaining = remaining[1:]
	}
	if *proposalID == "" && len(remaining) > 0 {
		*proposalID = remaining[0]
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	if *proposalID == "" {
		fmt.Fprintln(os.Stderr, "error: approve requires a session and proposal id")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := runner.ApproveProposal(ctx, resolvedSessionDir, runner.ApproveOptions{
		ProposalID:          *proposalID,
		Rounds:              *rounds,
		TimeoutSeconds:      *timeout,
		StallTimeoutSeconds: *stallTimeout,
		SettingsPath:        *settingsPath,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Printf("Approved %v and %v child %v into %v.\n", report["proposal_id"], report["status"], report["child_node_id"], report["session_id"])
}

func runReject(args []string) {
	flags := flag.NewFlagSet("reject", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	proposalID := flags.String("proposal", "", "Proposal id to reject")
	reason := flags.String("reason", "rejected by operator", "Rejection reason")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable rejection JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	remaining := flags.Args()
	if *sessionID == "" && *sessionDir == "" && len(remaining) > 0 {
		*sessionID = remaining[0]
		remaining = remaining[1:]
	}
	if *proposalID == "" && len(remaining) > 0 {
		*proposalID = remaining[0]
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	if *proposalID == "" {
		fmt.Fprintln(os.Stderr, "error: reject requires a session and proposal id")
		os.Exit(2)
	}
	report, err := runner.RejectProposal(resolvedSessionDir, runner.RejectOptions{ProposalID: *proposalID, Reason: *reason})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
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
	mode := flags.String("mode", "", "Typed mode control for resumed rounds: adversarial, cooperative, or steelman")
	_ = flags.String("context", "", "Attach resume context text files; may be repeated. Limits: 1 MiB per file, 2 MiB total")
	_ = flags.String("skill", "", "Attach resume capability text files; may be repeated")
	rounds := flags.Int("rounds", 0, "Resume for exactly N additional rounds; omit for auto-stop")
	maxRounds := flags.Int("max-rounds", 50, "Additional-round safety cap when --rounds is omitted")
	timeout := flags.Int("timeout", 600, "Per-turn timeout in seconds")
	stallTimeout := flags.Int("stall-timeout", 300, "Claude JSONL stall timeout in seconds")
	settingsPath := flags.String("settings", "", "Optional settings.toml path")
	facilitatorModel := flags.String("facilitator-model", "", "Facilitator model override")
	facilitatorEffort := flags.String("facilitator-effort", "", "Facilitator effort override")
	quick := flags.Bool("quick", false, "Force exactly 3 additional rounds")
	verbose := false
	output := ""
	flags.BoolVar(&verbose, "verbose", false, "Accepted for Python CLI compatibility")
	flags.BoolVar(&verbose, "v", false, "Accepted for Python CLI compatibility")
	flags.StringVar(&output, "output", "", "Write transcript or JSON export to file")
	flags.StringVar(&output, "o", "", "Alias for --output")
	modelA := flags.String("model-a", "", "Model override for slot_0")
	effortA := flags.String("effort-a", "", "Effort override for slot_0")
	modelB := flags.String("model-b", "", "Model override for slot_1")
	effortB := flags.String("effort-b", "", "Effort override for slot_1")
	replaceA := flags.String("replace-a", "", "Advanced: replace slot_0 backend/profile for resumed turns")
	replaceB := flags.String("replace-b", "", "Advanced: replace slot_1 backend/profile for resumed turns")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable run JSON")
	extracted, cleanedArgs, err := extractMultiValueFlags(args, "context", "skill")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	if err := parseFlags(flags, cleanedArgs); err != nil {
		os.Exit(2)
	}
	remaining := flags.Args()
	if *sessionID == "" && *sessionDir == "" && len(remaining) > 0 {
		*sessionID = remaining[0]
		remaining = remaining[1:]
	}
	if *prompt == "" && len(remaining) > 0 {
		*prompt = strings.Join(remaining, " ")
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	effectiveRounds := *rounds
	if *quick {
		effectiveRounds = 3
	}
	_ = verbose
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := runner.Resume(ctx, resolvedSessionDir, runner.ResumeOptions{
		Prompt:              *prompt,
		Mode:                *mode,
		ContextFiles:        extracted["context"],
		SkillFiles:          extracted["skill"],
		ReplaceAgents:       []string{*replaceA, *replaceB},
		Rounds:              effectiveRounds,
		MaxRounds:           *maxRounds,
		TimeoutSeconds:      *timeout,
		StallTimeoutSeconds: *stallTimeout,
		SlotConfigs:         []runner.SlotConfig{{Model: *modelA, Effort: *effortA}, {Model: *modelB, Effort: *effortB}},
		SettingsPath:        *settingsPath,
		FacilitatorModel:    *facilitatorModel,
		FacilitatorEffort:   *facilitatorEffort,
	})
	writeRunnerResult(result, err, *jsonOutput, output)
}

func runStop(args []string, forceKill bool) {
	flags := flag.NewFlagSet("stop", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory to stop")
	sessionID := flags.String("session-id", "", "Session id under --home when --session-dir is omitted")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	killFlag := flags.Bool("kill", forceKill, "Send SIGKILL and mark killed instead of SIGTERM")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable stop JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	report, err := runner.Stop(resolvedSessionDir, runner.StopOptions{ForceKill: *killFlag})
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

func runSteer(args []string) {
	flags := flag.NewFlagSet("steer", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	prompt := flags.String("prompt", "", "Prompt to inject before the next relay turn")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable steering JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	remaining := flags.Args()
	if *sessionID == "" && *sessionDir == "" && len(remaining) > 0 {
		*sessionID = remaining[0]
		remaining = remaining[1:]
	}
	if *prompt == "" && len(remaining) > 0 {
		*prompt = strings.Join(remaining, " ")
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	item, err := runner.QueueSteeringPrompt(resolvedSessionDir, *prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	report := map[string]any{"session_id": filepath.Base(filepath.Clean(resolvedSessionDir)), "steering": item}
	if *jsonOutput {
		writeJSON(report)
		return
	}
	fmt.Printf("Queued steering for %.8s: %.8s\n", report["session_id"], item["id"])
}

func runDiff(args []string) {
	flags := flag.NewFlagSet("diff", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	text, err := inspect.RenderDiff(resolvedSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	fmt.Print(text)
}

func runClean(args []string) {
	flags := flag.NewFlagSet("clean", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable clean JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	report, err := runner.CleanSession(resolvedSessionDir)
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

func runCleanup(args []string) {
	flags := flag.NewFlagSet("cleanup", flag.ExitOnError)
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	limit := flags.Int("limit", 500, "Maximum sessions to scan")
	force := flags.Bool("force", false, "Also mark sessions without PID files as orphaned")
	jsonOutput := flags.Bool("json", false, "Emit machine-readable cleanup JSON")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	report, err := runner.CleanupSessions(*relayHome, *limit, *force)
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
}

func runDisplay(args []string) {
	flags := flag.NewFlagSet("display", flag.ExitOnError)
	sessionDir := flags.String("session-dir", "", "Session directory")
	sessionID := flags.String("session-id", "", "Session id or prefix under --home")
	relayHome := flags.String("home", "", "Optional relay home; defaults to CODEX_CLAUDE_HOME or ~/.codex-claude")
	htmlOnly := flags.Bool("html-only", false, "Generate HTML only; skip the optional Python PDF helper")
	output := ""
	flags.StringVar(&output, "output", "", "Output path; defaults to <session_dir>/transcript.{html,pdf}")
	flags.StringVar(&output, "o", "", "Alias for --output")
	openOutput := flags.Bool("open", false, "Open generated file after creation")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	if *sessionID == "" && *sessionDir == "" && len(flags.Args()) > 0 {
		*sessionID = flags.Args()[0]
	}
	resolvedSessionDir := resolveSessionDirOrExit(*sessionDir, *sessionID, *relayHome)
	htmlText, err := inspect.BuildDisplayHTML(resolvedSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	htmlPath, pdfPath, openPath := displayOutputPaths(resolvedSessionDir, output, *htmlOnly)
	if *htmlOnly {
		if err := writeDisplayHTML(htmlPath, htmlText); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		fmt.Printf("HTML: %s\n", htmlPath)
	} else {
		helperPath, err := resolveDisplayPDFHelper()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		if err := writeDisplayPDF(htmlText, htmlPath, pdfPath, helperPath); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		}
		fmt.Printf("HTML: %s\n", htmlPath)
		fmt.Printf("PDF:  %s\n", pdfPath)
	}
	if *openOutput {
		openFile(openPath)
	}
}

const displayPDFHelperEnv = "CONVO_RELAY_PDF_HELPER"
const installedShareDir = "convo-relay"

func displayOutputPaths(sessionDir string, output string, htmlOnly bool) (string, string, string) {
	if htmlOnly {
		htmlPath := output
		if htmlPath == "" {
			htmlPath = filepath.Join(sessionDir, "transcript.html")
		}
		return htmlPath, "", htmlPath
	}
	pdfPath := output
	if pdfPath == "" {
		pdfPath = filepath.Join(sessionDir, "transcript.pdf")
	}
	htmlPath := filepath.Join(sessionDir, "transcript.html")
	return htmlPath, pdfPath, pdfPath
}

func writeDisplayHTML(outPath string, htmlText string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte(htmlText), 0o644)
}

func writeDisplayPDF(htmlText string, htmlPath string, pdfPath string, helperPath string) error {
	tempFile, err := os.CreateTemp("", "convo-relay-display-*.html")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.WriteString(htmlText); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pdfPath), 0o755); err != nil {
		return err
	}
	if err := renderDisplayPDFWithHelper(helperPath, tempPath, pdfPath); err != nil {
		_ = os.Remove(pdfPath)
		return err
	}
	return writeDisplayHTML(htmlPath, htmlText)
}

func resolveDisplayPDFHelper() (string, error) {
	if helperPath := strings.TrimSpace(os.Getenv(displayPDFHelperEnv)); helperPath != "" {
		if info, err := os.Stat(helperPath); err == nil && !info.IsDir() {
			return helperPath, nil
		}
		return "", fmt.Errorf("PDF export helper from %s is not available: %s", displayPDFHelperEnv, helperPath)
	}
	return resolveDisplayPDFHelperFromCandidates(displayPDFHelperCandidates())
}

func resolveDisplayPDFHelperFromCandidates(candidates []string) (string, error) {
	for _, helperPath := range candidates {
		if info, err := os.Stat(helperPath); err == nil && !info.IsDir() {
			return helperPath, nil
		}
	}
	return "", fmt.Errorf("PDF export requires the optional helper scripts/render_display_pdf.py and Python Playwright/Chromium; set %s or use --html-only", displayPDFHelperEnv)
}

func displayPDFHelperCandidates() []string {
	candidates := []string{filepath.Join("scripts", "render_display_pdf.py")}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, displayPDFHelperCandidatesForExecutable(executable)...)
	}
	return candidates
}

func displayPDFHelperCandidatesForExecutable(executable string) []string {
	execDir := filepath.Dir(executable)
	return []string{
		filepath.Join(execDir, "scripts", "render_display_pdf.py"),
		filepath.Join(execDir, "..", "scripts", "render_display_pdf.py"),
		filepath.Join(execDir, "..", "share", installedShareDir, "scripts", "render_display_pdf.py"),
	}
}

func renderDisplayPDFWithHelper(helperPath string, htmlPath string, pdfPath string) error {
	command := []string{helperPath, htmlPath, pdfPath}
	if strings.HasSuffix(helperPath, ".py") {
		command = append([]string{"python3"}, command...)
	}
	cmd := exec.Command(command[0], command[1:]...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("PDF helper failed: %s", detail)
	}
	return nil
}

func buildTaskWithContext(task string, contextFiles []string, skillFiles []string) (string, string, error) {
	contexts, err := runner.PreflightLaunchContexts(contextFiles)
	if err != nil {
		return "", "", err
	}
	taskWithContext := runner.BuildTaskWithLaunchContext(task, contexts)
	skillsText, err := buildSkillsText(skillFiles)
	if err != nil {
		return "", "", err
	}
	return taskWithContext, skillsText, nil
}

func buildSkillsText(skillFiles []string) (string, error) {
	var skillsBuilder strings.Builder
	for _, rawPath := range skillFiles {
		block, err := promptFileBlock(rawPath)
		if err != nil {
			return "", fmt.Errorf("unable to read skill file %q: %w", rawPath, err)
		}
		skillsBuilder.WriteString(block)
	}
	return skillsBuilder.String(), nil
}

func promptFileBlock(rawPath string) (string, error) {
	absPath, err := filepath.Abs(rawPath)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("\n### %s\n````text\n%s\n````\n", filepath.Base(absPath), string(data)), nil
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

func resolveSessionDirOrExit(sessionDir string, sessionID string, relayHome string) string {
	resolved, err := runner.ResolveSessionDir(relayHome, sessionDir, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	return resolved
}

func writeRunnerResult(result map[string]any, err error, jsonOutput bool, outputPath string) {
	if err != nil {
		if result != nil && jsonOutput {
			writeJSON(result)
		}
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
	savedOutput := ""
	if strings.TrimSpace(outputPath) != "" {
		var saveErr error
		savedOutput, saveErr = saveRunnerOutput(result, outputPath, jsonOutput)
		if saveErr != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", saveErr)
			os.Exit(1)
		}
	}
	if jsonOutput {
		writeJSON(result)
		return
	}
	fmt.Printf("Session %s completed at %s\n", result["session_id"], result["session_dir"])
	fmt.Printf("Rounds: %v/%v\n", result["actual_rounds"], result["max_rounds"])
	if savedOutput != "" {
		fmt.Printf("Output: %s\n", savedOutput)
	}
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
		report, err := inspect.BuildShowTranscriptReport(sessionDir, 0, "")
		if err != nil {
			return "", err
		}
		body = []byte(inspect.FormatTranscriptMarkdown(report))
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
		body = []byte(inspect.FormatExportMarkdown(report))
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
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  convo-relay install-skills --target all")
	fmt.Fprintln(os.Stderr, "  convo-relay list --home <relay-home> --json")
	fmt.Fprintln(os.Stderr, "  convo-relay show <session-id-prefix> --json")
	fmt.Fprintln(os.Stderr, "  convo-relay export <session-id-prefix> -o transcript.md")
	fmt.Fprintln(os.Stderr, "  convo-relay health [session-id-prefix] --json")
	fmt.Fprintln(os.Stderr, "  convo-relay recipes list --json")
	fmt.Fprintln(os.Stderr, "  convo-relay recipes show review-panel")
	fmt.Fprintln(os.Stderr, "  convo-relay recipes doctor")
	fmt.Fprintln(os.Stderr, "  convo-relay show <session-id-prefix> --graph --json")
	fmt.Fprintln(os.Stderr, "  convo-relay show <session-id-prefix> --trace <node-id>")
	fmt.Fprintln(os.Stderr, "  convo-relay contracts <session-id-prefix> --json")
	fmt.Fprintln(os.Stderr, "  convo-relay diff <session-id-prefix>")
	fmt.Fprintln(os.Stderr, "  convo-relay steer <session-id-prefix> <prompt>")
	fmt.Fprintln(os.Stderr, "  convo-relay display <session-id-prefix> --html-only -o transcript.html")
	fmt.Fprintln(os.Stderr, "  convo-relay display <session-id-prefix> -o transcript.pdf")
	fmt.Fprintln(os.Stderr, "  convo-relay clean <session-id-prefix>")
	fmt.Fprintln(os.Stderr, "  convo-relay cleanup --home <relay-home>")
	fmt.Fprintln(os.Stderr, "  convo-relay show-graph <session-id-prefix> --json")
	fmt.Fprintln(os.Stderr, "  convo-relay compile-recipe --recipe <id> --json")
	fmt.Fprintln(os.Stderr, "  convo-relay create-session --session-dir <path> --json")
	fmt.Fprintln(os.Stderr, "  convo-relay run --task <task> --agents codex,codex --rounds 2 --json")
	fmt.Fprintln(os.Stderr, "  convo-relay resume <session-id-prefix> --mode steelman --rounds 1 --json")
	fmt.Fprintln(os.Stderr, "  convo-relay proposals <session-id-prefix> --json")
	fmt.Fprintln(os.Stderr, "  convo-relay approve <session-id-prefix> <proposal-id> --json")
	fmt.Fprintln(os.Stderr, "  convo-relay stop <session-id-prefix>")
	fmt.Fprintln(os.Stderr, "  convo-relay version")
}

var cliVersion = "1.0.0"

type installSkillSpec struct {
	source string
	dest   string
	kind   string
	mode   os.FileMode
}

type installSkillFileRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
}

type installSkillTargetResult struct {
	Files []installSkillFileRecord `json:"files"`
}

type installSkillsResult struct {
	Schema    int                                 `json:"schema"`
	Name      string                              `json:"name"`
	Version   string                              `json:"version"`
	Operation string                              `json:"operation"`
	Kind      string                              `json:"kind"`
	Targets   map[string]installSkillTargetResult `json:"targets"`
	Warnings  []string                            `json:"warnings"`
}

func runInstallSkills(args []string) {
	flags := flag.NewFlagSet("install-skills", flag.ExitOnError)
	target := flags.String("target", "all", "Target skill host: claude, codex, tools, or all")
	plan := flags.Bool("plan", false, "Print intended files without writing")
	install := flags.Bool("install", false, "Install skill files")
	uninstall := flags.Bool("uninstall", false, "Remove skill files")
	jsonOutput := flags.Bool("json", false, "Emit delegated-installer JSON")
	installRoot := flags.String("install-root", "", "Stage install under this absolute directory as if it were HOME")
	if err := parseFlags(flags, args); err != nil {
		os.Exit(2)
	}
	selectedOps := 0
	for _, selected := range []bool{*plan, *install, *uninstall} {
		if selected {
			selectedOps++
		}
	}
	if selectedOps > 1 {
		fmt.Fprintln(os.Stderr, "error: --plan, --install, and --uninstall are mutually exclusive")
		os.Exit(2)
	}
	operation := "install"
	if *plan {
		operation = "plan"
	} else if *uninstall {
		operation = "uninstall"
	}
	perform := operation != "plan"
	result, err := delegatedInstallSkillsResult(operation, *target, *installRoot, perform)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		writeJSON(result)
		return
	}
	if operation != "install" {
		printInstallSkillsPlan(operation, result)
		return
	}
	for _, targetName := range sortedInstallResultTargets(result.Targets) {
		switch targetName {
		case "claude":
			fmt.Println("Installed Claude Code skills: /relay, /relay:steer")
		case "codex":
			fmt.Println("Installed Codex skills: $relay, $relay:steer")
		case "tools":
			fmt.Println("Tools target is managed by delegated installers")
		}
	}
}

func delegatedInstallSkillsResult(operation string, target string, installRoot string, perform bool) (installSkillsResult, error) {
	specs, err := installSkillTargetSpecs(target, installRoot)
	if err != nil {
		return installSkillsResult{}, err
	}
	result := installSkillsResult{
		Schema:    1,
		Name:      "convo-relay",
		Version:   cliVersion,
		Operation: operation,
		Kind:      "delegated",
		Targets:   map[string]installSkillTargetResult{},
		Warnings:  []string{},
	}
	for _, targetName := range sortedSpecTargets(specs) {
		records := []installSkillFileRecord{}
		for _, spec := range specs[targetName] {
			if operation == "install" && perform {
				if err := installSkillSpecFile(spec); err != nil {
					return installSkillsResult{}, err
				}
			} else if operation == "uninstall" && perform {
				if err := os.Remove(spec.dest); err != nil && !os.IsNotExist(err) {
					return installSkillsResult{}, err
				}
			}
			absDest, err := filepath.Abs(spec.dest)
			if err != nil {
				return installSkillsResult{}, err
			}
			record := installSkillFileRecord{Path: absDest}
			if operation == "install" {
				if digest, ok := sha256File(absDest); ok {
					record.SHA256 = digest
				}
			}
			records = append(records, record)
		}
		result.Targets[targetName] = installSkillTargetResult{Files: records}
	}
	return result, nil
}

func printInstallSkillsPlan(operation string, result installSkillsResult) {
	for _, targetName := range sortedInstallResultTargets(result.Targets) {
		fmt.Printf("%s %s:\n", operation, targetName)
		for _, file := range result.Targets[targetName].Files {
			fmt.Printf("  %s\n", file.Path)
		}
	}
}

func installSkillTargetSpecs(target string, installRoot string) (map[string][]installSkillSpec, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		target = "all"
	}
	home, err := installHome(installRoot)
	if err != nil {
		return nil, err
	}
	toolSpecs, err := installToolTargetSpecs(home)
	if err != nil {
		return nil, err
	}
	if target == "tools" {
		return map[string][]installSkillSpec{"tools": toolSpecs}, nil
	}
	skillDir, err := findSkillBundleDir()
	if err != nil {
		return nil, err
	}
	codexHome := filepath.Join(home, ".codex")
	if strings.TrimSpace(installRoot) == "" {
		if envCodexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); envCodexHome != "" {
			codexHome = envCodexHome
		}
	}
	specs := map[string][]installSkillSpec{
		"tools": toolSpecs,
		"claude": {
			{source: filepath.Join(skillDir, "SKILL.md"), dest: filepath.Join(home, ".claude", "skills", "relay", "SKILL.md")},
			{source: filepath.Join(skillDir, "steer", "SKILL.md"), dest: filepath.Join(home, ".claude", "skills", "relay:steer", "SKILL.md")},
		},
		"codex": {
			{source: filepath.Join(skillDir, "codex", "SKILL.md"), dest: filepath.Join(codexHome, "skills", "relay", "SKILL.md")},
			{source: filepath.Join(skillDir, "codex", "steer", "SKILL.md"), dest: filepath.Join(codexHome, "skills", "relay:steer", "SKILL.md")},
		},
	}
	if target == "all" {
		return specs, nil
	}
	if selected, ok := specs[target]; ok {
		if target == "codex" || target == "claude" {
			return map[string][]installSkillSpec{"tools": toolSpecs, target: selected}, nil
		}
		return map[string][]installSkillSpec{target: selected}, nil
	}
	return nil, fmt.Errorf("--target must be claude, codex, tools, or all")
}

func installHome(installRoot string) (string, error) {
	if strings.TrimSpace(installRoot) != "" {
		return filepath.Abs(installRoot)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return home, nil
}

const installSkillKindToolBinary = "tool-binary"

func installToolTargetSpecs(home string) ([]installSkillSpec, error) {
	specs := []installSkillSpec{{
		dest: filepath.Join(home, ".local", "bin", "convo-relay"),
		kind: installSkillKindToolBinary,
		mode: 0o755,
	}}
	shareRoot := filepath.Join(home, ".local", "share", installedShareDir)
	skillDir, err := findSkillBundleDir()
	if err != nil {
		return nil, err
	}
	if err := filepath.WalkDir(skillDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(skillDir, path)
		if err != nil {
			return err
		}
		specs = append(specs, installSkillSpec{
			source: path,
			dest:   filepath.Join(shareRoot, "skill", rel),
			mode:   0o644,
		})
		return nil
	}); err != nil {
		return nil, err
	}
	helper, err := sourcePDFHelperPath()
	if err != nil {
		return nil, err
	}
	specs = append(specs, installSkillSpec{
		source: helper,
		dest:   filepath.Join(shareRoot, "scripts", "render_display_pdf.py"),
		mode:   0o755,
	})
	return specs, nil
}

const skillBundleDirEnv = "CONVO_RELAY_SKILL_DIR"

func findSkillBundleDir() (string, error) {
	for _, candidate := range skillBundleCandidates() {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return filepath.Abs(candidate)
		}
	}
	return "", fmt.Errorf("bundled skill directory not found; set %s", skillBundleDirEnv)
}

func skillBundleCandidates() []string {
	candidates := []string{}
	if envSkillDir := strings.TrimSpace(os.Getenv(skillBundleDirEnv)); envSkillDir != "" {
		candidates = append(candidates, envSkillDir)
	}
	candidates = append(candidates, "skill")
	if execPath, err := os.Executable(); err == nil {
		candidates = append(candidates, skillBundleCandidatesForExecutable(execPath)...)
	}
	if _, sourceFile, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(sourceFile), "..", "..", "skill"))
	}
	return candidates
}

func skillBundleCandidatesForExecutable(executable string) []string {
	execDir := filepath.Dir(executable)
	return []string{
		filepath.Join(execDir, "skill"),
		filepath.Join(execDir, "..", "skill"),
		filepath.Join(execDir, "..", "share", installedShareDir, "skill"),
	}
}

func sourcePDFHelperPath() (string, error) {
	if root, ok := sourceCheckoutRoot(); ok {
		candidate := filepath.Join(root, "scripts", "render_display_pdf.py")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	for _, candidate := range displayPDFHelperCandidates() {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("bundled PDF helper not found; set %s", displayPDFHelperEnv)
}

func installSkillSpecFile(spec installSkillSpec) error {
	if spec.kind == installSkillKindToolBinary {
		return installToolBinary(spec.dest)
	}
	return copyInstallFile(spec.source, spec.dest, spec.mode)
}

func installToolBinary(dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if root, ok := sourceCheckoutRoot(); ok {
		command := exec.Command("go", "build", "-ldflags", "-X main.cliVersion="+cliVersion, "-o", dest, "./cmd/convo-relay")
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("build convo-relay tool: %w\n%s", err, strings.TrimSpace(string(output)))
		}
		return os.Chmod(dest, 0o755)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if samePath(executable, dest) {
		return os.Chmod(dest, 0o755)
	}
	return copyInstallFile(executable, dest, 0o755)
}

func sourceCheckoutRoot() (string, bool) {
	if _, sourceFile, _, ok := runtime.Caller(0); ok {
		root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
		if info, err := os.Stat(filepath.Join(root, "go.mod")); err == nil && !info.IsDir() {
			if cmdInfo, err := os.Stat(filepath.Join(root, "cmd", "convo-relay")); err == nil && cmdInfo.IsDir() {
				return root, true
			}
		}
	}
	return "", false
}

func samePath(a string, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}

func copyInstallFile(source string, dest string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if mode != 0 {
		return os.Chmod(dest, mode)
	}
	return nil
}

func sha256File(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", false
	}
	return hex.EncodeToString(hash.Sum(nil)), true
}

func sortedSpecTargets(specs map[string][]installSkillSpec) []string {
	targets := make([]string, 0, len(specs))
	for target := range specs {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
}

func sortedInstallResultTargets(targets map[string]installSkillTargetResult) []string {
	names := make([]string, 0, len(targets))
	for target := range targets {
		names = append(names, target)
	}
	sort.Strings(names)
	return names
}

func createCompatibilitySession(sessionDir string, task string, settingsPath string, recipeID string, withChildContracts bool) (map[string]any, error) {
	st := store.New(sessionDir)
	if err := st.EnsureSession(); err != nil {
		return nil, err
	}
	sessionID := filepath.Base(filepath.Clean(sessionDir))
	timestamp := "2026-05-19T00:00:00+00:00"
	if err := st.SaveMetaMap(map[string]any{
		"session_id":    sessionID,
		"task":          task,
		"title":         task,
		"status":        "completed",
		"mode":          "go-shadow",
		"dynamic_mode":  "off",
		"actual_rounds": 0,
		"max_rounds":    0,
		"created_at":    timestamp,
		"updated_at":    timestamp,
		"slots":         []any{},
	}); err != nil {
		return nil, err
	}
	if err := st.SaveTranscriptItems([]any{}); err != nil {
		return nil, err
	}
	if _, err := st.AppendSessionEventV1(
		"node_started",
		graph.RootNodeID,
		"Root relay node started",
		map[string]any{"session_ref": sessionID, "dynamic_mode": "off"},
		store.EventOptions{EventID: "evt_go_root_started", Timestamp: timestamp},
	); err != nil {
		return nil, err
	}

	refs := map[string]any{}
	if withChildContracts {
		childRefs, err := writeSampleChildContracts(st, task, settingsPath, recipeID)
		if err != nil {
			return nil, err
		}
		refs = childRefs
		traceRef, err := st.SaveArtifact("child_traces", "go-sample-child", map[string]any{
			"kind":                   "child_trace",
			"schema_version":         1,
			"child_node_id":          "relay_backend_child_go_sample",
			"child_session_id":       sessionID + "-child",
			"composition_path":       "root.slot_0",
			"portable_contract_refs": refs,
		})
		if err != nil {
			return nil, err
		}
		if _, err := st.AppendSessionEventV1(
			"relay_backend_child_completed",
			"relay_backend_child_go_sample",
			"Relay backend child completed",
			map[string]any{
				"parent_node_id":   graph.RootNodeID,
				"slot_id":          "slot_0",
				"composition_path": "root.slot_0",
				"recipe_id":        recipeID,
				"child_session_id": sessionID + "-child",
				"trace_ref":        traceRef,
				"contract_refs":    refs,
			},
			store.EventOptions{EventID: "evt_go_relay_backend_child_completed", Timestamp: timestamp},
		); err != nil {
			return nil, err
		}
	}
	if _, err := st.AppendSessionEventV1(
		"node_completed",
		graph.RootNodeID,
		"Root relay node completed",
		map[string]any{},
		store.EventOptions{EventID: "evt_go_root_completed", Timestamp: timestamp},
	); err != nil {
		return nil, err
	}
	repairedGraph, events, err := graph.RepairAndSaveFromEvents(st)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"session_id":    sessionID,
		"session_dir":   sessionDir,
		"event_count":   len(events),
		"graph":         repairedGraph,
		"contract_refs": refs,
	}, nil
}

func writeSampleChildContracts(st *store.Store, task string, settingsPath string, recipeID string) (map[string]any, error) {
	config, err := recipes.LoadRuntimeConfig(settingsPath)
	if err != nil {
		return nil, err
	}
	compileReport, err := recipes.BuildCompileReport(recipeID, config, recipes.CompileOptions{
		CompositionPath:    "root.slot_0",
		ValidateExecutable: false,
	})
	if err != nil {
		return nil, err
	}
	recipePayload := compileReport["recipe"].(map[string]any)
	compiledPlan := compileReport["compiled_plan"].(map[string]any)

	recipeRefPayload, _ := compiledPlan["recipe_ref"].(map[string]any)
	recipeRef, err := st.SaveContractArtifact("recipes", recipeID, recipePayload, stringValue(recipeRefPayload["id"]))
	if err != nil {
		return nil, err
	}
	compiledPlanRef, err := saveContractWithDigestRef(st, "compiled_plans", "go-sample-plan", "compiled_plan", compiledPlan)
	if err != nil {
		return nil, err
	}
	childInvocation := map[string]any{
		"kind":                  "child_invocation",
		"schema_version":        1,
		"compiled_plan_ref":     compiledPlanRef,
		"task":                  task,
		"origin_kind":           "relay-backend",
		"admitted_rounds":       1,
		"depth_policy":          map[string]any{"graph_depth": 0, "max_graph_depth": 1, "relay_backend_depth": 0, "max_relay_backend_depth": 1},
		"timeout_seconds":       600,
		"stall_timeout_seconds": 300,
	}
	childInvocationRef, err := saveContractWithDigestRef(st, "child_invocations", "go-sample-child", "child_invocation", childInvocation)
	if err != nil {
		return nil, err
	}
	childResult := map[string]any{
		"kind":                 "child_result",
		"schema_version":       1,
		"child_invocation_ref": childInvocationRef,
		"child_session_id":     st.SessionID() + "-child",
		"status":               "completed",
		"stop_reason":          "fixed_rounds",
		"actual_rounds":        1,
		"elapsed_seconds":      0,
		"error":                nil,
		"ledger":               map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}},
		"transcript": []any{
			map[string]any{
				"kind":           "transcript_entry",
				"schema_version": 1,
				"round":          1,
				"slot_id":        "slot_0",
				"speaker":        "Go Shadow",
				"content":        "Compatibility child result.",
				"ledger_after":   map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}},
			},
		},
		"last_content": "Compatibility child result.",
	}
	childResultRef, err := saveContractWithDigestRef(st, "child_results", "go-sample-child", "child_result", childResult)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"recipe_ref":           recipeRef,
		"compiled_plan_ref":    compiledPlanRef,
		"child_invocation_ref": childInvocationRef,
		"child_result_ref":     childResultRef,
	}, nil
}

func saveContractWithDigestRef(st *store.Store, category string, artifactID string, refPrefix string, payload map[string]any) (map[string]any, error) {
	digest, err := contracts.ContractDigest(payload)
	if err != nil {
		return nil, err
	}
	refID := refPrefix + ":" + strings.TrimPrefix(digest, contracts.DigestPrefix)[:16]
	return st.SaveContractArtifact(category, artifactID, payload, refID)
}

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

func openFile(path string) {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
		args = []string{path}
	case "windows":
		command = "cmd"
		args = []string{"/c", "start", "", path}
	default:
		command = "xdg-open"
		args = []string{path}
	}
	if err := exec.Command(command, args...).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open %s: %s\n", path, err)
	}
}
