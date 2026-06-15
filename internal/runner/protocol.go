package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/charlesnpx/convo-relay/internal/model"
)

var ledgerKeys = []string{"settled", "contested", "withdrawn"}

const facilitatorSystem = `You are tracking a debate between two AI agents. After each turn,
you update a ledger of what's settled, contested, and withdrawn.

Rules:
- "settled" means BOTH agents clearly agree. One agent stating something is not enough.
- "contested" means there's active disagreement or an unresolved challenge.
- "withdrawn" means an agent explicitly dropped a previous claim.
- Normalize phrasing: if both agents agree on the same point using different words,
  use a single canonical description.
- Be conservative: when in doubt, mark as contested rather than settled.
- Keep items concise - short noun phrases, not full sentences.

Respond with ONLY a JSON object, no other text:
{"settled": [...], "contested": [...], "withdrawn": [...]}`

type LedgerParseStatus string

const (
	LedgerParseParsedFull      LedgerParseStatus = "parsed_full"
	LedgerParseParsedExtracted LedgerParseStatus = "parsed_extracted"
	LedgerParseFallback        LedgerParseStatus = "fallback"
)

type LedgerParseReport struct {
	Status               LedgerParseStatus
	RawDigest            string
	RawExcerpt           string
	RawBytes             int
	ExcerptTruncated     bool
	FallbackLedgerCounts map[string]any
}

func emptyLedger() map[string]any {
	return model.EmptyLedger().ToMap()
}

func normalizeLedger(value any) map[string]any {
	return model.ParseLedger(value).ToMap()
}

func ledgerCounts(ledger map[string]any) map[string]any {
	return model.ParseLedger(ledger).CountsMap()
}

func makeTitle(text string) string {
	collapsed := collapseWhitespace(text)
	if collapsed == "" {
		return "Untitled relay session"
	}
	if len(collapsed) <= 80 {
		return collapsed
	}
	return strings.TrimSpace(collapsed[:77]) + "..."
}

func summarizeRecentTranscript(transcript model.Transcript) string {
	if transcript.Len() == 0 {
		return "No prior turns."
	}
	entries := transcript.Entries()
	start := len(entries) - 3
	if start < 0 {
		start = 0
	}
	var parts []string
	for _, entry := range entries[start:] {
		parts = append(parts, fmt.Sprintf("Round %v - %v: %s", entry.Round, entry.From, truncateWords(entry.Content, 160)))
	}
	return truncateWords(strings.Join(parts, "\n\n"), 500)
}

func truncateWords(text string, limit int) string {
	words := strings.Fields(text)
	if len(words) <= limit {
		return text
	}
	return strings.Join(words[:limit], " ") + " ..."
}

func relayHasConverged(transcript model.Transcript, ledger model.Ledger) bool {
	if transcript.Len() < 4 {
		return false
	}
	last, _ := transcript.At(transcript.Len() - 1)
	prev, _ := transcript.At(transcript.Len() - 2)
	if speakerID(last) == speakerID(prev) {
		return false
	}
	if hasDoneSignal(last.Content) && hasDoneSignal(prev.Content) {
		return true
	}
	counts := ledger.Counts()
	return (counts.Settled > 0 || counts.Withdrawn > 0) && counts.Contested == 0
}

func relayHasNoLedgerSignal(transcript model.Transcript, ledger model.Ledger) bool {
	if transcript.Len() < 4 || !ledger.IsEmpty() {
		return false
	}
	last, _ := transcript.At(transcript.Len() - 1)
	prev, _ := transcript.At(transcript.Len() - 2)
	return speakerID(last) != speakerID(prev)
}

func speakerID(entry model.TranscriptEntry) string {
	return entry.SpeakerID()
}

func hasDoneSignal(text string) bool {
	lowered := strings.ToLower(text)
	for _, signal := range []string{
		"task is complete",
		"work is complete",
		"no further changes",
		"ready to merge",
		"nothing else to add",
		"this covers everything",
	} {
		if strings.Contains(lowered, signal) {
			return true
		}
	}
	return false
}

func frameInitial(task string, partner string, maxRounds int, mode string, goesFirst bool, taskWithContext string, skillsText string, policy PromptPolicy) string {
	order := fmt.Sprintf("%s has already responded.", partner)
	if goesFirst {
		order = "You go first."
	}
	effectiveTask := task
	if strings.TrimSpace(taskWithContext) != "" {
		effectiveTask = taskWithContext
	}
	policyFragment := promptPolicyFragment(policy)
	if policyFragment != "" {
		policyFragment = "\n" + policyFragment
	}
	prompt := fmt.Sprintf(
		"You and %s are working through this together:\n\n> %s\n\n%d rounds max. %s\n\nGround rules:\n%s%s%s",
		partner,
		effectiveTask,
		maxRounds,
		order,
		modeNorms(mode),
		policyFragment,
		boundaryReminder(partner),
	)
	if strings.TrimSpace(skillsText) != "" {
		prompt += "\n\n--- Available Capabilities ---\n" + skillsText
	}
	return prompt
}

