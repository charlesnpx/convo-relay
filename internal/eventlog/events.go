package eventlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
)

// FormatV1 identifies the canonical append-only event envelope.
const FormatV1 = "relay.event/v1"

// Type is one of the concrete v2 event type names.
type Type string

const (
	SessionStarted    Type = "session.started"
	SessionFinished   Type = "session.finished"
	TurnBudgetGranted Type = "turn_budget.granted"
	TurnStarted       Type = "turn.started"
	TurnFinished      Type = "turn.finished"
	AttemptStarted    Type = "attempt.started"
	AttemptFinished   Type = "attempt.finished"
	ProviderFailed    Type = "provider.failed"
	ChildRequested    Type = "child.requested"
	ChildDecided      Type = "child.decided"
	ChildCompleted    Type = "child.completed"
	SteeringQueued    Type = "steering.queued"
	SteeringApplied   Type = "steering.applied"
	InputIngested     Type = "input.ingested"
	WorkspacePrepared Type = "workspace.prepared"
	ResultProduced    Type = "result.produced"
)

// Role is deliberately closed: a transcript can be reconstructed without
// guessing which actor emitted a participant, facilitator, or reducer turn.
type Role string

const (
	ParticipantRole Role = "participant"
	FacilitatorRole Role = "facilitator"
	ReducerRole     Role = "reducer"
)

// Event is the typed in-memory form of one canonical JSONL record.
type Event struct {
	Seq     uint64
	EventID string
	Time    time.Time
	Type    Type
	Payload Payload
}

// Payload is a concrete member of the event registry. The unexported marker
// prevents arbitrary untyped maps from entering the engine surface.
type Payload interface {
	eventType() Type
	validate() error
}

// SessionStartedPayload records the immutable plan/session binding.
type SessionStartedPayload struct {
	PlanDigest string `json:"plan_digest"`
	SessionID  string `json:"session_id"`
}

func (SessionStartedPayload) eventType() Type { return SessionStarted }
func (p SessionStartedPayload) validate() error {
	if err := validateDigest("plan_digest", p.PlanDigest); err != nil {
		return err
	}
	return validateToken("session_id", p.SessionID)
}

// SessionFinishedPayload records a terminal state and user-visible reason.
type SessionFinishedPayload struct {
	Status     string `json:"status"`
	StopReason string `json:"stop_reason"`
}

func (SessionFinishedPayload) eventType() Type { return SessionFinished }
func (p SessionFinishedPayload) validate() error {
	if err := validateToken("status", p.Status); err != nil {
		return err
	}
	return validateText("stop_reason", p.StopReason)
}

// TurnBudgetGrantedPayload records an explicit operator grant that extends
// the immutable plan's participant-turn budget without changing the plan. A
// prompted resume carries its durable steering reference here so one event
// records the grant and the prompt together.
type TurnBudgetGrantedPayload struct {
	GrantedBy string             `json:"granted_by"`
	Turns     int                `json:"turns"`
	Prompt    *blobstore.BlobRef `json:"prompt,omitempty"`
}

func (TurnBudgetGrantedPayload) eventType() Type { return TurnBudgetGranted }
func (p TurnBudgetGrantedPayload) validate() error {
	if err := validateToken("granted_by", p.GrantedBy); err != nil {
		return err
	}
	if p.Turns < 1 {
		return errors.New("turn_budget.granted turns must be positive")
	}
	if p.Prompt != nil {
		return blobstore.ValidateRef(*p.Prompt)
	}
	return nil
}

type TurnStartedPayload struct {
	ActorID string `json:"actor_id"`
	Round   int    `json:"round"`
	Role    Role   `json:"role"`
}

func (TurnStartedPayload) eventType() Type { return TurnStarted }
func (p TurnStartedPayload) validate() error {
	if err := validateActorRound(p.ActorID, p.Round); err != nil {
		return err
	}
	switch p.Role {
	case ParticipantRole, FacilitatorRole, ReducerRole:
		return nil
	default:
		return fmt.Errorf("turn.started role must be participant, facilitator, or reducer")
	}
}

type TurnFinishedPayload struct {
	ActorID string            `json:"actor_id"`
	Round   int               `json:"round"`
	Content blobstore.BlobRef `json:"content"`
}

