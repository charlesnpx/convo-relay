// Package engine executes one immutable v2 session plan from its event log.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/plan"
	"github.com/charlesnpx/convo-relay/internal/provider"
	"github.com/charlesnpx/convo-relay/internal/session"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	statusCompleted        = "completed"
	statusFailed           = "failed"
	statusInterrupted      = "interrupted"
	statusAwaitingDecision = "awaiting_decision"

	stopCompleted        = "completed"
	stopConverged        = "converged"
	stopNoLedgerSignal   = "stalled_no_ledger_signal"
	stopProviderFailed   = "provider_failed"
	stopChildFailed      = "child_failed"
	stopAbandonedAttempt = "abandoned_attempt"
	stopInvalidResult    = "invalid_result"

	mediaTypePlainTextUTF8  = "text/plain; charset=utf-8"
	mediaTypeChildResult    = "text/plain; charset=utf-8"
	mediaTypeChildPlan      = "application/vnd.convo-relay.plan+json"
	maxPromptTranscriptSize = 12
	resultSchemaURL         = "https://convo-relay.invalid/engine-result-schema.json"
)

// BackendFactory creates a backend for one actor in one managed session.
// It receives the session so workspace policy remains available to the edge
// that constructs the provider adapter.
type BackendFactory func(*session.Session, session.Actor) (provider.Backend, error)

// ChildRequest is an already-approved typed request extracted at the caller's
// boundary. The engine deliberately does not parse provider text into a child
// protocol; callers choose that ingress explicitly.
type ChildRequest struct {
	ID      string
	Request plan.ChildRequest
}

// ChildRequestExtractor returns requests emitted by a participant or
// facilitator result. The immutable parent plan lets ingress selection respect
// the parent's keep-list. A nil extractor means turns cannot request children.
type ChildRequestExtractor func(session.Plan, session.Actor, eventlog.Role, provider.TurnResult) []ChildRequest

// Deps contains the small imperative boundary required by Run. Writer is
// optional: when absent, Run opens the session's event writer itself. Recipes
// are used only to compile an admitted child plan.
type Deps struct {
	BackendFactory        BackendFactory
	Writer                *eventlog.Writer
	Recipes               []plan.Recipe
	ChildRequestExtractor ChildRequestExtractor
}

// Outcome contains the parent-facing result and execution classification needed
// by parent-child execution. Status is terminal when the session is terminal,
// interrupted when a caller cancelled active work, or awaiting_decision while
// an ask-mode child request is pending. Diagnostics and transcript details
// remain derived through sessionview.
type Outcome struct {
	Result string
	Status string
}

// PendingChild is the operator-facing view of one durable, undecided child
// request. Question is loaded from the request's durable blob.
type PendingChild struct {
	RequestID        string
	RequesterActorID string
	RecipeID         string
	Question         string
}

// Run starts a session with no execution history and executes until the plan is
// terminal or an ask-mode child request needs an operator decision. Launch
// provisioning events may already be present; they are not execution history.
func Run(ctx context.Context, sess *session.Session, deps Deps) (Outcome, error) {
	runner, err := newRunner(ctx, sess, deps)
	if err != nil {
		return Outcome{}, err
	}
	defer runner.closeOwnedWriter()

	events, err := readEvents(sess.Root)
	if err != nil {
		return Outcome{}, err
	}
	if err := runStartGuard(events); err != nil {
		return Outcome{}, err
	}
	if err := runner.rebuildProvisioningState(events); err != nil {
		return Outcome{}, err
	}
	if err := runner.append(eventlog.SessionStartedPayload{PlanDigest: runner.planDigest, SessionID: sess.Plan.SessionID}); err != nil {
		return Outcome{}, err
	}
	return runner.execute()
}

// Resume replays a started session and continues only work left by its
// immutable plan plus an explicit turn-budget extension when requestedTurns is
// positive. A provisioning-only launch is also resumed by recording its first
// session.started event, so a crash before the first turn never re-ingests
// inputs.
func Resume(ctx context.Context, sess *session.Session, deps Deps, prompt string, requestedTurns int) (Outcome, error) {
	if requestedTurns < 0 {
		return Outcome{}, errors.New("resume extra turns must not be negative")
	}
	if err := checkResumeLifecycle(sess, prompt); err != nil {
		return Outcome{}, err
	}
	runner, err := newRunner(ctx, sess, deps)
	if err != nil {
		return Outcome{}, err
	}
	defer runner.closeOwnedWriter()

	events, err := readEvents(sess.Root)
	if err != nil {
		return Outcome{}, err
	}
	if len(events) == 0 {
		return Outcome{}, errors.New("cannot resume a session with no session.started event")
	}
	if err := runStartGuard(events); err == nil {
		if err := runner.rebuildProvisioningState(events); err != nil {
			return Outcome{}, err
		}
		if err := runner.append(eventlog.SessionStartedPayload{PlanDigest: runner.planDigest, SessionID: sess.Plan.SessionID}); err != nil {
			return Outcome{}, err
		}
		return runner.execute()
	}
	if err := runner.rebuildExecutionState(events); err != nil {
		return Outcome{}, err
	}
	if runner.state.terminal != nil && !runner.hasUnstartedGrantedWork() && requestedTurns == 0 {
		return runner.outcome(), nil
	}
	text := strings.TrimSpace(prompt)
	if requestedTurns > 0 {
		if !runner.isExactPromptedTurnBudgetRetry(requestedTurns, text) {
			if err := runner.turnBudgetGrantApplicable(requestedTurns); err != nil {
				return Outcome{}, err
			}
			var promptRef *blobstore.BlobRef
			if text != "" {
				ref, err := runner.putText(text)
				if err != nil {
					return Outcome{}, err
				}
				promptRef = &ref
			}
			if err := runner.append(eventlog.TurnBudgetGrantedPayload{GrantedBy: "operator", Turns: requestedTurns, Prompt: promptRef}); err != nil {
				return Outcome{}, err
			}
		}
	} else if text != "" {
		ref, err := runner.putText(text)
		if err != nil {
			return Outcome{}, err
		}
		if err := runner.append(eventlog.SteeringQueuedPayload{Prompt: ref}); err != nil {
			return Outcome{}, err
		}
	}
	return runner.execute()
}

// QueueSteering records a durable operator direction without granting turns.
// It is intentionally separate from Resume so a completed session can retain
// a direction for a later explicit turn-budget grant.
func QueueSteering(sess *session.Session, prompt string) error {
	text := strings.TrimSpace(prompt)
	if text == "" {
		return errors.New("steering prompt is required")
	}
	if err := checkResumeLifecycle(sess, text); err != nil {
		return err
	}
	runner, err := newStateRunner(sess)
	if err != nil {
		return err
	}
	writer, err := sess.EventWriter(runner.blobs)
	if err != nil {
		return fmt.Errorf("open event writer: %w", err)
	}
	runner.writer = writer
	runner.closeLog = true
	defer runner.closeOwnedWriter()
	events, err := readEvents(sess.Root)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return errors.New("cannot steer a session with no session.started event")
	}
	if err := runner.rebuildExecutionState(events); err != nil {
		return err
	}
	ref, err := runner.putText(text)
	if err != nil {
		return err
	}
	return runner.append(eventlog.SteeringQueuedPayload{Prompt: ref})
}

// PendingChildren returns every durable child request that has not yet been
// resolved by a child.decided event.
func PendingChildren(sess *session.Session) ([]PendingChild, error) {
	runner, err := newStateRunner(sess)
	if err != nil {
		return nil, err
	}
	events, err := readEvents(sess.Root)
	if err != nil {
		return nil, err
	}
	if err := runner.rebuildExecutionState(events); err != nil {
		return nil, err
	}
	requestIDs := make([]string, 0, len(runner.state.requests))
	for requestID, child := range runner.state.requests {
		if !child.decided() {
			requestIDs = append(requestIDs, requestID)
		}
	}
	sort.Strings(requestIDs)
	pending := make([]PendingChild, 0, len(requestIDs))
	for _, requestID := range requestIDs {
		child := runner.state.requests[requestID]
		question, err := runner.readBlob(child.Request.Question)
		if err != nil {
			return nil, fmt.Errorf("read pending child request %q: %w", requestID, err)
		}
		pending = append(pending, PendingChild{
			RequestID:        child.Request.RequestID,
			RequesterActorID: child.Request.RequesterActorID,
			RecipeID:         child.Request.RecipeID,
			Question:         question,
		})
	}
	return pending, nil
}

