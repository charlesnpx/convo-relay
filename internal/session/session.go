// Package session owns the immutable v2 session.json plan and identity file.
package session

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charlesnpx/convo-relay/v2/internal/blobstore"
	"github.com/charlesnpx/convo-relay/v2/internal/eventlog"
	relayplan "github.com/charlesnpx/convo-relay/v2/plan"
)

// The aliases below preserve the internal package's existing spelling while
// keeping the public plan package as the sole owner of each document type.
type Plan = relayplan.Plan
type Actor = relayplan.Actor
type Schedule = relayplan.Schedule
type Timeouts = relayplan.Timeouts
type Facilitator = relayplan.Facilitator
type Reducer = relayplan.Reducer
type ProviderRetry = relayplan.ProviderRetry
type Workspace = relayplan.Workspace
type Input = relayplan.Input
type ChildPolicy = relayplan.ChildPolicy
type Result = relayplan.Result
type Instructions = relayplan.Instructions
type TurnInstruction = relayplan.TurnInstruction
type Lifecycle = relayplan.Lifecycle

const (
	ModeAdversarial = relayplan.ModeAdversarial
	ModeCooperative = relayplan.ModeCooperative
	ModeSteelman    = relayplan.ModeSteelman
)

const (
	InvestigationAuto        = relayplan.InvestigationAuto
	InvestigationNormal      = relayplan.InvestigationNormal
	InvestigationContextOnly = relayplan.InvestigationContextOnly
)

const (
	ProvenanceOrdinary = relayplan.ProvenanceOrdinary
	ProvenanceRecipe   = relayplan.ProvenanceRecipe
	ProvenanceChild    = relayplan.ProvenanceChild
	ProvenanceSupplied = relayplan.ProvenanceSupplied
)

const (
	PlanKind        = relayplan.PlanKind
	SchemaVersion   = relayplan.SchemaVersion
	SessionFilename = "session.json"
)

// Session is a decoded immutable plan plus its machine-local directory. Root
// is never emitted by Plan's JSON representation.
type Session struct {
	Root   string
	Plan   Plan
	Digest string
}

// CreateOptions controls managed-root creation. There is intentionally no
// target-session-directory field: Create always uses os.MkdirTemp below
// RelayHome and cannot initialize a caller-supplied existing directory.
type CreateOptions struct {
	RelayHome  string
	Prefix     string
	Plan       Plan
	BlobLimits blobstore.Limits
}

// Create makes a new managed session under relayHome using the default prefix.
func Create(relayHome string, plan Plan) (*Session, error) {
	return CreateWithOptions(CreateOptions{RelayHome: relayHome, Plan: plan})
}

// CreateWithOptions makes a new managed session and writes session.json once.
func CreateWithOptions(options CreateOptions) (*Session, error) {
	relayHome := strings.TrimSpace(options.RelayHome)
	if relayHome == "" {
		return nil, errors.New("relay home is required")
	}
	if err := os.MkdirAll(relayHome, 0o700); err != nil {
		return nil, err
	}
	prefix := strings.TrimSpace(options.Prefix)
	if prefix == "" {
		prefix = "session-"
	}
	root, err := os.MkdirTemp(relayHome, prefix)
	if err != nil {
		return nil, err
	}

	plan := options.Plan
	if plan.SessionID == "" {
		plan.SessionID = filepath.Base(root)
	}
	if err := relayplan.Validate(plan); err != nil {
		return nil, err
	}
	if err := eventlog.ValidatePortableValue(portableProjection(plan), root, relayHome); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "runtime"), 0o700); err != nil {
		return nil, err
	}
	if _, err := blobstore.New(root, options.BlobLimits); err != nil {
		return nil, err
	}
	body, err := eventlog.SemanticJSONBytes(plan)
	if err != nil {
		return nil, err
	}
	if err := writeSessionOnce(root, body); err != nil {
		return nil, err
	}
	if err := eventlog.Initialize(root); err != nil {
		return nil, err
	}
	if err := syncDirectory(root); err != nil {
		return nil, err
	}
	digest, err := eventlog.SemanticJSONDigestBytes(body)
	if err != nil {
		return nil, err
	}
	return &Session{Root: root, Plan: plan, Digest: digest}, nil
}