func (TurnFinishedPayload) eventType() Type { return TurnFinished }
func (p TurnFinishedPayload) validate() error {
	if err := validateActorRound(p.ActorID, p.Round); err != nil {
		return err
	}
	return blobstore.ValidateRef(p.Content)
}

type AttemptStartedPayload struct {
	ActorID string `json:"actor_id"`
	Attempt int    `json:"attempt"`
}

func (AttemptStartedPayload) eventType() Type { return AttemptStarted }
func (p AttemptStartedPayload) validate() error {
	return validateAttempt(p.ActorID, p.Attempt)
}

type AttemptFinishedPayload struct {
	ActorID           string            `json:"actor_id"`
	Attempt           int               `json:"attempt"`
	Outcome           string            `json:"outcome"`
	ProviderOutcome   string            `json:"provider_outcome"`
	ProviderSessionID string            `json:"provider_session_id"`
	Content           blobstore.BlobRef `json:"content"`
}

func (AttemptFinishedPayload) eventType() Type { return AttemptFinished }
func (p AttemptFinishedPayload) validate() error {
	if err := validateAttempt(p.ActorID, p.Attempt); err != nil {
		return err
	}
	if err := validateToken("outcome", p.Outcome); err != nil {
		return err
	}
	if p.ProviderOutcome != "" && !validAttemptProviderOutcome(p.ProviderOutcome) {
		return fmt.Errorf("attempt.finished provider_outcome %q is unsupported", p.ProviderOutcome)
	}
	if err := validateText("provider_session_id", p.ProviderSessionID); err != nil {
		return err
	}
	return blobstore.ValidateRef(p.Content)
}

func validAttemptProviderOutcome(value string) bool {
	switch value {
	case "completed", "failed", "recovered", "timed_out", "stalled",
		"timed_out_stalled", "timed_out_recovered", "stalled_recovered",
		"timed_out_stalled_recovered":
		return true
	default:
		return false
	}
}

type ProviderFailedPayload struct {
	ActorID         string `json:"actor_id"`
	Backend         string `json:"backend"`
	Category        string `json:"category"`
	Retryable       bool   `json:"retryable"`
	Attempts        int    `json:"attempts"`
	RemediationCode string `json:"remediation_code"`
	SanitizedDetail string `json:"sanitized_detail"`
}

func (ProviderFailedPayload) eventType() Type { return ProviderFailed }
func (p ProviderFailedPayload) validate() error {
	if err := validateToken("actor_id", p.ActorID); err != nil {
		return err
	}
	if err := validateToken("backend", p.Backend); err != nil {
		return err
	}
	if err := validateToken("category", p.Category); err != nil {
		return err
	}
	if p.Attempts < 1 {
		return errors.New("provider.failed attempts must be positive")
	}
	if err := validateToken("remediation_code", p.RemediationCode); err != nil {
		return err
	}
	if err := validateText("sanitized_detail", p.SanitizedDetail); err != nil {
		return err
	}
	if containsCredential(p.SanitizedDetail) {
		return errors.New("provider.failed sanitized_detail still contains credential-like data")
	}
	return nil
}

// ChildRequestedPayload includes request_id in addition to the prescribed
// fields. It is the correlation key required to derive correct graphs when
// several child requests are in flight concurrently.
type ChildRequestedPayload struct {
	RequestID        string            `json:"request_id"`
	RequesterActorID string            `json:"requester_actor_id"`
	RecipeID         string            `json:"recipe_id"`
	Question         blobstore.BlobRef `json:"question"`
}

func (ChildRequestedPayload) eventType() Type { return ChildRequested }
func (p ChildRequestedPayload) validate() error {
	if err := validateToken("request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateToken("requester_actor_id", p.RequesterActorID); err != nil {
		return err
	}
	if err := validateToken("recipe_id", p.RecipeID); err != nil {
		return err
	}
	return blobstore.ValidateRef(p.Question)
}

type ChildDecidedPayload struct {
	RequestID   string `json:"request_id"`
	Admitted    bool   `json:"admitted"`
	Reason      string `json:"reason"`
	BudgetState string `json:"budget_state"`
	// Plan is the canonical, immutable admitted child plan stored in the
	// parent's blob store. It is present exactly when admission succeeds so a
	// missing child root can be recreated without consulting a changed recipe
	// catalog.
	Plan *blobstore.BlobRef `json:"plan,omitempty"`
}