// ApproveChild resolves one pending ask-mode request. It compiles and stores
// the child plan at approval time, then records the ordinary child.decided
// event that makes the admission durable.
func ApproveChild(ctx context.Context, sess *session.Session, requestID string, recipes []plan.Recipe) error {
	runner, err := newAdmissionRunner(ctx, sess, recipes)
	if err != nil {
		return err
	}
	defer runner.closeOwnedWriter()
	child, err := runner.pendingChildForOperatorDecision(requestID)
	if err != nil {
		return err
	}
	return runner.admitChild(child, "admitted by operator")
}

// RejectChild resolves one pending ask-mode request without admitting it.
// An empty reason keeps the operator-default durable decision text.
func RejectChild(ctx context.Context, sess *session.Session, requestID string, reason string) error {
	runner, err := newAdmissionRunner(ctx, sess, nil)
	if err != nil {
		return err
	}
	defer runner.closeOwnedWriter()
	child, err := runner.pendingChildForOperatorDecision(requestID)
	if err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "rejected by operator"
	}
	return runner.rejectChild(child, reason, "rejected")
}

func checkResumeLifecycle(sess *session.Session, prompt string) error {
	if sess == nil {
		return errors.New("session is required")
	}
	if sess.Plan.Lifecycle == nil {
		return nil
	}
	if sess.Plan.Lifecycle.Resume == "forbid" {
		return errors.New("plan lifecycle forbids resume")
	}
	if strings.TrimSpace(prompt) != "" && sess.Plan.Lifecycle.Steering == "forbid" {
		return errors.New("plan lifecycle forbids steering")
	}
	return nil
}

type runner struct {
	ctx        context.Context
	sess       *session.Session
	deps       Deps
	blobs      *blobstore.Store
	writer     *eventlog.Writer
	closeLog   bool
	planDigest string

	backends     map[string]provider.Backend
	participants []session.Actor
	actors       map[string]session.Actor
	material     string
	state        *executionState
}

type executionPhase string

const (
	phaseParticipant executionPhase = "participant"
	phaseFacilitator executionPhase = "facilitator"
	phaseReducer     executionPhase = "reducer"
	phaseDone        executionPhase = "done"
)

// executionState is the complete execution decision state. It is populated
// solely by reduceEvent for both durable replay and each successful live append.
type executionState struct {
	sessionStarted bool
	terminal       *eventlog.SessionFinishedPayload
	phase          executionPhase

	provisionedInputs map[string]blobstore.BlobRef
	workspacePrepared bool

	active *turnState

	conversation        []conversationTurn
	ledger              model.Ledger
	lastResult          completedTurn
	grantedTurns        int
	conversationAtGrant int
	lastTurnBudgetGrant *eventlog.TurnBudgetGrantedPayload

	requests     map[string]*childState
	childResults []string
	childrenUsed int
	childTurns   int

	steering         []*steeringState
	providerSessions map[string]string
	resultValidation string
}

type turnState struct {
	ActorID  string
	Round    int
	Role     eventlog.Role
	Attempts map[int]*attemptState
}

type attemptState struct {
	Attempt int
	Outcome string
	Content blobstore.BlobRef
	Failure *providerFailureState
}

type providerFailureState struct {
	Category        string
	Retryable       bool
	SanitizedDetail string
}

type conversationTurn struct {
	ActorID string
	Text    string
}

type completedTurn struct {
	Text string
	Ref  blobstore.BlobRef
}

type childState struct {
	Request eventlog.ChildRequestedPayload
	Decided bool
	Plan    *session.Plan
	Status  string
	Result  blobstore.BlobRef
}

func (child *childState) decided() bool   { return child != nil && child.Decided }
func (child *childState) admitted() bool  { return child != nil && child.Plan != nil }
func (child *childState) completed() bool { return child != nil && child.Status != "" }

type steeringState struct {
	Ref          blobstore.BlobRef
	Text         string
	AppliedRound int
	Consumed     bool
}

type resultSchemaLoader struct{}

func (resultSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external result schema loading is disabled: %s", url)
}

type executionFailure struct {
	reason string
	cause  error
}

func (e *executionFailure) Error() string { return e.cause.Error() }
func (e *executionFailure) Unwrap() error { return e.cause }

type staticChildAwaitingError struct{}

func (*staticChildAwaitingError) Error() string { return "static child is awaiting a decision" }

func newStateRunner(sess *session.Session) (*runner, error) {
	if sess == nil {
		return nil, errors.New("session is required")
	}
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		return nil, fmt.Errorf("open blob store: %w", err)
	}
	return &runner{
		sess:       sess,
		blobs:      blobs,
		planDigest: sess.Digest,
		state:      newExecutionState(),
	}, nil
}

func newRunner(ctx context.Context, sess *session.Session, deps Deps) (*runner, error) {
	if deps.BackendFactory == nil {
		return nil, errors.New("backend factory is required")
	}
	runner, err := newStateRunner(sess)
	if err != nil {
		return nil, err
	}
	writer := deps.Writer
	closeLog := false
	if writer == nil {
		writer, err = sess.EventWriter(runner.blobs)
		if err != nil {
			return nil, fmt.Errorf("open event writer: %w", err)
		}
		closeLog = true
	}
	runner.writer = writer
	runner.closeLog = closeLog
	material, err := loadPromptMaterial(runner.blobs, sess.Plan)
	if err != nil {
		runner.closeOwnedWriter()
		return nil, err
	}
	runner.ctx = ctx
	runner.deps = deps
	runner.backends = make(map[string]provider.Backend, len(sess.Plan.Actors))
	runner.actors = make(map[string]session.Actor, len(sess.Plan.Actors))
	runner.material = material
	runner.participants = participantActors(sess.Plan)
	for _, actor := range sess.Plan.Actors {
		runner.actors[actor.ID] = actor
	}
	return runner, nil
}

func newAdmissionRunner(ctx context.Context, sess *session.Session, recipes []plan.Recipe) (*runner, error) {
	runner, err := newStateRunner(sess)
	if err != nil {
		return nil, err
	}
	writer, err := sess.EventWriter(runner.blobs)
	if err != nil {
		return nil, fmt.Errorf("open event writer: %w", err)
	}
	runner.ctx = ctx
	runner.deps.Recipes = recipes
	runner.writer = writer
	runner.closeLog = true
	events, err := readEvents(sess.Root)
	if err != nil {
		runner.closeOwnedWriter()
		return nil, err
	}
	if len(events) == 0 {
		runner.closeOwnedWriter()
		return nil, errors.New("cannot decide a child for a session with no session.started event")
	}
	if err := runner.rebuildExecutionState(events); err != nil {
		runner.closeOwnedWriter()
		return nil, err
	}
	return runner, nil
}

func newExecutionState() *executionState {
	return &executionState{
		phase:             phaseParticipant,
		ledger:            model.EmptyLedger(),
		provisionedInputs: make(map[string]blobstore.BlobRef),
		requests:          make(map[string]*childState),
		providerSessions:  make(map[string]string),
	}
}

func (r *runner) closeOwnedWriter() {
	if r != nil && r.closeLog && r.writer != nil {
		_ = r.writer.Close()
	}
}

func participantActors(value session.Plan) []session.Actor {
	controls := map[string]bool{}
	if value.Facilitator != nil {
		controls[value.Facilitator.Actor] = true
	}
	if value.Reducer != nil {
		controls[value.Reducer.Actor] = true
	}
	participants := make([]session.Actor, 0, len(value.Actors))
	for _, actor := range value.Actors {
		if !controls[actor.ID] {
			participants = append(participants, actor)
		}
	}
	return participants
}

// append makes an event durable, then updates exactly the same state reducer
// that resume uses for durable history.
func (r *runner) append(payload eventlog.Payload) error {
	if r.writer == nil {
		return errors.New("event writer is required")
	}
	identifier := fmt.Sprintf("engine-%d", r.writer.NextSeq())
	event, err := r.writer.Append(eventlog.NewEvent(identifier, time.Now(), payload))
	if err != nil {
		return err
	}
	return r.reduceEvent(event)
}

func (r *runner) putText(text string) (blobstore.BlobRef, error) {
	if r.blobs == nil {
		return blobstore.BlobRef{}, errors.New("blob store is required")
	}
	return r.blobs.PutBytes([]byte(text), mediaTypePlainTextUTF8)
}

