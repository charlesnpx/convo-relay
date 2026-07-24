package acceptance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type coverageCategory struct {
	name  string
	tests []string
}

func TestGenericAcceptanceCoverageIsComplete(t *testing.T) {
	requireAcceptanceCoverage(t, []coverageCategory{
		{name: "ordinary agent shorthand", tests: []string{"TestRunCompatibilityFlagsBuildContextSkillsPlanAndQuickMode", "TestGoOnlySmokeMatrix"}},
		{name: "direct neutral root execution", tests: []string{"TestRunRecipeUsesExplicitRootTargetAndPersistsDirectContractlessSession"}},
		{name: "contractless ordinary behavior", tests: []string{"TestRunRecipeContractlessPromptsUseOrdinaryContextWithoutStructuredOutput"}},
		{name: "catalog integration requirement", tests: []string{"TestRecipeCatalogIntegrationBindingAndDoctorSemantics"}},
		{name: "bundle-bound plan digest", tests: []string{"TestIntegrationBoundRecipeCompilesOnlyForRootWithMatchingBundle"}},
		{name: "contract preflight before provider launch", tests: []string{"TestRunRecipeContractPoliciesRejectBeforeSessionCreation", "TestRunRecipePurePreflightFailuresLeaveNoSession"}},
		{name: "named input constraints", tests: []string{"TestPrepareEnforcesEveryCardinality", "TestPrepareRejectsNonregularMissingAndOversizedFilesAtBoundaries", "TestPrepareAppliesStrictMediaEncodingAndPerItemJSONSchemas"}},
		{name: "bounded named input resources", tests: []string{"TestPrepareUsesMinimumContractAndRuntimePerFileLimits", "TestPrepareEnforcesAggregateRawByteBoundaryBeforeBase64Persistence", "TestReadRegularFileRejectsAggregateAccountingOverflow", "TestPrepareObservesContextCancellation"}},
		{name: "exact retained input verifier", tests: []string{"TestVerifyRetainedRejectsEveryExactLayoutMismatchWithoutContents", "TestVerifyRetainedObservesCancellationWhileHashing", "TestVerifyRetainedReportsAnExtraEntryThatSortsBeforeExpectedFiles"}},
		{name: "retry-aware retained input boundaries", tests: []string{"TestRootRetainedInputIntegrityRejectsSuccessfulProviderMutations", "TestRootRetainedInputIntegrityWinsOverEveryProviderOutcomeAndPreventsRetry", "TestRootRetainedInputIntegrityFailsClosedWhenPostVerifierErrorsOrExpires", "TestRootRetainedInputIntegrityRunsImmediatelyBeforeResultValidation"}},
		{name: "retained input terminal operations", tests: []string{"TestRootNamedInputIntegrityTerminalRejectsResumeBeforeConstructionOrWrites", "TestRootRecoveryPreservesPresentRetainedInputMismatchBeforeAnyWrite", "TestLegacyAbsentRetainedInputsMaterializeOnceAndThenVerifyAsPresent", "TestRootRecoveryAcceptsStoryTwoRetainedRefShapeAndRejectsConflicts", "TestRootIntegrityInspectionSurfacesShareContentFreeTerminalProjection"}},
		{name: "missing and undeclared inputs", tests: []string{"TestPrepareRejectsMissingContractUndeclaredNamesAndPositionalContext"}},
		{name: "exact participant schedule", tests: []string{"TestRunRecipeExecutesExactAlternatingParticipantsAndFacilitator"}},
		{name: "turn and slot instruction scoping", tests: []string{"TestRunRecipeScopesContractInstructionsAndProviderInputsPerTurn"}},
		{name: "fresh reducer context", tests: []string{"TestRunRecipeContractlessReducerUsesFreshContextAndRawProse"}},
		{name: "reducer excluded from participant count", tests: []string{"TestRunRecipeReducerFailuresAreTerminalWithoutChangingParticipantCount"}},
		{name: "canonical structured result", tests: []string{"TestRunRecipeStructuredResultsValidateAndCanonicalize"}},
		{name: "visible invalid result classifications", tests: []string{"TestRunRecipeStructuredValidationFailuresPersistRawDiagnostics"}},
		{name: "generic cross-document assertions", tests: []string{"TestUniqueUsesSemanticJSONEquality", "TestSetEqualIgnoresOrderingAndDuplicates", "TestValueEqualRequiresOneSemanticMatchPerSide", "TestFieldEqualByKeyComparesOnlyMatchingUniqueScalarKeys"}},
		{name: "lifecycle policy enforcement", tests: []string{"TestRootLifecycleForbidRejectsEveryMutationBeforeSessionWrites"}},
		{name: "required workspace isolation", tests: []string{"TestMaterializeRequiredPoliciesCreateVerifiedDetachedWorktreeAndArtifact", "TestPreflightRequiredIsolationRejectsUnavailableGitStatesAndPathsWithoutMutation"}},
		{name: "raw committed workspace semantics", tests: []string{"TestRawExportBypassesHooksFiltersReplacementSparseAndCheckoutConversion", "TestRawExportMaterializesGitlinksAsCountedEmptyDirectories"}},
		{name: "dirty source contract", tests: []string{"TestDirtyIsolatedLaunchRequiresExplicitCommittedHeadOverride", "TestDirtySourceOverrideRejectsInheritedAndNonGitExecution", "TestRootNamedInputIntegrityAndSourceMutationPreserveOrderedCauses", "TestRootNamedInputIntegritySurvivesNonSourceWorkspaceFinalizationFailure"}},
		{name: "trusted provider documentation", tests: []string{"TestRootRecipeDocumentationStatesTrustedProviderBoundaries"}},
		{name: "digest-checked session contracts", tests: []string{"TestEveryRootArtifactKindRoundTripsWithSafeRefAndRejectsTampering", "TestRootInspectionProjectionIsSharedDigestCheckedAndPayloadRedacted"}},
		{name: "persisted recovery snapshots", tests: []string{"TestRootRecoveryUsesPersistedBundleContractAndNamedInputSnapshots"}},
		{name: "readiness separated from recipe structure", tests: []string{"TestRecipeCatalogClassificationPrecedenceAndFilters", "TestDefaultReadinessRunsOnlyVersionProbes"}},
		{name: "generic operation without optional defaults", tests: []string{"TestGenericRecipeExecutionWorksWithoutIntegrationBoundDefaults"}},
		{name: "data-driven default registry validation", tests: []string{"TestEveryDefaultRecipeRecordPassesGenericRegistryChecks"}},
	})
}

