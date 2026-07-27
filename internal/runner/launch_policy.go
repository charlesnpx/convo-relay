package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

const (
	PromptPolicyVersion           = contracts.PromptPolicyV1
	PromptPolicyVersionV2         = contracts.PromptPolicyV2
	diagnosticCodePromptAuthority = "prompt_authority_unsatisfied"
	MaxLaunchContextFileBytes     = int64(1 << 20)
	MaxLaunchContextTotalBytes    = int64(2 << 20)
	defaultInvestigationMode      = "auto"
	investigationModeNormal       = "normal"
	investigationModeAuto         = "auto"
	investigationModeContextOnly  = "context_only"
)

type InputBundle struct {
	Kind        string
	Label       string
	DisplayName string
	SourcePath  string
	Digest      string
	SizeBytes   int64
	Embedded    bool
	Content     string
}

type LaunchContext = InputBundle

type PromptPolicy struct {
	Version              string
	InvestigationMode    string
	EvidenceRequired     bool
	AllowRepoInspection  bool
	UseLaunchContextOnly bool
	Sources              []map[string]any
}

func PreflightLaunchContexts(paths []string) ([]LaunchContext, error) {
	bundles, err := preflightInputBundles("context", "ctx", paths, "")
	return bundles, err
}

func PreflightSkillInputs(paths []string) ([]InputBundle, error) {
	return preflightInputBundles("skill", "skill", paths, "")
}

// preflightLaunchContextsAt and preflightSkillInputsAt preserve the ordinary
// run wrappers above while allowing recipe-mode inputs to resolve from the
// explicit launch CWD source anchor.
func preflightLaunchContextsAt(paths []string, sourceAnchor string) ([]LaunchContext, error) {
	return preflightInputBundles("context", "ctx", paths, sourceAnchor)
}

func preflightSkillInputsAt(paths []string, sourceAnchor string) ([]InputBundle, error) {
	return preflightInputBundles("skill", "skill", paths, sourceAnchor)
}

func preflightInputBundles(kind string, labelPrefix string, paths []string, sourceAnchor string) ([]InputBundle, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	type checkedInput struct {
		displayName string
		sourcePath  string
		digest      string
		sizeBytes   int64
		content     string
	}
	seen := map[string]bool{}
	var checked []checkedInput
	var total int64
	for _, rawPath := range paths {
		rawPath = strings.TrimSpace(rawPath)
		if rawPath == "" {
			return nil, fmt.Errorf("%s path is empty", kind)
		}
		inputPath := rawPath
		if !filepath.IsAbs(inputPath) && strings.TrimSpace(sourceAnchor) != "" {
			inputPath = filepath.Join(sourceAnchor, inputPath)
		}
		absPath, err := filepath.Abs(inputPath)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", kind, rawPath, err)
		}
		info, err := os.Stat(absPath)
		if err != nil {
			return nil, fmt.Errorf("%s %q is unreadable: %w", kind, rawPath, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%s %q is a directory; provide a text file", kind, rawPath)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s %q is not a regular file", kind, rawPath)
		}
		normalizedPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return nil, fmt.Errorf("%s %q cannot be normalized: %w", kind, rawPath, err)
		}
		if seen[normalizedPath] {
			return nil, fmt.Errorf("duplicate %s input after path normalization: %s", kind, rawPath)
		}
		seen[normalizedPath] = true
		if info.Size() > MaxLaunchContextFileBytes {
			return nil, fmt.Errorf("%s %q is %d bytes; limit is %d bytes per file", kind, rawPath, info.Size(), MaxLaunchContextFileBytes)
		}
		total += info.Size()
		if total > MaxLaunchContextTotalBytes {
			return nil, fmt.Errorf("%s inputs total %d bytes; limit is %d bytes", kind, total, MaxLaunchContextTotalBytes)
		}
		data, err := os.ReadFile(normalizedPath)
		if err != nil {
			return nil, fmt.Errorf("%s %q is unreadable: %w", kind, rawPath, err)
		}
		if !isSupportedPromptText(data) {
			return nil, fmt.Errorf("%s %q appears to be binary or unsupported text; only UTF-8 text files are supported", kind, rawPath)
		}
		sum := sha256.Sum256(data)
		checked = append(checked, checkedInput{
			displayName: filepath.Base(normalizedPath),
			sourcePath:  normalizedPath,
			digest:      "sha256:" + hex.EncodeToString(sum[:]),
			sizeBytes:   int64(len(data)),
			content:     string(data),
		})
	}
	contexts := make([]InputBundle, 0, len(checked))
	for index, item := range checked {
		contexts = append(contexts, InputBundle{
			Kind:        kind,
			Label:       fmt.Sprintf("%s%d", labelPrefix, index+1),
			DisplayName: item.displayName,
			SourcePath:  item.sourcePath,
			Digest:      item.digest,
			SizeBytes:   item.sizeBytes,
			Embedded:    true,
			Content:     item.content,
		})
	}
	return contexts, nil
}