func (r *runner) backend(actor session.Actor) (provider.Backend, error) {
	if backend, exists := r.backends[actor.ID]; exists {
		return backend, nil
	}
	backend, err := r.deps.BackendFactory(r.sess, actor)
	if err != nil {
		return nil, fmt.Errorf("create backend for %s: %w", actor.ID, err)
	}
	if backend == nil {
		return nil, fmt.Errorf("create backend for %s: nil backend", actor.ID)
	}
	if continuation := strings.TrimSpace(r.state.providerSessions[actor.ID]); continuation != "" {
		if err := backend.RestoreState(providerState(actor.Backend, continuation), provider.SlotConfig{
			Model:  actor.Model,
			Effort: actor.Effort,
		}); err != nil {
			return nil, fmt.Errorf("restore backend continuation for %s: %w", actor.ID, err)
		}
	}
	r.backends[actor.ID] = backend
	return backend, nil
}

func providerState(backend, continuation string) provider.SlotState {
	state := provider.SlotState{"started": true}
	switch backend {
	case "codex":
		state["thread_id"] = continuation
	case "gemini":
		state["session_ref"] = continuation
	default:
		state["session_id"] = continuation
	}
	return state
}

func providerSessionID(backend provider.Backend) string {
	if backend == nil {
		return ""
	}
	state := backend.SessionState()
	for _, key := range []string{"thread_id", "session_id", "session_ref"} {
		if value := strings.TrimSpace(stringValue(state[key])); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

// rebuildProvisioningState replays a launch prefix before its session.started
// event. This is the crash-recovery state between provisioning and execution.
func (r *runner) rebuildProvisioningState(events []eventlog.Event) error {
	if err := r.replayEvents(events); err != nil {
		return err
	}
	if r.state.sessionStarted {
		return errors.New("provisioning prefix has session.started event")
	}
	return nil
}

// rebuildExecutionState replays an execution history. It drives every event
// through reduceEvent, which is also called by append during a live execution.
func (r *runner) rebuildExecutionState(events []eventlog.Event) error {
	if err := r.replayEvents(events); err != nil {
		return err
	}
	if !r.state.sessionStarted {
		return errors.New("session has no session.started event")
	}
	return nil
}

func (r *runner) replayEvents(events []eventlog.Event) error {
	r.state = newExecutionState()
	for _, event := range events {
		if err := r.reduceEvent(event); err != nil {
			return err
		}
	}
	return nil
}

// runStartGuard is the one classification for the Run boundary. The two
// provisioning types form the only allowed prefix; every other durable event
// records execution and makes the session Resume-only.
func runStartGuard(events []eventlog.Event) error {
	for _, event := range events {
		switch event.Type {
		case eventlog.InputIngested, eventlog.WorkspacePrepared:
			continue
		default:
			return fmt.Errorf("session already has execution event %q; use Resume", event.Type)
		}
	}
	return nil
}

// reduceEvent is the one execution-state reducer for both the live and replay
// paths. Event replay has normalized payloads to value form before returning.
func (r *runner) reduceEvent(event eventlog.Event) error {
	state := r.state
	if state == nil {
		return errors.New("execution state is required")
	}
	switch payload := event.Payload.(type) {
	case eventlog.InputIngestedPayload:
		if state.sessionStarted {
			return errors.New("input.ingested follows session.started")
		}
		if _, exists := state.provisionedInputs[payload.LogicalName]; exists {
			return fmt.Errorf("duplicate input.ingested for %q", payload.LogicalName)
		}
		for _, input := range r.sess.Plan.Inputs {
			if input.Name != payload.LogicalName {
				continue
			}
			if !input.Content.Equal(payload.Content) {
				return fmt.Errorf("input.ingested for %q does not match immutable plan", payload.LogicalName)
			}
			state.provisionedInputs[payload.LogicalName] = payload.Content
			return nil
		}
		return fmt.Errorf("input.ingested for undeclared input %q", payload.LogicalName)
	case eventlog.WorkspacePreparedPayload:
		if state.sessionStarted {
			return errors.New("workspace.prepared follows session.started")
		}
		if state.workspacePrepared {
			return errors.New("session has more than one workspace.prepared event")
		}
		state.workspacePrepared = true
		return nil
	case eventlog.SessionStartedPayload:
		if state.sessionStarted {
			return errors.New("session has more than one session.started event")
		}
		if payload.PlanDigest != r.planDigest || payload.SessionID != r.sess.Plan.SessionID {
			return errors.New("session.started does not match immutable plan")
		}
		state.sessionStarted = true
		return nil
	}
	if !state.sessionStarted {
		return errors.New("event precedes session.started")
	}
	switch payload := event.Payload.(type) {
	case eventlog.TurnStartedPayload:
		if state.terminal != nil && !r.hasUnstartedGrantedWork() {
			return errors.New("turn.started follows session.finished")
		}
		if state.active != nil {
			return errors.New("session has more than one unfinished turn")
		}
		if state.terminal != nil {
			state.terminal = nil
			state.resultValidation = ""
		}
		turn := &turnState{
			ActorID:  payload.ActorID,
			Round:    payload.Round,
			Role:     payload.Role,
			Attempts: make(map[int]*attemptState),
		}
		state.active = turn
		return nil
	case eventlog.AttemptStartedPayload:
		turn, err := activeTurnForActor(state, payload.ActorID)
		if err != nil {
			return err
		}
		if _, exists := turn.Attempts[payload.Attempt]; exists {
			return fmt.Errorf("duplicate attempt.started for %s attempt %d", payload.ActorID, payload.Attempt)
		}
		turn.Attempts[payload.Attempt] = &attemptState{Attempt: payload.Attempt}
		return nil
	case eventlog.AttemptFinishedPayload:
		turn, err := activeTurnForActor(state, payload.ActorID)
		if err != nil {
			return err
		}
		attempt, exists := turn.Attempts[payload.Attempt]
		if !exists {
			return fmt.Errorf("attempt.finished for %s attempt %d has no attempt.started", payload.ActorID, payload.Attempt)
		}
		if attempt.Outcome != "" && (attempt.Outcome != "failed" || payload.Outcome != "failed" || attempt.Content != (blobstore.BlobRef{})) {
			return fmt.Errorf("duplicate attempt.finished for %s attempt %d", payload.ActorID, payload.Attempt)
		}
		if payload.Outcome != "success" && payload.Outcome != "failed" {
			return fmt.Errorf("attempt.finished outcome %q is unsupported", payload.Outcome)
		}
		attempt.Outcome = payload.Outcome
		attempt.Content = payload.Content
		if payload.Outcome == "success" && strings.TrimSpace(payload.ProviderSessionID) != "" {
			state.providerSessions[payload.ActorID] = payload.ProviderSessionID
		}
		return nil
	case eventlog.ProviderFailedPayload:
		turn, err := activeTurnForActor(state, payload.ActorID)
		if err != nil {
			return err
		}
		attempt, exists := turn.Attempts[payload.Attempts]
		if !exists {
			return fmt.Errorf("provider.failed for %s attempt %d has no attempt.started", payload.ActorID, payload.Attempts)
		}
		if attempt.Failure != nil {
			return fmt.Errorf("duplicate provider.failed for %s attempt %d", payload.ActorID, payload.Attempts)
		}
		// A classified provider failure is itself a durable failed outcome. This
		// lets replay decide retry policy even if the subsequent attempt.finished
		// event was not written before interruption.
		attempt.Outcome = "failed"
		attempt.Failure = &providerFailureState{
			Category: payload.Category, Retryable: payload.Retryable, SanitizedDetail: payload.SanitizedDetail,
		}
		return nil
	case eventlog.TurnFinishedPayload:
		turn, err := activeTurnForActor(state, payload.ActorID)
		if err != nil {
			return err
		}
		if turn.Round != payload.Round {
			return fmt.Errorf("turn.finished for %s/%d has no matching turn.started", payload.ActorID, payload.Round)
		}
		text, err := r.readBlob(payload.Content)
		if err != nil {
			return err
		}
		state.active = nil
		completed := completedTurn{Text: text, Ref: payload.Content}
		r.recordCompletedTurn(turn, completed)
		for _, steering := range state.steering {
			if steering.AppliedRound == turn.Round {
				steering.Consumed = true
			}
		}
		r.advancePhase(turn)
		return nil
	case eventlog.ChildRequestedPayload:
		if _, exists := state.requests[payload.RequestID]; exists {
			return fmt.Errorf("duplicate child request id %q", payload.RequestID)
		}
		state.requests[payload.RequestID] = &childState{Request: payload}
		return nil
	case eventlog.ChildDecidedPayload:
		child, exists := state.requests[payload.RequestID]
		if !exists {
			return fmt.Errorf("child.decided for unknown request %q", payload.RequestID)
		}
		if child.decided() {
			return fmt.Errorf("duplicate child.decided for request %q", payload.RequestID)
		}
		if payload.Admitted {
			if payload.Plan == nil {
				return errors.New("admitted child.decided has no plan")
			}
			childPlan, err := r.readAdmittedChildPlan(*payload.Plan)
			if err != nil {
				return fmt.Errorf("read admitted child plan for %q: %w", payload.RequestID, err)
			}
			if childPlan.SessionID != r.childSessionID(payload.RequestID) {
				return fmt.Errorf("admitted child plan for %q has unexpected session id %q", payload.RequestID, childPlan.SessionID)
			}
			child.Plan = &childPlan
			state.childrenUsed++
			state.childTurns += childPlan.Schedule.Turns
		}
		child.Decided = true
		return nil
	case eventlog.ChildCompletedPayload:
		child, exists := state.requests[payload.RequestID]
		if !exists {
			return fmt.Errorf("child.completed for unknown request %q", payload.RequestID)
		}
		if !child.admitted() {
			return fmt.Errorf("child.completed for unadmitted request %q", payload.RequestID)
		}
		if child.completed() {
			return fmt.Errorf("duplicate child.completed for request %q", payload.RequestID)
		}
		if child.Plan == nil || payload.ChildSessionID != child.Plan.SessionID {
			return fmt.Errorf("child.completed for %q does not match the admitted child plan", payload.RequestID)
		}
		text, err := r.readBlob(payload.Result)
		if err != nil {
			return err
		}
		child.Status = payload.Status
		child.Result = payload.Result
		if payload.Status == statusCompleted {
			state.childResults = append(state.childResults, text)
		}
		return nil
	case eventlog.SteeringQueuedPayload:
		text, err := r.readBlob(payload.Prompt)
		if err != nil {
			return err
		}
		state.steering = append(state.steering, &steeringState{Ref: payload.Prompt, Text: text})
		return nil
	case eventlog.SteeringAppliedPayload:
		if state.active == nil {
			return errors.New("steering.applied has no active turn")
		}
		if payload.Round != state.active.Round {
			return fmt.Errorf("steering.applied round %d does not match active turn round %d", payload.Round, state.active.Round)
		}
		for _, steering := range state.steering {
			if steering.Consumed || steering.AppliedRound != 0 || !steering.Ref.Equal(payload.Prompt) {
				continue
			}
			steering.AppliedRound = payload.Round
			return nil
		}
		return errors.New("steering.applied has no queued prompt")
	case eventlog.TurnBudgetGrantedPayload:
		if err := r.turnBudgetGrantApplicable(payload.Turns); err != nil {
			return fmt.Errorf("turn_budget.granted is inapplicable: %w", err)
		}
		var steering *steeringState
		if payload.Prompt != nil {
			text, err := r.readBlob(*payload.Prompt)
			if err != nil {
				return fmt.Errorf("read turn_budget.granted prompt: %w", err)
			}
			steering = &steeringState{Ref: *payload.Prompt, Text: text}
		}
		state.grantedTurns += payload.Turns
		state.conversationAtGrant = len(state.conversation)
		grant := payload
		state.lastTurnBudgetGrant = &grant
		if steering != nil {
			state.steering = append(state.steering, steering)
		}
		return nil
	case eventlog.ResultProducedPayload:
		if state.resultValidation != "" {
			return errors.New("session has more than one result.produced event")
		}
		state.resultValidation = payload.ValidationOutcome
		return nil
	case eventlog.SessionFinishedPayload:
		if state.terminal != nil {
			return errors.New("session has more than one session.finished event")
		}
		finished := payload
		state.terminal = &finished
		return nil
	default:
		return nil
	}
}

func activeTurnForActor(state *executionState, actorID string) (*turnState, error) {
	if state.active == nil {
		return nil, fmt.Errorf("event for %s has no unfinished turn", actorID)
	}
	if state.active.ActorID != actorID {
		return nil, fmt.Errorf("event for %s does not match unfinished turn for %s", actorID, state.active.ActorID)
	}
	return state.active, nil
}

func (r *runner) recordCompletedTurn(turn *turnState, completed completedTurn) {
	switch turn.Role {
	case eventlog.ParticipantRole:
		r.state.conversation = append(r.state.conversation, conversationTurn{ActorID: turn.ActorID, Text: completed.Text})
		r.state.lastResult = completed
	case eventlog.FacilitatorRole:
		r.state.ledger = parseLedger(completed.Text, r.state.ledger)
	case eventlog.ReducerRole:
		if r.sess.Plan.Result.Source == "reducer" {
			r.state.lastResult = completed
		}
	}
}

func (r *runner) advancePhase(turn *turnState) {
	switch turn.Role {
	case eventlog.ParticipantRole:
		if r.sess.Plan.Schedule.Kind == "dialogue" && r.sess.Plan.Facilitator != nil &&
			len(r.state.conversation)%r.sess.Plan.Facilitator.Cadence == 0 {
			r.state.phase = phaseFacilitator
			return
		}
		r.phaseAfterParticipants()
	case eventlog.FacilitatorRole:
		r.phaseAfterParticipants()
	case eventlog.ReducerRole:
		r.state.phase = phaseDone
	}
}

func (r *runner) phaseAfterParticipants() {
	if len(r.state.conversation) < r.effectiveTurnBudget() {
		r.state.phase = phaseParticipant
		return
	}
	if r.sess.Plan.Reducer != nil {
		r.state.phase = phaseReducer
		return
	}
	r.state.phase = phaseDone
}

func (r *runner) effectiveTurnBudget() int {
	return r.sess.Plan.Schedule.Turns + r.state.grantedTurns
}

// isExactPromptedTurnBudgetRetry recognizes a retry of the one durable event
// that already carries both the unused grant and its steering. It does not
// admit another grant while that work remains unstarted.
func (r *runner) isExactPromptedTurnBudgetRetry(turns int, prompt string) bool {
	grant := r.state.lastTurnBudgetGrant
	if prompt == "" || !r.hasUnstartedGrantedWork() || grant == nil ||
		grant.GrantedBy != "operator" || grant.Turns != turns || grant.Prompt == nil {
		return false
	}
	for _, steering := range r.state.steering {
		if steering.Consumed || steering.AppliedRound != 0 || !steering.Ref.Equal(*grant.Prompt) {
			continue
		}
		return steering.Text == prompt
	}
	return false
}

// turnBudgetGrantApplicable reports whether the current replayed execution
// state can consume a turn-budget grant without reopening incompatible work.
func (r *runner) turnBudgetGrantApplicable(turns int) error {
	if r.turnBudgetGrantExceedsIntegerRange(turns) {
		return errors.New("turn budget grants exceed integer range")
	}
	if r.state.terminal != nil && r.state.terminal.Status != statusCompleted {
		return fmt.Errorf("cannot grant turns to a %s session; retry or fork it", r.state.terminal.Status)
	}
	if r.hasScheduledWorkOutstanding() {
		return errors.New("cannot grant turns while scheduled work is outstanding")
	}
	if r.state.terminal == nil {
		return errors.New("turn budget grants require a completed terminal session")
	}
	return nil
}

func (r *runner) turnBudgetGrantExceedsIntegerRange(turns int) bool {
	return turns > maximumInt()-r.sess.Plan.Schedule.Turns-r.state.grantedTurns
}

func maximumInt() int { return int(^uint(0) >> 1) }

// hasScheduledWorkOutstanding answers whether replay already left work due.
// A terminal makes future participant turns non-due, but it cannot suppress a
// facilitator or reducer phase that was already selected. Once a grant makes
// a completed terminal historical, the effective budget determines whether a
// participant turn is newly due.
func (r *runner) hasScheduledWorkOutstanding() bool {
	return r.state.active != nil || r.hasUnstartedGrantedWork() ||
		r.state.phase == phaseFacilitator || r.state.phase == phaseReducer
}

func (r *runner) hasUnstartedGrantedWork() bool {
	return r.state.terminal != nil && r.state.terminal.Status == statusCompleted &&
		r.state.grantedTurns > 0 && len(r.state.conversation) == r.state.conversationAtGrant
}

func (r *runner) execute() (Outcome, error) {
	for {
		if err := r.ctx.Err(); err != nil {
			return r.finishInterrupted(err)
		}
		if r.state.terminal != nil && !r.hasUnstartedGrantedWork() {
			return r.outcome(), nil
		}
		if r.state.active != nil {
			if err := r.serviceActiveTurn(); err != nil {
				if cancelErr := r.ctx.Err(); cancelErr != nil {
					return r.finishInterrupted(cancelErr)
				}
				var awaiting *staticChildAwaitingError
				if errors.As(err, &awaiting) {
					outcome := r.outcome()
					outcome.Status = statusAwaitingDecision
					return outcome, nil
				}
				reason := stopProviderFailed
				var failure *executionFailure
				if errors.As(err, &failure) {
					reason = failure.reason
				}
				return r.finishFailure(reason, err)
			}
			continue
		}
		awaitingChild, err := r.servicePendingChildren()
		if err != nil {
			if cancelErr := r.ctx.Err(); cancelErr != nil {
				return r.finishInterrupted(cancelErr)
			}
			reason := stopProviderFailed
			var failure *executionFailure
			if errors.As(err, &failure) {
				reason = failure.reason
			}
			return r.finishFailure(reason, err)
		}
		if awaitingChild {
			outcome := r.outcome()
			outcome.Status = statusAwaitingDecision
			return outcome, nil
		}
		reason := r.dialogueStopReason()
		if reason != "" {
			return r.finishSuccess(reason)
		}
		if r.state.phase == phaseDone && !r.hasUnstartedGrantedWork() {
			return r.finishSuccess(stopCompleted)
		}
		next, err := r.nextTurn()
		if err != nil {
			if cancelErr := r.ctx.Err(); cancelErr != nil {
				return r.finishInterrupted(cancelErr)
			}
			return r.finishFailure(stopProviderFailed, err)
		}
		if err := r.ctx.Err(); err != nil {
			return r.finishInterrupted(err)
		}
		if err := r.append(eventlog.TurnStartedPayload{ActorID: next.Actor.ID, Round: next.Round, Role: next.Role}); err != nil {
			return Outcome{}, err
		}
	}
}

type turnSpec struct {
	Actor session.Actor
	Round int
	Role  eventlog.Role
}

func (r *runner) nextTurn() (turnSpec, error) {
	phase := r.state.phase
	if phase == phaseDone && r.hasUnstartedGrantedWork() {
		phase = phaseParticipant
	}
	switch phase {
	case phaseParticipant:
		if len(r.state.conversation) >= r.effectiveTurnBudget() {
			return turnSpec{}, errors.New("participant phase has no remaining turn")
		}
		round := len(r.state.conversation) + 1
		if r.sess.Plan.Schedule.Kind == "dialogue" {
			if len(r.participants) == 0 {
				return turnSpec{}, errors.New("dialogue schedule has no participant actors")
			}
			return turnSpec{
				Actor: r.participants[len(r.state.conversation)%len(r.participants)],
				Round: round,
				Role:  eventlog.ParticipantRole,
			}, nil
		}
		actor, err := r.actor(r.sess.Plan.Schedule.Order[len(r.state.conversation)%len(r.sess.Plan.Schedule.Order)])
		if err != nil {
			return turnSpec{}, err
		}
		return turnSpec{Actor: actor, Round: round, Role: eventlog.ParticipantRole}, nil
	case phaseFacilitator:
		if r.sess.Plan.Facilitator == nil {
			return turnSpec{}, errors.New("facilitator phase has no facilitator")
		}
		actor, err := r.actor(r.sess.Plan.Facilitator.Actor)
		if err != nil {
			return turnSpec{}, err
		}
		return turnSpec{Actor: actor, Round: len(r.state.conversation), Role: eventlog.FacilitatorRole}, nil
	case phaseReducer:
		if r.sess.Plan.Reducer == nil {
			return turnSpec{}, errors.New("reducer phase has no reducer")
		}
		actor, err := r.actor(r.sess.Plan.Reducer.Actor)
		if err != nil {
			return turnSpec{}, err
		}
		return turnSpec{Actor: actor, Round: r.effectiveTurnBudget() + 1, Role: eventlog.ReducerRole}, nil
	default:
		return turnSpec{}, errors.New("execution has no next turn")
	}
}

func (r *runner) dialogueStopReason() string {
	if r.sess.Plan.Schedule.Kind != "dialogue" || !r.sess.Plan.Schedule.StopOnConvergence ||
		r.state.active != nil || r.state.phase == phaseFacilitator {
		return ""
	}
	reason := ""
	if hasConverged(r.state.conversation, r.state.ledger) {
		reason = stopConverged
	}
	if reason == "" && hasNoLedgerSignal(r.state.conversation, r.state.ledger) {
		reason = stopNoLedgerSignal
	}
	if reason == "" || r.state.grantedTurns == 0 {
		return reason
	}
	if len(r.state.conversation) <= r.state.conversationAtGrant {
		return ""
	}
	return reason
}

func (r *runner) serviceActiveTurn() error {
	turn := r.state.active
	if turn == nil {
		return nil
	}
	actor, err := r.actor(turn.ActorID)
	if err != nil {
		return err
	}
	if actor.Backend == "child" {
		return r.serviceStaticChildTurn(turn, actor)
	}
	if success := turn.latest("success"); success != nil {
		content, err := r.readBlob(success.Content)
		if err != nil {
			return err
		}
		if err := r.persistChildRequests(turn, actor, provider.TurnResult{Content: content}); err != nil {
			return err
		}
		return r.finishActiveTurn(turn, success)
	}
	if failed := turn.latest("failed"); failed != nil {
		if failed.Failure != nil && r.shouldRetry(failed.Failure, failed.Attempt) {
			return r.callActiveAttempt(turn, len(turn.Attempts)+1)
		}
		return &executionFailure{
			reason: stopProviderFailed,
			cause:  recordedFailure(turn, failed),
		}
	}
	if abandoned := turn.latest(""); abandoned != nil {
		if !r.retryAbandoned(abandoned.Attempt) {
			return &executionFailure{
				reason: stopAbandonedAttempt,
				cause:  fmt.Errorf("abandoned provider attempt %d for %s cannot be retried by plan policy", abandoned.Attempt, turn.ActorID),
			}
		}
		return r.callActiveAttempt(turn, len(turn.Attempts)+1)
	}
	return r.callActiveAttempt(turn, len(turn.Attempts)+1)
}

func (turn *turnState) latest(outcome string) *attemptState {
	var selected *attemptState
	for _, attempt := range turn.Attempts {
		if attempt.Outcome == outcome && (selected == nil || attempt.Attempt > selected.Attempt) {
			selected = attempt
		}
	}
	return selected
}

func recordedFailure(turn *turnState, attempt *attemptState) error {
	if attempt.Failure == nil {
		return fmt.Errorf("recorded failed provider attempt %d for %s has no retryability record", attempt.Attempt, turn.ActorID)
	}
	return fmt.Errorf(
		"recorded provider failure for %s: %s (%s)",
		turn.ActorID,
		attempt.Failure.SanitizedDetail,
		attempt.Failure.Category,
	)
}

func (r *runner) retryAbandoned(attempt int) bool {
	return r.sess.Plan.ProviderRetry.Mode == "allow" && attempt < r.sess.Plan.ProviderRetry.MaxAttempts
}

func (r *runner) shouldRetry(failure *providerFailureState, attempt int) bool {
	return failure != nil &&
		r.sess.Plan.ProviderRetry.Mode == "allow" &&
		failure.Category != "auth" &&
		failure.Retryable &&
		attempt < r.sess.Plan.ProviderRetry.MaxAttempts
}

func (r *runner) callActiveAttempt(turn *turnState, attemptNumber int) error {
	actor, err := r.actor(turn.ActorID)
	if err != nil {
		return err
	}
	resumePrompt, err := r.applySteering(turn.Round)
	if err != nil {
		return err
	}
	backend, err := r.backend(actor)
	if err != nil {
		return err
	}
	if err := r.append(eventlog.AttemptStartedPayload{ActorID: actor.ID, Attempt: attemptNumber}); err != nil {
		return err
	}
	result, callErr := backend.RunTurn(r.ctx, r.promptFor(actor, turn.Round, turn.Role, resumePrompt), provider.TurnOptions{
		TimeoutSeconds:      r.sess.Plan.Timeouts.TurnSeconds,
		StallTimeoutSeconds: r.sess.Plan.Timeouts.StallSeconds,
	})
	if err := r.ctx.Err(); err != nil {
		return err
	}
	ref, putErr := r.putText(result.Content)
	if putErr != nil {
		return putErr
	}
	if callErr == nil {
		if err := r.append(eventlog.AttemptFinishedPayload{
			ActorID:           actor.ID,
			Attempt:           attemptNumber,
			Outcome:           "success",
			ProviderSessionID: providerSessionID(backend),
			Content:           ref,
		}); err != nil {
			return err
		}
		// A child request is derived from a successful turn, so record the
		// successful attempt first. This keeps every durable request prefix
		// resumable without treating its producing attempt as abandoned.
		if err := r.persistChildRequests(turn, actor, result); err != nil {
			return err
		}
		return r.finishActiveTurn(r.state.active, r.state.active.Attempts[attemptNumber])
	}
	failure := provider.NewProviderFailure("turn", actor.ID, actor.Backend, callErr, provider.ProviderResultForTurn(actor.Backend, result))
	if err := r.append(eventlog.ProviderFailedPayload{
		ActorID:         actor.ID,
		Backend:         actor.Backend,
		Category:        failure.Category,
		Retryable:       failure.Retryable,
		Attempts:        attemptNumber,
		RemediationCode: failure.RemediationCode,
		SanitizedDetail: failure.SanitizedDetail,
	}); err != nil {
		return err
	}
	return r.append(eventlog.AttemptFinishedPayload{
		ActorID: actor.ID,
		Attempt: attemptNumber,
		Outcome: "failed",
		Content: ref,
	})
}

func (r *runner) finishActiveTurn(turn *turnState, attempt *attemptState) error {
	if turn == nil || attempt == nil || attempt.Outcome != "success" {
		return errors.New("successful attempt is required to finish a turn")
	}
	return r.append(eventlog.TurnFinishedPayload{
		ActorID: turn.ActorID,
		Round:   turn.Round,
		Content: attempt.Content,
	})
}

func (r *runner) applySteering(round int) (string, error) {
	steering := r.applicableSteering(round)
	if steering == nil {
		return "", nil
	}
	if steering.AppliedRound == 0 {
		if err := r.append(eventlog.SteeringAppliedPayload{Prompt: steering.Ref, Round: round}); err != nil {
			return "", err
		}
	}
	return steering.Text, nil
}

func (r *runner) applicableSteering(round int) *steeringState {
	for _, steering := range r.state.steering {
		if steering.Consumed {
			continue
		}
		if steering.AppliedRound == 0 || steering.AppliedRound == round {
			return steering
		}
	}
	return nil
}

func (r *runner) persistChildRequests(turn *turnState, actor session.Actor, result provider.TurnResult) error {
	if r.deps.ChildRequestExtractor == nil || (turn.Role != eventlog.ParticipantRole && turn.Role != eventlog.FacilitatorRole) {
		return nil
	}
	for index, child := range r.deps.ChildRequestExtractor(r.sess.Plan, actor, turn.Role, result) {
		requestID := strings.TrimSpace(child.ID)
		if requestID == "" {
			requestID = fmt.Sprintf("%s-child-%d-%d", actor.ID, turn.Round, index+1)
		}
		if _, exists := r.state.requests[requestID]; exists {
			continue
		}
		recipeID := strings.TrimSpace(child.Request.RecipeID)
		if recipeID == "" {
			recipeID = "unspecified"
		}
		question, err := r.putText(child.Request.Question)
		if err != nil {
			return err
		}
		if err := r.append(eventlog.ChildRequestedPayload{
			RequestID:        requestID,
			RequesterActorID: actor.ID,
			RecipeID:         recipeID,
			Question:         question,
		}); err != nil {
			return err
		}
	}
	return nil
}

func staticChildRequestID(actorID string, round int) string {
	return fmt.Sprintf("static-%s-%d", actorID, round)
}

func (r *runner) staticChildForTurn(turn *turnState, actor session.Actor) (*childState, error) {
	if turn == nil || turn.Role != eventlog.ParticipantRole {
		return nil, errors.New("static child step must run as a participant turn")
	}
	requestID := staticChildRequestID(actor.ID, turn.Round)
	if child, found := r.state.requests[requestID]; found {
		if child.Request.RequesterActorID != actor.ID || child.Request.RecipeID != actor.ChildRecipeID {
			return nil, fmt.Errorf("static child request %q does not match actor %q", requestID, actor.ID)
		}
		return child, nil
	}
	resumePrompt, err := r.applySteering(turn.Round)
	if err != nil {
		return nil, err
	}
	question, err := r.putText(r.promptFor(actor, turn.Round, turn.Role, resumePrompt))
	if err != nil {
		return nil, err
	}
	if err := r.append(eventlog.ChildRequestedPayload{
		RequestID:        requestID,
		RequesterActorID: actor.ID,
		RecipeID:         actor.ChildRecipeID,
		Question:         question,
	}); err != nil {
		return nil, err
	}
	child, found := r.state.requests[requestID]
	if !found {
		return nil, errors.New("static child request was not reduced")
	}
	return child, nil
}

func (r *runner) serviceStaticChildTurn(turn *turnState, actor session.Actor) error {
	child, err := r.staticChildForTurn(turn, actor)
	if err != nil {
		return err
	}
	if !child.decided() {
		if err := r.admitStaticChild(actor, child); err != nil {
			return err
		}
	}
	if !child.admitted() {
		return errors.New("static child step was not admitted")
	}
	if !child.completed() {
		awaiting, err := r.runChild(child)
		if err != nil {
			return err
		}
		if awaiting {
			return &staticChildAwaitingError{}
		}
	}
	if child.Status == statusFailed {
		return &executionFailure{
			reason: stopChildFailed,
			cause:  fmt.Errorf("static child step %q finished failed", child.Request.RequestID),
		}
	}
	if child.Status != statusCompleted || child.Result == (blobstore.BlobRef{}) {
		return errors.New("static child step did not produce a completed result")
	}
	return r.append(eventlog.TurnFinishedPayload{
		ActorID: turn.ActorID,
		Round:   turn.Round,
		Content: child.Result,
	})
}

func (r *runner) admitStaticChild(actor session.Actor, child *childState) error {
	childPlan, err := r.staticChildPlanFor(actor, child)
	if err != nil {
		return fmt.Errorf("compile static child step %q: %w", actor.ID, err)
	}
	planRef, err := r.persistAdmittedChildPlan(childPlan)
	if err != nil {
		return err
	}
	return r.append(eventlog.ChildDecidedPayload{
		RequestID:   child.Request.RequestID,
		Admitted:    true,
		Reason:      "admitted by static child step",
		BudgetState: "available",
		Plan:        &planRef,
	})
}

func (r *runner) staticChildPlanFor(actor session.Actor, child *childState) (session.Plan, error) {
	question, err := r.readBlob(child.Request.Question)
	if err != nil {
		return session.Plan{}, err
	}
	return plan.ForStaticChild(r.sess.Plan, plan.ChildRequest{
		SessionID: r.childSessionID(child.Request.RequestID),
		RecipeID:  actor.ChildRecipeID,
		Question:  question,
		Turns:     actor.ChildTurns,
	}, r.deps.Recipes)
}

func (r *runner) servicePendingChildren() (bool, error) {
	requestIDs := make([]string, 0, len(r.state.requests))
	for requestID := range r.state.requests {
		requestIDs = append(requestIDs, requestID)
	}
	sort.Strings(requestIDs)
	awaitingDecision := false
	for _, requestID := range requestIDs {
		child := r.state.requests[requestID]
		if !child.decided() {
			if err := r.decideChild(child); err != nil {
				return false, err
			}
			if !child.decided() {
				awaitingDecision = true
			}
		}
		if child.admitted() && !child.completed() {
			childAwaitingDecision, err := r.runChild(child)
			if err != nil {
				return false, err
			}
			if childAwaitingDecision {
				return true, nil
			}
		}
		if child.completed() && child.Status == statusFailed {
			return false, &executionFailure{
				reason: stopChildFailed,
				cause:  fmt.Errorf("child request %q finished failed", child.Request.RequestID),
			}
		}
	}
	return awaitingDecision, nil
}

func (r *runner) decideChild(child *childState) error {
	switch r.sess.Plan.ChildPolicy.Mode {
	case "ask":
		return nil
	case "allow":
		return r.admitChild(child, "admitted by child policy")
	default:
		return r.rejectChild(child, "child policy denies child plans", "not_admitted")
	}
}

func (r *runner) admitChild(child *childState, reason string) error {
	if r.state.childrenUsed >= r.sess.Plan.ChildPolicy.MaxChildren {
		return r.rejectChild(child, "child capacity exhausted", "children_exhausted")
	}
	childPlan, err := r.childPlanFor(child)
	if err != nil {
		return r.rejectChild(child, provider.SanitizeProviderFailureDetail(err.Error()), "rejected")
	}
	if r.state.childTurns+childPlan.Schedule.Turns > r.sess.Plan.ChildPolicy.MaxTurns {
		return r.rejectChild(child, "child turn budget exhausted", "turns_exhausted")
	}
	planRef, err := r.persistAdmittedChildPlan(childPlan)
	if err != nil {
		return err
	}
	return r.append(eventlog.ChildDecidedPayload{
		RequestID:   child.Request.RequestID,
		Admitted:    true,
		Reason:      reason,
		BudgetState: "available",
		Plan:        &planRef,
	})
}

func (r *runner) rejectChild(child *childState, reason, budgetState string) error {
	return r.append(eventlog.ChildDecidedPayload{
		RequestID:   child.Request.RequestID,
		Admitted:    false,
		Reason:      reason,
		BudgetState: budgetState,
	})
}

func (r *runner) pendingChildForOperatorDecision(requestID string) (*childState, error) {
	if r.state.terminal != nil {
		return nil, errors.New("cannot decide a child for a terminal session")
	}
	if r.sess.Plan.ChildPolicy.Mode != "ask" {
		return nil, fmt.Errorf("child policy mode %q does not accept operator decisions", r.sess.Plan.ChildPolicy.Mode)
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, errors.New("child request id is required")
	}
	child, exists := r.state.requests[requestID]
	if !exists {
		return nil, fmt.Errorf("child request %q was not found", requestID)
	}
	if child.decided() {
		decision := "rejected"
		if child.admitted() {
			decision = "admitted"
		}
		return nil, fmt.Errorf("child request %q already has an existing %s decision", requestID, decision)
	}
	return child, nil
}

func (r *runner) childPlanFor(child *childState) (session.Plan, error) {
	question, err := r.readBlob(child.Request.Question)
	if err != nil {
		return session.Plan{}, err
	}
	return plan.ForChild(r.sess.Plan, plan.ChildRequest{
		SessionID: r.childSessionID(child.Request.RequestID),
		RecipeID:  child.Request.RecipeID,
		Question:  question,
	}, r.deps.Recipes)
}

func (r *runner) persistAdmittedChildPlan(childPlan session.Plan) (blobstore.BlobRef, error) {
	body, err := session.CanonicalBytes(childPlan)
	if err != nil {
		return blobstore.BlobRef{}, fmt.Errorf("canonicalize admitted child plan: %w", err)
	}
	ref, err := r.blobs.PutBytes(body, mediaTypeChildPlan)
	if err != nil {
		return blobstore.BlobRef{}, fmt.Errorf("store admitted child plan: %w", err)
	}
	return ref, nil
}

func (r *runner) readAdmittedChildPlan(ref blobstore.BlobRef) (session.Plan, error) {
	reader, err := r.blobs.Open(ref)
	if err != nil {
		return session.Plan{}, err
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return session.Plan{}, readErr
	}
	if closeErr != nil {
		return session.Plan{}, closeErr
	}
	var childPlan session.Plan
	if err := eventlog.DecodeCanonicalJSON(body, &childPlan); err != nil {
		return session.Plan{}, err
	}
	if err := session.ValidatePlan(childPlan); err != nil {
		return session.Plan{}, err
	}
	return childPlan, nil
}

func (r *runner) childSessionID(requestID string) string {
	return r.sess.Plan.SessionID + "-child-" + requestID
}

func (r *runner) runChild(child *childState) (bool, error) {
	childSession, err := r.openOrCreateChildSession(child)
	if err != nil {
		return false, err
	}
	childDeps := r.deps
	childDeps.Writer = nil
	childEvents, err := readEvents(childSession.Root)
	if err != nil {
		return false, err
	}
	var childOutcome Outcome
	var childErr error
	if len(childEvents) == 0 {
		childBlobs, err := childSession.BlobStore(blobstore.Limits{})
		if err != nil {
			return false, err
		}
		if err := copyPlanBlobs(r.blobs, childBlobs, session.BlobRefs(childSession.Plan)); err != nil {
			return false, err
		}
		childOutcome, childErr = Run(r.ctx, childSession, childDeps)
	} else {
		childOutcome, childErr = Resume(r.ctx, childSession, childDeps, "", 0)
	}
	if err := r.ctx.Err(); err != nil {
		return false, err
	}
	childStatus := childOutcome.Status
	if childErr != nil {
		childStatus = statusFailed
	}
	switch childStatus {
	case statusCompleted:
	case statusFailed:
		if childErr == nil {
			childErr = fmt.Errorf("child session %q finished failed", childSession.Plan.SessionID)
		}
	case statusAwaitingDecision:
		return true, nil
	default:
		childStatus = statusFailed
		childErr = fmt.Errorf("child session %q did not report a terminal status", childSession.Plan.SessionID)
	}
	resultText := childOutcome.Result
	if childStatus == statusFailed {
		resultText = "child execution failed: " + provider.SanitizeProviderFailureDetail(childErr.Error())
	}
	resultRef, err := r.blobs.PutBytes([]byte(resultText), mediaTypeChildResult)
	if err != nil {
		return false, err
	}
	if err := r.append(eventlog.ChildCompletedPayload{
		RequestID:      child.Request.RequestID,
		ChildSessionID: childSession.Plan.SessionID,
		Result:         resultRef,
		Status:         childStatus,
	}); err != nil {
		return false, err
	}
	return false, childErr
}

// openOrCreateChildSession reuses only a child root whose immutable plan binds
// to the durable child.decided snapshot. A missing root is created from that
// snapshot, never by recompiling the current recipe catalog.
func (r *runner) openOrCreateChildSession(child *childState) (*session.Session, error) {
	if !child.admitted() {
		return nil, errors.New("admitted child plan is required")
	}
	admittedPlan := *child.Plan
	admittedDigest, err := session.PlanDigest(admittedPlan)
	if err != nil {
		return nil, fmt.Errorf("digest admitted child plan: %w", err)
	}
	childSessionID := r.childSessionID(child.Request.RequestID)
	home := filepath.Dir(r.sess.Root)
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, fmt.Errorf("read child session home: %w", err)
	}
	var found *session.Session
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate, openErr := session.Open(filepath.Join(home, entry.Name()))
		if errors.Is(openErr, os.ErrNotExist) {
			continue
		}
		if openErr != nil {
			return nil, fmt.Errorf("open child session candidate %q: %w", entry.Name(), openErr)
		}
		if candidate.Plan.SessionID != childSessionID {
			continue
		}
		if candidate.Digest != admittedDigest {
			return nil, fmt.Errorf("child session %q does not match the durable admitted plan", childSessionID)
		}
		if found != nil {
			return nil, fmt.Errorf("multiple child sessions match admitted plan %q", childSessionID)
		}
		found = candidate
	}
	if found != nil {
		return found, nil
	}
	return session.Create(home, admittedPlan)
}

