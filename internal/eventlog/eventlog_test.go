package eventlog

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	planRef := ref
	promptRef := ref
	want := make([]Event, 0, 17)
	for index, payload := range []Payload{
		SessionStartedPayload{PlanDigest: RawBytesDigest([]byte("plan")), SessionID: "session-one"},
		TurnBudgetGrantedPayload{GrantedBy: "operator", Turns: 2, Prompt: &promptRef},
		TurnStartedPayload{ActorID: "actor-a", Round: 1, Role: ParticipantRole},
		TurnFinishedPayload{ActorID: "actor-a", Round: 1, Content: ref},
		AttemptStartedPayload{ActorID: "actor-a", Attempt: 1},
		AttemptFinishedPayload{ActorID: "actor-a", Attempt: 1, Outcome: "success", ProviderSessionID: "provider-a", Content: ref},
		ProviderFailedPayload{ActorID: "actor-a", Backend: "codex", Category: "transport", Retryable: true, Attempts: 1, RemediationCode: "retry", SanitizedDetail: "temporary network failure"},
		ChildRequestedPayload{RequestID: "request-one", RequesterActorID: "actor-a", RecipeID: "review", Question: ref},
		ChildDecidedPayload{RequestID: "request-one", Admitted: true, Reason: "within budget", BudgetState: "remaining", Plan: &planRef},
		ChildCompletedPayload{RequestID: "request-one", ChildSessionID: "child-one", Result: ref, Status: "completed"},
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

func TestTurnBudgetGrantedPayloadRequiresGrantorAndPositiveTurns(t *testing.T) {
	_, _, writer := newTestWriter(t)
	defer writer.Close()
	invalidPrompt := blobstore.BlobRef{}
	for index, payload := range []Payload{
		TurnBudgetGrantedPayload{GrantedBy: "", Turns: 1},
		TurnBudgetGrantedPayload{GrantedBy: "operator", Turns: 0},
		TurnBudgetGrantedPayload{GrantedBy: "operator", Turns: 1, Prompt: &invalidPrompt},
	} {
		if _, err := writer.Append(NewEvent(fmt.Sprintf("invalid-turn-budget-%d", index), fixtureTime(index), payload)); err == nil {
			t.Fatalf("Append(%T) unexpectedly accepted", payload)
		}
	}
}

func TestChildPayloadsRequireDurablePlanAndClassification(t *testing.T) {
	_, store, writer := newTestWriter(t)
	defer writer.Close()
	ref, err := store.PutMediaType(bytes.NewReader([]byte("payload")), "text/plain")
	if err != nil {
		t.Fatalf("put payload: %v", err)
	}
	for index, payload := range []Payload{
		ChildDecidedPayload{RequestID: "request-one", Admitted: true, Reason: "admitted", BudgetState: "available"},
		ChildDecidedPayload{RequestID: "request-two", Admitted: false, Reason: "rejected", BudgetState: "rejected", Plan: &ref},
		ChildCompletedPayload{RequestID: "request-three", ChildSessionID: "child-three", Result: ref},
		ChildCompletedPayload{RequestID: "request-four", ChildSessionID: "child-four", Result: ref, Status: "running"},
	} {
		if _, err := writer.Append(NewEvent(fmt.Sprintf("invalid-child-%d", index), fixtureTime(index), payload)); err == nil {
			t.Fatalf("Append(%T) unexpectedly accepted", payload)
		}
	}
}

func TestReplayRecoversOnlyMalformedTail(t *testing.T) {
	complete, err := Replay(bytes.NewReader(canonicalLine(t, fixtureEvent(1, "complete-no-newline"))))
	if err != nil || len(complete) != 1 {
		t.Fatalf("complete final line without newline: events=%d err=%v", len(complete), err)
	}
	terminated := append(canonicalLine(t, fixtureEvent(1, "complete")), []byte("\nnot-json\n")...)
	if _, err := Replay(bytes.NewReader(terminated)); err == nil {
		t.Fatal("malformed terminated tail replay unexpectedly succeeded")
	} else {
		var lineErr *ReplayLineError
		if !errors.As(err, &lineErr) || lineErr.Line != 2 {
			t.Fatalf("terminated tail error = %v, want line-2 ReplayLineError", err)
		}
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
	recovered, err := OpenWriter(root, nil)
	if err != nil {
		t.Fatalf("open recovered writer: %v", err)
	}
	appended, err := recovered.Append(NewEvent("replacement-tail", fixtureTime(4), CancelRequestedPayload{Source: "test", Force: false}))
	if err != nil {
		_ = recovered.Close()
		t.Fatalf("append replacement tail: %v", err)
	}
	if appended.Seq != 3 {
		_ = recovered.Close()
		t.Fatalf("replacement sequence = %d, want 3", appended.Seq)
	}
	if err := recovered.Close(); err != nil {
		t.Fatalf("close recovered writer: %v", err)
	}
	got, err = replayFile(filename)
	if err != nil || len(got) != 3 || got[2].EventID != "replacement-tail" {
		t.Fatalf("replacement replay: events=%#v err=%v", got, err)
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

func TestAppendRejectsFileURIInDurableFreeText(t *testing.T) {
	_, _, writer := newTestWriter(t)
	defer writer.Close()
	_, err := writer.Append(NewEvent("file-uri", fixtureTime(0), ProviderFailedPayload{
		ActorID: "actor-a", Backend: "codex", Category: "transport", Retryable: false, Attempts: 1, RemediationCode: "none",
		SanitizedDetail: "provider wrote file:///opt/provider/session.log",
	}))
	var local *PortableValueError
	if !errors.As(err, &local) {
		t.Fatalf("file URI append error = %v, want PortableValueError", err)
	}
}

func TestAppendRejectsChildProcessIDInDurableFreeText(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestPortableValueChildProcessHelper$")
	child.Env = append(os.Environ(), "EVENTLOG_CHILD_PID_HELPER=1")
	if err := child.Start(); err != nil {
		t.Fatalf("start child helper: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	_, _, writer := newTestWriter(t)
	defer writer.Close()
	_, err := writer.Append(NewEvent("child-pid", fixtureTime(0), ProviderFailedPayload{
		ActorID: "actor-a", Backend: "codex", Category: "transport", Retryable: false, Attempts: 1, RemediationCode: "none",
		SanitizedDetail: fmt.Sprintf("child process id=%d exited unexpectedly", child.Process.Pid),
	}))
	var local *PortableValueError
	if !errors.As(err, &local) {
		t.Fatalf("child pid append error = %v, want PortableValueError", err)
	}
}

func TestPortableValueChildProcessHelper(t *testing.T) {
	if os.Getenv("EVENTLOG_CHILD_PID_HELPER") == "1" {
		select {}
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

func TestAppendRejectsTurnBudgetGrantWithNonDurablePrompt(t *testing.T) {
	_, _, writer := newTestWriter(t)
	defer writer.Close()
	missing := blobstore.BlobRef{SHA256: "0000000000000000000000000000000000000000000000000000000000000000", Size: 1, MediaType: "text/plain"}
	_, err := writer.Append(NewEvent("missing-grant-prompt", fixtureTime(0), TurnBudgetGrantedPayload{
		GrantedBy: "operator", Turns: 1, Prompt: &missing,
	}))
	var notDurable *BlobNotDurableError
	if !errors.As(err, &notDurable) {
		t.Fatalf("append prompted grant with missing blob error = %v, want BlobNotDurableError", err)
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
	originalRead := readEventLogFile
	readCalls := 0
	readEventLogFile = func(filename string) ([]byte, error) {
		readCalls++
		return originalRead(filename)
	}
	t.Cleanup(func() { readEventLogFile = originalRead })
	writer, err := OpenWriter(root, nil)
	if err != nil {
		t.Fatalf("open history writer: %v", err)
	}
	defer writer.Close()
	if readCalls != 1 {
		t.Fatalf("startup log reads = %d, want 1", readCalls)
	}
	readsBeforeAppend := readCalls
	appended, err := writer.Append(NewEvent("after-history", fixtureTime(10001), CancelRequestedPayload{Source: "history", Force: false}))
	if err != nil {
		t.Fatalf("append after history: %v", err)
	}
	if appended.Seq != 10001 {
		t.Fatalf("appended sequence = %d, want 10001", appended.Seq)
	}
	if readCalls != readsBeforeAppend {
		t.Fatalf("append performed %d extra log reads, want 0", readCalls-readsBeforeAppend)
	}
}

func TestOpenWriterExcludesConcurrentHandles(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(root); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	first, err := OpenWriter(root, nil)
	if err != nil {
		t.Fatalf("open first writer: %v", err)
	}
	defer first.Close()
	lockInfo, err := os.Lstat(filepath.Join(root, "runtime", writerLockFilename))
	if err != nil || !lockInfo.Mode().IsRegular() {
		t.Fatalf("runtime lock = %#v err=%v, want regular file", lockInfo, err)
	}
	if _, err := OpenWriter(root, nil); err == nil {
		t.Fatal("second writer unexpectedly opened")
	} else {
		var locked *WriterLockedError
		if !errors.As(err, &locked) {
			t.Fatalf("second writer error = %v, want WriterLockedError", err)
		}
	}
	firstEvent, err := first.Append(NewEvent("first", fixtureTime(0), CancelRequestedPayload{Source: "test", Force: false}))
	if err != nil {
		t.Fatalf("append first event: %v", err)
	}
	if firstEvent.Seq != 1 {
		t.Fatalf("first sequence = %d, want 1", firstEvent.Seq)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first writer: %v", err)
	}
	second, err := OpenWriter(root, nil)
	if err != nil {
		t.Fatalf("reopen writer after close: %v", err)
	}
	secondEvent, err := second.Append(NewEvent("second", fixtureTime(1), CancelRequestedPayload{Source: "test", Force: false}))
	if err != nil {
		_ = second.Close()
		t.Fatalf("append second event: %v", err)
	}
	if secondEvent.Seq != 2 {
		_ = second.Close()
		t.Fatalf("second sequence = %d, want 2", secondEvent.Seq)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second writer: %v", err)
	}
	events, err := replayFile(filepath.Join(root, EventsFilename))
	if err != nil || len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("replay after exclusive writers: events=%#v err=%v", events, err)
	}
}

func TestOpenWriterExcludesOtherProcess(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(root); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	writer, err := OpenWriter(root, nil)
	if err != nil {
		t.Fatalf("open parent writer: %v", err)
	}
	defer writer.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestWriterLockHelperProcess$")
	child.Env = append(os.Environ(), "EVENTLOG_WRITER_LOCK_HELPER=1", "EVENTLOG_WRITER_LOCK_ROOT="+root)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-process lock helper: %v\n%s", err, output)
	}
}

func TestWriterLockHelperProcess(t *testing.T) {
	if os.Getenv("EVENTLOG_WRITER_LOCK_HELPER") != "1" {
		return
	}
	writer, err := OpenWriter(os.Getenv("EVENTLOG_WRITER_LOCK_ROOT"), nil)
	if err == nil {
		_ = writer.Close()
		t.Fatal("child writer unexpectedly opened")
	}
	var locked *WriterLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("child writer error = %v, want WriterLockedError", err)
	}
}

func TestAppendPoisonsWriterAfterAmbiguousFailure(t *testing.T) {
	tests := []struct {
		name string
		file *failingAppendFile
	}{
		{name: "partial-write", file: &failingAppendFile{writeCount: 1, writeErr: errors.New("partial write")}},
		{name: "sync", file: &failingAppendFile{syncErr: errors.New("sync failure")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer := &Writer{root: t.TempDir(), file: test.file, nextSeq: 1}
			_, err := writer.Append(NewEvent("ambiguous", fixtureTime(0), CancelRequestedPayload{Source: "test", Force: false}))
			var poisoned *WriterPoisonedError
			if !errors.As(err, &poisoned) {
				t.Fatalf("first append error = %v, want WriterPoisonedError", err)
			}
			if writer.NextSeq() != 1 {
				t.Fatalf("next sequence after ambiguous append = %d, want 1", writer.NextSeq())
			}
			writesBeforeRetry := test.file.writes
			_, err = writer.Append(NewEvent("retry", fixtureTime(1), CancelRequestedPayload{Source: "test", Force: false}))
			if !errors.As(err, &poisoned) {
				t.Fatalf("retry error = %v, want WriterPoisonedError", err)
			}
			if test.file.writes != writesBeforeRetry {
				t.Fatalf("poisoned retry wrote %d additional times", test.file.writes-writesBeforeRetry)
			}
			if writer.NextSeq() != 1 {
				t.Fatalf("next sequence after poisoned retry = %d, want 1", writer.NextSeq())
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close poisoned writer: %v", err)
			}
		})
	}
}

type failingAppendFile struct {
	writeCount int
	writeErr   error
	syncErr    error
	writes     int
}

func (f *failingAppendFile) Write(body []byte) (int, error) {
	f.writes++
	if f.writeErr != nil {
		return f.writeCount, f.writeErr
	}
	return len(body), nil
}

func (f *failingAppendFile) Sync() error  { return f.syncErr }
func (f *failingAppendFile) Close() error { return nil }

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

func TestEmbeddedUNCPathIsRejected(t *testing.T) {
	rejected := []string{
		`failed to read \\fileserver\share\input.json`,
		`\\host\share\x`,
		`copied from \\BUILDBOX01\artifacts\out.bin then verified`,
	}
	for _, value := range rejected {
		if err := ValidatePortableValue(value); err == nil {
			t.Fatalf("embedded UNC path accepted: %q", value)
		}
	}
	accepted := []string{
		`escaped backslashes \\\\ are not a path`,
		`plain prose with no path at all`,
	}
	for _, value := range accepted {
		if err := ValidatePortableValue(value); err != nil {
			t.Fatalf("false positive on %q: %v", value, err)
		}
	}
}