func TestUnifiedCompilerAcceptanceCoverageIsComplete(t *testing.T) {
	requireAcceptanceCoverage(t, []coverageCategory{
		{name: "contractless root target", tests: []string{"TestContractlessRecipeCompilesForRootAndChildTargets"}},
		{name: "contractless child target", tests: []string{"TestContractlessRecipeCompilesForRootAndChildTargets"}},
		{name: "integration-bound root target", tests: []string{"TestIntegrationBoundRecipeCompilesOnlyForRootWithMatchingBundle"}},
		{name: "integration-bound child rejection", tests: []string{"TestIntegrationBoundRecipeCompilesOnlyForRootWithMatchingBundle"}},
		{name: "missing and unknown API targets", tests: []string{"TestCompileRecipeRequiresExplicitKnownTarget"}},
		{name: "omitted CLI target compatibility", tests: []string{"TestRecipeCatalogAndCompileTargetCLIContracts"}},
		{name: "explicit CLI root target", tests: []string{"TestRecipeCatalogAndCompileTargetCLIContracts"}},
		{name: "explicit CLI child target", tests: []string{"TestRecipeCatalogAndCompileTargetCLIContracts"}},
		{name: "root run routing without target flag", tests: []string{"TestRunRecipeCLIDispatchesDirectRootExecution", "TestRunRecipeUsesExplicitRootTargetAndPersistsDirectContractlessSession"}},
		{name: "nested and dynamic child routing", tests: []string{"TestNestedAndDynamicChildRoutesRejectIntegrationBoundRecipes", "TestEveryRunnerRecipeCompilerCallSelectsChildTargetExplicitly"}},
		{name: "target-specific digest fields", tests: []string{"TestRootAndChildDigestsRetainTargetSpecificFields"}},
		{name: "child payload compatibility", tests: []string{"TestExistingChildPlanPayloadAndDigestsRemainCompatible"}},
		{name: "ordinary runner and agent compatibility", tests: []string{"TestRunCompatibilityFlagsBuildContextSkillsPlanAndQuickMode", "TestGoOnlySmokeMatrix"}},
		{name: "single exported canonical compiler", tests: []string{"TestCompileRecipeIsOnlyExportedCanonicalCompiler"}},
	})
}

func TestRootRecipeDocumentationStatesTrustedProviderBoundaries(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate acceptance test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	documents := []string{
		"README.md",
		filepath.Join("docs", "root-recipe-integration.md"),
		filepath.Join("docs", "provider-lifecycle-contract.md"),
	}
	var combined strings.Builder
	for _, relative := range documents {
		data, err := os.ReadFile(filepath.Join(repoRoot, relative))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		combined.Write(data)
		combined.WriteByte('\n')
	}
	text := strings.Join(strings.Fields(strings.ToLower(combined.String())), " ")
	for _, required := range []string{
		"trusted same-user process",
		"not a security boundary",
		"writable, session-managed detached worktrees",
		"does not create a read-only filesystem",
		"can still mutate a copy between checks",
		"inventories only the source set",
		"failed or interrupted root sessions retain their managed worktree",
		"raw git objects",
		"git lfs smudging",
		"working-tree-encoding",
		"end-of-line conversion",
		"export attributes",
		"gitlinks are materialized as counted empty directories",
		"not fetched or recursively materialized",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("root recipe documentation is missing %q", required)
		}
	}
	if strings.Contains(text, "named immutable inputs") {
		t.Error("root recipe documentation still describes named inputs as immutable")
	}
}

func requireAcceptanceCoverage(t *testing.T, categories []coverageCategory) {
	t.Helper()
	available := repositoryTestFunctions(t)
	for index, category := range categories {
		if strings.TrimSpace(category.name) == "" || len(category.tests) == 0 {
			t.Fatalf("acceptance category %d has incomplete metadata: %#v", index+1, category)
		}
		for _, testName := range category.tests {
			if !available[testName] {
				t.Errorf("acceptance category %d (%s) references missing test %s", index+1, category.name, testName)
			}
		}
	}
}

func repositoryTestFunctions(t *testing.T) map[string]bool {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate acceptance test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	result := map[string]bool{}
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != repoRoot && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") {
				result[function.Name.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover repository tests: %v", err)
	}
	return result
}