func copyPlanBlobs(source *blobstore.Store, destination *blobstore.Store, refs []blobstore.BlobRef) error {
	for _, ref := range refs {
		reader, err := source.Open(ref)
		if err != nil {
			return fmt.Errorf("open child plan blob %s: %w", ref.SHA256, err)
		}
		copied, putErr := destination.PutMediaType(reader, ref.MediaType)
		closeErr := reader.Close()
		if putErr != nil {
			return fmt.Errorf("copy child plan blob %s: %w", ref.SHA256, putErr)
		}
		if closeErr != nil {
			return fmt.Errorf("verify child plan blob %s: %w", ref.SHA256, closeErr)
		}
		if !copied.Equal(ref) {
			return fmt.Errorf("copied child plan blob %s does not retain its reference", ref.SHA256)
		}
	}
	return nil
}

func (r *runner) finishSuccess(reason string) (Outcome, error) {
	if r.state.resultValidation == "" {
		outcome := "valid"
		validationErr := r.validateSelectedResult()
		if validationErr != nil {
			outcome = "invalid"
		}
		if err := r.append(eventlog.ResultProducedPayload{
			Result:            r.state.lastResult.Ref,
			Format:            r.sess.Plan.Result.Format,
			ValidationOutcome: outcome,
		}); err != nil {
			return Outcome{}, err
		}
		if validationErr != nil {
			return r.finishFailure(stopInvalidResult, validationErr)
		}
	}
	if r.state.resultValidation != "valid" {
		return r.finishFailure(stopInvalidResult, errors.New("selected result failed declared format or schema validation"))
	}
	if err := r.append(eventlog.SessionFinishedPayload{Status: statusCompleted, StopReason: reason}); err != nil {
		return Outcome{}, err
	}
	return r.outcome(), nil
}

