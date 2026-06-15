package runner

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseLedgerFromTextReportsFullExtractedAndFallback(t *testing.T) {
	fallback := map[string]any{
		"settled":   []any{"previous"},
		"contested": []any{},
		"withdrawn": []any{},
	}

	fullLedger, fullReport := parseLedgerFromText(`{"settled":["done"],"contested":[],"withdrawn":[]}`, fallback)
	if fullReport.Status != LedgerParseParsedFull {
		t.Fatalf("full parse status = %q", fullReport.Status)
	}
	if got := fullLedger.Settled(); len(got) != 1 || got[0] != "done" {
		t.Fatalf("full ledger = %#v", fullLedger.ToMap())
	}

	extractedLedger, extractedReport := parseLedgerFromText(`ledger: {"settled":[],"contested":["risk"],"withdrawn":[]} thanks`, fallback)
	if extractedReport.Status != LedgerParseParsedExtracted {
		t.Fatalf("extracted parse status = %q", extractedReport.Status)
	}
	if got := extractedLedger.Contested(); len(got) != 1 || got[0] != "risk" {
		t.Fatalf("extracted ledger = %#v", extractedLedger.ToMap())
	}

	fallbackLedger, fallbackReport := parseLedgerFromText("not a ledger", fallback)
	if fallbackReport.Status != LedgerParseFallback {
		t.Fatalf("fallback parse status = %q", fallbackReport.Status)
	}
	if got := fallbackLedger.Settled(); len(got) != 1 || got[0] != "previous" {
		t.Fatalf("fallback ledger = %#v", fallbackLedger.ToMap())
	}
	if fallbackReport.RawDigest == "" || fallbackReport.RawBytes != len("not a ledger") || fallbackReport.RawExcerpt != "not a ledger" {
		t.Fatalf("fallback report = %#v", fallbackReport)
	}
}

func TestParseLedgerFallbackExcerptIsUTF8SafeAndCapped(t *testing.T) {
	raw := strings.Repeat("abc\U0001F4A5", 50)
	_, report := parseLedgerFromText(raw, map[string]any{
		"settled":   []any{},
		"contested": []any{},
		"withdrawn": []any{},
	})
	if report.Status != LedgerParseFallback {
		t.Fatalf("parse status = %q", report.Status)
	}
	if !report.ExcerptTruncated {
		t.Fatalf("excerpt_truncated = false, want true")
	}
	if len([]byte(report.RawExcerpt)) > 160 {
		t.Fatalf("excerpt is %d bytes, want <= 160", len([]byte(report.RawExcerpt)))
	}
	if !utf8.ValidString(report.RawExcerpt) {
		t.Fatalf("excerpt is not valid UTF-8: %q", report.RawExcerpt)
	}
	if report.RawBytes != len([]byte(raw)) {
		t.Fatalf("raw bytes = %d, want %d", report.RawBytes, len([]byte(raw)))
	}
}

func TestParseLedgerFallbackRepairsInvalidUTF8WithoutMarkingTruncated(t *testing.T) {
	raw := string([]byte{'b', 'a', 'd', 0xff})
	_, report := parseLedgerFromText(raw, map[string]any{
		"settled":   []any{},
		"contested": []any{},
		"withdrawn": []any{},
	})
	if report.Status != LedgerParseFallback {
		t.Fatalf("parse status = %q", report.Status)
	}
	if report.ExcerptTruncated {
		t.Fatalf("excerpt_truncated = true, want false")
	}
	if !utf8.ValidString(report.RawExcerpt) {
		t.Fatalf("excerpt is not valid UTF-8: %q", report.RawExcerpt)
	}
	if report.RawBytes != 4 {
		t.Fatalf("raw bytes = %d, want 4", report.RawBytes)
	}
}