func (ChildDecidedPayload) eventType() Type { return ChildDecided }
func (p ChildDecidedPayload) validate() error {
	if err := validateToken("request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateText("reason", p.Reason); err != nil {
		return err
	}
	if err := validateText("budget_state", p.BudgetState); err != nil {
		return err
	}
	if !p.Admitted {
		if p.Plan != nil {
			return errors.New("child.decided plan is only valid for an admitted child")
		}
		return nil
	}
	if p.Plan == nil {
		return errors.New("child.decided admitted child requires a plan")
	}
	return blobstore.ValidateRef(*p.Plan)
}

type ChildCompletedPayload struct {
	RequestID      string            `json:"request_id"`
	ChildSessionID string            `json:"child_session_id"`
	Result         blobstore.BlobRef `json:"result"`
	Status         string            `json:"status"`
}

func (ChildCompletedPayload) eventType() Type { return ChildCompleted }
func (p ChildCompletedPayload) validate() error {
	if err := validateToken("request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateToken("child_session_id", p.ChildSessionID); err != nil {
		return err
	}
	if err := blobstore.ValidateRef(p.Result); err != nil {
		return err
	}
	switch p.Status {
	case "completed", "failed":
		return nil
	default:
		return errors.New("child.completed status must be completed or failed")
	}
}

type SteeringQueuedPayload struct {
	Prompt blobstore.BlobRef `json:"prompt"`
}

func (SteeringQueuedPayload) eventType() Type   { return SteeringQueued }
func (p SteeringQueuedPayload) validate() error { return blobstore.ValidateRef(p.Prompt) }

type SteeringAppliedPayload struct {
	Prompt blobstore.BlobRef `json:"prompt"`
	Round  int               `json:"round"`
}

func (SteeringAppliedPayload) eventType() Type { return SteeringApplied }
func (p SteeringAppliedPayload) validate() error {
	if p.Round < 1 {
		return errors.New("steering.applied round must be positive")
	}
	return blobstore.ValidateRef(p.Prompt)
}

type InputIngestedPayload struct {
	LogicalName string            `json:"logical_name"`
	Content     blobstore.BlobRef `json:"content"`
}

func (InputIngestedPayload) eventType() Type { return InputIngested }
func (p InputIngestedPayload) validate() error {
	if err := validateLogicalName(p.LogicalName); err != nil {
		return err
	}
	return blobstore.ValidateRef(p.Content)
}

type WorkspacePreparedPayload struct {
	Mode         string `json:"mode"`
	Commit       string `json:"commit"`
	TreeHash     string `json:"tree_hash"`
	RelativePath string `json:"relative_path,omitempty"`
}

func (WorkspacePreparedPayload) eventType() Type { return WorkspacePrepared }
func (p WorkspacePreparedPayload) validate() error {
	if p.Mode != "current" && p.Mode != "head-copy" {
		return errors.New("workspace.prepared mode must be current or head-copy")
	}
	if p.Mode == "head-copy" || p.Commit != "" || p.TreeHash != "" {
		if err := validateToken("commit", p.Commit); err != nil {
			return err
		}
		if err := validateToken("tree_hash", p.TreeHash); err != nil {
			return err
		}
	}
	return validateWorkspaceRelativePath(p.RelativePath)
}

func validateWorkspaceRelativePath(value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\\\\\r\n\x00") {
		return errors.New("relative_path must use portable slash-separated path components")
	}
	if path.IsAbs(value) {
		return errors.New("relative_path must be relative")
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return errors.New("relative_path must stay within the repository")
	}
	return nil
}

type ResultProducedPayload struct {
	Result            blobstore.BlobRef `json:"result"`
	Format            string            `json:"format"`
	ValidationOutcome string            `json:"validation_outcome"`
}

func (ResultProducedPayload) eventType() Type { return ResultProduced }
func (p ResultProducedPayload) validate() error {
	if err := blobstore.ValidateRef(p.Result); err != nil {
		return err
	}
	if err := validateToken("format", p.Format); err != nil {
		return err
	}
	return validateToken("validation_outcome", p.ValidationOutcome)
}

// NewEvent creates an unsequenced event. Writer.Append assigns the contiguous
// sequence and normalizes Time to UTC.
func NewEvent(eventID string, at time.Time, payload Payload) Event {
	typ := Type("")
	if payload != nil {
		typ = payload.eventType()
	}
	return Event{EventID: eventID, Time: at, Type: typ, Payload: payload}
}

