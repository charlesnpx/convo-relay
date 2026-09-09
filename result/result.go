// Package result defines the typed JSON document emitted by convo-relay run
// --json and derived by relayv2.BuildReport.
package result

import (
	"errors"
	"fmt"
	"strings"
)

// Result is the typed run report returned by the relay. It is invalid when a
// required report field is absent, a count is negative, or a nested projection
// does not describe the report shape emitted by BuildReport.
type Result struct {
	// SessionID identifies the run session; an empty or whitespace-only value
	// makes the result invalid.
	SessionID string `json:"session_id"`
	// SessionDir identifies the local session directory used by the CLI; an empty
	// or whitespace-only value makes the result invalid.
	SessionDir string `json:"session_dir"`
	// ExecutionKind identifies ordinary, recipe, child, or supplied execution; an
	// empty value makes the result invalid.
	ExecutionKind string `json:"execution_kind"`
	// RecipeID identifies the source recipe when one exists; it may be empty for
	// non-recipe reports and is not independently invalid.
	RecipeID string `json:"recipe_id"`
	// Task is the operator task recorded in the plan; an empty or whitespace-only
	// value makes the result invalid.
	Task string `json:"task"`
	// Title is the report title, currently the task text; an empty or
	// whitespace-only value makes the result invalid.
	Title string `json:"title"`
	// Mode is the plan's relay mode; an empty value makes the result invalid.
	Mode string `json:"mode"`
	// InvestigationMode is the plan's investigation level; an empty value makes
	// the result invalid.
	InvestigationMode string `json:"investigation_mode"`
	// TimeoutSeconds is the configured provider-turn timeout; a non-positive
	// value makes the result invalid.
	TimeoutSeconds int `json:"timeout_seconds"`
	// StallTimeoutSeconds is the configured provider stall timeout; a non-positive
	// value makes the result invalid.
	StallTimeoutSeconds int `json:"stall_timeout_seconds"`
	// Status is the public run status; an empty value makes the result invalid.
	Status string `json:"status"`
	// StopReason explains why execution stopped and may be empty for a running
	// report; an empty value is therefore valid.
	StopReason string `json:"stop_reason"`
	// Summary contains aggregate run counts; missing status or mode, or negative
	// counts, makes the result invalid.
	Summary Summary `json:"summary"`
	// Transcript contains the ordered participant transcript; an entry with a
	// non-positive round, empty source, or empty mode makes the result invalid.
	Transcript []TranscriptEntry `json:"transcript"`
	// TranscriptPayload repeats Transcript for export consumers; its entries must
	// have the same valid shape as Transcript entries.
	TranscriptPayload []TranscriptEntry `json:"transcript_payload"`
	// Result is the textual result payload; it may be empty before a result is
	// produced and is not independently invalid.
	Result string `json:"result"`
	// ResultSource identifies the plan result source; an empty value makes the
	// result invalid.
	ResultSource string `json:"result_source"`
	// ValidationStatus reports pending, validated, or invalid result validation;
	// an empty value is accepted for compatibility with a report before
	// validation.
	ValidationStatus string `json:"validation_status"`
	// ReducerAttempts records reducer invocation count; a negative count makes
	// the result invalid.
	ReducerAttempts Count `json:"reducer_attempts"`
	// Recipe contains the recipe projection and is empty for non-recipe plans; an
	// empty ID is valid for such reports.
	Recipe Recipe `json:"recipe"`
	// Source records the plan provenance; an empty value makes the result invalid.
	Source string `json:"source"`
	// Slots describes participant provider slots; a slot without an ID or backend
	// makes the result invalid.
	Slots []Slot `json:"slots"`
	// ActualRounds is the number of participant turns completed; a negative value
	// makes the result invalid.
	ActualRounds int `json:"actual_rounds"`
	// ActualParticipantTurns repeats ActualRounds for the CLI contract; a
	// negative value makes the result invalid.
	ActualParticipantTurns int `json:"actual_participant_turns"`
	// ParticipantTurns is the configured participant-turn limit; a negative value
	// makes the result invalid.
	ParticipantTurns int `json:"participant_turns"`
	// MaxRounds is the configured limit plus any granted turns; a negative value
	// makes the result invalid.
	MaxRounds int `json:"max_rounds"`
	// RoundLimitMode identifies fixed or auto round limits; an empty value makes
	// the result invalid.
	RoundLimitMode string `json:"round_limit_mode"`
	// ProviderFailures lists classified provider failures; a negative attempt
	// count makes an entry invalid.
	ProviderFailures []ProviderFailure `json:"provider_failures"`
	// ProviderRetry records the plan retry mode; an empty value makes the result
	// invalid.
	ProviderRetry string `json:"provider_retry"`
	// WorkspaceContentSource identifies working-tree or committed-HEAD content;
	// an empty value makes the result invalid.
	WorkspaceContentSource string `json:"workspace_content_source"`
	// WorkingTreeChangesIncluded reports whether working-tree changes were used;
	// both boolean values are valid.
	WorkingTreeChangesIncluded bool `json:"working_tree_changes_included"`
	// Diagnostics contains recovery and blob diagnostics; an abandoned attempt
	// with a non-positive number makes the result invalid.
	Diagnostics Diagnostics `json:"diagnostics"`
	// Root contains the root recipe projection for recipe and child runs; when
	// present, missing identity fields or negative counts make the result invalid.
	Root *Root `json:"root,omitempty"`
}

