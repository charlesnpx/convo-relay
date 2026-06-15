package model

import "encoding/json"

var ledgerKeys = []string{"settled", "contested", "withdrawn"}

type Ledger struct {
	settled   []string
	contested []string
	withdrawn []string
}

type LedgerCounts struct {
	Settled   int `json:"settled"`
	Contested int `json:"contested"`
	Withdrawn int `json:"withdrawn"`
}

func EmptyLedger() Ledger {
	return Ledger{
		settled:   []string{},
		contested: []string{},
		withdrawn: []string{},
	}
}

func NewLedger(settled []string, contested []string, withdrawn []string) Ledger {
	return Ledger{
		settled:   cloneStringSlice(settled),
		contested: cloneStringSlice(contested),
		withdrawn: cloneStringSlice(withdrawn),
	}
}

func ParseLedger(value any) Ledger {
	switch typed := value.(type) {
	case Ledger:
		return typed.Clone()
	case *Ledger:
		if typed == nil {
			return EmptyLedger()
		}
		return typed.Clone()
	case map[string]any:
		return NewLedger(
			normalizeStringItems(typed["settled"]),
			normalizeStringItems(typed["contested"]),
			normalizeStringItems(typed["withdrawn"]),
		)
	case map[string][]string:
		return NewLedger(typed["settled"], typed["contested"], typed["withdrawn"])
	default:
		return EmptyLedger()
	}
}

func (l Ledger) Clone() Ledger {
	return NewLedger(l.settled, l.contested, l.withdrawn)
}

func (l Ledger) Settled() []string {
	return cloneStringSlice(l.settled)
}

func (l Ledger) Contested() []string {
	return cloneStringSlice(l.contested)
}

func (l Ledger) Withdrawn() []string {
	return cloneStringSlice(l.withdrawn)
}

func (l Ledger) Counts() LedgerCounts {
	return LedgerCounts{
		Settled:   len(l.settled),
		Contested: len(l.contested),
		Withdrawn: len(l.withdrawn),
	}
}

func (l Ledger) CountsMap() map[string]any {
	counts := l.Counts()
	return map[string]any{
		"settled":   counts.Settled,
		"contested": counts.Contested,
		"withdrawn": counts.Withdrawn,
	}
}

func (l Ledger) IsEmpty() bool {
	counts := l.Counts()
	return counts.Settled == 0 && counts.Contested == 0 && counts.Withdrawn == 0
}

func (l Ledger) HasContested() bool {
	return len(l.contested) > 0
}

func (l Ledger) ToMap() map[string]any {
	return map[string]any{
		"settled":   stringsToAny(l.settled),
		"contested": stringsToAny(l.contested),
		"withdrawn": stringsToAny(l.withdrawn),
	}
}

func (l Ledger) MarshalJSON() ([]byte, error) {
	return json.Marshal(l.ToMap())
}

func (l *Ledger) UnmarshalJSON(data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*l = ParseLedger(value)
	return nil
}

func LedgerFromJSON(data []byte) (Ledger, error) {
	var ledger Ledger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return EmptyLedger(), err
	}
	return ledger, nil
}
