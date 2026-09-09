package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/relayv2"
	"github.com/charlesnpx/convo-relay/internal/session"
)

func TestSuppliedPlanRunsAndPersistsItsFields(t *testing.T) {
	installSuppliedCodex(t, fakeCodexAppServerScript)
	planValue := suppliedPlanFixture("chosen-session", nil)
	planPath := writeSuppliedPlan(t, planValue)
	sessionDir := filepath.Join(t.TempDir(), "session")

	if _, err := v2RunSuppliedPlan(context.Background(), v2SuppliedPlanRunOptions{
		SessionDir: sessionDir,
		PlanPath:   planPath,
		LaunchCWD:  t.TempDir(),
	}); err != nil {
		t.Fatalf("run supplied plan: %v", err)
	}
	got, err := session.Open(sessionDir)
	if err != nil {
		t.Fatalf("open supplied session: %v", err)
	}
	if !reflect.DeepEqual(got.Plan, planValue) {
		t.Fatalf("stored plan = %#v, want %#v", got.Plan, planValue)
	}
	report, err := relayv2.BuildReport(got, relayv2.ProjectionOptions{})
	if err != nil {
		t.Fatalf("build supplied report: %v", err)
	}
	if report["execution_kind"] != "supplied" {
		t.Fatalf("supplied execution_kind = %v, want supplied", report["execution_kind"])
	}
}

func TestSuppliedPlanWithBlobsExportsPortableBundle(t *testing.T) {
	root := t.TempDir()
	installSuppliedCodex(t, fakeCodexAppServerScript)
	input := []byte("portable supplied input")
	ref := blobstore.RefForBytes(input, "text/plain")
	sourceDir := filepath.Join(root, "source")
	writeSuppliedBlob(t, sourceDir, ref, input)
	planValue := suppliedPlanFixture("portable-supplied-session", []session.Input{{
		Name:     "brief",
		Contents: []blobstore.BlobRef{ref},
	}})
	sessionDir := filepath.Join(root, "session")
	launchCWD := filepath.Join(root, "launch")
	if err := os.MkdirAll(launchCWD, 0o700); err != nil {
		t.Fatalf("create supplied portable launch CWD: %v", err)
	}
	if _, err := v2RunSuppliedPlan(context.Background(), v2SuppliedPlanRunOptions{
		SessionDir: sessionDir,
		PlanPath:   writeSuppliedPlan(t, planValue),
		BlobsPath:  sourceDir,
		LaunchCWD:  launchCWD,
	}); err != nil {
		t.Fatalf("run supplied plan with portable input: %v", err)
	}
	sess, err := session.Open(sessionDir)
	if err != nil {
		t.Fatalf("open supplied portable session: %v", err)
	}
	for _, jsonMode := range []bool{false, true} {
		report, err := v2ExportReport(sess, jsonMode)
		if err != nil {
			t.Fatalf("build supplied export report (json=%t): %v", jsonMode, err)
		}
		output := filepath.Join(root, map[bool]string{false: "export.md", true: "export.json"}[jsonMode])
		if _, err := writeExportOutput(report, output, jsonMode); err != nil {
			t.Fatalf("write supplied export (json=%t): %v", jsonMode, err)
		}
	}
	bundle := filepath.Join(root, "portable")
	if _, err := v2ExportPortable(sess, bundle, "test"); err != nil {
		t.Fatalf("export supplied portable bundle: %v", err)
	}
	verified, err := v2VerifyPortableDirectory(bundle)
	if err != nil {
		t.Fatalf("verify supplied portable bundle: %v", err)
	}
	if verified["status"] != "valid" {
		t.Fatalf("supplied portable verification = %#v", verified)
	}
	inputPayloads, err := filepath.Glob(filepath.Join(bundle, "payloads", "input", "*.json"))
	if err != nil || len(inputPayloads) != 1 {
		t.Fatalf("supplied portable input payloads = %v, %v", inputPayloads, err)
	}
	payload, err := os.ReadFile(inputPayloads[0])
	if err != nil {
		t.Fatalf("read supplied portable input payload: %v", err)
	}
	if !bytes.Equal(payload, input) {
		t.Fatalf("supplied portable input payload = %q, want %q", payload, input)
	}
}

