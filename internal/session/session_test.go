package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
)

func TestCreateUsesManagedFreshRootAndNeverOverwritesSessionJSON(t *testing.T) {
	relayHome := filepath.Join(t.TempDir(), "relay-home")
	first, err := Create(relayHome, testPlan())
	if err != nil {
		t.Fatalf("create first session: %v", err)
	}
	second, err := Create(relayHome, testPlan())
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	if first.Root == second.Root {
		t.Fatal("Create reused a caller-visible session directory")
	}
	relative, err := filepath.Rel(relayHome, first.Root)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("session root %q is not under relay home %q", first.Root, relayHome)
	}
	opened, err := Open(first.Root)
	if err != nil {
		t.Fatalf("open first session: %v", err)
	}
	if !opened.Plan.Equal(first.Plan) || opened.Digest != first.Digest {
		t.Fatalf("opened session differs: %#v != %#v", opened, first)
	}
	body, err := CanonicalBytes(first.Plan)
	if err != nil {
		t.Fatalf("canonical plan: %v", err)
	}
	if err := writeSessionOnce(first.Root, body); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second session.json write error = %v, want os.ErrExist", err)
	}
	entries, err := os.ReadDir(first.Root)
	if err != nil {
		t.Fatalf("read session root: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "blobs,events.jsonl,runtime,session.json"; got != want {
		t.Fatalf("session layout = %s, want %s", got, want)
	}
}

func TestPortableCanonicalRecordsContainNoLocalValues(t *testing.T) {
	relayHome := filepath.Join(t.TempDir(), "distinct-relay-home")
	created, err := Create(relayHome, testPlan())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	store, err := created.BlobStore(blobstore.Limits{})
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	content, err := store.PutMediaType(bytes.NewReader([]byte("portable turn")), "text/plain")
	if err != nil {
		t.Fatalf("put content: %v", err)
	}
	writer, err := created.EventWriter(store)
	if err != nil {
		t.Fatalf("open event writer: %v", err)
	}
	digest, err := PlanDigest(created.Plan)
	if err != nil {
		t.Fatalf("plan digest: %v", err)
	}
	if _, err := writer.Append(eventlog.NewEvent("started", fixedTime(), eventlog.SessionStartedPayload{PlanDigest: digest, SessionID: created.Plan.SessionID})); err != nil {
		t.Fatalf("append started: %v", err)
	}
	if _, err := writer.Append(eventlog.NewEvent("workspace", fixedTime(), eventlog.WorkspacePreparedPayload{Mode: "current", Commit: "abcdef", TreeHash: "123456"})); err != nil {
		t.Fatalf("append workspace: %v", err)
	}
	if _, err := writer.Append(eventlog.NewEvent("turn", fixedTime(), eventlog.TurnFinishedPayload{ActorID: "actor-a", Round: 1, Content: content})); err != nil {
		t.Fatalf("append turn: %v", err)
	}
	_, err = writer.Append(eventlog.NewEvent("bad-local", fixedTime(), eventlog.ProviderFailedPayload{
		ActorID: "actor-a", Backend: "codex", Category: "transport", Retryable: false, Attempts: 1, RemediationCode: "none", SanitizedDetail: "at " + created.Root,
	}))
	var local *eventlog.PortableValueError
	if !errors.As(err, &local) {
		t.Fatalf("local-path event error = %v, want PortableValueError", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	forbidden := []string{created.Root, relayHome, os.TempDir(), strconv.Itoa(os.Getpid())}
	if home, err := os.UserHomeDir(); err == nil {
		forbidden = append(forbidden, home)
	}
	if host, err := os.Hostname(); err == nil {
		forbidden = append(forbidden, host)
	}
	if current, err := user.Current(); err == nil {
		forbidden = append(forbidden, current.Username)
	}
	for _, filename := range []string{SessionFilename, eventlog.EventsFilename} {
		body, err := os.ReadFile(filepath.Join(created.Root, filename))
		if err != nil {
			t.Fatalf("read %s: %v", filename, err)
		}
		for _, line := range bytes.Split(body, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			if err := walkJSONStrings(line, func(value string) {
				for _, banned := range forbidden {
					if banned != "" && strings.Contains(value, banned) {
						t.Fatalf("%s serializes local value %q in string %q", filename, banned, value)
					}
				}
			}); err != nil {
				t.Fatalf("walk %s: %v", filename, err)
			}
		}
	}
}

func TestCreateRejectsAbsolutePathSchemaObjectKey(t *testing.T) {
	plan := testPlan()
	plan.Result.Schema = json.RawMessage(`{"/machine/local/result-schema": {"type": "string"}}`)
	_, err := Create(filepath.Join(t.TempDir(), "relay-home"), plan)
	var local *eventlog.PortableValueError
	if !errors.As(err, &local) {
		t.Fatalf("schema object-key error = %v, want PortableValueError", err)
	}
}

func walkJSONStrings(body []byte, visit func(string)) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := walkJSONValue(decoder, visit); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("JSON contains trailing token")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, visit func(string)) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		if text, ok := token.(string); ok {
			visit(text)
		}
		return nil
	}
	switch delimiter {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := walkJSONValue(decoder, visit); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder, visit); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func testPlan() Plan {
	return Plan{
		Kind:          PlanKind,
		Provenance:    ProvenanceOrdinary,
		Task:          "trace task",
		Timeouts:      Timeouts{TurnSeconds: 30, StallSeconds: 30},
		Mode:          ModeAdversarial,
		Investigation: InvestigationAuto,
		SchemaVersion: SchemaVersion,
		Actors: []Actor{
			{ID: "actor-a", Backend: "codex", Model: "test-model", Effort: "medium"},
			{ID: "actor-b", Backend: "claude", Model: "test-model", Effort: "medium"},
		},
		Schedule:      Schedule{Kind: "dialogue", Turns: 2, StopOnConvergence: true},
		ProviderRetry: ProviderRetry{Mode: "allow", MaxAttempts: 2},
		Workspace:     Workspace{Mode: "current"},
		Inputs:        []Input{},
		ChildPolicy:   ChildPolicy{Mode: "deny", MaxDepth: 0, MaxChildren: 0, MaxTurns: 0, AllowedRecipes: []string{}},
		Result:        Result{Source: "last_turn", Format: "text"},
	}
}

