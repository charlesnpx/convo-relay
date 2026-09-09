// Package bundle defines, validates, and verifies portable export bundles.
package bundle

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/charlesnpx/convo-relay/v2/internal/semanticjson"
	"github.com/charlesnpx/convo-relay/v2/plan"
)

const (
	// Kind is the required value of Manifest.Kind; any other value makes a
	// manifest invalid.
	Kind = "relay.bundle/v1"
)

// Manifest is the portable export directory's manifest.json document. It is
// invalid when required payload names, inventory ordering, blob references, or
// either semantic digest does not match the manifest contents.
type Manifest struct {
	// Kind identifies this manifest family and must equal Kind; another value
	// makes the manifest invalid.
	Kind string `json:"kind"`
	// ConvoRelayVersion records the CLI version that produced the bundle and must
	// be non-empty; an empty value makes the manifest invalid.
	ConvoRelayVersion string `json:"convo_relay_version"`
	// TerminalStatus records the exported session status and must be non-empty;
	// an empty value makes the manifest invalid.
	TerminalStatus string `json:"terminal_status"`
	// StopReason records the optional terminal stop reason; nil means no reason
	// was recorded and is valid.
	StopReason *string `json:"stop_reason"`
	// SessionPayload names the required root-session inventory path; it must name
	// an inventoried payload or the manifest is invalid.
	SessionPayload string `json:"session_payload"`
	// TranscriptPayload names the required participant-transcript inventory path;
	// it must name an inventoried payload or the manifest is invalid.
	TranscriptPayload string `json:"transcript_payload"`
	// DiagnosticsPayload names the required diagnostics inventory path; it must
	// name an inventoried payload or the manifest is invalid.
	DiagnosticsPayload string `json:"diagnostics_payload"`
	// PayloadInventory lists every exported payload in ascending path order; it
	// must be non-empty, use unique identities, and contain valid entries.
	PayloadInventory []InventoryEntry `json:"payload_inventory"`
	// InventoryDigest is the semantic-JSON digest of PayloadInventory; a missing
	// or mismatched digest makes the manifest invalid.
	InventoryDigest string `json:"inventory_digest"`
	// ManifestDigest is the semantic-JSON digest of this manifest with this field
	// omitted; a missing or mismatched digest makes the manifest invalid.
	ManifestDigest string `json:"manifest_digest"`
}

// UnmarshalJSON decodes canonical semantic-JSON numbers into Manifest's
// integer-containing blob references while retaining strict unknown-field
// rejection. It returns an error for malformed JSON, unknown fields, or values
// that cannot be decoded into Manifest.
func (value *Manifest) UnmarshalJSON(body []byte) error {
	type plainManifest Manifest
	var decoded plainManifest
	if err := semanticjson.DecodeJSON(body, &decoded); err != nil {
		return err
	}
	*value = Manifest(decoded)
	return nil
}

// InventoryEntry identifies one payload file in a portable bundle. It is
// invalid when its path, identity, or blob metadata does not agree.
type InventoryEntry struct {
	// Kind is the path's payload category and must be a safe path component; an
	// empty or unsafe value makes the entry invalid.
	Kind string `json:"kind"`
	// PortableID is the stable payload identity and must be a safe path
	// component; an empty or unsafe value makes the entry invalid.
	PortableID string `json:"portable_id"`
	// Path is the exact relative path payloads/<kind>/<portable_id>.json; any
	// other path makes the entry invalid.
	Path string `json:"path"`
	// Blob describes the raw bytes stored at Path; malformed digest, size, or
	// media type makes the entry invalid.
	Blob plan.BlobRef `json:"blob"`
}