func TestSuppliedPlanAcceptsRelayWrittenSession(t *testing.T) {
	want := suppliedPlanFixture("relay-written-session", nil)
	created, err := session.CreateWithOptions(session.CreateOptions{
		RelayHome: t.TempDir(),
		Plan:      want,
	})
	if err != nil {
		t.Fatalf("create relay session: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(created.Root, session.SessionFilename))
	if err != nil {
		t.Fatalf("read relay session.json: %v", err)
	}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, body, 0o600); err != nil {
		t.Fatalf("write supplied plan: %v", err)
	}

	got, err := readSuppliedPlan(planPath)
	if err != nil {
		t.Fatalf("read relay-written supplied plan: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded plan = %#v, want %#v", got, want)
	}
}

func TestSuppliedPlanMaterializesEveryPayloadBeforeFirstTurn(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session")
	sourceDir := filepath.Join(root, "source")
	first := []byte("first review file")
	second := []byte("second review file")
	firstRef := blobstore.RefForBytes(first, "text/plain")
	secondRef := blobstore.RefForBytes(second, "text/plain")
	writeSuppliedBlob(t, sourceDir, firstRef, first)
	writeSuppliedBlob(t, sourceDir, secondRef, second)
	marker := filepath.Join(root, "turn-check")
	checkedScript := strings.Replace(fakeCodexAppServerScript,
		`        elif method == "turn/start":
            turn_number += 1`,
		`        elif method == "turn/start":
            check_root = os.environ.get("SUPPLIED_BLOB_CHECK", "")
            digests = [item for item in os.environ.get("SUPPLIED_BLOB_DIGESTS", "").split(",") if item]
            present = all((Path(check_root) / "blobs" / "sha256" / item).is_file() for item in digests)
            Path(os.environ["SUPPLIED_BLOB_MARKER"]).write_text("present" if present else "missing", encoding="utf-8")
            turn_number += 1`, 1)
	installSuppliedCodex(t, checkedScript)
	planValue := suppliedPlanFixture("materialized-session", []session.Input{{
		Name:     "review-files",
		Contents: []blobstore.BlobRef{firstRef, secondRef},
	}})
	launchCWD := filepath.Join(root, "launch")
	if err := os.MkdirAll(launchCWD, 0o700); err != nil {
		t.Fatalf("create launch CWD: %v", err)
	}
	t.Setenv("SUPPLIED_BLOB_CHECK", sessionDir)
	t.Setenv("SUPPLIED_BLOB_DIGESTS", firstRef.SHA256+","+secondRef.SHA256)
	t.Setenv("SUPPLIED_BLOB_MARKER", marker)

	if _, err := v2RunSuppliedPlan(context.Background(), v2SuppliedPlanRunOptions{
		SessionDir: sessionDir,
		PlanPath:   writeSuppliedPlan(t, planValue),
		BlobsPath:  sourceDir,
		LaunchCWD:  launchCWD,
	}); err != nil {
		t.Fatalf("run supplied plan with blobs: %v", err)
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read first-turn blob marker: %v", err)
	}
	if string(body) != "present" {
		t.Fatalf("first-turn blob marker = %q, want present", body)
	}
	stored, err := session.Open(sessionDir)
	if err != nil {
		t.Fatalf("open materialized session: %v", err)
	}
	store, err := stored.BlobStore(blobstore.Limits{})
	if err != nil {
		t.Fatalf("open materialized blob store: %v", err)
	}
	for _, ref := range []blobstore.BlobRef{firstRef, secondRef} {
		if err := store.Verify(ref); err != nil {
			t.Fatalf("verify materialized %s: %v", ref.SHA256, err)
		}
	}
}

func TestSuppliedPlanMissingBlobStopsBeforeExecution(t *testing.T) {
	digest := strings.Repeat("a", sha256.Size*2)
	planValue := suppliedPlanFixture("missing-blob-session", []session.Input{{
		Name:     "payload",
		Contents: []blobstore.BlobRef{{SHA256: digest, Size: 3, MediaType: "text/plain"}},
	}})
	sessionDir := filepath.Join(t.TempDir(), "session")
	err := runSuppliedPlanError(t, planValue, sessionDir, filepath.Join(t.TempDir(), "source"))
	if !strings.Contains(err.Error(), digest) || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("missing blob error = %v, want digest and input name", err)
	}
	assertNoTurnEvents(t, sessionDir)
}

func TestSuppliedPlanDigestMismatchStopsBeforeExecution(t *testing.T) {
	body := []byte("wrong")
	ref := blobstore.RefForBytes([]byte("right"), "text/plain")
	sourceDir := t.TempDir()
	writeSuppliedBlob(t, sourceDir, ref, body)
	planValue := suppliedPlanFixture("mismatch-blob-session", []session.Input{{
		Name:     "payload",
		Contents: []blobstore.BlobRef{ref},
	}})
	sessionDir := filepath.Join(t.TempDir(), "session")
	err := runSuppliedPlanError(t, planValue, sessionDir, sourceDir)
	if !strings.Contains(err.Error(), ref.SHA256) || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("digest mismatch error = %v, want digest and input name", err)
	}
	assertNoTurnEvents(t, sessionDir)
}

