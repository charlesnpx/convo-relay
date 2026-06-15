package model

type SlotState struct {
	Fields map[string]any
}

func NewSlotState(fields map[string]any) SlotState {
	return SlotState{Fields: cloneMap(fields)}
}

func (s SlotState) ToMap() map[string]any {
	return cloneMap(s.Fields)
}

type SlotEnvelope struct {
	Backend       string
	ProfileID     any
	SlotID        string
	LogicalSlotID string
	Generation    int
	Label         string
	State         SlotState
}

func NewSlotEnvelope(fields map[string]any) SlotEnvelope {
	return SlotEnvelope{
		Backend:       stringFromAny(fields["backend"]),
		ProfileID:     cloneValue(fields["profile_id"]),
		SlotID:        stringFromAny(fields["slot_id"]),
		LogicalSlotID: firstNonEmpty(stringFromAny(fields["logical_slot_id"]), logicalSlotIDForSlotID(stringFromAny(fields["slot_id"]))),
		Generation:    intFromAny(fields["generation"], slotGenerationForSlotID(stringFromAny(fields["slot_id"]))),
		Label:         stringFromAny(fields["label"]),
		State:         NewSlotState(mapFromAny(fields["state"])),
	}
}

func (s SlotEnvelope) ToMap() map[string]any {
	return map[string]any{
		"backend":         s.Backend,
		"profile_id":      cloneValue(s.ProfileID),
		"slot_id":         s.SlotID,
		"logical_slot_id": s.LogicalSlotID,
		"generation":      s.Generation,
		"label":           s.Label,
		"state":           s.State.ToMap(),
	}
}

type ModeControlRecord struct {
	Fields map[string]any
}

type SlotReplacementRecord struct {
	Fields map[string]any
}

type InputBundleRef struct {
	Fields map[string]any
}

type TransientRecipeRef struct {
	Fields map[string]any
}

func NewModeControlRecord(fields map[string]any) ModeControlRecord {
	return ModeControlRecord{Fields: cloneMap(fields)}
}

func NewSlotReplacementRecord(fields map[string]any) SlotReplacementRecord {
	return SlotReplacementRecord{Fields: cloneMap(fields)}
}

func NewInputBundleRef(fields map[string]any) InputBundleRef {
	return InputBundleRef{Fields: cloneMap(fields)}
}

func NewTransientRecipeRef(fields map[string]any) TransientRecipeRef {
	return TransientRecipeRef{Fields: cloneMap(fields)}
}

func (r ModeControlRecord) ToMap() map[string]any     { return cloneMap(r.Fields) }
func (r SlotReplacementRecord) ToMap() map[string]any { return cloneMap(r.Fields) }
func (r InputBundleRef) ToMap() map[string]any        { return cloneMap(r.Fields) }
func (r TransientRecipeRef) ToMap() map[string]any    { return cloneMap(r.Fields) }

type Proposal struct {
	fields map[string]any
}

type AdmissionDecision struct {
	fields map[string]any
}

func NewProposal(fields map[string]any) Proposal {
	return Proposal{fields: cloneMap(fields)}
}

func (p Proposal) ToMap() map[string]any {
	return cloneMap(p.fields)
}

func (p Proposal) Get(key string) any {
	return cloneValue(p.fields[key])
}

func (p Proposal) String(key string) string {
	return stringFromAny(p.fields[key])
}

func (p Proposal) ProposalID() string {
	return p.String("proposal_id")
}

func (p Proposal) Status() string {
	return p.String("status")
}

func (p Proposal) With(key string, value any) Proposal {
	next := p.ToMap()
	next[key] = cloneValue(value)
	return NewProposal(next)
}

func NewAdmissionDecision(fields map[string]any) AdmissionDecision {
	return AdmissionDecision{fields: cloneMap(fields)}
}

func (d AdmissionDecision) ToMap() map[string]any {
	return cloneMap(d.fields)
}

func (d AdmissionDecision) ProposalID() string {
	return stringFromAny(d.fields["proposal_id"])
}

type ShowReport struct {
	SessionID  string         `json:"session_id,omitempty"`
	SessionDir string         `json:"session_dir,omitempty"`
	Fields     map[string]any `json:"-"`
}

type HealthReport struct {
	Scope     string         `json:"scope,omitempty"`
	Status    string         `json:"status,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Fields    map[string]any `json:"-"`
}

type ExportReport struct {
	SessionID string         `json:"session_id,omitempty"`
	Fields    map[string]any `json:"-"`
}

func NewShowReport(fields map[string]any) ShowReport {
	return ShowReport{
		SessionID:  stringFromAny(fields["session_id"]),
		SessionDir: stringFromAny(fields["session_dir"]),
		Fields:     cloneMap(fields),
	}
}

func NewHealthReport(fields map[string]any) HealthReport {
	return HealthReport{
		Scope:     stringFromAny(fields["scope"]),
		Status:    stringFromAny(fields["status"]),
		SessionID: stringFromAny(fields["session_id"]),
		Fields:    cloneMap(fields),
	}
}

func NewExportReport(fields map[string]any) ExportReport {
	return ExportReport{
		SessionID: stringFromAny(fields["session_id"]),
		Fields:    cloneMap(fields),
	}
}

func (r ShowReport) ToMap() map[string]any   { return cloneMap(r.Fields) }
func (r HealthReport) ToMap() map[string]any { return cloneMap(r.Fields) }
func (r ExportReport) ToMap() map[string]any { return cloneMap(r.Fields) }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func mapFromAny(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}