// Validate checks one decoded or programmatically built manifest. It returns an
// error naming the invalid field or relationship and never panics. It rejects
// missing payloads, unsafe inventory entries, duplicate or unsorted paths, and
// either mismatched semantic digest.
func Validate(value Manifest) error {
	if value.Kind != Kind {
		return fmt.Errorf("portable bundle manifest kind must be %s", Kind)
	}
	if strings.TrimSpace(value.ConvoRelayVersion) == "" {
		return errors.New("portable bundle manifest convo_relay_version must be a non-empty string")
	}
	if strings.TrimSpace(value.TerminalStatus) == "" {
		return errors.New("portable bundle manifest terminal_status must be a non-empty string")
	}
	if len(value.PayloadInventory) == 0 {
		return errors.New("portable bundle payload_inventory must be a non-empty array")
	}
	paths := map[string]bool{}
	identities := map[string]bool{}
	previousPath := ""
	for index, entry := range value.PayloadInventory {
		if err := validateInventoryEntry(entry, index); err != nil {
			return err
		}
		identity := entry.Kind + ":" + entry.PortableID
		if paths[entry.Path] || identities[identity] || previousPath != "" && previousPath >= entry.Path {
			return errors.New("portable bundle payload_inventory must use unique identities and ascending paths")
		}
		paths[entry.Path] = true
		identities[identity] = true
		previousPath = entry.Path
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "session_payload", value: value.SessionPayload},
		{name: "transcript_payload", value: value.TranscriptPayload},
		{name: "diagnostics_payload", value: value.DiagnosticsPayload},
	} {
		if strings.TrimSpace(field.value) == "" || !paths[field.value] {
			return fmt.Errorf("portable bundle %s must name an inventoried payload", field.name)
		}
	}
	wantInventoryDigest, err := semanticjson.SemanticJSONDigest(value.PayloadInventory)
	if err != nil {
		return err
	}
	if value.InventoryDigest != wantInventoryDigest {
		return errors.New("portable bundle inventory digest mismatch")
	}
	wantManifestDigest, err := semanticjson.SemanticJSONDigest(manifestDigestMaterial(value))
	if err != nil {
		return err
	}
	if value.ManifestDigest != wantManifestDigest {
		return errors.New("portable bundle manifest digest mismatch")
	}
	return nil
}

type manifestDigestMaterialType struct {
	Kind               string           `json:"kind"`
	ConvoRelayVersion  string           `json:"convo_relay_version"`
	TerminalStatus     string           `json:"terminal_status"`
	StopReason         *string          `json:"stop_reason"`
	SessionPayload     string           `json:"session_payload"`
	TranscriptPayload  string           `json:"transcript_payload"`
	DiagnosticsPayload string           `json:"diagnostics_payload"`
	PayloadInventory   []InventoryEntry `json:"payload_inventory"`
	InventoryDigest    string           `json:"inventory_digest"`
}

func manifestDigestMaterial(value Manifest) manifestDigestMaterialType {
	return manifestDigestMaterialType{
		Kind:               value.Kind,
		ConvoRelayVersion:  value.ConvoRelayVersion,
		TerminalStatus:     value.TerminalStatus,
		StopReason:         value.StopReason,
		SessionPayload:     value.SessionPayload,
		TranscriptPayload:  value.TranscriptPayload,
		DiagnosticsPayload: value.DiagnosticsPayload,
		PayloadInventory:   value.PayloadInventory,
		InventoryDigest:    value.InventoryDigest,
	}
}

func validateInventoryEntry(value InventoryEntry, index int) error {
	if strings.TrimSpace(value.Kind) == "" || strings.TrimSpace(value.PortableID) == "" || strings.TrimSpace(value.Path) == "" {
		return fmt.Errorf("portable bundle inventory entry %d requires kind, portable_id, and path", index)
	}
	if !portablePathComponent(value.Kind) || !portablePathComponent(value.PortableID) || value.Path != path.Join("payloads", value.Kind, value.PortableID+".json") {
		return fmt.Errorf("portable bundle payload_inventory[%d] has an invalid path identity", index)
	}
	if err := validateBlobRef(value.Blob); err != nil {
		return fmt.Errorf("portable bundle payload_inventory[%d] has an invalid blob: %w", index, err)
	}
	return nil
}

func validateBlobRef(value plan.BlobRef) error {
	if len(value.SHA256) != 64 {
		return errors.New("sha256 must be 64 lower-case hexadecimal characters")
	}
	for _, character := range value.SHA256 {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return errors.New("sha256 must be 64 lower-case hexadecimal characters")
	}
	if value.Size < 0 {
		return errors.New("size must not be negative")
	}
	if strings.TrimSpace(value.MediaType) == "" {
		return errors.New("media_type is required")
	}
	if strings.ContainsAny(value.MediaType, "\r\n\x00") {
		return errors.New("media_type contains a control character")
	}
	return nil
}

func portablePathComponent(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return false
	}
	first := value[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9') {
		return false
	}
	for _, runeValue := range value {
		if runeValue >= 'a' && runeValue <= 'z' || runeValue >= 'A' && runeValue <= 'Z' || runeValue >= '0' && runeValue <= '9' || runeValue == '_' || runeValue == '-' || runeValue == '.' {
			continue
		}
		return false
	}
	return true
}
