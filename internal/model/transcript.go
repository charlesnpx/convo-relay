package model

import (
	"fmt"
	"strings"
)

type TranscriptEntry struct {
	Round          int
	SlotID         string
	LogicalSlotID  string
	SlotGeneration int
	From           string
	Mode           string
	Content        string
	ContentPresent bool
	Ledger         Ledger
	LedgerPresent  bool
	Timestamp      string
	ProviderResult *ProviderResult
	Extra          map[string]any
}

type Transcript struct {
	entries []TranscriptEntry
}

func NewTranscript(entries []TranscriptEntry) Transcript {
	result := make([]TranscriptEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Clone())
	}
	return Transcript{entries: result}
}

func EmptyTranscript() Transcript {
	return Transcript{entries: []TranscriptEntry{}}
}

func ParseTranscript(value any) (Transcript, error) {
	switch typed := value.(type) {
	case Transcript:
		return typed.Clone(), nil
	case []map[string]any:
		return TranscriptFromMaps(typed)
	case []any:
		entries := make([]TranscriptEntry, 0, len(typed))
		for index, rawEntry := range typed {
			entryMap, ok := rawEntry.(map[string]any)
			if !ok {
				return EmptyTranscript(), fmt.Errorf("transcript entry %d must be an object", index+1)
			}
			entries = append(entries, ParseTranscriptEntry(entryMap))
		}
		return NewTranscript(entries), nil
	default:
		return EmptyTranscript(), fmt.Errorf("transcript.json must contain an array")
	}
}

func TranscriptFromMaps(items []map[string]any) (Transcript, error) {
	entries := make([]TranscriptEntry, 0, len(items))
	for _, item := range items {
		entries = append(entries, ParseTranscriptEntry(item))
	}
	return NewTranscript(entries), nil
}

func ParseTranscriptEntry(value any) TranscriptEntry {
	object, _ := value.(map[string]any)
	entry := TranscriptEntry{
		Round:   intFromAny(object["round"], 0),
		SlotID:  stringFromAny(object["slot_id"]),
		From:    stringFromAny(object["from"]),
		Mode:    stringFromAny(object["mode"]),
		Content: contentFromAny(object["content"]),
		ContentPresent: func() bool {
			_, ok := object["content"]
			return ok
		}(),
		Ledger: ParseLedger(object["ledger"]),
		LedgerPresent: func() bool {
			_, ok := object["ledger"]
			return ok
		}(),
		Timestamp: stringFromAny(object["timestamp"]),
		Extra:     map[string]any{},
	}
	entry.LogicalSlotID = stringFromAny(object["logical_slot_id"])
	if entry.LogicalSlotID == "" {
		entry.LogicalSlotID = logicalSlotIDForSlotID(entry.SlotID)
	}
	entry.SlotGeneration = intFromAny(object["slot_generation"], 0)
	if entry.SlotGeneration <= 0 {
		entry.SlotGeneration = slotGenerationForSlotID(entry.SlotID)
	}
	if providerResult, ok := ParseProviderResult(object["provider_result"]); ok {
		entry.ProviderResult = &providerResult
	}
	for key, item := range object {
		if transcriptEntryKnownKeys[key] {
			continue
		}
		entry.Extra[key] = cloneValue(item)
	}
	return entry
}

var transcriptEntryKnownKeys = map[string]bool{
	"round":           true,
	"slot_id":         true,
	"logical_slot_id": true,
	"slot_generation": true,
	"from":            true,
	"mode":            true,
	"content":         true,
	"ledger":          true,
	"timestamp":       true,
	"provider_result": true,
}

func (e TranscriptEntry) Clone() TranscriptEntry {
	e.Ledger = e.Ledger.Clone()
	if e.ProviderResult != nil {
		providerResult := e.ProviderResult.Clone()
		e.ProviderResult = &providerResult
	}
	e.Extra = cloneMap(e.Extra)
	return e
}