// Summary contains aggregate counts for a run report. It is invalid when status
// or mode is empty, or when any count is negative.
type Summary struct {
	// Status repeats the current report status; an empty value makes Summary
	// invalid.
	Status string `json:"status"`
	// Mode repeats the plan relay mode; an empty value makes Summary invalid.
	Mode string `json:"mode"`
	// Agents lists participant backend names in plan order; an empty list is
	// allowed for a report with no participant projection.
	Agents []string `json:"agents"`
	// ConfiguredRounds is the plan's configured participant-turn count; a
	// negative value makes Summary invalid.
	ConfiguredRounds int `json:"configured_rounds"`
	// MaxRounds is the effective participant-turn limit; a negative value makes
	// Summary invalid.
	MaxRounds int `json:"max_rounds"`
	// ActualRounds is the number of participant turns completed; a negative value
	// makes Summary invalid.
	ActualRounds int `json:"actual_rounds"`
	// FilteredRounds is the count after any report projection filter; a negative
	// value makes Summary invalid.
	FilteredRounds int `json:"filtered_rounds"`
	// LedgerCounts contains settled, contested, and withdrawn item counts; a
	// negative count makes Summary invalid.
	LedgerCounts LedgerCounts `json:"ledger_counts"`
}

// LedgerCounts contains the latest facilitator ledger counts. It is invalid
// when any count is negative.
type LedgerCounts struct {
	// Settled is the number of settled ledger items; a negative value makes the
	// counts invalid.
	Settled int `json:"settled"`
	// Contested is the number of contested ledger items; a negative value makes
	// the counts invalid.
	Contested int `json:"contested"`
	// Withdrawn is the number of withdrawn ledger items; a negative value makes
	// the counts invalid.
	Withdrawn int `json:"withdrawn"`
}

// TranscriptEntry is one participant transcript entry and its latest ledger.
// It is invalid when Round is non-positive, From is empty, or Mode is empty.
type TranscriptEntry struct {
	// Round is the participant round number; a non-positive value makes the entry
	// invalid.
	Round int `json:"round"`
	// From identifies the actor or synthetic child source; an empty value makes
	// the entry invalid.
	From string `json:"from"`
	// Content is the resolved turn text; an empty value is allowed for an empty
	// provider response.
	Content string `json:"content"`
	// Mode is the relay mode used for the turn; an empty value makes the entry
	// invalid.
	Mode string `json:"mode"`
	// Ledger is the latest settled, contested, and withdrawn ledger state; its
	// lists may be empty.
	Ledger Ledger `json:"ledger"`
	// FacilitatorProviderResult identifies the facilitator provider when present;
	// a nil value is valid when no facilitator projection exists.
	FacilitatorProviderResult *ProviderResult `json:"facilitator_provider_result,omitempty"`
	// Synthetic reports a child result projected into the parent transcript; both
	// boolean values are valid.
	Synthetic bool `json:"synthetic,omitempty"`
}

// Ledger contains the facilitator's three ordered item lists. Empty lists are
// valid because a report may have no settled, contested, or withdrawn items.
type Ledger struct {
	// Settled lists settled items; nil or empty is valid.
	Settled []string `json:"settled"`
	// Contested lists contested items; nil or empty is valid.
	Contested []string `json:"contested"`
	// Withdrawn lists withdrawn items; nil or empty is valid.
	Withdrawn []string `json:"withdrawn"`
}