func fixedTime() time.Time {
	return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
}

// Task carries operator prose that may deliberately name a path, so it is the one
// field excluded from the portability walk. Every other field must still reject one.
func TestTaskIsExemptFromPortabilityWalkButOtherFieldsAreNot(t *testing.T) {
	plan := testPlan()
	plan.SessionID = "task-exempt"
	plan.Task = "review /Users/someone/project/main.go and report back"
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("operator task naming a path was rejected: %v", err)
	}

	leaky := testPlan()
	leaky.SessionID = "task-exempt"
	leaky.Provenance = ProvenanceRecipe
	leaky.RecipeID = "/Users/someone/recipes/panel.toml"
	if err := ValidatePlan(leaky); err == nil {
		t.Fatal("an absolute path in recipe_id was accepted")
	}
}

func TestValidatePlanRejectsInvalidSequenceOrder(t *testing.T) {
	base := testPlan()
	base.SessionID = "sequence-test"
	base.Actors = []Actor{
		{ID: "alpha", Backend: "codex", Model: "test-model", Effort: "medium"},
		{ID: "facilitator", Backend: "codex", Model: "test-model", Effort: "medium"},
		{ID: "reducer", Backend: "codex", Model: "test-model", Effort: "medium"},
	}
	base.Facilitator = &Facilitator{Actor: "facilitator", Cadence: 1}
	base.Reducer = &Reducer{Actor: "reducer"}
	for _, test := range []struct {
		name  string
		order []string
		want  string
	}{
		{name: "wrong length", order: []string{"alpha"}, want: "exactly 2"},
		{name: "facilitator", order: []string{"facilitator", "alpha"}, want: "control actor"},
		{name: "reducer", order: []string{"reducer", "alpha"}, want: "control actor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := base
			plan.Schedule = Schedule{Kind: "sequence", Turns: 2, Order: test.order}
			if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidatePlan error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidatePlanSequenceCoversEveryNonControlActor(t *testing.T) {
	plan := testPlan()
	plan.SessionID = "sequence-coverage"
	plan.Actors = []Actor{
		{ID: "alpha", Backend: "codex", Model: "test-model", Effort: "medium"},
		{ID: "beta", Backend: "claude", Model: "test-model", Effort: "medium"},
	}
	plan.Schedule = Schedule{Kind: "sequence", Turns: 3, Order: []string{"alpha", "beta", "alpha"}}
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("repeated sequence order was rejected: %v", err)
	}

	plan.Actors = append(plan.Actors, Actor{ID: "never-scheduled", Backend: "gemini", Model: "test-model", Effort: "medium"})
	if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "must include every actor") {
		t.Fatalf("unused sequence actor error = %v", err)
	}
}

func TestValidatePlanRejectsUnsupportedActorBackend(t *testing.T) {
	plan := testPlan()
	plan.SessionID = "relay-plan"
	plan.Actors[0].Backend = "relay"
	if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("ValidatePlan relay actor error = %v", err)
	}

	relayHome := filepath.Join(t.TempDir(), "relay-home")
	if _, err := Create(relayHome, plan); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Create relay actor error = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(relayHome, "*", SessionFilename))
	if err != nil {
		t.Fatalf("glob session files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("Create persisted unsupported backend plan: %v", matches)
	}
}

func TestValidatePlanRejectsForbiddenDynamicWithPermissiveChildPolicy(t *testing.T) {
	plan := testPlan()
	plan.SessionID = "forbidden-dynamic"
	plan.Lifecycle = &Lifecycle{
		Resume:             "allow",
		Steering:           "allow",
		Dynamic:            "forbid",
		WorkspaceIsolation: "inherited",
	}
	plan.ChildPolicy = ChildPolicy{Mode: "allow", MaxDepth: 1, MaxChildren: 1, MaxTurns: 1, AllowedRecipes: []string{}}
	if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "dynamic forbid") {
		t.Fatalf("lifecycle/child policy error = %v", err)
	}
}

func TestValidatePlanRejectsUnknownChildPolicyMode(t *testing.T) {
	for _, mode := range []string{"explode", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			plan := testPlan()
			plan.SessionID = "child-policy-" + mode
			plan.ChildPolicy.Mode = mode
			if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "deny, ask, or allow") {
				t.Fatalf("ValidatePlan child-policy error = %v", err)
			}
		})
	}
}
