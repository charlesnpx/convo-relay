package model

import "strings"

type SessionMeta struct {
	fields map[string]any
}

func NewSessionMeta(fields map[string]any) SessionMeta {
	return SessionMeta{fields: normalizeSessionMetaFields(fields)}
}

func EmptySessionMeta() SessionMeta {
	return NewSessionMeta(map[string]any{})
}

func normalizeSessionMetaFields(fields map[string]any) map[string]any {
	result := cloneMap(fields)
	if rawLedger, ok := result["ledger"]; ok {
		result["ledger"] = ParseLedger(rawLedger).ToMap()
	}
	return result
}

func (m SessionMeta) ToMap() map[string]any {
	return cloneMap(m.fields)
}

func (m SessionMeta) Get(key string) any {
	return cloneValue(m.fields[key])
}

func (m SessionMeta) Has(key string) bool {
	_, ok := m.fields[key]
	return ok
}

func (m SessionMeta) String(key string) string {
	return stringFromAny(m.fields[key])
}

func (m SessionMeta) Int(key string, fallback int) int {
	return intFromAny(m.fields[key], fallback)
}

func (m SessionMeta) Bool(key string) bool {
	return boolFromAny(m.fields[key])
}

func (m SessionMeta) Slice(key string) []any {
	return asSlice(m.fields[key])
}

func (m SessionMeta) Ledger() Ledger {
	return ParseLedger(m.fields["ledger"])
}

func (m SessionMeta) Counts() LedgerCounts {
	return m.Ledger().Counts()
}

func (m SessionMeta) With(key string, value any) SessionMeta {
	next := m.ToMap()
	next[key] = cloneValue(value)
	return NewSessionMeta(next)
}

func (m SessionMeta) Without(key string) SessionMeta {
	next := m.ToMap()
	delete(next, key)
	return NewSessionMeta(next)
}

func (m SessionMeta) WithStatus(status string) SessionMeta {
	return m.With("status", strings.TrimSpace(status))
}

func (m SessionMeta) WithMode(mode string) SessionMeta {
	return m.With("mode", strings.TrimSpace(mode))
}

func (m SessionMeta) WithLedger(ledger Ledger) SessionMeta {
	return m.With("ledger", ledger.ToMap())
}

func (m SessionMeta) WithActualRounds(rounds int) SessionMeta {
	return m.With("actual_rounds", rounds)
}

func (m SessionMeta) WithSlots(slots []any) SessionMeta {
	return m.With("slots", slots)
}

func (m SessionMeta) WithRoundLimit(mode string, rounds any, maxRounds int) SessionMeta {
	next := m.With("round_limit_mode", mode)
	next = next.With("rounds", rounds)
	return next.With("max_rounds", maxRounds)
}

func (m SessionMeta) WithTitleIfEmpty(title string) SessionMeta {
	if strings.TrimSpace(m.String("title")) != "" {
		return m
	}
	return m.With("title", strings.TrimSpace(title))
}

func (m SessionMeta) AppendToSlice(key string, values ...any) SessionMeta {
	items := asSlice(m.fields[key])
	for _, value := range values {
		items = append(items, cloneValue(value))
	}
	return m.With(key, items)
}

func (m SessionMeta) WithCompleted(actualRounds int, elapsedSeconds float64, completedAt string, stopReason string) SessionMeta {
	next := m.WithStatus("completed").
		WithActualRounds(actualRounds).
		With("elapsed_seconds", elapsedSeconds).
		With("completed_at", completedAt).
		With("stop_reason", stopReason)
	return next.Without("error")
}

func (m SessionMeta) WithInterrupted(actualRounds int, interruptedAt string, reason string) SessionMeta {
	return m.WithStatus("interrupted").
		With("error", reason).
		WithActualRounds(actualRounds).
		With("interrupted_at", interruptedAt)
}

func (m SessionMeta) WithFailed(actualRounds int, failedAt string, err error) SessionMeta {
	return m.WithStatus("failed").
		With("error", err.Error()).
		WithActualRounds(actualRounds).
		With("failed_at", failedAt)
}

func (m SessionMeta) WithAttentionRequired(actualRounds int, createdAt string, err error, rejection map[string]any) SessionMeta {
	return m.WithStatus("attention_required").
		With("attention_required", true).
		With("attention_required_at", createdAt).
		With("error", err.Error()).
		WithActualRounds(actualRounds).
		AppendToSlice("mode_control_rejections", rejection)
}
