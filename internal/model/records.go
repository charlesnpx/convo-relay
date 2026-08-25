package model

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
