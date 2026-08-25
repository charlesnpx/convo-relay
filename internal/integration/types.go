package integration

import (
	"fmt"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

const (
	BundleSchemaVersionV1 = contracts.IntegrationBundleV1
	BundleSchemaVersionV2 = contracts.IntegrationBundleV2
	DefaultMediaType      = "application/octet-stream"

	CardinalityOne  = "one"
	CardinalityMany = "many"

	ResultSourceLastTurn = "last_turn"
	ResultSourceReducer  = "reducer"

	PromptContextPolicyVersion    = contracts.PromptContextProjectionV1
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
// integration bundle. SourcePath is informational and is deliberately absent
// from ToMap and Digest.
type Bundle struct {
	schemaVersion string
	id            string
	contracts     map[string]*Contract
	sourcePath    string
	digest        string
}

type Contract struct {
	Turns         []TurnDeclaration
	Reducer       *ReducerDeclaration
	Inputs        map[string]*InputDeclaration
	Result        ResultDeclaration
	PromptContext PromptContextProjection
}

type PromptContextProjection struct {
	SchemaVersion         string
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
	id            string
	contract      *Contract
	digest        string
	bundleVersion string
}

func (b *Bundle) ToMap() map[string]any {
	contractMaps := make(map[string]any, len(b.contracts))
	for id, contract := range b.contracts {
		contractMaps[id] = contract.toMap(b.schemaVersion)
	}
	return map[string]any{
		"schema_version": b.schemaVersion,
		"id":             b.id,
		"contracts":      contractMaps,
	}
}

func (b *Bundle) SchemaVersion() string {
	if b == nil {
		return ""
	}
	return b.schemaVersion
}

func (b *Bundle) ID() string {
	if b == nil {
		return ""
	}
	return b.id
}

func (b *Bundle) SourcePath() string {
	if b == nil {
		return ""
	}
	return b.sourcePath
}

func (b *Bundle) Digest() string {
	if b == nil {
		return ""
	}
	return b.digest
}

func (b *Bundle) ArtifactPayload() (map[string]any, error) {
	fields := map[string]any{
		"bundle_id":     b.ID(),
		"bundle_digest": b.Digest(),
		"bundle":        b.ToMap(),
	}
	if b.SchemaVersion() == BundleSchemaVersionV2 {
		return contracts.NormalizeRootArtifactVersion(contracts.RootArtifactKindIntegrationBundle, contracts.RootArtifactSchemaVersionV2, fields)
	}
	return contracts.NormalizeRootArtifact(contracts.RootArtifactKindIntegrationBundle, fields)
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
	return c.toMap(BundleSchemaVersionV1)
}

func (c *Contract) toMap(bundleVersion string) map[string]any {
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
		result["schema"] = contracts.Materialize(c.Result.Schema)
	}
	payload := map[string]any{
		"turns":  turns,
		"inputs": inputs,
		"result": result,
	}
	if c.Reducer != nil {
		payload["reducer"] = map[string]any{"instructions": c.Reducer.Instructions}
	}
	if bundleVersion == BundleSchemaVersionV2 {
		payload["prompt_context"] = c.PromptContext.ToMap()
	}
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
	version := s.bundleVersion
	if version == "" {
		version = BundleSchemaVersionV1
	}
	payload := s.contract.toMap(version)
	payload["id"] = s.id
	return payload
}

func (s *SelectedContract) BundleVersion() string {
	if s == nil {
		return ""
	}
	return s.bundleVersion
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

func (s *SelectedContract) ArtifactPayload() (map[string]any, error) {
	fields := map[string]any{
		"contract_id":     s.ID(),
		"contract_digest": s.Digest(),
		"contract":        s.ToMap(),
	}
	if s.BundleVersion() == BundleSchemaVersionV2 {
		return contracts.NormalizeRootArtifactVersion(contracts.RootArtifactKindIntegrationContract, contracts.RootArtifactSchemaVersionV2, fields)
	}
	return contracts.NormalizeRootArtifact(contracts.RootArtifactKindIntegrationContract, fields)
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
	cloned, _ := contracts.Materialize(value).(map[string]any)
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
	cloned.Result.Schema = contracts.Materialize(contract.Result.Schema)
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