func (e TranscriptEntry) ToMap() map[string]any {
	result := cloneMap(e.Extra)
	if e.Round != 0 {
		result["round"] = e.Round
	}
	if e.SlotID != "" {
		result["slot_id"] = e.SlotID
	}
	if e.LogicalSlotID != "" {
		result["logical_slot_id"] = e.LogicalSlotID
	}
	if e.SlotGeneration != 0 {
		result["slot_generation"] = e.SlotGeneration
	}
	if e.From != "" {
		result["from"] = e.From
	}
	if e.Mode != "" {
		result["mode"] = e.Mode
	}
	if e.ContentPresent || e.Content != "" {
		result["content"] = e.Content
	}
	if e.LedgerPresent || !e.Ledger.IsEmpty() || result["ledger"] != nil {
		result["ledger"] = e.Ledger.ToMap()
	}
	if e.Timestamp != "" {
		result["timestamp"] = e.Timestamp
	}
	if e.ProviderResult != nil {
		result["provider_result"] = e.ProviderResult.ToMap()
	}
	return result
}

func contentFromAny(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func (e TranscriptEntry) WithMode(mode string) TranscriptEntry {
	next := e.Clone()
	next.Mode = strings.TrimSpace(mode)
	return next
}

func (e TranscriptEntry) IsSyntheticChild() bool {
	if synthetic, _ := e.Extra["synthetic"].(bool); synthetic {
		return true
	}
	return stringFromAny(e.Extra["source_type"]) == "child_result"
}

func (e TranscriptEntry) SpeakerID() string {
	if e.SlotID != "" {
		return e.SlotID
	}
	return e.From
}

func (t Transcript) Clone() Transcript {
	return NewTranscript(t.entries)
}

func (t Transcript) Len() int {
	return len(t.entries)
}

func (t Transcript) Entries() []TranscriptEntry {
	return NewTranscript(t.entries).entries
}

func (t Transcript) At(index int) (TranscriptEntry, bool) {
	if index < 0 || index >= len(t.entries) {
		return TranscriptEntry{}, false
	}
	return t.entries[index].Clone(), true
}

func (t Transcript) Last() (TranscriptEntry, bool) {
	return t.At(len(t.entries) - 1)
}

func (t Transcript) Append(entry TranscriptEntry) Transcript {
	entries := t.Entries()
	entries = append(entries, entry.Clone())
	return Transcript{entries: entries}
}

func (t Transcript) WithEntry(index int, entry TranscriptEntry) Transcript {
	entries := t.Entries()
	if index < 0 || index >= len(entries) {
		return Transcript{entries: entries}
	}
	entries[index] = entry.Clone()
	return Transcript{entries: entries}
}

func (t Transcript) StampMissingModes(mode string) Transcript {
	entries := t.Entries()
	for index, entry := range entries {
		if strings.TrimSpace(entry.Mode) == "" {
			entries[index] = entry.WithMode(mode)
		}
	}
	return Transcript{entries: entries}
}

func (t Transcript) LastBackendEntry() (TranscriptEntry, bool) {
	for index := len(t.entries) - 1; index >= 0; index-- {
		entry := t.entries[index]
		if entry.IsSyntheticChild() {
			continue
		}
		if entry.SlotID != "" {
			return entry.Clone(), true
		}
	}
	return TranscriptEntry{}, false
}

func (t Transcript) Filter(predicate func(TranscriptEntry) bool) Transcript {
	entries := make([]TranscriptEntry, 0, len(t.entries))
	for _, entry := range t.entries {
		if predicate(entry.Clone()) {
			entries = append(entries, entry.Clone())
		}
	}
	return Transcript{entries: entries}
}

func (t Transcript) ToSlice() []any {
	items := make([]any, 0, len(t.entries))
	for _, entry := range t.entries {
		items = append(items, entry.ToMap())
	}
	return items
}

func (t Transcript) ToMaps() []map[string]any {
	items := make([]map[string]any, 0, len(t.entries))
	for _, entry := range t.entries {
		items = append(items, entry.ToMap())
	}
	return items
}
