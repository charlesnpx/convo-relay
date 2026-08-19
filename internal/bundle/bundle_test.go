package bundle

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/session"
)

func TestCreateVerifyAndRelocate(t *testing.T) {
	directory, _ := fixtureBundle(t)
	manifest, err := Verify(directory)
	if err != nil {
		t.Fatalf("verify created bundle: %v", err)
	}
	if manifest.FormatVersion != FormatVersion || len(manifest.BlobInventory) != 1 {
		t.Fatalf("verified manifest = %#v", manifest)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read bundle directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "blobs,events.jsonl,manifest.json,session.json"; got != want {
		t.Fatalf("bundle layout = %s, want %s", got, want)
	}
	relocated := filepath.Join(t.TempDir(), "relocated-bundle")
	if err := os.Rename(directory, relocated); err != nil {
		t.Fatalf("relocate bundle: %v", err)
	}
	if _, err := Verify(relocated); err != nil {
		t.Fatalf("verify relocated bundle: %v", err)
	}
}

func TestVerifyRejectsEachTamperClass(t *testing.T) {
	t.Run("blob", func(t *testing.T) {
		directory, manifest := fixtureBundle(t)
		filename := filepath.Join(directory, "blobs", blobstore.SHA256Directory, manifest.BlobInventory[0].SHA256)
		if err := os.WriteFile(filename, []byte("corrupt blob"), 0o600); err != nil {
			t.Fatalf("corrupt blob: %v", err)
		}
		assertDigestMismatch(t, directory, filepath.ToSlash(filepath.Join("blobs", blobstore.SHA256Directory, manifest.BlobInventory[0].SHA256)))
	})
	t.Run("events", func(t *testing.T) {
		directory, _ := fixtureBundle(t)
		if err := os.WriteFile(filepath.Join(directory, eventlog.EventsFilename), []byte("tampered"), 0o600); err != nil {
			t.Fatalf("corrupt events: %v", err)
		}
		assertDigestMismatch(t, directory, eventlog.EventsFilename)
	})
	t.Run("session", func(t *testing.T) {
		directory, _ := fixtureBundle(t)
		filename := filepath.Join(directory, session.SessionFilename)
		body, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read session: %v", err)
		}
		changed := bytes.Replace(body, []byte("test-model"), []byte("test-modal"), 1)
		if bytes.Equal(changed, body) {
			t.Fatal("session fixture has no model token to change")
		}
		if err := os.WriteFile(filename, changed, 0o600); err != nil {
			t.Fatalf("corrupt session: %v", err)
		}
		assertDigestMismatch(t, directory, session.SessionFilename)
	})
	t.Run("unexpected-file", func(t *testing.T) {
		directory, _ := fixtureBundle(t)
		if err := os.WriteFile(filepath.Join(directory, "unexpected.txt"), []byte("extra"), 0o600); err != nil {
			t.Fatalf("write extra: %v", err)
		}
		_, err := Verify(directory)
		var unexpected *UnexpectedFileError
		if !errors.As(err, &unexpected) {
			t.Fatalf("extra-file error = %v, want UnexpectedFileError", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		directory, _ := fixtureBundle(t)
		if err := os.Symlink(eventlog.EventsFilename, filepath.Join(directory, "link")); err != nil {
			t.Fatalf("create symlink: %v", err)
		}
		_, err := Verify(directory)
		var link *SymlinkError
		if !errors.As(err, &link) {
			t.Fatalf("symlink error = %v, want SymlinkError", err)
		}
	})
}

func TestCreateAndVerifyRejectMalformedTerminatedFinalRecord(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		created := fixtureSourceSession(t)
		filename := filepath.Join(created.Root, eventlog.EventsFilename)
		body, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read source events: %v", err)
		}
		body = append(body, []byte("not-json\n")...)
		if err := os.WriteFile(filename, body, 0o600); err != nil {
			t.Fatalf("append malformed terminated tail: %v", err)
		}
		_, err = Create(created.Root, filepath.Join(t.TempDir(), "portable-bundle"))
		var lineErr *eventlog.ReplayLineError
		if !errors.As(err, &lineErr) {
			t.Fatalf("create malformed-tail error = %v, want ReplayLineError", err)
		}
	})
	t.Run("verify", func(t *testing.T) {
		directory, manifest := fixtureBundle(t)
		filename := filepath.Join(directory, eventlog.EventsFilename)
		body, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read bundle events: %v", err)
		}
		body = append(body, []byte("not-json\n")...)
		if err := os.WriteFile(filename, body, 0o600); err != nil {
			t.Fatalf("append malformed bundle tail: %v", err)
		}
		manifest.EventsJSONLDigest = eventlog.RawBytesDigest(body)
		manifestBody, err := eventlog.SemanticJSONBytes(manifest)
		if err != nil {
			t.Fatalf("encode updated manifest: %v", err)
		}
		if err := os.WriteFile(filepath.Join(directory, ManifestFilename), manifestBody, 0o600); err != nil {
			t.Fatalf("write updated manifest: %v", err)
		}
		_, err = Verify(directory)
		var lineErr *eventlog.ReplayLineError
		if !errors.As(err, &lineErr) {
			t.Fatalf("verify malformed-tail error = %v, want ReplayLineError", err)
		}
	})
}