// ProviderResult identifies the provider used for a facilitator projection. A
// present value with an empty backend or actor ID is invalid.
type ProviderResult struct {
	// Backend identifies the provider backend; an empty value makes the provider
	// result invalid.
	Backend string `json:"backend"`
	// ActorID identifies the facilitator actor; an empty value makes the provider
	// result invalid.
	ActorID string `json:"actor_id"`
}

// Count records a single invocation count. A negative count is invalid.
type Count struct {
	// Count is the non-negative invocation count; a negative value is invalid.
	Count int `json:"count"`
}

// Recipe is the report's optional recipe identity projection. An empty ID is
// valid for ordinary or supplied plans.
type Recipe struct {
	// ID identifies the recipe and is empty for a non-recipe plan; it is not
	// independently invalid when empty.
	ID string `json:"id,omitempty"`
}

// Slot describes one participant provider slot and its continuation state. It
// is invalid when SlotID or Backend is empty.
type Slot struct {
	// SlotID identifies the actor slot; an empty value makes the slot invalid.
	SlotID string `json:"slot_id"`
	// ProfileID identifies the configured provider profile; an empty value is
	// allowed when the plan did not name a profile.
	ProfileID string `json:"profile_id"`
	// Backend identifies the provider backend; an empty value makes the slot
	// invalid.
	Backend string `json:"backend"`
	// Label is the display label for the backend; an empty value is allowed in a
	// partial provider projection.
	Label string `json:"label"`
	// Model is the configured provider model; an empty value is allowed when the
	// backend selected its default.
	Model string `json:"model"`
	// Effort is the configured provider effort; an empty value is allowed when the
	// backend selected its default.
	Effort string `json:"effort"`
	// State contains the backend-specific continuation identifier; an empty state
	// is valid before a continuation is established.
	State SlotState `json:"state"`
}

// SlotState contains the known provider continuation identifiers. At most one
// field is populated by the current report projection; an empty state is valid
// before continuation.
type SlotState struct {
	// ThreadID is the Codex continuation identifier; an empty value is valid when
	// Codex has not established continuation.
	ThreadID string `json:"thread_id,omitempty"`
	// SessionRef is the Gemini continuation identifier; an empty value is valid
	// when Gemini has not established continuation.
	SessionRef string `json:"session_ref,omitempty"`
	// SessionID is the Claude continuation identifier; an empty value is valid
	// when Claude has not established continuation.
	SessionID string `json:"session_id,omitempty"`
}

// ProviderFailure records one classified provider failure. It is invalid when
// Attempts is negative.
type ProviderFailure struct {
	// ActorID identifies the failed actor; an empty value is allowed only when the
	// provider did not identify an actor.
	ActorID string `json:"actor_id"`
	// Backend identifies the failed provider backend; an empty value is allowed
	// only when the provider did not identify a backend.
	Backend string `json:"backend"`
	// Category classifies the failure; an empty value is allowed only for an
	// unclassified provider failure.
	Category string `json:"category"`
	// Retryable reports whether retry policy may retry it; both boolean values are
	// valid.
	Retryable bool `json:"retryable"`
	// Attempts is the number of attempts made; a negative value makes the entry
	// invalid.
	Attempts int `json:"attempts"`
	// RemediationCode is the stable operator remediation code; an empty value is
	// allowed when no code was assigned.
	RemediationCode string `json:"remediation_code"`
	// SanitizedDetail is the provider detail with local secrets removed; it may be
	// empty when no detail was available.
	SanitizedDetail string `json:"sanitized_detail"`
}

// Diagnostics contains run recovery and blob-store diagnostics. It is invalid
// when an abandoned attempt has a non-positive attempt number.
type Diagnostics struct {
	// AbandonedAttempts lists attempts started without a terminal event; a
	// non-positive attempt number makes an entry invalid.
	AbandonedAttempts []AbandonedAttempt `json:"abandoned_attempts"`
	// UnreferencedBlobs lists physical blobs not referenced by the authority; the
	// list may be empty.
	UnreferencedBlobs []BlobDiagnostic `json:"unreferenced_blobs"`
	// BudgetState is the latest child-budget state; an empty value is allowed when
	// no child budget was used.
	BudgetState string `json:"budget_state"`
}

// AbandonedAttempt identifies an incomplete provider attempt. It is invalid
// when Attempt is not positive.
type AbandonedAttempt struct {
	// ActorID identifies the actor with the incomplete attempt; an empty value is
	// allowed when the event did not identify an actor.
	ActorID string `json:"actor_id"`
	// Attempt is the positive attempt number; a non-positive value makes the
	// entry invalid.
	Attempt int `json:"attempt"`
}

