package provider

import "testing"

func TestGeminiParseOutputAcceptsKnownShapesAndPlainText(t *testing.T) {
	tests := []struct {
		name     string
		stdout   string
		wantText string
		wantRef  string
	}{
		{name: "response", stdout: `{"response":"Gemini reply","session_id":"gemini-session"}`, wantText: "Gemini reply", wantRef: "gemini-session"},
		{name: "text", stdout: `{"text":"Text reply","session_id":"s1"}`, wantText: "Text reply", wantRef: "s1"},
		{name: "content", stdout: `{"content":"Content reply"}`, wantText: "Content reply"},
		{name: "message", stdout: `{"message":"Message reply"}`, wantText: "Message reply"},
		{name: "structured error", stdout: `{"error":{"message":"Structured error"}}`, wantText: "Structured error"},
		{name: "string error", stdout: `{"error":"String error"}`, wantText: "String error"},
		{name: "plain text", stdout: `plain fallback`, wantText: "plain fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, ref := parseGeminiOutput(tt.stdout)
			if text != tt.wantText || ref != tt.wantRef {
				t.Fatalf("parseGeminiOutput = (%q, %q), want (%q, %q)", text, ref, tt.wantText, tt.wantRef)
			}
		})
	}
}
