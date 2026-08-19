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
)

const (
	statusCompleted = "completed"
	statusFailed    = "failed"

	stopCompleted           = "completed"
	stopConverged           = "converged"
	stopNoLedgerSignal      = "stalled_no_ledger_signal"
	stopProviderFailed      = "provider_failed"
	stopAbandonedAttempt    = "abandoned_attempt"
	mediaTypePlainTextUTF8  = "text/plain; charset=utf-8"
	mediaTypeChildResult    = "text/plain; charset=utf-8"
	maxPromptTranscriptSize = 12
)

// Clock supplies event times. It is a dependency so an execution trace is
// deterministic under test.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function into a Clock.
type ClockFunc func() time.Time

// Now implements Clock.
func (f ClockFunc) Now() time.Time {
	return f()
}

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
	Clock                 Clock
	Writer                *eventlog.Writer
	Recipes               []plan.Recipe
	ChildRequestExtractor ChildRequestExtractor
}

// AbandonedAttempt describes a persisted attempt.started record that did not
// reach attempt.finished before a resume.
type AbandonedAttempt struct {
	ActorID string
	Attempt int
}

// Outcome is the terminal or current result derived from the execution log.
// Result is the selected textual result, while ResultRef names its durable
// content when the result came from a completed turn.
type Outcome struct {
	Status     string
	StopReason string
	Result     string
	ResultRef  blobstore.BlobRef
	Turns      int
	Abandoned  []AbandonedAttempt
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
	return runner.execute(executionProgress{})
}

// Resume replays a started, nonterminal session and continues only the work
// left by its immutable plan. The prompt is deliberately the only new input.
func Resume(ctx context.Context, sess *session.Session, deps Deps, prompt string) (Outcome, error) {
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
	if err := session.ValidateEventBindings(sess.Plan, events); err != nil {
		return Outcome{}, err
	}
	progress, outcome, terminal, err := runner.replay(events)
	if err != nil {
		return Outcome{}, err
	}
	if terminal {
		return outcome, nil
	}
	if text := strings.TrimSpace(prompt); text != "" {
		ref, err := runner.putText(text)
		if err != nil {
			return Outcome{}, err
		}
		if err := runner.append(eventlog.SteeringQueuedPayload{Prompt: ref}); err != nil {
			return Outcome{}, err
		}
		progress.resumePrompt = text
		progress.resumePromptRef = &ref
	}
	return runner.execute(progress)
}

type runner struct {
	ctx        context.Context
	sess       *session.Session
	deps       Deps
	blobs      *blobstore.Store
	writer     *eventlog.Writer
	closeLog   bool
	clock      Clock
	planDigest string

	backends     map[string]provider.Backend
	participants []session.Actor
	actors       map[string]session.Actor

	conversation []conversationTurn
	childResults []string
	ledger       model.Ledger
	lastResult   completedTurn
	material     string
	childrenUsed int
	childTurns   int
	requestIDs   map[string]struct{}
}

type conversationTurn struct {
	ActorID string
	Text    string
}

type completedTurn struct {
	Text string
	Ref  blobstore.BlobRef
}

type executionProgress struct {
	participantTurns int
	sequenceTurns    int
	reducerDone      bool
	startedTurn      *startedTurn
	abandoned        []AbandonedAttempt
	resumePrompt     string
	resumePromptRef  *blobstore.BlobRef
}

type startedTurn struct {
	ActorID     string
	Round       int
	Role        eventlog.Role
	NextAttempt int
}

