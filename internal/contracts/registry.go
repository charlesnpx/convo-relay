package contracts

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	DiagnosticCodeUnsupportedContractVersion = "unsupported_contract_version"

	ContractRecipe                      = "recipe"
	ContractRootRecipePlan              = "root_recipe_plan"
	ContractIntegrationBundle           = "integration_bundle"
	ContractSelectedIntegrationContract = "selected_integration_contract"
	ContractRootArtifact                = "root_artifact"
	ContractExecutionWorkspace          = "execution_workspace"
	ContractRootSessionResult           = "root_session_result"

	IntegrationBundleV1 = "relay-integration-bundle-v1"
	IntegrationBundleV2 = "relay-integration-bundle-v2"

	PromptPolicyV1             = "prompt-policy/v1"
	PromptPolicyV2             = "prompt-policy/v2"
	PromptContextProjectionV1  = "relay-prompt-context-v1"
	ProviderRetryPolicyV1      = "relay-provider-retry-policy-v1"
	ProviderInvocationV1       = "relay-provider-invocation-v1"
	RenderedPromptV1           = "relay-rendered-prompt-v1"
	DigestProfileV1            = "relay-root-digests-v1"
	WorkspaceIsolationReportV1 = "relay-workspace-isolation-v1"
	PortableExportV1           = "relay-root-portable-export-v1"
	CapabilitiesV1             = "relay-capabilities-v1"
)

// VersionRegistry is the single implementation-owned inventory used by
// public decoders, writers, documentation fixtures, and capability output.
// Returned values are defensive copies so callers cannot change the registry.
type VersionRegistry struct {
	numeric map[string][]int
	string  map[string][]string
}

var publicVersionRegistry = VersionRegistry{
	numeric: map[string][]int{
		ContractRecipe:                      {1, 2},
		ContractRootRecipePlan:              {1, 2},
		ContractSelectedIntegrationContract: {1, 2},
		ContractRootArtifact:                {1, 2},
		ContractExecutionWorkspace:          {1, 2},
		ContractRootSessionResult:           {1, 2},
	},
	string: map[string][]string{
		ContractIntegrationBundle:   {IntegrationBundleV1, IntegrationBundleV2},
		"prompt_policy":             {PromptPolicyV1, PromptPolicyV2},
		"prompt_context_projection": {PromptContextProjectionV1},
		"provider_retry_policy":     {ProviderRetryPolicyV1},
		"provider_invocation":       {ProviderInvocationV1},
		"rendered_prompt":           {RenderedPromptV1},
		"portable_export":           {PortableExportV1},
		"digest_profile":            {DigestProfileV1},
		"isolation_report":          {WorkspaceIsolationReportV1},
		"workspace_mechanisms":      {"inherited", "detached_writable_git_worktree"},
		"capability_advertisement":  {CapabilitiesV1},
	},
}

func PublicVersionRegistry() VersionRegistry {
	return VersionRegistry{
		numeric: cloneNumericVersions(publicVersionRegistry.numeric),
		string:  cloneStringVersions(publicVersionRegistry.string),
	}
}

func (r VersionRegistry) Numeric(contract string) []int {
	return append([]int(nil), r.numeric[contract]...)
}

func (r VersionRegistry) Strings(contract string) []string {
	return append([]string(nil), r.string[contract]...)
}

func (r VersionRegistry) NumericContracts() []string {
	return sortedVersionKeys(r.numeric)
}

func (r VersionRegistry) StringContracts() []string {
	return sortedVersionKeys(r.string)
}

