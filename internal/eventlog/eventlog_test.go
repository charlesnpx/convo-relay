package eventlog

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
)

func TestRoundTripEveryTypedEvent(t *testing.T) {
	root, store, writer := newTestWriter(t)
	defer writer.Close()
	ref, err := store.PutMediaType(bytes.NewReader([]byte("payload")), "text/plain")
	if err != nil {
		t.Fatalf("put payload: %v", err)
	}
	want := make([]Event, 0, 16)
	for index, payload := range []Payload{
		SessionStartedPayload{PlanDigest: RawBytesDigest([]byte("plan")), SessionID: "session-one"},
		TurnStartedPayload{ActorID: "actor-a", Round: 1, Role: ParticipantRole},
		TurnFinishedPayload{ActorID: "actor-a", Round: 1, Content: ref},
		AttemptStartedPayload{ActorID: "actor-a", Attempt: 1},
		AttemptFinishedPayload{ActorID: "actor-a", Attempt: 1, Outcome: "success", ProviderSessionID: "provider-a", Content: ref},
		ProviderFailedPayload{ActorID: "actor-a", Backend: "codex", Category: "transport", Retryable: true, Attempts: 1, RemediationCode: "retry", SanitizedDetail: "temporary network failure"},
		ChildRequestedPayload{RequestID: "request-one", RequesterActorID: "actor-a", RecipeID: "review", Question: ref},
		ChildDecidedPayload{RequestID: "request-one", Admitted: true, Reason: "within budget", BudgetState: "remaining"},
		ChildCompletedPayload{RequestID: "request-one", ChildSessionID: "child-one", Result: ref},
		SteeringQueuedPayload{Prompt: ref},
		SteeringAppliedPayload{Prompt: ref, Round: 1},
		CancelRequestedPayload{Source: "user", Force: false},
		InputIngestedPayload{LogicalName: "brief", Content: ref},
		WorkspacePreparedPayload{Mode: "head-copy", Commit: "abcdef", TreeHash: "123456"},
		ResultProducedPayload{Result: ref, Format: "text", ValidationOutcome: "valid"},
		SessionFinishedPayload{Status: "completed", StopReason: "converged"},
	} {
		appended, err := writer.Append(NewEvent(fmt.Sprintf("event-%02d", index+1), fixtureTime(index), payload))
		if err != nil {
			t.Fatalf("append %T: %v", payload, err)
		}
		want = append(want, appended)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	got, err := replayFile(filepath.Join(root, EventsFilename))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("event count = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if !got[index].Equal(want[index]) {
			t.Fatalf("event %d did not round trip: got %#v want %#v", index+1, got[index], want[index])
		}
	}
	if _, ok := got[0].Payload.(SessionStartedPayload); !ok {
		t.Fatalf("replay left first payload untyped: %T", got[0].Payload)
	}
}