func writeSessionOnce(root string, body []byte) error {
	filename := filepath.Join(root, SessionFilename)
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := writeAll(file, body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// Open reads a session.json file, verifies it is strict semantic JSON, checks
// its typed shape, and returns its semantic-json digest.
func Open(root string) (*Session, error) {
	cleaned := filepath.Clean(root)
	filename := filepath.Join(cleaned, SessionFilename)
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("session.json must be a regular file")
	}
	body, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var plan Plan
	if err := eventlog.DecodeCanonicalJSON(body, &plan); err != nil {
		return nil, err
	}
	if err := relayplan.Validate(plan); err != nil {
		return nil, err
	}
	if err := eventlog.ValidatePortableValue(portableProjection(plan), cleaned); err != nil {
		return nil, err
	}
	digest, err := eventlog.SemanticJSONDigestBytes(body)
	if err != nil {
		return nil, err
	}
	return &Session{Root: cleaned, Plan: plan, Digest: digest}, nil
}

// BlobStore opens the session's physical blob store without creating a second
// authority or exposing its root in Plan.
func (s *Session) BlobStore(limits blobstore.Limits) (*blobstore.Store, error) {
	if s == nil {
		return nil, errors.New("nil session")
	}
	return blobstore.Open(s.Root, limits)
}

// EventWriter opens the append-only authority. Supplying the blob store is
// required for payload-bearing events, enforcing blob-before-event ordering.
func (s *Session) EventWriter(blobs eventlog.BlobVerifier) (*eventlog.Writer, error) {
	if s == nil {
		return nil, errors.New("nil session")
	}
	return eventlog.OpenWriter(s.Root, blobs)
}

// ValidatePlan delegates to the public plan validator for internal callers
// that still use the session package boundary.
func ValidatePlan(value Plan) error { return relayplan.Validate(value) }

// PlanDigest delegates to the public plan digest for internal event bindings.
func PlanDigest(value Plan) (string, error) { return relayplan.Digest(value) }

// CanonicalBytes delegates to the public plan encoding for internal callers
// that still write session.json or embedded child-plan payloads.
func CanonicalBytes(value Plan) ([]byte, error) { return relayplan.CanonicalBytes(value) }

// BlobRefs delegates to the public plan reference walk for internal storage
// and export callers.
func BlobRefs(value Plan) []relayplan.BlobRef { return relayplan.BlobRefs(value) }

// ValidateEventBindings checks the optional session.started binding when it is
// present in a log. Empty, newly-created logs remain valid; a recorded start
// must bind this exact immutable plan digest and session identity.
func ValidateEventBindings(plan Plan, events []eventlog.Event) error {
	digest, err := relayplan.Digest(plan)
	if err != nil {
		return err
	}
	for _, event := range events {
		var started eventlog.SessionStartedPayload
		switch payload := event.Payload.(type) {
		case eventlog.SessionStartedPayload:
			started = payload
		case *eventlog.SessionStartedPayload:
			if payload == nil {
				continue
			}
			started = *payload
		default:
			continue
		}
		if started.PlanDigest != digest {
			return errors.New("session.started plan_digest does not bind session.json")
		}
		if started.SessionID != plan.SessionID {
			return errors.New("session.started session_id does not bind session.json")
		}
	}
	return nil
}

func writeAll(file *os.File, body []byte) error {
	for len(body) > 0 {
		count, err := file.Write(body)
		if err != nil {
			return err
		}
		if count == 0 {
			return errors.New("short session.json write")
		}
		body = body[count:]
	}
	return nil
}

func syncDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// portableProjection returns the plan with Task cleared, for the portability walk
// only. Task is operator-authored prose that may deliberately name a path; it is
// identical on every machine, so it cannot break relocation of a bundle. Every
// other field stays bound, including the recipe id and every input name.
func portableProjection(plan Plan) Plan {
	plan.Task = ""
	return plan
}