// MarshalJSON emits the canonical wire envelope while retaining a typed
// Payload in memory.
func (event Event) MarshalJSON() ([]byte, error) {
	normalized, err := normalizeEvent(event, false)
	if err != nil {
		return nil, err
	}
	return SemanticJSONBytes(encodeWireEvent{
		Kind:    FormatV1,
		Seq:     normalized.Seq,
		EventID: normalized.EventID,
		Time:    normalized.Time,
		Type:    normalized.Type,
		Payload: normalized.Payload,
	})
}

// UnmarshalJSON decodes via the type registry rather than leaving a generic
// map at the engine boundary.
func (event *Event) UnmarshalJSON(body []byte) error {
	decoded, err := decodeEvent(body)
	if err != nil {
		return err
	}
	*event = decoded
	return nil
}

type wireEvent struct {
	Kind    string          `json:"kind"`
	Seq     json.RawMessage `json:"seq"`
	EventID string          `json:"event_id"`
	Time    time.Time       `json:"time"`
	Type    Type            `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type encodeWireEvent struct {
	Kind    string    `json:"kind"`
	Seq     uint64    `json:"seq"`
	EventID string    `json:"event_id"`
	Time    time.Time `json:"time"`
	Type    Type      `json:"type"`
	Payload Payload   `json:"payload"`
}

func decodeEvent(body []byte) (Event, error) {
	var wire wireEvent
	if _, err := ValidateCanonicalJSON(body); err != nil {
		return Event{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Event{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Event{}, err
	}
	if wire.Kind != FormatV1 {
		return Event{}, fmt.Errorf("event kind must be %s", FormatV1)
	}
	seq, err := decodeCanonicalUint64(wire.Seq)
	if err != nil {
		return Event{}, err
	}
	if len(wire.Payload) == 0 {
		return Event{}, errors.New("event payload is required")
	}
	payload, err := decodePayload(wire.Type, wire.Payload)
	if err != nil {
		return Event{}, err
	}
	return normalizeEvent(Event{
		Seq:     seq,
		EventID: wire.EventID,
		Time:    wire.Time,
		Type:    wire.Type,
		Payload: payload,
	}, true)
}

func decodeCanonicalUint64(body json.RawMessage) (uint64, error) {
	node, err := parseSemanticJSON(body)
	if err != nil {
		return 0, err
	}
	if node.kind != semanticNumber {
		return 0, errors.New("event seq must be a number")
	}
	plain, err := plainSemanticNumber(node.text)
	if err != nil {
		return 0, err
	}
	if strings.Contains(plain, ".") || strings.HasPrefix(plain, "-") {
		return 0, errors.New("event seq must be a non-negative integer")
	}
	return strconv.ParseUint(plain, 10, 64)
}

// CanonicalEventBytes is the one event serialization path used by append and
// by replay's byte-equality check.
func CanonicalEventBytes(event Event) ([]byte, error) {
	normalized, err := normalizeEvent(event, true)
	if err != nil {
		return nil, err
	}
	return SemanticJSONBytes(encodeWireEvent{
		Kind:    FormatV1,
		Seq:     normalized.Seq,
		EventID: normalized.EventID,
		Time:    normalized.Time,
		Type:    normalized.Type,
		Payload: normalized.Payload,
	})
}

func normalizeEvent(event Event, requireSequence bool, localValues ...string) (Event, error) {
	if event.Payload == nil {
		return Event{}, errors.New("event payload is required")
	}
	payload, err := normalizePayload(event.Payload)
	if err != nil {
		return Event{}, err
	}
	if event.Type == "" {
		event.Type = payload.eventType()
	}
	if event.Type != payload.eventType() {
		return Event{}, fmt.Errorf("event type %q does not match payload type %q", event.Type, payload.eventType())
	}
	if requireSequence && event.Seq == 0 {
		return Event{}, errors.New("event seq must be positive")
	}
	if err := validateToken("event_id", event.EventID); err != nil {
		return Event{}, err
	}
	if event.Time.IsZero() {
		return Event{}, errors.New("event time is required")
	}
	event.Time = event.Time.UTC()
	event.Payload = payload
	if err := payload.validate(); err != nil {
		return Event{}, err
	}
	if err := ValidatePortableValue(encodeWireEvent{
		Kind:    FormatV1,
		Seq:     event.Seq,
		EventID: event.EventID,
		Time:    event.Time,
		Type:    event.Type,
		Payload: event.Payload,
	}, localValues...); err != nil {
		return Event{}, err
	}
	return event, nil
}

func normalizePayload(payload Payload) (Payload, error) {
	switch value := payload.(type) {
	case SessionStartedPayload:
		return value, nil
	case *SessionStartedPayload:
		if value == nil {
			return nil, errors.New("nil session.started payload")
		}
		return *value, nil
	case SessionFinishedPayload:
		return value, nil
	case *SessionFinishedPayload:
		if value == nil {
			return nil, errors.New("nil session.finished payload")
		}
		return *value, nil
	case TurnBudgetGrantedPayload:
		return value, nil
	case *TurnBudgetGrantedPayload:
		if value == nil {
			return nil, errors.New("nil turn_budget.granted payload")
		}
		return *value, nil
	case TurnStartedPayload:
		return value, nil
	case *TurnStartedPayload:
		if value == nil {
			return nil, errors.New("nil turn.started payload")
		}
		return *value, nil
	case TurnFinishedPayload:
		return value, nil
	case *TurnFinishedPayload:
		if value == nil {
			return nil, errors.New("nil turn.finished payload")
		}
		return *value, nil
	case AttemptStartedPayload:
		return value, nil
	case *AttemptStartedPayload:
		if value == nil {
			return nil, errors.New("nil attempt.started payload")
		}
		return *value, nil
	case AttemptFinishedPayload:
		return value, nil
	case *AttemptFinishedPayload:
		if value == nil {
			return nil, errors.New("nil attempt.finished payload")
		}
		return *value, nil
	case ProviderFailedPayload:
		return value, nil
	case *ProviderFailedPayload:
		if value == nil {
			return nil, errors.New("nil provider.failed payload")
		}
		return *value, nil
	case ChildRequestedPayload:
		return value, nil
	case *ChildRequestedPayload:
		if value == nil {
			return nil, errors.New("nil child.requested payload")
		}
		return *value, nil
	case ChildDecidedPayload:
		return value, nil
	case *ChildDecidedPayload:
		if value == nil {
			return nil, errors.New("nil child.decided payload")
		}
		return *value, nil
	case ChildCompletedPayload:
		return value, nil
	case *ChildCompletedPayload:
		if value == nil {
			return nil, errors.New("nil child.completed payload")
		}
		return *value, nil
	case SteeringQueuedPayload:
		return value, nil
	case *SteeringQueuedPayload:
		if value == nil {
			return nil, errors.New("nil steering.queued payload")
		}
		return *value, nil
	case SteeringAppliedPayload:
		return value, nil
	case *SteeringAppliedPayload:
		if value == nil {
			return nil, errors.New("nil steering.applied payload")
		}
		return *value, nil
	case InputIngestedPayload:
		return value, nil
	case *InputIngestedPayload:
		if value == nil {
			return nil, errors.New("nil input.ingested payload")
		}
		return *value, nil
	case WorkspacePreparedPayload:
		return value, nil
	case *WorkspacePreparedPayload:
		if value == nil {
			return nil, errors.New("nil workspace.prepared payload")
		}
		return *value, nil
	case ResultProducedPayload:
		return value, nil
	case *ResultProducedPayload:
		if value == nil {
			return nil, errors.New("nil result.produced payload")
		}
		return *value, nil
	default:
		return nil, fmt.Errorf("unsupported event payload %T", payload)
	}
}

var payloadRegistry = map[Type]func() Payload{
	SessionStarted:    func() Payload { return &SessionStartedPayload{} },
	SessionFinished:   func() Payload { return &SessionFinishedPayload{} },
	TurnBudgetGranted: func() Payload { return &TurnBudgetGrantedPayload{} },
	TurnStarted:       func() Payload { return &TurnStartedPayload{} },
	TurnFinished:      func() Payload { return &TurnFinishedPayload{} },
	AttemptStarted:    func() Payload { return &AttemptStartedPayload{} },
	AttemptFinished:   func() Payload { return &AttemptFinishedPayload{} },
	ProviderFailed:    func() Payload { return &ProviderFailedPayload{} },
	ChildRequested:    func() Payload { return &ChildRequestedPayload{} },
	ChildDecided:      func() Payload { return &ChildDecidedPayload{} },
	ChildCompleted:    func() Payload { return &ChildCompletedPayload{} },
	SteeringQueued:    func() Payload { return &SteeringQueuedPayload{} },
	SteeringApplied:   func() Payload { return &SteeringAppliedPayload{} },
	InputIngested:     func() Payload { return &InputIngestedPayload{} },
	WorkspacePrepared: func() Payload { return &WorkspacePreparedPayload{} },
	ResultProduced:    func() Payload { return &ResultProducedPayload{} },
}

// UnknownTypeError prevents a newer producer from being silently ignored by
// an older reader.
type UnknownTypeError struct {
	Type Type
}

func (e *UnknownTypeError) Error() string { return fmt.Sprintf("unknown event type %q", e.Type) }

func decodePayload(typ Type, body json.RawMessage) (Payload, error) {
	factory, found := payloadRegistry[typ]
	if !found {
		return nil, &UnknownTypeError{Type: typ}
	}
	target := factory()
	if err := DecodeCanonicalJSON(body, target); err != nil {
		return nil, err
	}
	return normalizePayload(target)
}

// BlobRefs returns every direct blob reference carried by the typed events.
// Duplicates are preserved so callers may retain event-level provenance.
func BlobRefs(events []Event) []blobstore.BlobRef {
	refs := []blobstore.BlobRef{}
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case TurnBudgetGrantedPayload:
			if payload.Prompt != nil {
				refs = append(refs, *payload.Prompt)
			}
		case TurnFinishedPayload:
			refs = append(refs, payload.Content)
		case AttemptFinishedPayload:
			refs = append(refs, payload.Content)
		case ChildRequestedPayload:
			refs = append(refs, payload.Question)
		case ChildDecidedPayload:
			if payload.Plan != nil {
				refs = append(refs, *payload.Plan)
			}
		case ChildCompletedPayload:
			refs = append(refs, payload.Result)
		case SteeringQueuedPayload:
			refs = append(refs, payload.Prompt)
		case SteeringAppliedPayload:
			refs = append(refs, payload.Prompt)
		case InputIngestedPayload:
			refs = append(refs, payload.Content)
		case ResultProducedPayload:
			refs = append(refs, payload.Result)
		}
	}
	return refs
}

func validateActorRound(actor string, round int) error {
	if err := validateToken("actor_id", actor); err != nil {
		return err
	}
	if round < 1 {
		return errors.New("round must be positive")
	}
	return nil
}

func validateAttempt(actor string, attempt int) error {
	if err := validateToken("actor_id", actor); err != nil {
		return err
	}
	if attempt < 1 {
		return errors.New("attempt must be positive")
	}
	return nil
}

func validateToken(label string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s contains a control character", label)
	}
	return nil
}

func validateText(label string, value string) error {
	if strings.ContainsAny(value, "\x00") {
		return fmt.Errorf("%s contains a NUL", label)
	}
	return nil
}

func validateLogicalName(value string) error {
	if err := validateToken("logical_name", value); err != nil {
		return err
	}
	if strings.Contains(value, "/") || strings.Contains(value, "\\") || value == "." || value == ".." {
		return errors.New("logical_name must not be a path")
	}
	return nil
}

func validateDigest(label string, value string) error {
	if len(value) != len(digestPrefix)+64 || !strings.HasPrefix(value, digestPrefix) {
		return fmt.Errorf("%s must be a sha256 digest", label)
	}
	for _, character := range value[len(digestPrefix):] {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return fmt.Errorf("%s must be a sha256 digest", label)
	}
	return nil
}

var credentialPattern = regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret|authorization)\s*(?:=|:|\s)\s*[^\s,;]+`)

func containsCredential(value string) bool { return credentialPattern.MatchString(value) }

// SanitizeProviderDetail redacts the credential-shaped fragments that callers
// may otherwise receive from provider adapters. Append additionally rejects
// unsanitized credential-like text, so redaction cannot be accidentally lost.
func SanitizeProviderDetail(value string) string {
	return credentialPattern.ReplaceAllString(value, "[redacted]")
}

// Equal compares events by their canonical wire representation.
func (event Event) Equal(other Event) bool {
	left, leftErr := CanonicalEventBytes(event)
	right, rightErr := CanonicalEventBytes(other)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}
