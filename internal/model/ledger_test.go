package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestLedgerNormalizesCountsAndCopies(t *testing.T) {
	input := map[string]any{
		"settled":   []any{" done ", "", "kept"},
		"contested": []any{"risk"},
		"withdrawn": []any{},
	}
	ledger := ParseLedger(input)
	input["settled"].([]any)[0] = "mutated"

	if got := ledger.Settled(); !reflect.DeepEqual(got, []string{"done", "kept"}) {
		t.Fatalf("settled = %#v", got)
	}
	if counts := ledger.Counts(); counts.Settled != 2 || counts.Contested != 1 || counts.Withdrawn != 0 {
		t.Fatalf("counts = %#v", counts)
	}
	settled := ledger.Settled()
	settled[0] = "mutated"
	if got := ledger.Settled()[0]; got != "done" {
		t.Fatalf("ledger accessor did not copy, got %q", got)
	}
}

func TestLedgerJSONRoundTripUsesCompatibleShape(t *testing.T) {
	ledger := NewLedger([]string{"one"}, []string{"two"}, []string{"three"})
	data, err := json.Marshal(ledger)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string][]string
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded["settled"], []string{"one"}) ||
		!reflect.DeepEqual(decoded["contested"], []string{"two"}) ||
		!reflect.DeepEqual(decoded["withdrawn"], []string{"three"}) {
		t.Fatalf("decoded = %#v", decoded)
	}
}