func frameRelay(content string, source string, roundNum int, maxRounds int, task string, mode string, ledger any, policy PromptPolicy) string {
	current := model.ParseLedger(ledger)
	settled := joinLedgerItems(current.Settled())
	contested := joinLedgerItems(current.Contested())
	state := fmt.Sprintf("\nSettled so far: %s\nStill contested: %s\n\n%s", settled, contested, promptPolicyFragment(policy))
	return fmt.Sprintf(
		"Round %d/%d - %s\n%s%s said:\n---\n%s\n---\n\n%s Respond with your own position only. Do not anticipate or simulate your partner's reply.",
		roundNum,
		maxRounds,
		task,
		state,
		source,
		content,
		modeCloser(mode),
	)
}

func frameResumeDirection(lastEntry any, roundNum int, maxRounds int, task string, direction string, mode string, ledger any, policy PromptPolicy) string {
	entry := transcriptEntryFromAny(lastEntry)
	current := model.ParseLedger(ledger)
	settled := joinLedgerItems(current.Settled())
	contested := joinLedgerItems(current.Contested())
	return fmt.Sprintf(
		"Round %d/%d - %s\nThe relay has been resumed with a new direction:\n> %s\n\nSettled so far: %s\nStill contested: %s\n\n%s%s said:\n---\n%s\n---\n\n%s Respond with your own position only. Do not anticipate or simulate your partner's reply.",
		roundNum,
		maxRounds,
		task,
		direction,
		settled,
		contested,
		promptPolicyFragment(policy),
		entry.From,
		entry.Content,
		modeCloser(mode),
	)
}

func transcriptEntryFromAny(value any) model.TranscriptEntry {
	switch typed := value.(type) {
	case model.TranscriptEntry:
		return typed
	case map[string]any:
		return model.ParseTranscriptEntry(typed)
	default:
		return model.TranscriptEntry{}
	}
}

func modeNorms(mode string) string {
	switch mode {
	case "cooperative":
		return "- Make a concrete contribution: analyze, propose, write code.\n- Build on your partner's work and add what they missed.\n- Be specific. Keep it under 800 words."
	case "steelman":
		return "- Before challenging any claim, restate it in its strongest form.\n- If you still disagree after steelmanning, explain why with evidence.\n- If the other agent's steelman of your position is actually better than your original, adopt it and say so.\n- Keep it under 600 words."
	default:
		return "- If you disagree, say so and say why. Don't hedge.\n- If the other agent challenges something you said, either defend it with evidence or drop it explicitly. No weaseling.\n- Don't repeat what's already settled. Move forward.\n- Add at most one new substantive point per round.\n- Keep it under 600 words. Tighter is better."
	}
}

func modeCloser(mode string) string {
	switch mode {
	case "cooperative":
		return "Continue. Build on what's there."
	case "steelman":
		return "Steelman their position before responding. Then advance or concede."
	default:
		return "Respond to what matters. Challenge what's wrong. Add only what's missing. You must contribute at least one substantive point - a new observation, rebuttal, or refinement. Only pass if you have genuinely exhausted all topics."
	}
}

func boundaryReminder(partner string) string {
	return fmt.Sprintf("\nYou are one voice in this dialogue. Respond with your own contribution only. Do not simulate or anticipate %s's responses. Do not run an internal relay or multi-agent conversation. Do not summarize a hypothetical completed exchange. %s will respond in the next round - you will see their actual words and respond to those.", partner, partner)
}

func joinLedgerItems(value any) string {
	items := normalizeLedgerJoinItems(value)
	if len(items) == 0 {
		return "nothing"
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, item)
	}
	return strings.Join(parts, ", ")
}

func normalizeLedgerJoinItems(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				items = append(items, text)
			}
		}
		return items
	default:
		return nil
	}
}

func parseLedgerFromText(raw string, fallback any) (model.Ledger, LedgerParseReport) {
	candidates := []string{strings.TrimSpace(raw)}
	re := regexp.MustCompile(`(?s)\{.*\}`)
	candidates = append(candidates, re.FindAllString(raw, -1)...)
	for index, candidate := range candidates {
		if candidate == "" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(candidate), &value); err != nil {
			continue
		}
		if hasLedgerShape(value) {
			status := LedgerParseParsedExtracted
			if index == 0 {
				status = LedgerParseParsedFull
			}
			return model.ParseLedger(value), LedgerParseReport{Status: status}
		}
	}
	ledger := model.ParseLedger(fallback)
	excerpt, truncated := utf8SafeByteExcerpt(raw, 160)
	return ledger, LedgerParseReport{
		Status:               LedgerParseFallback,
		RawDigest:            sha256DigestString(raw),
		RawExcerpt:           excerpt,
		RawBytes:             len([]byte(raw)),
		ExcerptTruncated:     truncated,
		FallbackLedgerCounts: ledger.CountsMap(),
	}
}

func sha256DigestString(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func utf8SafeByteExcerpt(text string, limit int) (string, bool) {
	if limit <= 0 {
		return "", len([]byte(text)) > 0
	}
	valid := strings.ToValidUTF8(text, "\uFFFD")
	data := []byte(valid)
	if len(data) <= limit {
		return valid, false
	}
	data = data[:limit]
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data), true
}

func hasLedgerShape(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range ledgerKeys {
		if _, ok := object[key].([]any); !ok {
			return false
		}
	}
	return true
}

func mustJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}
