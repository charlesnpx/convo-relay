package recipes

var backendRegistry = map[string]bool{
	"claude": true,
	"codex":  true,
	"gemini": true,
	"child":  true,
}

var defaultBackendProfiles = map[string]map[string]any{
	"codex-deep": {
		"id":          "codex-deep",
		"backend":     "codex",
		"model":       "gpt-5.5",
		"effort":      "xhigh",
		"description": "Deep code reasoning and high-stakes implementation review.",
	},
	"codex-fast": {
		"id":          "codex-fast",
		"backend":     "codex",
		"model":       "gpt-5.5",
		"effort":      "medium",
		"description": "Fast code reasoning, facilitation, and reducer work.",
	},
	"gemini-vision": {
		"id":          "gemini-vision",
		"backend":     "gemini",
		"model":       nil,
		"effort":      nil,
		"description": "Gemini profile for visual, screenshot, and image-heavy questions.",
	},
}

var defaultRelayRecipeRecords = map[string]map[string]any{
	"review-panel": {
		"id":            "review-panel",
		"purpose":       "Use when an unresolved implementation or design risk needs a focused child step.",
		"participants":  []any{"codex-deep", "codex-fast"},
		"facilitator":   "codex-fast",
		"reducer":       "codex-deep",
		"mode":          "adversarial",
		"max_rounds":    6,
		"max_depth":     1,
		"auto_approval": "ask",
	},
	"vision-review": {
		"id":            "vision-review",
		"purpose":       "Use when the unresolved item depends on screenshots, images, UI, or visual inspection.",
		"participants":  []any{"gemini-vision", "codex-deep"},
		"facilitator":   "codex-fast",
		"reducer":       "codex-deep",
		"mode":          "adversarial",
		"max_rounds":    4,
		"max_depth":     1,
		"auto_approval": "ask",
	},
	"one-pass-review": {
		"id":            "one-pass-review",
		"purpose":       "Use when deliberation credit is low and a single focused pass is preferable.",
		"participants":  []any{"codex-fast", "codex-deep"},
		"facilitator":   "codex-fast",
		"reducer":       "codex-deep",
		"mode":          "steelman",
		"max_rounds":    1,
		"max_depth":     2,
		"auto_approval": "ask",
	},
}

var defaultRelayRecipes = selectedDefaultRelayRecipeRecords(defaultRelayRecipeRecords)

func selectedDefaultRelayRecipeRecords(records map[string]map[string]any) map[string]map[string]any {
	selected := make(map[string]map[string]any, len(records))
	for recipeID, record := range records {
		selected[recipeID] = cloneObject(record)
	}
	return selected
}