func (r *runner) finishFailure(reason string, cause error) (Outcome, error) {
	if r.state.terminal == nil {
		appendErr := r.append(eventlog.SessionFinishedPayload{Status: statusFailed, StopReason: reason})
		if appendErr != nil {
			return r.outcome(), errors.Join(cause, appendErr)
		}
	}
	if cause == nil {
		cause = errors.New(reason)
	}
	return r.outcome(), cause
}

// finishInterrupted intentionally appends no terminal event. The outstanding
// attempt remains an abandoned durable prefix, which Resume already retries
// according to the plan's provider retry policy.
func (r *runner) finishInterrupted(cause error) (Outcome, error) {
	if cause == nil {
		cause = context.Canceled
	}
	return Outcome{Result: r.state.lastResult.Text, Status: statusInterrupted}, cause
}

func (r *runner) outcome() Outcome {
	status := ""
	if r.state.terminal != nil {
		status = r.state.terminal.Status
	}
	return Outcome{Result: r.state.lastResult.Text, Status: status}
}

func (r *runner) validateSelectedResult() error {
	if r.state.lastResult.Ref == (blobstore.BlobRef{}) {
		return errors.New("selected result has no durable content")
	}
	if strings.TrimSpace(r.state.lastResult.Text) == "" {
		return errors.New("selected result is blank")
	}
	format := strings.ToLower(strings.TrimSpace(r.sess.Plan.Result.Format))
	if format != "text" && format != "json" {
		return fmt.Errorf("unsupported result format %q", r.sess.Plan.Result.Format)
	}
	if format == "text" && len(r.sess.Plan.Result.Schema) == 0 {
		return nil
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(r.state.lastResult.Text))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("result is not valid JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}
	if len(r.sess.Plan.Result.Schema) == 0 {
		return nil
	}
	var schema any
	if err := json.Unmarshal(r.sess.Plan.Result.Schema, &schema); err != nil {
		return fmt.Errorf("decode declared result schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(resultSchemaLoader{})
	if err := compiler.AddResource(resultSchemaURL, schema); err != nil {
		return fmt.Errorf("register declared result schema: %w", err)
	}
	compiled, err := compiler.Compile(resultSchemaURL)
	if err != nil {
		return fmt.Errorf("compile declared result schema: %w", err)
	}
	if err := compiled.Validate(value); err != nil {
		return fmt.Errorf("result does not match declared schema: %w", err)
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing result JSON: %w", err)
	}
	return errors.New("result JSON has trailing value")
}

func (r *runner) actor(identifier string) (session.Actor, error) {
	actor, exists := r.actors[identifier]
	if !exists {
		return session.Actor{}, fmt.Errorf("plan names unknown actor %q", identifier)
	}
	return actor, nil
}

func (r *runner) promptFor(actor session.Actor, round int, role eventlog.Role, resumePrompt string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Task: %s\n", r.sess.Plan.Task)
	fmt.Fprintf(&builder, "Mode: %s\n", r.sess.Plan.Mode)
	fmt.Fprintf(&builder, "Investigation: %s\n", r.sess.Plan.Investigation)
	fmt.Fprintf(&builder, "Actor: %s\nRole: %s\nTurn: %d\n", actor.ID, role, round)
	if prompt := strings.TrimSpace(resumePrompt); prompt != "" {
		fmt.Fprintf(&builder, "Resume direction: %s\n", prompt)
	}
	if role == eventlog.ParticipantRole {
		if instructions := r.integrationTurnInstructions(actor.ID, round); instructions != "" {
			fmt.Fprintf(&builder, "\n--- Integration Contract Instructions for This Turn ---\n%s\n", instructions)
		}
	}
	if material := r.promptMaterial(); material != "" {
		builder.WriteString("\nInputs:\n")
		builder.WriteString(material)
	}
	if r.sess.Plan.Investigation != session.InvestigationContextOnly {
		if transcript := r.transcriptText(); transcript != "" {
			builder.WriteString("\nConversation:\n")
			builder.WriteString(transcript)
		}
	}
	if len(r.state.childResults) > 0 {
		builder.WriteString("\nChild results:\n")
		for _, result := range r.state.childResults {
			builder.WriteString(result)
			builder.WriteByte('\n')
		}
	}
	if role == eventlog.FacilitatorRole {
		counts := r.state.ledger.Counts()
		fmt.Fprintf(&builder, "\nReturn a JSON ledger with settled, contested, and withdrawn arrays. Current counts: settled=%d contested=%d withdrawn=%d.\n", counts.Settled, counts.Contested, counts.Withdrawn)
	}
	if role == eventlog.ReducerRole {
		if r.sess.Plan.IntegrationInstructions != nil {
			instructions := strings.TrimSpace(r.sess.Plan.IntegrationInstructions.ReducerInstructions)
			if instructions != "" {
				fmt.Fprintf(&builder, "\n--- Integration Contract Reducer Instructions ---\n%s\n", instructions)
			}
		}
		builder.WriteString("\nReturn the final reduced result for this task.\n")
	}
	return builder.String()
}

func (r *runner) integrationTurnInstructions(actorID string, round int) string {
	if r.sess.Plan.IntegrationInstructions == nil {
		return ""
	}
	for _, turn := range r.sess.Plan.IntegrationInstructions.Turns {
		if turn.ParticipantTurn == round && turn.Actor == actorID {
			return strings.TrimSpace(turn.Instructions)
		}
	}
	return ""
}

func (r *runner) promptMaterial() string {
	return r.material
}

func loadPromptMaterial(blobs *blobstore.Store, value session.Plan) (string, error) {
	groups := []struct {
		label string
		items []session.Input
	}{
		{label: "input", items: value.Inputs},
		{label: "context", items: value.Context},
		{label: "skill", items: value.Skills},
	}
	var builder strings.Builder
	for _, group := range groups {
		for _, input := range group.items {
			reader, err := blobs.Open(input.Content)
			if err != nil {
				return "", fmt.Errorf("open %s %q: %w", group.label, input.Name, err)
			}
			body, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil {
				return "", fmt.Errorf("read %s %q: %w", group.label, input.Name, readErr)
			}
			if closeErr != nil {
				return "", fmt.Errorf("verify %s %q: %w", group.label, input.Name, closeErr)
			}
			fmt.Fprintf(&builder, "[%s:%s]\n%s\n", group.label, input.Name, string(body))
		}
	}
	if len(value.TaskPlan) > 0 {
		fmt.Fprintf(&builder, "[task_plan]\n%s\n", string(value.TaskPlan))
	}
	return builder.String(), nil
}

func (r *runner) transcriptText() string {
	start := 0
	if len(r.state.conversation) > maxPromptTranscriptSize {
		start = len(r.state.conversation) - maxPromptTranscriptSize
	}
	var builder strings.Builder
	for _, turn := range r.state.conversation[start:] {
		fmt.Fprintf(&builder, "%s: %s\n", turn.ActorID, turn.Text)
	}
	return builder.String()
}

func (r *runner) readBlob(ref blobstore.BlobRef) (string, error) {
	reader, err := r.blobs.Open(ref)
	if err != nil {
		return "", err
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return string(body), nil
}

func readEvents(root string) ([]eventlog.Event, error) {
	body, err := os.ReadFile(filepath.Join(root, eventlog.EventsFilename))
	if err != nil {
		return nil, err
	}
	return eventlog.Replay(bytes.NewReader(body))
}

var ledgerObject = regexp.MustCompile("(?s)\\{.*\\}")

func parseLedger(raw string, fallback model.Ledger) model.Ledger {
	candidate := ledgerObject.FindString(raw)
	if candidate == "" {
		return fallback
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(candidate), &decoded); err != nil || !hasLedgerShape(decoded) {
		return fallback
	}
	return model.ParseLedger(decoded)
}

func hasLedgerShape(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"settled", "contested", "withdrawn"} {
		if _, exists := object[key]; !exists {
			return false
		}
	}
	return true
}

func hasConverged(turns []conversationTurn, ledger model.Ledger) bool {
	if len(turns) < 4 {
		return false
	}
	last := turns[len(turns)-1]
	previous := turns[len(turns)-2]
	if last.ActorID == previous.ActorID {
		return false
	}
	if hasDoneSignal(last.Text) && hasDoneSignal(previous.Text) {
		return true
	}
	counts := ledger.Counts()
	return counts.Contested == 0 && (counts.Settled > 0 || counts.Withdrawn > 0)
}

func hasNoLedgerSignal(turns []conversationTurn, ledger model.Ledger) bool {
	if len(turns) < 4 {
		return false
	}
	counts := ledger.Counts()
	if counts.Settled != 0 || counts.Contested != 0 || counts.Withdrawn != 0 {
		return false
	}
	return !hasDoneSignal(turns[len(turns)-1].Text) && !hasDoneSignal(turns[len(turns)-2].Text)
}

func hasDoneSignal(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	for _, signal := range []string{"done", "complete", "converged", "resolved", "final answer"} {
		if strings.Contains(normalized, signal) {
			return true
		}
	}
	return false
}
