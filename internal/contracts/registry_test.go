package contracts

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestPublicVersionRegistryIsCompleteOrderedAndImmutable(t *testing.T) {
	registry := PublicVersionRegistry()
	if got, want := registry.Numeric(ContractRecipe), []int{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recipe versions = %#v, want %#v", got, want)
	}
	if got, want := registry.Strings(ContractIntegrationBundle), []string{IntegrationBundleV1, IntegrationBundleV2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bundle versions = %#v, want %#v", got, want)
	}
	versions := registry.Numeric(ContractRecipe)
	versions[0] = 99
	if got := PublicVersionRegistry().Numeric(ContractRecipe); got[0] != 1 {
		t.Fatalf("registry mutated through returned slice: %#v", got)
	}
	if got := registry.NumericContracts(); !sortStringsAreStrict(got) {
		t.Fatalf("numeric contract names are not ordered: %#v", got)
	}
	if got := registry.StringContracts(); !sortStringsAreStrict(got) {
		t.Fatalf("string contract names are not ordered: %#v", got)
	}
}

func TestVersionDispatchAcceptsOnlyExplicitRegisteredVersions(t *testing.T) {
	for _, version := range []any{1, json.Number("1"), 2, json.Number("2")} {
		if _, err := RequireNumericVersion(map[string]any{"schema_version": version}, ContractRootArtifact); err != nil {
			t.Fatalf("numeric version %v rejected: %v", version, err)
		}
	}
	for _, version := range []any{nil, true, 1.0, json.Number("1.0"), json.Number("1e0"), 0, 3, "2"} {
		_, err := RequireNumericVersion(map[string]any{"schema_version": version}, ContractRootArtifact)
		assertUnsupportedVersionDiagnostic(t, err)
	}
	for _, version := range []string{IntegrationBundleV1, IntegrationBundleV2} {
		if _, err := RequireStringVersion(map[string]any{"schema_version": version}, ContractIntegrationBundle); err != nil {
			t.Fatalf("string version %q rejected: %v", version, err)
		}
	}
	for _, version := range []any{nil, 1, "relay-integration-bundle-v3"} {
		_, err := RequireStringVersion(map[string]any{"schema_version": version}, ContractIntegrationBundle)
		assertUnsupportedVersionDiagnostic(t, err)
	}
}

func assertUnsupportedVersionDiagnostic(t *testing.T, err error) {
	t.Helper()
	var diagnosticErr *DiagnosticError
	if !errors.As(err, &diagnosticErr) || len(diagnosticErr.Diagnostics) != 1 {
		t.Fatalf("error = %T %v, want one diagnostic", err, err)
	}
	if diagnosticErr.Diagnostics[0].Code != DiagnosticCodeUnsupportedContractVersion || diagnosticErr.Diagnostics[0].Path != "/schema_version" {
		t.Fatalf("diagnostic = %#v", diagnosticErr.Diagnostics[0])
	}
}

func sortStringsAreStrict(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}