func newRunner(ctx context.Context, sess *session.Session, deps Deps) (*runner, error) {
	if sess == nil {
		return nil, errors.New("session is required")
	}
	if err := session.ValidatePlan(sess.Plan); err != nil {
		return nil, fmt.Errorf("validate session plan: %w", err)
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
	clock := deps.Clock
	if clock == nil {
		clock = ClockFunc(time.Now)
	}
	digest, err := session.PlanDigest(sess.Plan)
	if err != nil {
		if closeLog {
			_ = writer.Close()
		}
		return nil, err
	}
	material, err := loadPromptMaterial(blobs, sess.Plan)
	if err != nil {
		if closeLog {
			_ = writer.Close()
		}
		return nil, err
	}
	runner := &runner{
		ctx:        ctx,
		sess:       sess,
		deps:       deps,
		blobs:      blobs,
		writer:     writer,
		closeLog:   closeLog,
		clock:      clock,
		planDigest: digest,
		backends:   make(map[string]provider.Backend, len(sess.Plan.Actors)),
		actors:     make(map[string]session.Actor, len(sess.Plan.Actors)),
		ledger:     model.EmptyLedger(),
		material:   material,
		requestIDs: make(map[string]struct{}),
	}
	for _, actor := range sess.Plan.Actors {
		runner.actors[actor.ID] = actor
	}
	runner.participants = participantActors(sess.Plan)
	return runner, nil
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

func (r *runner) append(payload eventlog.Payload) error {
	if r.writer == nil {
		return errors.New("event writer is required")
	}
	identifier := fmt.Sprintf("engine-%d", r.writer.NextSeq())
	_, err := r.writer.Append(eventlog.NewEvent(identifier, r.clock.Now(), payload))
	return err
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
	r.backends[actor.ID] = backend
	return backend, nil
}

func (r *runner) execute(progress executionProgress) (Outcome, error) {
	if r.sess.Plan.Schedule.Kind == "dialogue" {
		return r.executeDialogue(progress)
	}
	return r.executeSequence(progress)
}

func (r *runner) executeDialogue(progress executionProgress) (Outcome, error) {
	if progress.startedTurn != nil {
		if err := r.applyResumePrompt(&progress, progress.startedTurn.Round); err != nil {
			return Outcome{}, err
		}
		turn, outcome, done, err := r.resumeStartedTurn(progress)
		if done || err != nil {
			return outcome, err
		}
		switch progress.startedTurn.Role {
		case eventlog.ParticipantRole:
			progress.participantTurns++
		case eventlog.FacilitatorRole:
			r.ledger = parseLedger(turn.Text, r.ledger)
		}
	}

	for progress.participantTurns < r.sess.Plan.Schedule.Turns {
		actor := r.participants[progress.participantTurns%len(r.participants)]
		round := progress.participantTurns + 1
		if err := r.applyResumePrompt(&progress, round); err != nil {
			return Outcome{}, err
		}
		if _, err := r.runTurn(actor, round, eventlog.ParticipantRole, 1, true, progress.resumePrompt); err != nil {
			return r.finishFailure(stopProviderFailed, progress.abandoned, err)
		}
		progress.resumePrompt = ""
		progress.participantTurns++

		if r.sess.Plan.Facilitator != nil && progress.participantTurns%r.sess.Plan.Facilitator.Cadence == 0 {
			facilitator, err := r.actor(r.sess.Plan.Facilitator.Actor)
			if err != nil {
				return r.finishFailure(stopProviderFailed, progress.abandoned, err)
			}
			turn, err := r.runTurn(facilitator, round, eventlog.FacilitatorRole, 1, true, "")
			if err != nil {
				return r.finishFailure(stopProviderFailed, progress.abandoned, err)
			}
			r.ledger = parseLedger(turn.Text, r.ledger)
		}

		if r.sess.Plan.Schedule.StopOnConvergence {
			if hasConverged(r.conversation, r.ledger) {
				return r.finishSuccess(stopConverged, progress.abandoned)
			}
			if hasNoLedgerSignal(r.conversation, r.ledger) {
				return r.finishSuccess(stopNoLedgerSignal, progress.abandoned)
			}
		}
	}
	if r.sess.Plan.Reducer != nil {
		if err := r.applyResumePrompt(&progress, r.sess.Plan.Schedule.Turns+1); err != nil {
			return Outcome{}, err
		}
		if _, err := r.runReducer(r.sess.Plan.Schedule.Turns+1, progress.resumePrompt); err != nil {
			return r.finishFailure(stopProviderFailed, progress.abandoned, err)
		}
	}
	return r.finishSuccess(stopCompleted, progress.abandoned)
}

func (r *runner) executeSequence(progress executionProgress) (Outcome, error) {
	if progress.startedTurn != nil {
		if err := r.applyResumePrompt(&progress, progress.startedTurn.Round); err != nil {
			return Outcome{}, err
		}
		_, outcome, done, err := r.resumeStartedTurn(progress)
		if done || err != nil {
			return outcome, err
		}
		switch progress.startedTurn.Role {
		case eventlog.ParticipantRole:
			progress.sequenceTurns++
		case eventlog.ReducerRole:
			progress.reducerDone = true
		}
	}
	for progress.sequenceTurns < len(r.sess.Plan.Schedule.Order) {
		actor, err := r.actor(r.sess.Plan.Schedule.Order[progress.sequenceTurns])
		if err != nil {
			return r.finishFailure(stopProviderFailed, progress.abandoned, err)
		}
		round := progress.sequenceTurns + 1
		if err := r.applyResumePrompt(&progress, round); err != nil {
			return Outcome{}, err
		}
		if _, err := r.runTurn(actor, round, eventlog.ParticipantRole, 1, true, progress.resumePrompt); err != nil {
			return r.finishFailure(stopProviderFailed, progress.abandoned, err)
		}
		progress.resumePrompt = ""
		progress.sequenceTurns++
	}
	if r.sess.Plan.Reducer != nil && !progress.reducerDone {
		if err := r.applyResumePrompt(&progress, len(r.sess.Plan.Schedule.Order)+1); err != nil {
			return Outcome{}, err
		}
		if _, err := r.runReducer(len(r.sess.Plan.Schedule.Order)+1, progress.resumePrompt); err != nil {
			return r.finishFailure(stopProviderFailed, progress.abandoned, err)
		}
	}
	return r.finishSuccess(stopCompleted, progress.abandoned)
}

func (r *runner) resumeStartedTurn(progress executionProgress) (completedTurn, Outcome, bool, error) {
	started := progress.startedTurn
	if started == nil {
		return completedTurn{}, Outcome{}, false, nil
	}
	actor, err := r.actor(started.ActorID)
	if err != nil {
		outcome, finishErr := r.finishFailure(stopProviderFailed, progress.abandoned, err)
		return completedTurn{}, outcome, true, finishErr
	}
	if len(progress.abandoned) > 0 {
		last := progress.abandoned[len(progress.abandoned)-1]
		if last.ActorID == actor.ID && (r.sess.Plan.ProviderRetry.Mode != "allow" || last.Attempt >= r.sess.Plan.ProviderRetry.MaxAttempts) {
			err := fmt.Errorf("abandoned provider attempt %d for %s cannot be retried by plan policy", last.Attempt, actor.ID)
			outcome, finishErr := r.finishFailure(stopAbandonedAttempt, progress.abandoned, err)
			return completedTurn{}, outcome, true, finishErr
		}
	}
	turn, err := r.runTurn(actor, started.Round, started.Role, started.NextAttempt, false, progress.resumePrompt)
	if err != nil {
		outcome, finishErr := r.finishFailure(stopProviderFailed, progress.abandoned, err)
		return completedTurn{}, outcome, true, finishErr
	}
	return turn, Outcome{}, false, nil
}

func (r *runner) applyResumePrompt(progress *executionProgress, round int) error {
	if progress == nil || progress.resumePromptRef == nil {
		return nil
	}
	if err := r.append(eventlog.SteeringAppliedPayload{Prompt: *progress.resumePromptRef, Round: round}); err != nil {
		return err
	}
	progress.resumePromptRef = nil
	return nil
}

func (r *runner) runReducer(round int, resumePrompt string) (completedTurn, error) {
	actor, err := r.actor(r.sess.Plan.Reducer.Actor)
	if err != nil {
		return completedTurn{}, err
	}
	return r.runTurn(actor, round, eventlog.ReducerRole, 1, true, resumePrompt)
}

func (r *runner) actor(identifier string) (session.Actor, error) {
	actor, exists := r.actors[identifier]
	if !exists {
		return session.Actor{}, fmt.Errorf("plan names unknown actor %q", identifier)
	}
	return actor, nil
}

func (r *runner) runTurn(actor session.Actor, round int, role eventlog.Role, firstAttempt int, appendStarted bool, resumePrompt string) (completedTurn, error) {
	if appendStarted {
		if err := r.append(eventlog.TurnStartedPayload{ActorID: actor.ID, Round: round, Role: role}); err != nil {
			return completedTurn{}, err
		}
	}
	backend, err := r.backend(actor)
	if err != nil {
		return completedTurn{}, err
	}
	prompt := r.promptFor(actor, round, role, resumePrompt)
	for attempt := firstAttempt; ; attempt++ {
		if err := r.append(eventlog.AttemptStartedPayload{ActorID: actor.ID, Attempt: attempt}); err != nil {
			return completedTurn{}, err
		}
		result, callErr := backend.RunTurn(r.ctx, prompt, provider.TurnOptions{
			TimeoutSeconds:      r.sess.Plan.Timeouts.TurnSeconds,
			StallTimeoutSeconds: r.sess.Plan.Timeouts.StallSeconds,
		})
		ref, putErr := r.putText(result.Content)
		if putErr != nil {
			return completedTurn{}, putErr
		}
		if callErr == nil {
			if err := r.append(eventlog.AttemptFinishedPayload{
				ActorID: actor.ID,
				Attempt: attempt,
				Outcome: "success",
				Content: ref,
			}); err != nil {
				return completedTurn{}, err
			}
			if err := r.append(eventlog.TurnFinishedPayload{ActorID: actor.ID, Round: round, Content: ref}); err != nil {
				return completedTurn{}, err
			}
			turn := completedTurn{Text: result.Content, Ref: ref}
			r.recordCompletedTurn(actor, role, turn)
			if err := r.handleChildRequests(actor, role, result); err != nil {
				return completedTurn{}, err
			}
			return turn, nil
		}
		if err := r.append(eventlog.AttemptFinishedPayload{
			ActorID: actor.ID,
			Attempt: attempt,
			Outcome: "failed",
			Content: ref,
		}); err != nil {
			return completedTurn{}, err
		}
		failure := provider.NewProviderFailure("turn", actor.ID, actor.Backend, callErr, provider.ProviderResultForTurn(actor.Backend, result))
		if err := r.append(eventlog.ProviderFailedPayload{
			ActorID:         actor.ID,
			Backend:         actor.Backend,
			Category:        failure.Category,
			Retryable:       failure.Retryable,
			Attempts:        attempt,
			RemediationCode: failure.RemediationCode,
			SanitizedDetail: failure.SanitizedDetail,
		}); err != nil {
			return completedTurn{}, err
		}
		if r.shouldRetry(failure, attempt) {
			continue
		}
		return completedTurn{}, callErr
	}
}

func (r *runner) shouldRetry(failure provider.ProviderFailure, attempt int) bool {
	return r.sess.Plan.ProviderRetry.Mode == "allow" &&
		failure.Category != "auth" &&
		failure.Retryable &&
		attempt < r.sess.Plan.ProviderRetry.MaxAttempts
}

func (r *runner) recordCompletedTurn(actor session.Actor, role eventlog.Role, turn completedTurn) {
	if role == eventlog.ParticipantRole {
		r.conversation = append(r.conversation, conversationTurn{ActorID: actor.ID, Text: turn.Text})
		r.lastResult = turn
	}
	if role == eventlog.ReducerRole && r.sess.Plan.Result.Source == "reducer" {
		r.lastResult = turn
	}
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
	if len(r.childResults) > 0 {
		builder.WriteString("\nChild results:\n")
		for _, result := range r.childResults {
			builder.WriteString(result)
			builder.WriteByte('\n')
		}
	}
	if role == eventlog.FacilitatorRole {
		counts := r.ledger.Counts()
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
	if len(r.conversation) > maxPromptTranscriptSize {
		start = len(r.conversation) - maxPromptTranscriptSize
	}
	var builder strings.Builder
	for _, turn := range r.conversation[start:] {
		fmt.Fprintf(&builder, "%s: %s\n", turn.ActorID, turn.Text)
	}
	return builder.String()
}

func (r *runner) handleChildRequests(actor session.Actor, role eventlog.Role, result provider.TurnResult) error {
	if r.deps.ChildRequestExtractor == nil || (role != eventlog.ParticipantRole && role != eventlog.FacilitatorRole) {
		return nil
	}
	for index, child := range r.deps.ChildRequestExtractor(actor, role, result) {
		if err := r.handleChildRequest(actor, child, index); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) handleChildRequest(actor session.Actor, child ChildRequest, index int) error {
	request := child.Request
	requestID := strings.TrimSpace(child.ID)
	if requestID == "" {
		requestID = fmt.Sprintf("child-%d-%d", r.writer.NextSeq(), index+1)
	}
	if _, exists := r.requestIDs[requestID]; exists {
		return fmt.Errorf("duplicate child request id %q", requestID)
	}
	r.requestIDs[requestID] = struct{}{}
	recipeID := strings.TrimSpace(request.RecipeID)
	if recipeID == "" {
		recipeID = "unspecified"
	}
	question, err := r.putText(request.Question)
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

	if r.sess.Plan.ChildPolicy.Mode != "allow" {
		return r.append(eventlog.ChildDecidedPayload{
			RequestID:   requestID,
			Admitted:    false,
			Reason:      childDecisionReason(r.sess.Plan.ChildPolicy.Mode),
			BudgetState: "not_admitted",
		})
	}
	if r.childrenUsed >= r.sess.Plan.ChildPolicy.MaxChildren {
		return r.append(eventlog.ChildDecidedPayload{
			RequestID:   requestID,
			Admitted:    false,
			Reason:      "child capacity exhausted",
			BudgetState: "children_exhausted",
		})
	}
	if request.SessionID == "" {
		request.SessionID = fmt.Sprintf("%s-child-%d", r.sess.Plan.SessionID, r.childrenUsed+1)
	}
	childPlan, compileErr := plan.ForChild(r.sess.Plan, request, r.deps.Recipes)
	if compileErr != nil {
		return r.append(eventlog.ChildDecidedPayload{
			RequestID:   requestID,
			Admitted:    false,
			Reason:      provider.SanitizeProviderFailureDetail(compileErr.Error()),
			BudgetState: "rejected",
		})
	}
	if r.childTurns+childPlan.Schedule.Turns > r.sess.Plan.ChildPolicy.MaxTurns {
		return r.append(eventlog.ChildDecidedPayload{
			RequestID:   requestID,
			Admitted:    false,
			Reason:      "child turn budget exhausted",
			BudgetState: "turns_exhausted",
		})
	}
	if err := r.append(eventlog.ChildDecidedPayload{
		RequestID:   requestID,
		Admitted:    true,
		Reason:      "admitted by child policy",
		BudgetState: "available",
	}); err != nil {
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
	r.childrenUsed++
	r.childTurns += childPlan.Schedule.Turns
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
		RequestID:      requestID,
		ChildSessionID: childSession.Plan.SessionID,
		Result:         resultRef,
	}); err != nil {
		return err
	}
	r.childResults = append(r.childResults, resultText)
	if childErr != nil {
		return childErr
	}
	return nil
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

func (r *runner) finishSuccess(reason string, abandoned []AbandonedAttempt) (Outcome, error) {
	if err := r.append(eventlog.SessionFinishedPayload{Status: statusCompleted, StopReason: reason}); err != nil {
		return Outcome{}, err
	}
	return Outcome{
		Status:     statusCompleted,
		StopReason: reason,
		Result:     r.lastResult.Text,
		ResultRef:  r.lastResult.Ref,
		Turns:      len(r.conversation),
		Abandoned:  append([]AbandonedAttempt{}, abandoned...),
	}, nil
}

func (r *runner) finishFailure(reason string, abandoned []AbandonedAttempt, cause error) (Outcome, error) {
	appendErr := r.append(eventlog.SessionFinishedPayload{Status: statusFailed, StopReason: reason})
	outcome := Outcome{
		Status:     statusFailed,
		StopReason: reason,
		Result:     r.lastResult.Text,
		ResultRef:  r.lastResult.Ref,
		Turns:      len(r.conversation),
		Abandoned:  append([]AbandonedAttempt{}, abandoned...),
	}
	if appendErr != nil {
		return outcome, errors.Join(cause, appendErr)
	}
	return outcome, cause
}

func (r *runner) replay(events []eventlog.Event) (executionProgress, Outcome, bool, error) {
	progress := executionProgress{}
	roles := make(map[string]eventlog.Role)
	finished := make(map[string]bool)
	started := make(map[string]startedTurn)
	attempts := make(map[string]map[int]bool)
	terminal := false
	sessionStarted := false
	outcome := Outcome{}

	for _, event := range events {
		switch payload := event.Payload.(type) {
		case eventlog.SessionStartedPayload:
			if sessionStarted {
				return executionProgress{}, Outcome{}, false, errors.New("session has more than one session.started event")
			}
			if payload.PlanDigest != r.planDigest || payload.SessionID != r.sess.Plan.SessionID {
				return executionProgress{}, Outcome{}, false, errors.New("session.started does not match immutable plan")
			}
			sessionStarted = true
		case *eventlog.SessionStartedPayload:
			if payload != nil {
				if sessionStarted {
					return executionProgress{}, Outcome{}, false, errors.New("session has more than one session.started event")
				}
				if payload.PlanDigest != r.planDigest || payload.SessionID != r.sess.Plan.SessionID {
					return executionProgress{}, Outcome{}, false, errors.New("session.started does not match immutable plan")
				}
				sessionStarted = true
			}
		case eventlog.TurnStartedPayload:
			key := turnKey(payload.ActorID, payload.Round)
			roles[key] = payload.Role
			started[key] = startedTurn{ActorID: payload.ActorID, Round: payload.Round, Role: payload.Role, NextAttempt: 1}
		case *eventlog.TurnStartedPayload:
			if payload != nil {
				key := turnKey(payload.ActorID, payload.Round)
				roles[key] = payload.Role
				started[key] = startedTurn{ActorID: payload.ActorID, Round: payload.Round, Role: payload.Role, NextAttempt: 1}
			}
		case eventlog.AttemptStartedPayload:
			for key, turn := range started {
				if turn.ActorID != payload.ActorID || finished[key] {
					continue
				}
				if attempts[key] == nil {
					attempts[key] = make(map[int]bool)
				}
				attempts[key][payload.Attempt] = false
				turn.NextAttempt = maxInt(turn.NextAttempt, payload.Attempt+1)
				started[key] = turn
			}
		case *eventlog.AttemptStartedPayload:
			if payload != nil {
				for key, turn := range started {
					if turn.ActorID != payload.ActorID || finished[key] {
						continue
					}
					if attempts[key] == nil {
						attempts[key] = make(map[int]bool)
					}
					attempts[key][payload.Attempt] = false
					turn.NextAttempt = maxInt(turn.NextAttempt, payload.Attempt+1)
					started[key] = turn
				}
			}
		case eventlog.AttemptFinishedPayload:
			for key, turn := range started {
				if turn.ActorID == payload.ActorID && !finished[key] && attempts[key] != nil {
					if _, exists := attempts[key][payload.Attempt]; exists {
						attempts[key][payload.Attempt] = true
					}
				}
			}
		case *eventlog.AttemptFinishedPayload:
			if payload != nil {
				for key, turn := range started {
					if turn.ActorID == payload.ActorID && !finished[key] && attempts[key] != nil {
						if _, exists := attempts[key][payload.Attempt]; exists {
							attempts[key][payload.Attempt] = true
						}
					}
				}
			}
		case eventlog.TurnFinishedPayload:
			if err := r.replayFinishedTurn(payload, roles, finished, &progress); err != nil {
				return executionProgress{}, Outcome{}, false, err
			}
		case *eventlog.TurnFinishedPayload:
			if payload != nil {
				if err := r.replayFinishedTurn(*payload, roles, finished, &progress); err != nil {
					return executionProgress{}, Outcome{}, false, err
				}
			}
		case eventlog.ChildCompletedPayload:
			text, err := r.readBlob(payload.Result)
			if err != nil {
				return executionProgress{}, Outcome{}, false, err
			}
			r.childResults = append(r.childResults, text)
		case *eventlog.ChildCompletedPayload:
			if payload != nil {
				text, err := r.readBlob(payload.Result)
				if err != nil {
					return executionProgress{}, Outcome{}, false, err
				}
				r.childResults = append(r.childResults, text)
			}
		case eventlog.SessionFinishedPayload:
			terminal = true
			outcome = Outcome{Status: payload.Status, StopReason: payload.StopReason, Result: r.lastResult.Text, ResultRef: r.lastResult.Ref, Turns: len(r.conversation)}
		case *eventlog.SessionFinishedPayload:
			if payload != nil {
				terminal = true
				outcome = Outcome{Status: payload.Status, StopReason: payload.StopReason, Result: r.lastResult.Text, ResultRef: r.lastResult.Ref, Turns: len(r.conversation)}
			}
		}
	}
	if !sessionStarted {
		return executionProgress{}, Outcome{}, false, errors.New("session has no session.started event")
	}
	if terminal {
		return progress, outcome, true, nil
	}
	open := openStartedTurns(started, finished)
	if len(open) > 1 {
		return executionProgress{}, Outcome{}, false, errors.New("session has more than one unfinished turn")
	}
	if len(open) == 1 {
		turn := open[0]
		progress.startedTurn = &turn
		for attempt, complete := range attempts[turnKey(turn.ActorID, turn.Round)] {
			if !complete {
				progress.abandoned = append(progress.abandoned, AbandonedAttempt{ActorID: turn.ActorID, Attempt: attempt})
			}
		}
		sort.Slice(progress.abandoned, func(left, right int) bool {
			return progress.abandoned[left].Attempt < progress.abandoned[right].Attempt
		})
	}
	return progress, Outcome{}, false, nil
}

func (r *runner) replayFinishedTurn(payload eventlog.TurnFinishedPayload, roles map[string]eventlog.Role, finished map[string]bool, progress *executionProgress) error {
	key := turnKey(payload.ActorID, payload.Round)
	role, exists := roles[key]
	if !exists {
		return fmt.Errorf("turn.finished for %s has no turn.started", key)
	}
	finished[key] = true
	text, err := r.readBlob(payload.Content)
	if err != nil {
		return err
	}
	actor, err := r.actor(payload.ActorID)
	if err != nil {
		return err
	}
	r.recordCompletedTurn(actor, role, completedTurn{Text: text, Ref: payload.Content})
	switch role {
	case eventlog.ParticipantRole:
		if r.sess.Plan.Schedule.Kind == "dialogue" {
			progress.participantTurns++
		} else {
			progress.sequenceTurns++
		}
	case eventlog.FacilitatorRole:
		r.ledger = parseLedger(text, r.ledger)
	case eventlog.ReducerRole:
		progress.reducerDone = true
	}
	return nil
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

func openStartedTurns(started map[string]startedTurn, finished map[string]bool) []startedTurn {
	open := make([]startedTurn, 0, len(started))
	for key, turn := range started {
		if !finished[key] {
			open = append(open, turn)
		}
	}
	sort.Slice(open, func(left, right int) bool {
		if open[left].Round == open[right].Round {
			return open[left].ActorID < open[right].ActorID
		}
		return open[left].Round < open[right].Round
	})
	return open
}

func turnKey(actorID string, round int) string {
	return actorID + "\x00" + fmt.Sprintf("%d", round)
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}

var ledgerObject = regexp.MustCompile(`(?s)\{.*\}`)

func parseLedger(raw string, fallback model.Ledger) model.Ledger {
	candidates := append([]string{strings.TrimSpace(raw)}, ledgerObject.FindAllString(raw, -1)...)
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(candidate), &value); err != nil {
			continue
		}
		if hasLedgerShape(value) {
			return model.ParseLedger(value)
		}
	}
	return fallback
}

func hasLedgerShape(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"settled", "contested", "withdrawn"} {
		if _, exists := object[key].([]any); !exists {
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
	return (counts.Settled > 0 || counts.Withdrawn > 0) && counts.Contested == 0
}

func hasNoLedgerSignal(turns []conversationTurn, ledger model.Ledger) bool {
	if len(turns) < 4 || !ledger.IsEmpty() {
		return false
	}
	last := turns[len(turns)-1]
	previous := turns[len(turns)-2]
	return last.ActorID != previous.ActorID
}

func hasDoneSignal(text string) bool {
	lowered := strings.ToLower(text)
	for _, signal := range []string{
		"task is complete",
		"work is complete",
		"no further changes",
		"ready to merge",
		"nothing else to add",
		"this covers everything",
	} {
		if strings.Contains(lowered, signal) {
			return true
		}
	}
	return false
}
