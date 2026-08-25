package integration

import (
	"fmt"

	"github.com/charlesnpx/convo-relay/internal/format"
)

const (
	DefaultMediaType = "application/octet-stream"

	CardinalityOne  = "one"
	CardinalityMany = "many"

	ResultSourceLastTurn = "last_turn"
	ResultSourceReducer  = "reducer"

	ParticipantTranscriptComplete = "complete"
	FacilitatorLedgerInclude      = "include"
	FacilitatorLedgerTraceOnly    = "trace_only"
)

const (
	DiagnosticCodeBundleReadFailed = "integration_bundle_read_failed"
	DiagnosticCodeInvalidBundle    = "invalid_integration_bundle"
	DiagnosticCodeContractNotFound = "integration_contract_not_found"
	DiagnosticCodeScheduleMismatch = "integration_schedule_mismatch"
	DiagnosticCodeInvalidSchema    = "invalid_integration_schema"
)

// Bundle is the normalized, immutable representation of one consumer-owned
// integration bundle.
type Bundle struct {
	id        string
	contracts map[string]*Contract
	digest    string
}

type Contract struct {
	Turns         []TurnDeclaration
	Reducer       *ReducerDeclaration
	Inputs        map[string]*InputDeclaration
	Result        ResultDeclaration
	PromptContext PromptContextProjection
}

type PromptContextProjection struct {
	ParticipantTranscript string
	FacilitatorLedger     string
}

type TurnDeclaration struct {
	ParticipantTurn int
	Slot            string
	Instructions    string
}

type ReducerDeclaration struct {
	Instructions string
}

type InputDeclaration struct {
	Required    bool
	Cardinality string
	MediaType   string
	MaxBytes    int64
}

type ResultDeclaration struct {
	Format string
	Schema any
}

// ScheduledTurn is the compiler-owned schedule entry against which a selected
// contract is checked. The integration package never infers a recipe schedule.
type ScheduledTurn struct {
	ParticipantTurn int
	Slot            string
}

type ScheduleRequirement struct {
	Turns        []ScheduledTurn
	ResultSource string
}

type SelectedContract struct {
	id       string
	contract *Contract
	digest   string
}

func (b *Bundle) ToMap() map[string]any {
	contractMaps := make(map[string]any, len(b.contracts))
	for id, contract := range b.contracts {
		contractMaps[id] = contract.ToMap()
	}
	return map[string]any{
		"id":        b.id,
		"contracts": contractMaps,
	}
}

func (b *Bundle) ID() string {
	if b == nil {
		return ""
	}
	return b.id
}

func (b *Bundle) Digest() string {
	if b == nil {
		return ""
	}
	return b.digest
}

func (b *Bundle) Contract(id string) (*Contract, bool) {
	if b == nil {
		return nil, false
	}
	contract, exists := b.contracts[id]
	if !exists {
		return nil, false
	}
	return cloneContract(contract), true
}

func (c *Contract) ToMap() map[string]any {
	turns := make([]any, 0, len(c.Turns))
	for _, turn := range c.Turns {
		turns = append(turns, map[string]any{
			"participant_turn": turn.ParticipantTurn,
			"slot":             turn.Slot,
			"instructions":     turn.Instructions,
		})
	}
	inputs := make(map[string]any, len(c.Inputs))
	for name, input := range c.Inputs {
		inputs[name] = input.ToMap()
	}
	result := map[string]any{"format": c.Result.Format}
	if c.Result.Schema != nil {
		result["schema"] = format.Materialize(c.Result.Schema)
	}
	payload := map[string]any{
		"turns":  turns,
		"inputs": inputs,
		"result": result,
	}
	if c.Reducer != nil {
		payload["reducer"] = map[string]any{"instructions": c.Reducer.Instructions}
	}
	payload["prompt_context"] = c.PromptContext.ToMap()
	return payload
}

func (p PromptContextProjection) ToMap() map[string]any {
	participantTranscript := p.ParticipantTranscript
	if participantTranscript == "" {
		participantTranscript = ParticipantTranscriptComplete
	}
	facilitatorLedger := p.FacilitatorLedger
	if facilitatorLedger == "" {
		facilitatorLedger = FacilitatorLedgerInclude
	}
	return map[string]any{
		"participant_transcript": participantTranscript,
		"facilitator_ledger":     facilitatorLedger,
	}
}

func (d *InputDeclaration) ToMap() map[string]any {
	payload := map[string]any{
		"required":    d.Required,
		"cardinality": d.Cardinality,
		"media_type":  d.MediaType,
		"max_bytes":   d.MaxBytes,
	}
	return payload
}

func (s *SelectedContract) ToMap() map[string]any {
	payload := s.contract.ToMap()
	payload["id"] = s.id
	return payload
}

func (s *SelectedContract) PromptContext() PromptContextProjection {
	if s == nil || s.contract == nil {
		return PromptContextProjection{}
	}
	return s.contract.PromptContext
}

func (s *SelectedContract) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

func (s *SelectedContract) Contract() *Contract {
	if s == nil {
		return nil
	}
	return cloneContract(s.contract)
}

func (s *SelectedContract) Digest() string {
	if s == nil {
		return ""
	}
	return s.digest
}

func AlternatingSchedule(participantTurns int) ([]ScheduledTurn, error) {
	if participantTurns < 1 {
		return nil, fmt.Errorf("participant turns must be positive")
	}
	turns := make([]ScheduledTurn, participantTurns)
	for index := range turns {
		turns[index] = ScheduledTurn{
			ParticipantTurn: index + 1,
			Slot:            fmt.Sprintf("slot_%d", index%2),
		}
	}
	return turns, nil
}

func cloneMap(value map[string]any) map[string]any {
	cloned, _ := format.Materialize(value).(map[string]any)
	return cloned
}

func cloneContract(contract *Contract) *Contract {
	if contract == nil {
		return nil
	}
	cloned := &Contract{
		Turns:         append([]TurnDeclaration(nil), contract.Turns...),
		Inputs:        make(map[string]*InputDeclaration, len(contract.Inputs)),
		Result:        contract.Result,
		PromptContext: contract.PromptContext,
	}
	cloned.Result.Schema = format.Materialize(contract.Result.Schema)
	if contract.Reducer != nil {
		reducer := *contract.Reducer
		cloned.Reducer = &reducer
	}
	for name, input := range contract.Inputs {
		if input == nil {
			cloned.Inputs[name] = nil
			continue
		}
		declaration := *input
		cloned.Inputs[name] = &declaration
	}
	return cloned
}
