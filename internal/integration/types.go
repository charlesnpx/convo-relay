package integration

import (
	"fmt"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

const (
	BundleSchemaVersion = "relay-integration-bundle-v1"
	DefaultMediaType    = "application/octet-stream"

	CardinalityOne  = "one"
	CardinalityMany = "many"

	ResultTransportJSON  = "json"
	ResultSourceLastTurn = "last_turn"
	ResultSourceReducer  = "reducer"
)

const (
	DiagnosticCodeBundleReadFailed       = "integration_bundle_read_failed"
	DiagnosticCodeInvalidBundle          = "invalid_integration_bundle"
	DiagnosticCodeContractNotFound       = "integration_contract_not_found"
	DiagnosticCodeScheduleMismatch       = "integration_schedule_mismatch"
	DiagnosticCodeInvalidSchema          = "invalid_integration_schema"
	DiagnosticCodeUnsupportedSchema      = "unsupported_schema_keyword"
	DiagnosticCodeInvalidSchemaReference = "invalid_schema_reference"
	DiagnosticCodeSchemaMismatch         = "schema_mismatch"
	DiagnosticCodeInvalidAssertion       = "invalid_assertion_declaration"
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
	Turns   []TurnDeclaration
	Reducer *ReducerDeclaration
	Inputs  map[string]*InputDeclaration
	Result  ResultDeclaration
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
	Schema      *CompiledSchema
}

type ResultDeclaration struct {
	Transport  string
	Schema     *CompiledSchema
	Assertions []AssertionDeclaration
}

type AssertionDeclaration struct {
	Type    string
	Source  string
	Pointer string
	Left    *AssertionOperand
	Right   *AssertionOperand
}

type AssertionOperand struct {
	Source       string
	Pointer      string
	ItemsPointer string
	KeyPointer   string
	ValuePointer string
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

func (b *Bundle) Contracts() map[string]*Contract {
	if b == nil {
		return nil
	}
	result := make(map[string]*Contract, len(b.contracts))
	for id, contract := range b.contracts {
		result[id] = cloneContract(contract)
	}
	return result
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
	result := map[string]any{
		"transport":  c.Result.Transport,
		"schema":     c.Result.Schema.Document(),
		"assertions": assertionMaps(c.Result.Assertions),
	}
	payload := map[string]any{
		"turns":  turns,
		"inputs": inputs,
		"result": result,
	}
	if c.Reducer != nil {
		payload["reducer"] = map[string]any{"instructions": c.Reducer.Instructions}
	}
	return payload
}

func (d *InputDeclaration) ToMap() map[string]any {
	payload := map[string]any{
		"required":    d.Required,
		"cardinality": d.Cardinality,
		"media_type":  d.MediaType,
		"max_bytes":   d.MaxBytes,
	}
	if d.Schema != nil {
		payload["schema"] = d.Schema.Document()
	}
	return payload
}

func (s *SelectedContract) ToMap() map[string]any {
	payload := s.contract.ToMap()
	payload["id"] = s.id
	return payload
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

func assertionMaps(assertions []AssertionDeclaration) []any {
	result := make([]any, 0, len(assertions))
	for _, assertion := range assertions {
		result = append(result, assertion.ToMap())
	}
	return result
}

func (a AssertionDeclaration) ToMap() map[string]any {
	payload := map[string]any{"type": a.Type}
	switch a.Type {
	case "unique":
		payload["source"] = a.Source
		payload["pointer"] = a.Pointer
	case "set_equal", "value_equal", "field_equal_by_key":
		payload["left"] = a.Left.ToMap(a.Type == "field_equal_by_key")
		payload["right"] = a.Right.ToMap(a.Type == "field_equal_by_key")
	}
	return payload
}

func (o *AssertionOperand) ToMap(fields bool) map[string]any {
	if fields {
		return map[string]any{
			"source":        o.Source,
			"items_pointer": o.ItemsPointer,
			"key_pointer":   o.KeyPointer,
			"value_pointer": o.ValuePointer,
		}
	}
	return map[string]any{
		"source":  o.Source,
		"pointer": o.Pointer,
	}
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
		Turns:  append([]TurnDeclaration(nil), contract.Turns...),
		Inputs: make(map[string]*InputDeclaration, len(contract.Inputs)),
		Result: contract.Result,
	}
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
	cloned.Result.Assertions = make([]AssertionDeclaration, len(contract.Result.Assertions))
	for index, assertion := range contract.Result.Assertions {
		cloned.Result.Assertions[index] = assertion
		if assertion.Left != nil {
			left := *assertion.Left
			cloned.Result.Assertions[index].Left = &left
		}
		if assertion.Right != nil {
			right := *assertion.Right
			cloned.Result.Assertions[index].Right = &right
		}
	}
	return cloned
}