// BlobDiagnostic identifies an unreferenced physical blob. Its fields are a
// diagnostic projection and are not independently rejected by Validate.
type BlobDiagnostic struct {
	// SHA256 is the blob's lower-case hexadecimal digest; malformed text is
	// retained as a diagnostic projection rather than rejected by Validate.
	SHA256 string `json:"sha256"`
	// Size is the blob size in bytes; negative diagnostic values are retained
	// rather than rejected by Validate.
	Size int64 `json:"size"`
	// MediaType is the blob media type; an empty diagnostic value is retained
	// rather than rejected by Validate.
	MediaType string `json:"media_type"`
}

// Root is the nested root recipe projection emitted for recipe and child runs.
// It is invalid when execution kind or status is empty, or when a count is
// negative.
type Root struct {
	// ExecutionKind identifies the nested execution kind; an empty value makes
	// Root invalid.
	ExecutionKind string `json:"execution_kind"`
	// Status is the nested run status; an empty value makes Root invalid.
	Status string `json:"status"`
	// Recipe identifies the nested recipe; an empty ID is valid for an ordinary
	// nested projection.
	Recipe Recipe `json:"recipe"`
	// Turns contains configured and completed turn counts; a negative count makes
	// Root invalid.
	Turns Turns `json:"turns"`
	// Result contains the nested result projection; its strings may be empty while
	// the nested run is still pending.
	Result RootResult `json:"result"`
	// Workspace contains the nested workspace projection; empty fields are valid
	// when no workspace state was recorded.
	Workspace Workspace `json:"workspace"`
	// Providers maps actor IDs to provider continuation IDs; an empty map is valid
	// when no provider continuation exists.
	Providers map[string]string `json:"providers"`
	// ProviderRetry records nested retry mode; an empty value is retained for a
	// pending nested projection.
	ProviderRetry string `json:"provider_retry"`
	// Invocations records provider invocation evidence. Nil means no invocation
	// evidence was recorded; a non-nil count is present even when Count is zero.
	// A negative present count makes Root invalid.
	Invocations *Count `json:"invocations,omitempty"`
	// ReducerAttempts records nested reducer invocation count; a negative count
	// makes Root invalid.
	ReducerAttempts Count `json:"reducer_attempts"`
}

// Turns contains configured and completed participant-turn counts. Negative
// counts are invalid.
type Turns struct {
	// Configured is the plan's configured turn count; a negative value is invalid.
	Configured int `json:"configured"`
	// Completed is the completed participant-turn count; a negative value is
	// invalid.
	Completed int `json:"completed"`
}

// RootResult contains the nested result projection. Empty strings are allowed
// while a nested run is pending.
type RootResult struct {
	// Source identifies the nested result source; an empty value is allowed while
	// the nested run is pending.
	Source string `json:"source"`
	// ValidationStatus reports nested result validation; an empty value is allowed
	// while the nested run is pending.
	ValidationStatus string `json:"validation_status"`
	// Value is the nested textual result value; an empty value is allowed before a
	// result is produced.
	Value string `json:"value"`
}

// Workspace contains the report's durable workspace projection. Empty strings
// are valid when the workspace has not recorded a commit or tree hash.
type Workspace struct {
	// Mode identifies current or head-copy execution; an empty value is retained
	// for a missing nested workspace projection.
	Mode string `json:"mode"`
	// Commit identifies the selected commit when available; it may be empty.
	Commit string `json:"commit"`
	// TreeHash identifies the selected tree when available; it may be empty.
	TreeHash string `json:"tree_hash"`
	// WorkspaceContentSource identifies working-tree or committed-HEAD content;
	// it may be empty in a pending nested projection.
	WorkspaceContentSource string `json:"workspace_content_source"`
	// WorkingTreeChangesIncluded reports whether working-tree changes were used;
	// both boolean values are valid.
	WorkingTreeChangesIncluded bool `json:"working_tree_changes_included"`
}

