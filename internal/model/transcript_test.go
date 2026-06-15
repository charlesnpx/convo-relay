package model

import "testing"

func TestTranscriptLegacyDecodeAndAppendAreImmutable(t *testing.T) {
	transcript, err := ParseTranscript([]any{
		map[string]any{
			"round":   float64(1),
			"slot_id": "slot_0_gen2",
			"from":    "Codex",
			"content": "\nlegacy response\n",
			"custom":  "kept",
		},
	})
	if err != nil {
		t.Fatalf("parse transcript: %v", err)
	}
	entry, ok := transcript.At(0)
	if !ok {
		t.Fatal("missing entry")
	}
	if entry.Mode != "" || entry.LogicalSlotID != "slot_0" || entry.SlotGeneration != 2 || entry.ProviderResult != nil {
		t.Fatalf("legacy entry normalized incorrectly: %#v", entry)
	}
	if entry.ToMap()["custom"] != "kept" {
		t.Fatalf("unknown field was not preserved: %#v", entry.ToMap())
	}
	if entry.Content != "\nlegacy response\n" || entry.ToMap()["content"] != "\nlegacy response\n" {
		t.Fatalf("content was not preserved exactly: %#v", entry.ToMap())
	}
	if _, ok := entry.ToMap()["ledger"]; ok {
		t.Fatalf("legacy entry unexpectedly gained a missing ledger field: %#v", entry.ToMap())
	}

	appended := transcript.Append(TranscriptEntry{Round: 2, SlotID: "slot_1", From: "Claude", Content: "next"})
	if transcript.Len() != 1 || appended.Len() != 2 {
		t.Fatalf("append mutated original: original=%d appended=%d", transcript.Len(), appended.Len())
	}
}

func TestTranscriptPreservesKnownEmptyLedger(t *testing.T) {
	entry := TranscriptEntry{Round: 1, ContentPresent: true, Ledger: EmptyLedger(), LedgerPresent: true}
	if content, ok := entry.ToMap()["content"]; !ok || content != "" {
		t.Fatalf("known empty content was omitted: %#v", entry.ToMap())
	}
	if _, ok := entry.ToMap()["ledger"]; !ok {
		t.Fatalf("known empty ledger was omitted: %#v", entry.ToMap())
	}
}

func TestTranscriptPreservesProviderResultExtras(t *testing.T) {
	transcript, err := ParseTranscript([]any{map[string]any{
		"round": 1,
		"provider_result": map[string]any{
			"backend": "codex",
			"custom":  map[string]any{"nested": "kept"},
		},
	}})
	if err != nil {
		t.Fatalf("parse transcript: %v", err)
	}
	entry, _ := transcript.At(0)
	providerResult := entry.ToMap()["provider_result"].(map[string]any)
	if providerResult["custom"].(map[string]any)["nested"] != "kept" {
		t.Fatalf("provider result extra field lost: %#v", providerResult)
	}
	if _, ok := providerResult["timed_out"]; ok {
		t.Fatalf("provider result gained an absent field: %#v", providerResult)
	}
}

func TestGeneratedProviderResultKeepsCurrentShape(t *testing.T) {
	providerResult := NewProviderResult("codex", 0, true).ToMap()
	if _, ok := providerResult["retryable_error"]; ok {
		t.Fatalf("successful provider result should omit retryable_error: %#v", providerResult)
	}
	if _, ok := providerResult["warnings"]; !ok {
		t.Fatalf("generated provider result omitted warnings: %#v", providerResult)
	}
}

func TestTranscriptStampMissingModesReturnsCopy(t *testing.T) {
	transcript, err := ParseTranscript([]any{map[string]any{"round": 1, "slot_id": "slot_0"}})
	if err != nil {
		t.Fatalf("parse transcript: %v", err)
	}
	stamped := transcript.StampMissingModes("steelman")
	original, _ := transcript.At(0)
	updated, _ := stamped.At(0)
	if original.Mode != "" || updated.Mode != "steelman" {
		t.Fatalf("mode copy behavior original=%q updated=%q", original.Mode, updated.Mode)
	}
}
