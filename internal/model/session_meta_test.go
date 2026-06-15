package model

import "testing"

func TestSessionMetaCopyOnWrite(t *testing.T) {
	meta := NewSessionMeta(map[string]any{
		"status": "running",
		"ledger": map[string]any{"settled": []any{"done"}},
	})
	next := meta.WithStatus("completed").AppendToSlice("provider_failures", map[string]any{"actor": "Codex"})

	if meta.String("status") != "running" {
		t.Fatalf("original status mutated: %q", meta.String("status"))
	}
	if next.String("status") != "completed" {
		t.Fatalf("next status = %q", next.String("status"))
	}
	if len(meta.Slice("provider_failures")) != 0 || len(next.Slice("provider_failures")) != 1 {
		t.Fatalf("provider failures original=%#v next=%#v", meta.Slice("provider_failures"), next.Slice("provider_failures"))
	}

	raw := next.ToMap()
	raw["status"] = "mutated"
	if next.String("status") != "completed" {
		t.Fatalf("ToMap exposed internal map")
	}
}

func TestSessionMetaDoesNotInjectMissingOptionalFields(t *testing.T) {
	meta := NewSessionMeta(map[string]any{"status": "running"})
	raw := meta.ToMap()
	if _, ok := raw["ledger"]; ok {
		t.Fatalf("missing ledger was injected: %#v", raw)
	}
	if _, ok := raw["contested_lineages"]; ok {
		t.Fatalf("missing contested_lineages was injected: %#v", raw)
	}
	if !meta.Ledger().IsEmpty() {
		t.Fatalf("missing ledger should still read as empty")
	}
}