// BuildCapabilityAdvertisement projects the public registry without probing
// providers, authentication, or the host environment.
func BuildCapabilityAdvertisement(convoRelayVersion string, goos string, goarch string, schemaVersion string) (map[string]any, error) {
	if strings.TrimSpace(schemaVersion) == "" {
		schemaVersion = CapabilitiesV1
	}
	if _, err := RequireStringVersion(map[string]any{"schema_version": schemaVersion}, "capability_advertisement"); err != nil {
		return nil, err
	}
	convoRelayVersion = strings.TrimSpace(convoRelayVersion)
	goos = strings.TrimSpace(goos)
	goarch = strings.TrimSpace(goarch)
	if convoRelayVersion == "" || goos == "" || goarch == "" {
		return nil, NewValidationError("capability advertisement requires CLI version and build platform")
	}

	registry := PublicVersionRegistry()
	contractVersions := map[string]any{}
	for _, contract := range registry.NumericContracts() {
		contractVersions[contract] = registry.Numeric(contract)
	}
	contractVersions[ContractIntegrationBundle] = registry.Strings(ContractIntegrationBundle)

	report := map[string]any{
		"schema_version":      schemaVersion,
		"convo_relay_version": convoRelayVersion,
		"build_platform": map[string]any{
			"goos":   goos,
			"goarch": goarch,
		},
		"contracts": contractVersions,
	}
	for _, contract := range registry.StringContracts() {
		switch contract {
		case ContractIntegrationBundle, "capability_advertisement":
			continue
		default:
			report[contract] = registry.Strings(contract)
		}
	}
	return report, nil
}

func RequireNumericVersion(object map[string]any, contract string) (int, error) {
	raw, exists := object["schema_version"]
	if !exists {
		return 0, unsupportedVersionError(contract, nil, publicVersionRegistry.Numeric(contract))
	}
	version, ok := exactJSONInteger(raw)
	if !ok {
		return 0, unsupportedVersionError(contract, raw, publicVersionRegistry.Numeric(contract))
	}
	for _, supported := range publicVersionRegistry.numeric[contract] {
		if version == supported {
			return version, nil
		}
	}
	return 0, unsupportedVersionError(contract, raw, publicVersionRegistry.Numeric(contract))
}

func RequireStringVersion(object map[string]any, contract string) (string, error) {
	raw, exists := object["schema_version"]
	version, ok := raw.(string)
	if !exists || !ok {
		return "", unsupportedVersionError(contract, raw, publicVersionRegistry.Strings(contract))
	}
	for _, supported := range publicVersionRegistry.string[contract] {
		if version == supported {
			return version, nil
		}
	}
	return "", unsupportedVersionError(contract, raw, publicVersionRegistry.Strings(contract))
}

func unsupportedVersionError(contract string, observed any, supported any) error {
	diagnostic := NewDiagnostic(
		DiagnosticCodeUnsupportedContractVersion,
		DiagnosticPhaseDecode,
		"/schema_version",
		fmt.Sprintf("Unsupported %s schema version.", contract),
		map[string]any{
			"contract":  contract,
			"observed":  observed,
			"supported": supported,
		},
	)
	return NewDiagnosticError(diagnostic.Message, diagnostic)
}

func exactJSONInteger(value any) (int, bool) {
	if _, boolean := value.(bool); boolean {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		converted := int(typed)
		return converted, int64(converted) == typed
	case uint:
		converted := int(typed)
		return converted, converted >= 0 && uint(converted) == typed
	case uint8:
		return int(typed), true
	case uint16:
		return int(typed), true
	case uint32:
		converted := int(typed)
		return converted, converted >= 0 && uint32(converted) == typed
	case uint64:
		converted := int(typed)
		return converted, converted >= 0 && uint64(converted) == typed
	case json.Number:
		integer, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		converted := int(integer)
		return converted, int64(converted) == integer
	default:
		return 0, false
	}
}

func cloneNumericVersions(source map[string][]int) map[string][]int {
	result := make(map[string][]int, len(source))
	for key, versions := range source {
		result[key] = append([]int(nil), versions...)
	}
	return result
}

func cloneStringVersions(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for key, versions := range source {
		result[key] = append([]string(nil), versions...)
	}
	return result
}

func sortedVersionKeys[T any](source map[string]T) []string {
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