// Validate checks a decoded or programmatically built run result. It returns an
// error naming the invalid field and never panics. It rejects missing required
// projections, negative counts, and invalid nested identity fields.
func Validate(value Result) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "session_id", value: value.SessionID},
		{name: "session_dir", value: value.SessionDir},
		{name: "execution_kind", value: value.ExecutionKind},
		{name: "task", value: value.Task},
		{name: "title", value: value.Title},
		{name: "mode", value: value.Mode},
		{name: "investigation_mode", value: value.InvestigationMode},
		{name: "status", value: value.Status},
		{name: "result_source", value: value.ResultSource},
		{name: "source", value: value.Source},
		{name: "provider_retry", value: value.ProviderRetry},
		{name: "workspace_content_source", value: value.WorkspaceContentSource},
		{name: "round_limit_mode", value: value.RoundLimitMode},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("result %s is required", field.name)
		}
	}
	if value.TimeoutSeconds <= 0 {
		return errors.New("result timeout_seconds must be positive")
	}
	if value.StallTimeoutSeconds <= 0 {
		return errors.New("result stall_timeout_seconds must be positive")
	}
	if value.ActualRounds < 0 || value.ActualParticipantTurns < 0 || value.ParticipantTurns < 0 || value.MaxRounds < 0 {
		return errors.New("result round counts must not be negative")
	}
	if value.Summary.Status == "" {
		return errors.New("result summary.status is required")
	}
	if value.Summary.Mode == "" {
		return errors.New("result summary.mode is required")
	}
	if value.Summary.ConfiguredRounds < 0 || value.Summary.MaxRounds < 0 || value.Summary.ActualRounds < 0 || value.Summary.FilteredRounds < 0 {
		return errors.New("result summary round counts must not be negative")
	}
	if value.Summary.LedgerCounts.Settled < 0 || value.Summary.LedgerCounts.Contested < 0 || value.Summary.LedgerCounts.Withdrawn < 0 {
		return errors.New("result summary ledger counts must not be negative")
	}
	if value.ReducerAttempts.Count < 0 {
		return errors.New("result reducer_attempts.count must not be negative")
	}
	if err := validateTranscript("transcript", value.Transcript); err != nil {
		return err
	}
	if err := validateTranscript("transcript_payload", value.TranscriptPayload); err != nil {
		return err
	}
	for index, failure := range value.ProviderFailures {
		if failure.Attempts < 0 {
			return fmt.Errorf("result provider_failures[%d].attempts must not be negative", index)
		}
	}
	for index, attempt := range value.Diagnostics.AbandonedAttempts {
		if attempt.Attempt < 1 {
			return fmt.Errorf("result diagnostics.abandoned_attempts[%d].attempt must be positive", index)
		}
	}
	for index, slot := range value.Slots {
		if strings.TrimSpace(slot.SlotID) == "" {
			return fmt.Errorf("result slots[%d].slot_id is required", index)
		}
		if strings.TrimSpace(slot.Backend) == "" {
			return fmt.Errorf("result slots[%d].backend is required", index)
		}
		stateFields := 0
		if slot.State.ThreadID != "" {
			stateFields++
		}
		if slot.State.SessionRef != "" {
			stateFields++
		}
		if slot.State.SessionID != "" {
			stateFields++
		}
		if stateFields > 1 {
			return fmt.Errorf("result slots[%d].state has multiple continuation identifiers", index)
		}
	}
	if value.Root != nil {
		if strings.TrimSpace(value.Root.ExecutionKind) == "" {
			return errors.New("result root.execution_kind is required")
		}
		if strings.TrimSpace(value.Root.Status) == "" {
			return errors.New("result root.status is required")
		}
		if value.Root.Turns.Configured < 0 || value.Root.Turns.Completed < 0 {
			return errors.New("result root.turns counts must not be negative")
		}
		if (value.Root.Invocations != nil && value.Root.Invocations.Count < 0) || value.Root.ReducerAttempts.Count < 0 {
			return errors.New("result root invocation counts must not be negative")
		}
	}
	return nil
}

func validateTranscript(label string, entries []TranscriptEntry) error {
	for index, entry := range entries {
		if entry.Round < 1 {
			return fmt.Errorf("result %s[%d].round must be positive", label, index)
		}
		if strings.TrimSpace(entry.From) == "" {
			return fmt.Errorf("result %s[%d].from is required", label, index)
		}
		if strings.TrimSpace(entry.Mode) == "" {
			return fmt.Errorf("result %s[%d].mode is required", label, index)
		}
		if provider := entry.FacilitatorProviderResult; provider != nil {
			if strings.TrimSpace(provider.Backend) == "" {
				return fmt.Errorf("result %s[%d].facilitator_provider_result.backend is required", label, index)
			}
			if strings.TrimSpace(provider.ActorID) == "" {
				return fmt.Errorf("result %s[%d].facilitator_provider_result.actor_id is required", label, index)
			}
		}
	}
	return nil
}