func isSupportedPromptText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	return true
}

func BuildTaskWithLaunchContext(task string, contexts []LaunchContext) string {
	if len(contexts) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(task)
	builder.WriteString("\n\n--- Launch Context ---\n")
	for _, context := range contexts {
		builder.WriteString("\n### ")
		builder.WriteString(context.Label)
		builder.WriteString(": ")
		builder.WriteString(context.DisplayName)
		builder.WriteString("\n")
		builder.WriteString("Source: ")
		builder.WriteString(context.SourcePath)
		builder.WriteString("\nDigest: ")
		builder.WriteString(context.Digest)
		builder.WriteString("\nEmbedded: true\n````text\n")
		builder.WriteString(context.Content)
		if !strings.HasSuffix(context.Content, "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString("````\n")
	}
	return builder.String()
}

func BuildSkillsPromptText(skills []InputBundle) string {
	var builder strings.Builder
	for _, skill := range skills {
		builder.WriteString(inputBundlePromptBlock(skill))
	}
	return builder.String()
}

func BuildResumeInputDirection(direction string, contexts []InputBundle, skills []InputBundle) string {
	if len(contexts) == 0 && len(skills) == 0 {
		return strings.TrimSpace(direction)
	}
	var builder strings.Builder
	if strings.TrimSpace(direction) != "" {
		builder.WriteString(strings.TrimSpace(direction))
		builder.WriteString("\n\n")
	}
	builder.WriteString("Use these additional resume inputs for subsequent turns. Cite labels such as [ctx1] when grounding empirical claims in attached context.\n")
	if len(contexts) > 0 {
		builder.WriteString("\n--- Resume Context ---\n")
		for _, context := range contexts {
			builder.WriteString(inputBundlePromptBlock(context))
		}
	}
	if len(skills) > 0 {
		builder.WriteString("\n--- Resume Capabilities ---\n")
		for _, skill := range skills {
			builder.WriteString(inputBundlePromptBlock(skill))
		}
	}
	return strings.TrimSpace(builder.String())
}

func inputBundlePromptBlock(bundle InputBundle) string {
	var builder strings.Builder
	builder.WriteString("\n### ")
	builder.WriteString(bundle.Label)
	builder.WriteString(": ")
	builder.WriteString(bundle.DisplayName)
	builder.WriteString("\n")
	builder.WriteString("Source: ")
	builder.WriteString(bundle.SourcePath)
	builder.WriteString("\nDigest: ")
	builder.WriteString(bundle.Digest)
	builder.WriteString("\nEmbedded: true\n````text\n")
	builder.WriteString(bundle.Content)
	if !strings.HasSuffix(bundle.Content, "\n") {
		builder.WriteString("\n")
	}
	builder.WriteString("````\n")
	return builder.String()
}

func BuildPromptPolicy(rawMode string, hasLaunchContext bool) (PromptPolicy, error) {
	mode := strings.TrimSpace(rawMode)
	if mode == "" {
		mode = defaultInvestigationMode
	}
	switch mode {
	case investigationModeNormal:
		return PromptPolicy{Version: PromptPolicyVersion, InvestigationMode: mode}, nil
	case investigationModeAuto:
		return PromptPolicy{
			Version:             PromptPolicyVersion,
			InvestigationMode:   mode,
			EvidenceRequired:    true,
			AllowRepoInspection: true,
		}, nil
	case investigationModeContextOnly:
		if !hasLaunchContext {
			return PromptPolicy{}, fmt.Errorf("--investigation context_only requires at least one --context file")
		}
		return PromptPolicy{
			Version:              PromptPolicyVersion,
			InvestigationMode:    mode,
			EvidenceRequired:     true,
			AllowRepoInspection:  false,
			UseLaunchContextOnly: true,
		}, nil
	default:
		return PromptPolicy{}, fmt.Errorf("--investigation must be normal, auto, or context_only")
	}
}

func BuildRootPromptPolicy(rawMode string, hasLaunchContext bool, declaresNamedInputs bool, boundNames []string) (PromptPolicy, error) {
	if !declaresNamedInputs {
		return BuildPromptPolicy(rawMode, hasLaunchContext)
	}
	mode := strings.TrimSpace(rawMode)
	if mode == "" {
		mode = defaultInvestigationMode
	}
	if mode != investigationModeNormal && mode != investigationModeAuto && mode != investigationModeContextOnly {
		return PromptPolicy{}, fmt.Errorf("--investigation must be normal, auto, or context_only")
	}
	if mode == investigationModeContextOnly && len(boundNames) == 0 {
		diagnostic := contracts.NewDiagnostic(
			diagnosticCodePromptAuthority,
			contracts.DiagnosticPhasePolicy,
			"/investigation",
			"context_only requires at least one successfully bound authoritative named input.",
			nil,
		)
		return PromptPolicy{}, contracts.NewDiagnosticError(diagnostic.Message, diagnostic)
	}
	policy := PromptPolicy{
		Version:             PromptPolicyVersionV2,
		InvestigationMode:   mode,
		EvidenceRequired:    mode != investigationModeNormal,
		AllowRepoInspection: mode == investigationModeAuto,
	}
	seen := map[string]bool{}
	for _, name := range boundNames {
		if !seen[name] {
			policy.Sources = append(policy.Sources, map[string]any{"kind": "named_input", "label": name})
			seen[name] = true
		}
	}
	return policy, nil
}

func (p PromptPolicy) ToMap() map[string]any {
	version := p.Version
	if version == "" {
		version = PromptPolicyVersion
	}
	mode := p.InvestigationMode
	if mode == "" {
		mode = investigationModeNormal
	}
	if version == PromptPolicyVersionV2 {
		sources := make([]any, 0, len(p.Sources))
		for _, source := range p.Sources {
			sources = append(sources, cloneMap(source))
		}
		return map[string]any{
			"schema_version":        version,
			"investigation_mode":    mode,
			"evidence_required":     p.EvidenceRequired,
			"allow_repo_inspection": p.AllowRepoInspection,
			"sources":               sources,
		}
	}
	return map[string]any{
		"version":                 version,
		"investigation_mode":      mode,
		"evidence_required":       p.EvidenceRequired,
		"allow_repo_inspection":   p.AllowRepoInspection,
		"use_launch_context_only": p.UseLaunchContextOnly,
	}
}

func finalizeNamedInputPromptPolicy(policy PromptPolicy, manifestRef map[string]any, manifest map[string]any) (PromptPolicy, error) {
	if policy.Version != PromptPolicyVersionV2 {
		return policy, nil
	}
	validatedManifestRef, err := contracts.ValidateArtifactRef(manifestRef)
	if err != nil {
		return PromptPolicy{}, persistenceIntegrityError("Prompt policy requires a valid named input manifest ref.", map[string]any{"cause": err.Error()})
	}
	rawInputs, ok := manifest["inputs"].([]any)
	if !ok {
		return PromptPolicy{}, persistenceIntegrityError("Prompt policy requires an ordered named input manifest.", nil)
	}
	sources := make([]map[string]any, 0)
	byLabel := map[string]int{}
	for index, raw := range rawInputs {
		entry, ok := raw.(map[string]any)
		if !ok {
			return PromptPolicy{}, persistenceIntegrityError("Prompt policy named input manifest entry is invalid.", map[string]any{"ordinal": index + 1})
		}
		label := stringFromAny(entry["name"])
		contentRef, err := contracts.ValidateArtifactRef(entry["content_ref"])
		if strings.TrimSpace(label) == "" || err != nil {
			return PromptPolicy{}, persistenceIntegrityError("Prompt policy named input manifest entry is incomplete.", map[string]any{"ordinal": index + 1})
		}
		sourceIndex, exists := byLabel[label]
		if !exists {
			sourceIndex = len(sources)
			byLabel[label] = sourceIndex
			sources = append(sources, map[string]any{
				"kind":         "named_input",
				"label":        label,
				"manifest_ref": cloneMap(validatedManifestRef),
				"content_refs": []any{},
			})
		}
		sources[sourceIndex]["content_refs"] = append(sources[sourceIndex]["content_refs"].([]any), cloneMap(contentRef))
	}
	policy.Sources = sources
	return policy, nil
}

func promptPolicyFromMeta(meta map[string]any) PromptPolicy {
	mode := stringFromAny(meta["investigation_mode"])
	policyMap, _ := meta["prompt_policy"].(map[string]any)
	version := stringFromAny(policyMap["schema_version"])
	if version == "" {
		version = firstNonEmpty(stringFromAny(policyMap["version"]), stringFromAny(meta["prompt_policy_version"]))
	}
	if version == PromptPolicyVersionV2 {
		if mode == "" {
			mode = stringFromAny(policyMap["investigation_mode"])
		}
		if mode == "" {
			mode = investigationModeNormal
		}
		evidenceRequired, _ := policyMap["evidence_required"].(bool)
		allowRepoInspection, _ := policyMap["allow_repo_inspection"].(bool)
		policy := PromptPolicy{
			Version:             version,
			InvestigationMode:   mode,
			EvidenceRequired:    evidenceRequired,
			AllowRepoInspection: allowRepoInspection,
		}
		for _, raw := range asSlice(policyMap["sources"]) {
			if source, ok := raw.(map[string]any); ok {
				policy.Sources = append(policy.Sources, cloneMap(source))
			}
		}
		return policy
	}
	if mode == "" {
		mode = stringFromAny(policyMap["investigation_mode"])
	}
	if mode == "" {
		mode = investigationModeNormal
	}
	policy, err := BuildPromptPolicy(mode, len(asSlice(meta["launch_context_refs"])) > 0)
	if err != nil {
		return PromptPolicy{Version: PromptPolicyVersion, InvestigationMode: investigationModeNormal}
	}
	if value, ok := policyMap["evidence_required"].(bool); ok {
		policy.EvidenceRequired = value
	}
	if value, ok := policyMap["allow_repo_inspection"].(bool); ok {
		policy.AllowRepoInspection = value
	}
	if value, ok := policyMap["use_launch_context_only"].(bool); ok {
		policy.UseLaunchContextOnly = value
	}
	return policy
}

func promptPolicyFragment(policy PromptPolicy) string {
	if !policy.EvidenceRequired {
		return ""
	}
	if policy.InvestigationMode == investigationModeContextOnly {
		if policy.Version == PromptPolicyVersionV2 {
			labels := make([]string, 0, len(policy.Sources))
			for _, source := range policy.Sources {
				labels = append(labels, "["+stringFromAny(source["label"])+"]")
			}
			return fmt.Sprintf(
				"- Evidence policy: use only the authoritative named inputs %s for empirical claims. Cite their stable names and retained artifact refs; do not inspect repositories, files, or data beyond those inputs.\n- Unsupported empirical claims should remain contested until grounded in a cited named input.\n",
				strings.Join(labels, ", "),
			)
		}
		return "- Evidence policy: use only the supplied launch context when making empirical claims. Cite launch context labels such as [ctx1] and do not inspect repositories, files, or data beyond that supplied context.\n- Unsupported empirical claims should remain contested until grounded in a cited context label.\n"
	}
	return "- Evidence policy: inspect relevant repository files, data, or supplied launch context before making empirical claims. Cite inspected file paths or launch context labels such as [ctx1].\n- Unsupported empirical claims should remain contested until grounded in cited evidence.\n"
}

func launchContextMetadata(context LaunchContext) map[string]any {
	return map[string]any{
		"kind":           "input_bundle",
		"schema_version": 1,
		"bundle_kind":    firstNonEmpty(context.Kind, "context"),
		"label":          context.Label,
		"display_name":   context.DisplayName,
		"source_path":    context.SourcePath,
		"digest":         context.Digest,
		"size_bytes":     context.SizeBytes,
		"embedded":       context.Embedded,
	}
}

func launchContextArtifactPayload(context LaunchContext) map[string]any {
	payload := launchContextMetadata(context)
	payload["content"] = context.Content
	return payload
}
