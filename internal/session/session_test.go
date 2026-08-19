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
		SchemaVersion: SchemaVersion,
		Actors: []Actor{{
			ID: "actor-a", Backend: "codex", Model: "test-model", Effort: "medium",
		}},
		Schedule:      Schedule{Kind: "dialogue", Turns: 2, StopOnConvergence: true},
		Facilitator:   &Facilitator{Actor: "actor-a", Cadence: 1},
		Reducer:       &Reducer{Actor: "actor-a"},
		ProviderRetry: ProviderRetry{Mode: "allow", MaxAttempts: 2},
		Workspace:     Workspace{Mode: "current"},
		Inputs:        []Input{},
		ChildPolicy:   ChildPolicy{Mode: "disabled", MaxDepth: 0, MaxChildren: 0, MaxTurns: 0, AllowedRecipes: []string{}},
		Result:        Result{Format: "text"},
	}
}

func fixedTime() time.Time {
	return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
}