func assertDigestMismatch(t *testing.T, directory string, path string) {
	t.Helper()
	_, err := Verify(directory)
	var mismatch *DigestMismatchError
	if !errors.As(err, &mismatch) || mismatch.Path != path {
		t.Fatalf("verify tamper error = %v, want DigestMismatchError for %s", err, path)
	}
}

func fixtureBundle(t *testing.T) (string, *Manifest) {
	t.Helper()
	created := fixtureSourceSession(t)
	target := filepath.Join(t.TempDir(), "portable-bundle")
	manifest, err := Create(created.Root, target)
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	return target, manifest
}

func fixtureSourceSession(t *testing.T) *session.Session {
	t.Helper()
	relayHome := filepath.Join(t.TempDir(), "relay-home")
	created, err := session.Create(relayHome, bundlePlan())
	if err != nil {
		t.Fatalf("create source session: %v", err)
	}
	store, err := created.BlobStore(blobstore.Limits{})
	if err != nil {
		t.Fatalf("open source blobs: %v", err)
	}
	content, err := store.PutMediaType(bytes.NewReader([]byte("bundle payload")), "text/plain")
	if err != nil {
		t.Fatalf("put source blob: %v", err)
	}
	writer, err := created.EventWriter(store)
	if err != nil {
		t.Fatalf("open source log: %v", err)
	}
	digest, err := session.PlanDigest(created.Plan)
	if err != nil {
		t.Fatalf("plan digest: %v", err)
	}
	for index, payload := range []eventlog.Payload{
		eventlog.SessionStartedPayload{PlanDigest: digest, SessionID: created.Plan.SessionID},
		eventlog.TurnStartedPayload{ActorID: "actor-a", Round: 1, Role: eventlog.ParticipantRole},
		eventlog.TurnFinishedPayload{ActorID: "actor-a", Round: 1, Content: content},
		eventlog.SessionFinishedPayload{Status: "completed", StopReason: "done"},
	} {
		if _, err := writer.Append(eventlog.NewEvent("bundle-event-"+string(rune('a'+index)), bundleTime(), payload)); err != nil {
			t.Fatalf("append source event %d: %v", index, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close source log: %v", err)
	}
	return created
}

func bundlePlan() session.Plan {
	return session.Plan{
		Kind:          session.PlanKind,
		Provenance:    session.ProvenanceOrdinary,
		Task:          "trace task",
		Timeouts:      session.Timeouts{TurnSeconds: 30, StallSeconds: 30},
		SchemaVersion: session.SchemaVersion,
		Actors:        []session.Actor{{ID: "actor-a", Backend: "codex", Model: "test-model", Effort: "medium"}},
		Schedule:      session.Schedule{Kind: "dialogue", Turns: 1},
		ProviderRetry: session.ProviderRetry{Mode: "allow", MaxAttempts: 1},
		Workspace:     session.Workspace{Mode: "current"},
		Inputs:        []session.Input{},
		ChildPolicy:   session.ChildPolicy{Mode: "disabled", AllowedRecipes: []string{}},
		Result:        session.Result{Format: "text"},
	}
}

func bundleTime() time.Time {
	return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
}
