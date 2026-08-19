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
	"strconv"
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
	statusCompleted = "completed"
	statusFailed    = "failed"

	stopCompleted        = "completed"
	stopConverged        = "converged"
	stopNoLedgerSignal   = "stalled_no_ledger_signal"
	stopProviderFailed   = "provider_failed"
	stopAbandonedAttempt = "abandoned_attempt"
	stopInvalidResult    = "invalid_result"

	mediaTypePlainTextUTF8  = "text/plain; charset=utf-8"
	mediaTypeChildResult    = "text/plain; charset=utf-8"
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
// facilitator result. A nil extractor means turns cannot request children.
type ChildRequestExtractor func(session.Actor, eventlog.Role, provider.TurnResult) []ChildRequest

// Deps contains the small imperative boundary required by Run. Writer is
// optional: when absent, Run opens the session's event writer itself. Recipes
// are used only to compile an admitted child plan.
type Deps struct {
	BackendFactory        BackendFactory
	Writer                *eventlog.Writer
	Recipes               []plan.Recipe
	ChildRequestExtractor ChildRequestExtractor
}

// Outcome contains the textual result needed by parent-child execution.
// Status, diagnostics, and transcript details are derived through sessionview.
type Outcome struct {
	Result string
}

// Run starts an empty session log and executes its compiled plan to completion.
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
	if len(events) != 0 {
		return Outcome{}, errors.New("session already has events; use Resume")
	}
	if err := runner.append(eventlog.SessionStartedPayload{PlanDigest: runner.planDigest, SessionID: sess.Plan.SessionID}); err != nil {
		return Outcome{}, err
	}
	return runner.execute()
}