func TestReplayRecoversOnlyMalformedTail(t *testing.T) {
	complete, err := Replay(bytes.NewReader(canonicalLine(t, fixtureEvent(1, "complete-no-newline"))))
	if err != nil || len(complete) != 1 {
		t.Fatalf("complete final line without newline: events=%d err=%v", len(complete), err)
	}

	root, _, writer := newTestWriter(t)
	for index := 0; index < 3; index++ {
		if _, err := writer.Append(NewEvent(fmt.Sprintf("tail-%d", index), fixtureTime(index), CancelRequestedPayload{Source: "test", Force: false})); err != nil {
			t.Fatalf("append %d: %v", index, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	filename := filepath.Join(root, EventsFilename)
	body, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if err := os.Truncate(filename, int64(len(body)-5)); err != nil {
		t.Fatalf("truncate final line: %v", err)
	}
	got, err := replayFile(filename)
	if err != nil {
		t.Fatalf("replay truncated tail: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recovered event count = %d, want 2", len(got))
	}
}

func TestPortableValueRejectsEmbeddedAbsolutePath(t *testing.T) {
	type record struct {
		Detail string `json:"detail"`
	}
	err := ValidatePortableValue(record{Detail: "provider said at /machine/local/path"})
	var local *PortableValueError
	if !errors.As(err, &local) {
		t.Fatalf("embedded absolute path error = %v, want PortableValueError", err)
	}
}

func TestReplayRejectsMalformedMiddleAndBadSequence(t *testing.T) {
	first := fixtureEvent(1, "first")
	third := fixtureEvent(3, "third")
	duplicate := fixtureEvent(1, "duplicate")
	firstLine := canonicalLine(t, first)
	thirdLine := canonicalLine(t, third)
	duplicateLine := canonicalLine(t, duplicate)

	if _, err := Replay(bytes.NewReader(append(append(append([]byte{}, firstLine...), []byte("\nnot-json\n")...), append(thirdLine, '\n')...))); err == nil {
		t.Fatal("malformed middle line replay unexpectedly succeeded")
	} else {
		var lineErr *ReplayLineError
		if !errors.As(err, &lineErr) || lineErr.Line != 2 {
			t.Fatalf("malformed middle error = %v, want line-2 ReplayLineError", err)
		}
	}

	if _, err := Replay(bytes.NewReader(append(append(firstLine, '\n'), append(thirdLine, '\n')...))); err == nil {
		t.Fatal("non-contiguous sequence replay unexpectedly succeeded")
	} else {
		var sequence *SequenceError
		if !errors.As(err, &sequence) || sequence.Expected != 2 || sequence.Actual != 3 {
			t.Fatalf("non-contiguous error = %v", err)
		}
	}

	if _, err := Replay(bytes.NewReader(append(append(firstLine, '\n'), append(duplicateLine, '\n')...))); err == nil {
		t.Fatal("duplicate sequence replay unexpectedly succeeded")
	} else {
		var sequence *SequenceError
		if !errors.As(err, &sequence) || sequence.Expected != 2 || sequence.Actual != 1 {
			t.Fatalf("duplicate error = %v", err)
		}
	}
}

func TestAppendRequiresDurableBlobAndLeavesUnreferencedBlobReadable(t *testing.T) {
	root, store, writer := newTestWriter(t)
	defer writer.Close()
	missing := blobstore.BlobRef{SHA256: "0000000000000000000000000000000000000000000000000000000000000000", Size: 1, MediaType: "text/plain"}
	_, err := writer.Append(NewEvent("missing", fixtureTime(0), TurnFinishedPayload{ActorID: "actor-a", Round: 1, Content: missing}))
	var notDurable *BlobNotDurableError
	if !errors.As(err, &notDurable) {
		t.Fatalf("append missing blob error = %v, want BlobNotDurableError", err)
	}
	orphan, err := store.PutMediaType(bytes.NewReader([]byte("orphan")), "text/plain")
	if err != nil {
		t.Fatalf("put orphan: %v", err)
	}
	content, err := store.PutMediaType(bytes.NewReader([]byte("turn")), "text/plain")
	if err != nil {
		t.Fatalf("put content: %v", err)
	}
	if _, err := writer.Append(NewEvent("durable", fixtureTime(1), TurnFinishedPayload{ActorID: "actor-a", Round: 1, Content: content})); err != nil {
		t.Fatalf("append durable blob: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	got, err := replayFile(filepath.Join(root, EventsFilename))
	if err != nil || len(got) != 1 {
		t.Fatalf("replay with orphan: events=%d err=%v", len(got), err)
	}
	report, err := store.Sweep(BlobRefs(got))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(report.Unreferenced) != 1 || report.Unreferenced[0].SHA256 != orphan.SHA256 {
		t.Fatalf("unreferenced report = %#v", report)
	}
}

func TestAppendDoesNotReplayExistingEvents(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(root); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	var body bytes.Buffer
	for sequence := 1; sequence <= 10000; sequence++ {
		line := canonicalLine(t, Event{
			Seq:     uint64(sequence),
			EventID: fmt.Sprintf("history-%05d", sequence),
			Time:    fixtureTime(sequence),
			Type:    CancelRequested,
			Payload: CancelRequestedPayload{Source: "history", Force: false},
		})
		body.Write(line)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(root, EventsFilename), body.Bytes(), 0o600); err != nil {
		t.Fatalf("write history: %v", err)
	}
	writer, err := OpenWriter(root, nil)
	if err != nil {
		t.Fatalf("open history writer: %v", err)
	}
	defer writer.Close()
	if stats := writer.Stats(); stats.StartupEvents != 10000 {
		t.Fatalf("startup replay events = %d, want 10000", stats.StartupEvents)
	}
	appended, err := writer.Append(NewEvent("after-history", fixtureTime(10001), CancelRequestedPayload{Source: "history", Force: false}))
	if err != nil {
		t.Fatalf("append after history: %v", err)
	}
	if appended.Seq != 10001 {
		t.Fatalf("appended sequence = %d, want 10001", appended.Seq)
	}
	if stats := writer.Stats(); stats.AppendReadOperations != 0 {
		t.Fatalf("append read operations = %d, want 0", stats.AppendReadOperations)
	}
}

func newTestWriter(t *testing.T) (string, *blobstore.Store, *Writer) {
	t.Helper()
	root := t.TempDir()
	store, err := blobstore.New(root, blobstore.Limits{})
	if err != nil {
		t.Fatalf("new blob store: %v", err)
	}
	if err := Initialize(root); err != nil {
		t.Fatalf("initialize log: %v", err)
	}
	writer, err := OpenWriter(root, store)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	return root, store, writer
}

func replayFile(filename string) ([]Event, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return Replay(file)
}

func fixtureEvent(sequence uint64, identifier string) Event {
	return Event{
		Seq:     sequence,
		EventID: identifier,
		Time:    fixtureTime(int(sequence)),
		Type:    SessionStarted,
		Payload: SessionStartedPayload{PlanDigest: RawBytesDigest([]byte("plan")), SessionID: "session-one"},
	}
}

func canonicalLine(t *testing.T, event Event) []byte {
	t.Helper()
	line, err := CanonicalEventBytes(event)
	if err != nil {
		t.Fatalf("canonical event: %v", err)
	}
	return line
}

func fixtureTime(offset int) time.Time {
	return time.Date(2026, 8, 18, 12, 0, offset%60, 0, time.UTC)
}