func TestSuppliedPlanDoesNotFillMissingTimeoutDefaults(t *testing.T) {
	planValue := suppliedPlanFixture("missing-timeout-session", nil)
	planValue.Timeouts.TurnSeconds = 0
	planPath := writeSuppliedPlan(t, planValue)
	sessionDir := filepath.Join(t.TempDir(), "session")
	_, err := v2RunSuppliedPlan(context.Background(), v2SuppliedPlanRunOptions{
		SessionDir: sessionDir,
		PlanPath:   planPath,
		LaunchCWD:  t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "timeouts.turn_seconds") {
		t.Fatalf("missing timeout error = %v, want direct validation failure", err)
	}
	if _, statErr := os.Stat(sessionDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid supplied plan created session directory: %v", statErr)
	}
}

func TestSuppliedPlanRejectsUnknownJSONField(t *testing.T) {
	planValue := suppliedPlanFixture("unknown-field-session", nil)
	body, err := json.Marshal(planValue)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode plan object: %v", err)
	}
	document["unexpected_field"] = true
	body, err = json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal unknown-field plan: %v", err)
	}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, body, 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	if _, err := readSuppliedPlan(planPath); err == nil || !strings.Contains(err.Error(), "unexpected_field") {
		t.Fatalf("unknown field error = %v, want field name", err)
	}
}

func TestSuppliedPlanRejectsAgentsOverride(t *testing.T) {
	err := validateSuppliedPlanRunStructuralOverrides(map[string]bool{"agents": true})
	if err == nil || !strings.Contains(err.Error(), "run --plan") || !strings.Contains(err.Error(), "--agents") {
		t.Fatalf("agents conflict error = %v", err)
	}
}

func suppliedPlanFixture(sessionID string, inputs []session.Input) session.Plan {
	if inputs == nil {
		inputs = []session.Input{}
	}
	return session.Plan{
		Kind:          session.PlanKind,
		SchemaVersion: session.SchemaVersion,
		SessionID:     sessionID,
		Provenance:    session.ProvenanceSupplied,
		Task:          "run the supplied plan",
		Timeouts:      session.Timeouts{TurnSeconds: 60, StallSeconds: 30},
		Mode:          session.ModeCooperative,
		Investigation: session.InvestigationNormal,
		Actors:        []session.Actor{{ID: "actor", Backend: "codex"}},
		Schedule:      session.Schedule{Kind: "sequence", Turns: 1, Order: []string{"actor"}},
		ProviderRetry: session.ProviderRetry{Mode: "forbid", MaxAttempts: 1},
		Workspace:     session.Workspace{Mode: "current"},
		Inputs:        inputs,
		Context:       []session.Input{},
		Skills:        []session.Input{},
		ChildPolicy:   session.ChildPolicy{Mode: "deny", AllowedRecipes: []string{}},
		Result:        session.Result{Source: "last_turn", Format: "text"},
	}
}

func writeSuppliedPlan(t *testing.T, value session.Plan) string {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal supplied plan: %v", err)
	}
	filename := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(filename, body, 0o600); err != nil {
		t.Fatalf("write supplied plan: %v", err)
	}
	return filename
}

func writeSuppliedBlob(t *testing.T, root string, ref blobstore.BlobRef, body []byte) {
	t.Helper()
	directory := filepath.Join(root, blobstore.SHA256Directory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create supplied blob directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, ref.SHA256), body, 0o600); err != nil {
		t.Fatalf("write supplied blob: %v", err)
	}
}

func installSuppliedCodex(t *testing.T, script string) {
	t.Helper()
	directory := t.TempDir()
	filename := filepath.Join(directory, "codex")
	if err := os.WriteFile(filename, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake supplied codex: %v", err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func runSuppliedPlanError(t *testing.T, value session.Plan, sessionDir string, sourceDir string) error {
	t.Helper()
	_, err := v2RunSuppliedPlan(context.Background(), v2SuppliedPlanRunOptions{
		SessionDir: sessionDir,
		PlanPath:   writeSuppliedPlan(t, value),
		BlobsPath:  sourceDir,
		LaunchCWD:  t.TempDir(),
	})
	if err == nil {
		t.Fatal("supplied plan unexpectedly succeeded")
	}
	return err
}

func assertNoTurnEvents(t *testing.T, sessionDir string) {
	t.Helper()
	events, err := relayv2.Events(mustOpenSession(t, sessionDir))
	if err != nil {
		t.Fatalf("read supplied plan events: %v", err)
	}
	for _, event := range events {
		if event.Type == eventlog.TurnStarted || event.Type == eventlog.TurnFinished {
			t.Fatalf("supplied plan wrote turn event before blob failure: %s", event.Type)
		}
	}
}

func mustOpenSession(t *testing.T, directory string) *session.Session {
	t.Helper()
	value, err := session.Open(directory)
	if err != nil {
		t.Fatalf("open supplied plan session: %v", err)
	}
	return value
}