// Resume replays a started, nonterminal session and continues only the work
// left by its immutable plan. The prompt is deliberately the only new input.
func Resume(ctx context.Context, sess *session.Session, deps Deps, prompt string) (Outcome, error) {
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
	if err := runner.rebuildExecutionState(events); err != nil {
		return Outcome{}, err
	}
	if runner.state.terminal != nil {
		return runner.outcome(), nil
	}
	if text := strings.TrimSpace(prompt); text != "" {
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

	participantTurns int
	reducerDone      bool
	active           *turnState

	conversation []conversationTurn
	ledger       model.Ledger
	lastResult   completedTurn

	requests     map[string]*childState
	childResults []string
	childrenUsed int
	childTurns   int

	steering         []*steeringState
	providerSessions map[string]string
	result           *resultState
}

type turnState struct {
	ActorID  string
	Round    int
	Role     eventlog.Role
	Attempts map[int]*attemptState
}

type attemptState struct {
	Attempt  int
	Finished bool
	Outcome  string
	Content  blobstore.BlobRef
	Failure  *providerFailureState
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
	Request   eventlog.ChildRequestedPayload
	Decided   bool
	Admitted  bool
	Completed bool
}

type steeringState struct {
	Ref          blobstore.BlobRef
	Text         string
	AppliedRound int
	Consumed     bool
}

type resultState struct {
	ValidationOutcome string
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

func newRunner(ctx context.Context, sess *session.Session, deps Deps) (*runner, error) {
	if sess == nil {
		return nil, errors.New("session is required")
	}
	if deps.BackendFactory == nil {
		return nil, errors.New("backend factory is required")
	}
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		return nil, fmt.Errorf("open blob store: %w", err)
	}
	writer := deps.Writer
	closeLog := false
	if writer == nil {
		writer, err = sess.EventWriter(blobs)
		if err != nil {
			return nil, fmt.Errorf("open event writer: %w", err)
		}
		closeLog = true
	}
	material, err := loadPromptMaterial(blobs, sess.Plan)
	if err != nil {
		if closeLog {
			_ = writer.Close()
		}
		return nil, err
	}
	runner := &runner{
		ctx:          ctx,
		sess:         sess,
		deps:         deps,
		blobs:        blobs,
		writer:       writer,
		closeLog:     closeLog,
		planDigest:   sess.Digest,
		backends:     make(map[string]provider.Backend, len(sess.Plan.Actors)),
		actors:       make(map[string]session.Actor, len(sess.Plan.Actors)),
		material:     material,
		state:        newExecutionState(),
		participants: participantActors(sess.Plan),
	}
	for _, actor := range sess.Plan.Actors {
		runner.actors[actor.ID] = actor
	}
	return runner, nil
}

func newExecutionState() *executionState {
	return &executionState{
		phase:            phaseParticipant,
		ledger:           model.EmptyLedger(),
		requests:         make(map[string]*childState),
		providerSessions: make(map[string]string),
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

// rebuildExecutionState is the sole replay path. It drives every event through
// reduceEvent, which is also called by append during a live execution.
func (r *runner) rebuildExecutionState(events []eventlog.Event) error {
	r.state = newExecutionState()
	for _, event := range events {
		if err := r.reduceEvent(event); err != nil {
			return err
		}
	}
	if !r.state.sessionStarted {
		return errors.New("session has no session.started event")
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
		if state.terminal != nil {
			return errors.New("turn.started follows session.finished")
		}
		if state.active != nil {
			return errors.New("session has more than one unfinished turn")
		}
		turn := &turnState{
			ActorID:  payload.ActorID,
			Round:    payload.Round,
			Role:     payload.Role,
			Attempts: make(map[int]*attemptState),
		}
		state.active = turn
		state.phase = phaseParticipant
		if payload.Role == eventlog.FacilitatorRole {
			state.phase = phaseFacilitator
		} else if payload.Role == eventlog.ReducerRole {
			state.phase = phaseReducer
		}
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
		if attempt.Finished {
			return fmt.Errorf("duplicate attempt.finished for %s attempt %d", payload.ActorID, payload.Attempt)
		}
		if payload.Outcome != "success" && payload.Outcome != "failed" {
			return fmt.Errorf("attempt.finished outcome %q is unsupported", payload.Outcome)
		}
		attempt.Finished = true
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
		if child.Decided {
			return fmt.Errorf("duplicate child.decided for request %q", payload.RequestID)
		}
		child.Decided = true
		child.Admitted = payload.Admitted
		if payload.Admitted {
			turns, err := r.childTurnsFor(child, payload.BudgetState)
			if err != nil {
				return err
			}
			state.childrenUsed++
			state.childTurns += turns
		}
		return nil
	case eventlog.ChildCompletedPayload:
		child, exists := state.requests[payload.RequestID]
		if !exists {
			return fmt.Errorf("child.completed for unknown request %q", payload.RequestID)
		}
		if child.Completed {
			return fmt.Errorf("duplicate child.completed for request %q", payload.RequestID)
		}
		text, err := r.readBlob(payload.Result)
		if err != nil {
			return err
		}
		child.Completed = true
		state.childResults = append(state.childResults, text)
		return nil
	case eventlog.SteeringQueuedPayload:
		text, err := r.readBlob(payload.Prompt)
		if err != nil {
			return err
		}
		state.steering = append(state.steering, &steeringState{Ref: payload.Prompt, Text: text})
		return nil
	case eventlog.SteeringAppliedPayload:
		for _, steering := range state.steering {
			if steering.Consumed || steering.AppliedRound != 0 || !steering.Ref.Equal(payload.Prompt) {
				continue
			}
			steering.AppliedRound = payload.Round
			return nil
		}
		return errors.New("steering.applied has no queued prompt")
	case eventlog.ResultProducedPayload:
		if state.result != nil {
			return errors.New("session has more than one result.produced event")
		}
		state.result = &resultState{
			ValidationOutcome: payload.ValidationOutcome,
		}
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
		r.state.reducerDone = true
		if r.sess.Plan.Result.Source == "reducer" {
			r.state.lastResult = completed
		}
	}
}

func (r *runner) advancePhase(turn *turnState) {
	switch turn.Role {
	case eventlog.ParticipantRole:
		r.state.participantTurns++
		if r.sess.Plan.Schedule.Kind == "dialogue" && r.sess.Plan.Facilitator != nil &&
			r.state.participantTurns%r.sess.Plan.Facilitator.Cadence == 0 {
			r.state.phase = phaseFacilitator
			return
		}
		r.phaseAfterParticipants()
	case eventlog.FacilitatorRole:
		r.phaseAfterParticipants()
	case eventlog.ReducerRole:
		r.state.reducerDone = true
		r.state.phase = phaseDone
	}
}

func (r *runner) phaseAfterParticipants() {
	if r.state.participantTurns < r.sess.Plan.Schedule.Turns {
		r.state.phase = phaseParticipant
		return
	}
	if r.sess.Plan.Reducer != nil && !r.state.reducerDone {
		r.state.phase = phaseReducer
		return
	}
	r.state.phase = phaseDone
}

func (r *runner) execute() (Outcome, error) {
	for {
		if r.state.terminal != nil {
			return r.outcome(), nil
		}
		if r.state.active != nil {
			if err := r.serviceActiveTurn(); err != nil {
				reason := stopProviderFailed
				var failure *executionFailure
				if errors.As(err, &failure) {
					reason = failure.reason
				}
				return r.finishFailure(reason, err)
			}
			continue
		}
		if err := r.servicePendingChildren(); err != nil {
			return r.finishFailure(stopProviderFailed, err)
		}
		if reason := r.dialogueStopReason(); reason != "" {
			return r.finishSuccess(reason)
		}
		if r.state.phase == phaseDone {
			return r.finishSuccess(stopCompleted)
		}
		next, err := r.nextTurn()
		if err != nil {
			return r.finishFailure(stopProviderFailed, err)
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
	switch r.state.phase {
	case phaseParticipant:
		if r.state.participantTurns >= r.sess.Plan.Schedule.Turns {
			return turnSpec{}, errors.New("participant phase has no remaining turn")
		}
		round := r.state.participantTurns + 1
		if r.sess.Plan.Schedule.Kind == "dialogue" {
			if len(r.participants) == 0 {
				return turnSpec{}, errors.New("dialogue schedule has no participant actors")
			}
			return turnSpec{
				Actor: r.participants[r.state.participantTurns%len(r.participants)],
				Round: round,
				Role:  eventlog.ParticipantRole,
			}, nil
		}
		actor, err := r.actor(r.sess.Plan.Schedule.Order[r.state.participantTurns])
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
		return turnSpec{Actor: actor, Round: r.state.participantTurns, Role: eventlog.FacilitatorRole}, nil
	case phaseReducer:
		if r.sess.Plan.Reducer == nil {
			return turnSpec{}, errors.New("reducer phase has no reducer")
		}
		actor, err := r.actor(r.sess.Plan.Reducer.Actor)
		if err != nil {
			return turnSpec{}, err
		}
		return turnSpec{Actor: actor, Round: r.sess.Plan.Schedule.Turns + 1, Role: eventlog.ReducerRole}, nil
	default:
		return turnSpec{}, errors.New("execution has no next turn")
	}
}

func (r *runner) dialogueStopReason() string {
	if r.sess.Plan.Schedule.Kind != "dialogue" || !r.sess.Plan.Schedule.StopOnConvergence ||
		r.state.active != nil || r.state.phase == phaseFacilitator {
		return ""
	}
	if hasConverged(r.state.conversation, r.state.ledger) {
		return stopConverged
	}
	if hasNoLedgerSignal(r.state.conversation, r.state.ledger) {
		return stopNoLedgerSignal
	}
	return ""
}

func (r *runner) serviceActiveTurn() error {
	turn := r.state.active
	if turn == nil {
		return nil
	}
	if success := turn.latest("success"); success != nil {
		return r.finishActiveTurn(turn, success)
	}
	if abandoned := turn.latest(""); abandoned != nil {
		if !r.retryAbandoned(abandoned.Attempt) {
			return &executionFailure{
				reason: stopAbandonedAttempt,
				cause:  fmt.Errorf("abandoned provider attempt %d for %s cannot be retried by plan policy", abandoned.Attempt, turn.ActorID),
			}
		}
		return r.callActiveAttempt(turn, turn.nextAttempt())
	}
	if failed := turn.latest("failed"); failed != nil {
		if failed.Failure != nil && r.shouldRetry(failed.Failure, failed.Attempt) {
			return r.callActiveAttempt(turn, turn.nextAttempt())
		}
		return &executionFailure{
			reason: stopProviderFailed,
			cause:  recordedFailure(turn, failed),
		}
	}
	return r.callActiveAttempt(turn, turn.nextAttempt())
}

func (turn *turnState) latest(outcome string) *attemptState {
	var selected *attemptState
	for _, attempt := range turn.Attempts {
		matches := !attempt.Finished
		if outcome != "" {
			matches = attempt.Finished && attempt.Outcome == outcome
		}
		if matches && (selected == nil || attempt.Attempt > selected.Attempt) {
			selected = attempt
		}
	}
	return selected
}

func (turn *turnState) nextAttempt() int {
	next := 1
	for number := range turn.Attempts {
		if number >= next {
			next = number + 1
		}
	}
	return next
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
	ref, putErr := r.putText(result.Content)
	if putErr != nil {
		return putErr
	}
	if callErr == nil {
		// Request extraction happens before the success event. If the process
		// stops after extraction, the request survives; if it stops earlier,
		// the unfinished provider attempt is retried and extracted again.
		if err := r.persistChildRequests(turn, actor, result); err != nil {
			return err
		}
		if err := r.append(eventlog.AttemptFinishedPayload{
			ActorID:           actor.ID,
			Attempt:           attemptNumber,
			Outcome:           "success",
			ProviderSessionID: providerSessionID(backend),
			Content:           ref,
		}); err != nil {
			return err
		}
		return r.finishActiveTurn(r.state.active, r.state.active.Attempts[attemptNumber])
	}
	if err := r.append(eventlog.AttemptFinishedPayload{
		ActorID: actor.ID,
		Attempt: attemptNumber,
		Outcome: "failed",
		Content: ref,
	}); err != nil {
		return err
	}
	failure := provider.NewProviderFailure("turn", actor.ID, actor.Backend, callErr, provider.ProviderResultForTurn(actor.Backend, result))
	return r.append(eventlog.ProviderFailedPayload{
		ActorID:         actor.ID,
		Backend:         actor.Backend,
		Category:        failure.Category,
		Retryable:       failure.Retryable,
		Attempts:        attemptNumber,
		RemediationCode: failure.RemediationCode,
		SanitizedDetail: failure.SanitizedDetail,
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
	for index, child := range r.deps.ChildRequestExtractor(actor, turn.Role, result) {
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

func (r *runner) servicePendingChildren() error {
	requestIDs := make([]string, 0, len(r.state.requests))
	for requestID := range r.state.requests {
		requestIDs = append(requestIDs, requestID)
	}
	sort.Strings(requestIDs)
	for _, requestID := range requestIDs {
		child := r.state.requests[requestID]
		if !child.Decided {
			if err := r.decideChild(child); err != nil {
				return err
			}
		}
		if child.Admitted && !child.Completed {
			if err := r.runChild(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *runner) decideChild(child *childState) error {
	if r.sess.Plan.ChildPolicy.Mode != "allow" {
		return r.append(eventlog.ChildDecidedPayload{
			RequestID:   child.Request.RequestID,
			Admitted:    false,
			Reason:      childDecisionReason(r.sess.Plan.ChildPolicy.Mode),
			BudgetState: "not_admitted",
		})
	}
	if r.state.childrenUsed >= r.sess.Plan.ChildPolicy.MaxChildren {
		return r.append(eventlog.ChildDecidedPayload{
			RequestID:   child.Request.RequestID,
			Admitted:    false,
			Reason:      "child capacity exhausted",
			BudgetState: "children_exhausted",
		})
	}
	childPlan, err := r.childPlanFor(child)
	if err != nil {
		return r.append(eventlog.ChildDecidedPayload{
			RequestID:   child.Request.RequestID,
			Admitted:    false,
			Reason:      provider.SanitizeProviderFailureDetail(err.Error()),
			BudgetState: "rejected",
		})
	}
	if r.state.childTurns+childPlan.Schedule.Turns > r.sess.Plan.ChildPolicy.MaxTurns {
		return r.append(eventlog.ChildDecidedPayload{
			RequestID:   child.Request.RequestID,
			Admitted:    false,
			Reason:      "child turn budget exhausted",
			BudgetState: "turns_exhausted",
		})
	}
	return r.append(eventlog.ChildDecidedPayload{
		RequestID:   child.Request.RequestID,
		Admitted:    true,
		Reason:      "admitted by child policy",
		BudgetState: childBudgetState("available", childPlan.Schedule.Turns),
	})
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

func (r *runner) childSessionID(requestID string) string {
	return r.sess.Plan.SessionID + "-child-" + requestID
}

func childBudgetState(state string, turns int) string {
	return state + ";child_turns=" + strconv.Itoa(turns)
}

func (r *runner) childTurnsFor(child *childState, budgetState string) (int, error) {
	if _, suffix, found := strings.Cut(budgetState, ";child_turns="); found {
		turns, err := strconv.Atoi(suffix)
		if err != nil || turns < 0 {
			return 0, fmt.Errorf("child.decided has invalid child turn count %q", suffix)
		}
		return turns, nil
	}
	childPlan, err := r.childPlanFor(child)
	if err != nil {
		return 0, fmt.Errorf("reconstruct child budget for %s: %w", child.Request.RequestID, err)
	}
	return childPlan.Schedule.Turns, nil
}

func (r *runner) runChild(child *childState) error {
	childPlan, err := r.childPlanFor(child)
	if err != nil {
		return err
	}
	childSession, err := session.Create(filepath.Dir(r.sess.Root), childPlan)
	if err != nil {
		return err
	}
	childBlobs, err := childSession.BlobStore(blobstore.Limits{})
	if err != nil {
		return err
	}
	if err := copyPlanBlobs(r.blobs, childBlobs, session.BlobRefs(childPlan)); err != nil {
		return err
	}
	childDeps := r.deps
	childDeps.Writer = nil
	childOutcome, childErr := Run(r.ctx, childSession, childDeps)
	resultText := childOutcome.Result
	if childErr != nil {
		resultText = "child execution failed: " + provider.SanitizeProviderFailureDetail(childErr.Error())
	}
	resultRef, err := r.blobs.PutBytes([]byte(resultText), mediaTypeChildResult)
	if err != nil {
		return err
	}
	if err := r.append(eventlog.ChildCompletedPayload{
		RequestID:      child.Request.RequestID,
		ChildSessionID: childSession.Plan.SessionID,
		Result:         resultRef,
	}); err != nil {
		return err
	}
	return childErr
}

func childDecisionReason(mode string) string {
	switch mode {
	case "deny":
		return "child policy denies child plans"
	case "ask":
		return "child request requires operator approval"
	default:
		return "child policy does not admit request"
	}
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
	if r.state.result == nil {
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
	if r.state.result.ValidationOutcome != "valid" {
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

func (r *runner) outcome() Outcome {
	return Outcome{Result: r.state.lastResult.Text}
}

func (r *runner) validateSelectedResult() error {
	if r.state.lastResult.Ref == (blobstore.BlobRef{}) {
		return errors.New("selected result has no durable content")
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
		builder.WriteString("\nReturn the final reduced result for this task.\n")
	}
	return builder.String()
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
