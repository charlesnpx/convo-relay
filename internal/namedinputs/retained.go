package namedinputs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

type Materialized struct {
	ProviderInputs map[string]any
	Descriptor     map[string]any
	DescriptorRef  map[string]any
}

// LoadRetained verifies a present retained materialization and reconstructs
// the provider projection exclusively from its digest-checked descriptor and
// manifest. It performs no writes.
func LoadRetained(
	ctx context.Context,
	st *store.Store,
	manifestRef map[string]any,
	descriptorRef map[string]any,
	role string,
	boundary string,
) (*Materialized, error) {
	if err := VerifyRetained(ctx, st, descriptorRef, role, boundary); err != nil {
		return nil, err
	}
	descriptor, descriptorEntries, _, err := loadRetainedDescriptor(st, descriptorRef)
	if err != nil {
		return nil, err
	}
	manifest, manifestEntries, err := loadManifest(st, manifestRef)
	if err != nil {
		return nil, err
	}
	descriptorManifestRef, err := contracts.ValidateArtifactRef(descriptor["manifest_ref"])
	if err != nil || !matchingArtifactRefs(descriptorManifestRef, manifestRef) {
		return nil, integrityError(err, "Retained named input descriptor does not match its manifest ref.", nil)
	}
	if descriptor["contract_id"] != manifest["contract_id"] ||
		len(descriptorEntries) != len(manifestEntries) {
		return nil, integrityError(nil, "Retained named input descriptor does not match its manifest.", nil)
	}

	rawDescriptorEntries, _ := descriptor["inputs"].([]any)
	providerItems := make([]any, 0, len(descriptorEntries))
	for index, descriptorEntry := range descriptorEntries {
		if err := retainedContextError(ctx); err != nil {
			return nil, err
		}
		manifestEntry := manifestEntries[index]
		rawDescriptorEntry, _ := rawDescriptorEntries[index].(map[string]any)
		if descriptorEntry.ordinal != manifestEntry.ordinal ||
			descriptorEntry.name != manifestEntry.name ||
			descriptorEntry.nameOrdinal != manifestEntry.nameOrdinal ||
			descriptorEntry.sizeBytes != manifestEntry.sizeBytes ||
			descriptorEntry.rawDigest != manifestEntry.rawDigest ||
			!matchingArtifactRefs(rawDescriptorEntry["content_ref"], manifestEntry.contentRef) {
			return nil, integrityError(
				nil,
				"Retained named input descriptor entry does not match its manifest.",
				map[string]any{"ordinal": index + 1},
			)
		}
		providerItems = append(providerItems, map[string]any{
			"name":              manifestEntry.name,
			"ordinal":           manifestEntry.ordinal,
			"name_ordinal":      manifestEntry.nameOrdinal,
			"materialized_path": descriptorEntry.path,
			"size_bytes":        manifestEntry.sizeBytes,
			"raw_digest":        manifestEntry.rawDigest,
			"media_type":        manifestEntry.mediaType,
			"schema_status":     manifestEntry.schemaStatus,
			"content_ref":       cloneMap(manifestEntry.contentRef),
		})
	}
	return &Materialized{
		ProviderInputs: map[string]any{
			"contract_id":  manifest["contract_id"],
			"manifest_ref": cloneMap(manifestRef),
			"inputs":       providerItems,
		},
		Descriptor:    cloneMap(descriptor),
		DescriptorRef: cloneMap(descriptorRef),
	}, nil
}

// MaterializeRetained writes the manifest bytes to an exact ordinal layout
// and persists the descriptor consumed by the exact no-follow verifier.
func MaterializeRetained(
	ctx context.Context,
	st *store.Store,
	manifestRef map[string]any,
	executionInputDir string,
) (*Materialized, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if st == nil || strings.TrimSpace(st.Root) == "" {
		return nil, diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, "", "Named input materialization requires a session store.", nil)
	}
	manifest, entries, err := loadManifest(st, manifestRef)
	if err != nil {
		return nil, err
	}
	absoluteDir, err := safeMaterializationDirectory(st.Root, executionInputDir)
	if err != nil {
		return nil, err
	}
	if err := validateMaterializationDirectory(absoluteDir, len(entries)); err != nil {
		return nil, err
	}
	providerItems := make([]any, 0, len(entries))
	descriptorItems := make([]any, 0, len(entries))
	for _, entry := range entries {
		if err := retainedContextError(ctx); err != nil {
			return nil, err
		}
		filename := fmt.Sprintf("%06d", entry.ordinal)
		target := filepath.Join(absoluteDir, filename)
		if info, err := os.Lstat(target); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
			return nil, diagnosticError(nil, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePolicy, "", "Named input materialization target is not a regular file.", map[string]any{"ordinal": entry.ordinal})
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err := st.WriteFileAtomically(target, entry.data, 0o444); err != nil {
			return nil, err
		}
		providerItem := map[string]any{
			"name":              entry.name,
			"ordinal":           entry.ordinal,
			"name_ordinal":      entry.nameOrdinal,
			"materialized_path": target,
			"size_bytes":        entry.sizeBytes,
			"raw_digest":        entry.rawDigest,
			"media_type":        entry.mediaType,
			"schema_status":     entry.schemaStatus,
			"content_ref":       cloneMap(entry.contentRef),
		}
		providerItems = append(providerItems, providerItem)
		descriptorItems = append(descriptorItems, map[string]any{
			"name":              entry.name,
			"ordinal":           entry.ordinal,
			"name_ordinal":      entry.nameOrdinal,
			"materialized_path": target,
			"filename":          filename,
			"entry_type":        "regular",
			"mode":              "0444",
			"size_bytes":        entry.sizeBytes,
			"raw_digest":        entry.rawDigest,
			"content_ref":       cloneMap(entry.contentRef),
		})
	}
	if err := validateMaterializationDirectory(absoluteDir, len(entries)); err != nil {
		return nil, err
	}
	descriptor, err := contracts.NormalizeRootArtifact(contracts.RootArtifactKindRetainedInputs, map[string]any{
		"contract_id":  manifest["contract_id"],
		"manifest_ref": cloneMap(manifestRef),
		"directory":    absoluteDir,
		"input_count":  len(descriptorItems),
		"inputs":       descriptorItems,
	})
	if err != nil {
		return nil, err
	}
	identity, err := contracts.RootArtifactIdentityFor(contracts.RootArtifactKindRetainedInputs, 0)
	if err != nil {
		return nil, err
	}
	ref, err := st.SaveContractArtifact(
		contracts.RootArtifactKindRetainedInputs,
		identity.ArtifactID,
		descriptor,
		identity.RefID,
	)
	if err != nil {
		return nil, err
	}
	return &Materialized{
		ProviderInputs: map[string]any{
			"contract_id":  manifest["contract_id"],
			"manifest_ref": cloneMap(manifestRef),
			"inputs":       providerItems,
		},
		Descriptor:    cloneMap(descriptor),
		DescriptorRef: cloneMap(ref),
	}, nil
}

// VerifyRetained validates the descriptor ref and exact directory layout
// without following symlinks. Its diagnostics cannot contain input bytes.
func VerifyRetained(
	ctx context.Context,
	st *store.Store,
	descriptorRef map[string]any,
	role string,
	boundary string,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	descriptor, entries, directory, err := loadRetainedDescriptor(st, descriptorRef)
	if err != nil {
		return err
	}
	if err := retainedContextError(ctx); err != nil {
		return err
	}
	directory, err = safeExistingMaterializationDirectory(st.Root, directory)
	if err != nil {
		mismatch := retainedMismatch(role, boundary, entries, 0, IntegrityMismatchMissing)
		mismatch.Observed.Path = directory
		return retainedMismatchError(mismatch, err)
	}
	actual, err := readRetainedDirectoryNoFollow(directory)
	if err != nil {
		mismatch := retainedMismatch(role, boundary, entries, 0, IntegrityMismatchMissing)
		mismatch.Observed.Path = directory
		return retainedMismatchError(mismatch, err)
	}
	sort.Slice(actual, func(left int, right int) bool { return actual[left].Name() < actual[right].Name() })
	if len(actual) > len(entries) {
		expectedNames := make(map[string]bool, len(entries))
		for _, entry := range entries {
			expectedNames[entry.filename] = true
		}
		observed := ""
		for _, entry := range actual {
			if !expectedNames[entry.Name()] {
				observed = entry.Name()
				break
			}
		}
		mismatch := retainedMismatch(role, boundary, entries, len(entries), IntegrityMismatchUnexpected)
		mismatch.Observed.Path = filepath.Join(directory, observed)
		return retainedMismatchError(mismatch, nil)
	}
	if len(actual) < len(entries) {
		mismatch := retainedMismatch(role, boundary, entries, len(actual), IntegrityMismatchMissing)
		return retainedMismatchError(mismatch, nil)
	}
	for index, entry := range entries {
		if err := retainedContextError(ctx); err != nil {
			return err
		}
		if entry.ordinal != index+1 {
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchReordered)
			return retainedMismatchError(mismatch, nil)
		}
		if actual[index].Name() != entry.filename {
			category := IntegrityMismatchReordered
			if !retainedFilenamePresent(actual, entry.filename) {
				category = IntegrityMismatchPath
			}
			mismatch := retainedMismatch(role, boundary, entries, index, category)
			mismatch.Observed.Path = filepath.Join(directory, actual[index].Name())
			return retainedMismatchError(mismatch, nil)
		}
		target := filepath.Join(directory, entry.filename)
		if filepath.Clean(target) != filepath.Clean(entry.path) {
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchPath)
			mismatch.Observed.Path = target
			return retainedMismatchError(mismatch, nil)
		}
		info, err := os.Lstat(target)
		if err != nil {
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchMissing)
			return retainedMismatchError(mismatch, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchType)
			mismatch.Observed.Type = retainedEntryType(info.Mode())
			return retainedMismatchError(mismatch, nil)
		}
		if gotMode := fmt.Sprintf("%04o", info.Mode().Perm()); gotMode != entry.mode {
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchMode)
			mismatch.Observed.Mode = gotMode
			return retainedMismatchError(mismatch, nil)
		}
		if info.Size() != entry.sizeBytes {
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchSize)
			observed := info.Size()
			mismatch.Observed.SizeBytes = &observed
			return retainedMismatchError(mismatch, nil)
		}
		handle, err := openNamedInputFileNoFollow(target)
		if err != nil {
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchType)
			return retainedMismatchError(mismatch, err)
		}
		opened, statErr := handle.Stat()
		if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) ||
			opened.Size() != info.Size() || opened.Mode() != info.Mode() ||
			!opened.ModTime().Equal(info.ModTime()) {
			closeErr := handle.Close()
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchPath)
			return retainedMismatchError(mismatch, errors.Join(statErr, closeErr))
		}
		hasher := sha256.New()
		reader := &retainedContextReader{ctx: ctx, reader: handle}
		size, readErr := io.CopyN(hasher, reader, entry.sizeBytes)
		var extra [1]byte
		extraCount, extraErr := reader.Read(extra[:])
		if extraErr != nil && !errors.Is(extraErr, io.EOF) {
			readErr = errors.Join(readErr, extraErr)
		}
		after, statErr := handle.Stat()
		closeErr := handle.Close()
		if statErr != nil || closeErr != nil {
			return errors.Join(readErr, statErr, closeErr)
		}
		pathAfter, pathErr := os.Lstat(target)
		if pathErr != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) ||
			!os.SameFile(opened, pathAfter) || after.Size() != opened.Size() ||
			after.Mode() != opened.Mode() || !after.ModTime().Equal(opened.ModTime()) ||
			pathAfter.Size() != opened.Size() || pathAfter.Mode() != opened.Mode() ||
			!pathAfter.ModTime().Equal(opened.ModTime()) {
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchPath)
			return retainedMismatchError(mismatch, pathErr)
		}
		if readErr != nil || size != entry.sizeBytes || extraCount != 0 {
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchSize)
			observed := size + int64(extraCount)
			if after.Size() != entry.sizeBytes {
				observed = after.Size()
			}
			mismatch.Observed.SizeBytes = &observed
			return retainedMismatchError(mismatch, readErr)
		}
		digest := contracts.DigestPrefix + hex.EncodeToString(hasher.Sum(nil))
		if digest != entry.rawDigest {
			mismatch := retainedMismatch(role, boundary, entries, index, IntegrityMismatchDigest)
			mismatch.Observed.Digest = digest
			return retainedMismatchError(mismatch, nil)
		}
	}
	_ = descriptor
	return nil
}

type retainedEntry struct {
	ordinal     int
	name        string
	nameOrdinal int
	filename    string
	path        string
	mode        string
	sizeBytes   int64
	rawDigest   string
}

func loadRetainedDescriptor(
	st *store.Store,
	ref map[string]any,
) (map[string]any, []retainedEntry, string, error) {
	if st == nil {
		return nil, nil, "", integrityError(nil, "Retained named input verification requires a session store.", nil)
	}
	payload, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return nil, nil, "", integrityError(err, "Retained named input descriptor could not be loaded.", nil)
	}
	descriptor, err := contracts.ValidateRootArtifact(payload, contracts.RootArtifactKindRetainedInputs)
	if err != nil {
		return nil, nil, "", integrityError(err, "Retained named input descriptor is invalid.", nil)
	}
	if _, err := contracts.ValidateRootArtifactRef(ref, contracts.RootArtifactKindRetainedInputs, 0, descriptor); err != nil {
		return nil, nil, "", integrityError(err, "Retained named input descriptor ref is invalid.", nil)
	}
	directory, ok := descriptor["directory"].(string)
	if !ok || strings.TrimSpace(directory) == "" {
		return nil, nil, "", integrityError(nil, "Retained named input directory is invalid.", nil)
	}
	_, directory, _, err = materializationDirectoryPaths(st.Root, directory)
	if err != nil {
		return nil, nil, "", err
	}
	rawEntries, ok := descriptor["inputs"].([]any)
	if !ok {
		return nil, nil, "", integrityError(nil, "Retained named input entries are invalid.", nil)
	}
	count, ok := integerField(descriptor["input_count"])
	if !ok || count != int64(len(rawEntries)) {
		return nil, nil, "", integrityError(nil, "Retained named input count is invalid.", nil)
	}
	entries := make([]retainedEntry, 0, len(rawEntries))
	for index, raw := range rawEntries {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, "", integrityError(nil, "Retained named input entry is invalid.", map[string]any{"ordinal": index + 1})
		}
		ordinal, ordinalOK := integerField(item["ordinal"])
		nameOrdinal, nameOrdinalOK := integerField(item["name_ordinal"])
		size, sizeOK := integerField(item["size_bytes"])
		entry := retainedEntry{
			ordinal:     int(ordinal),
			name:        stringValue(item["name"]),
			nameOrdinal: int(nameOrdinal),
			filename:    stringValue(item["filename"]),
			path:        stringValue(item["materialized_path"]),
			mode:        stringValue(item["mode"]),
			sizeBytes:   size,
			rawDigest:   stringValue(item["raw_digest"]),
		}
		if !ordinalOK || !nameOrdinalOK || !sizeOK || ordinal < 1 || nameOrdinal < 1 || size < 0 ||
			strings.TrimSpace(entry.name) == "" || strings.TrimSpace(entry.filename) == "" ||
			entry.filename == "." || entry.filename == ".." || filepath.Base(entry.filename) != entry.filename ||
			strings.TrimSpace(entry.path) == "" ||
			item["entry_type"] != "regular" || entry.mode != "0444" ||
			!strings.HasPrefix(entry.rawDigest, contracts.DigestPrefix) {
			return nil, nil, "", integrityError(nil, "Retained named input entry metadata is invalid.", map[string]any{"ordinal": index + 1})
		}
		entries = append(entries, entry)
	}
	return descriptor, entries, directory, nil
}

func retainedMismatch(
	role string,
	boundary string,
	entries []retainedEntry,
	index int,
	category string,
) IntegrityMismatch {
	entry := retainedEntry{ordinal: index + 1}
	if index >= 0 && index < len(entries) {
		entry = entries[index]
	}
	size := entry.sizeBytes
	return IntegrityMismatch{
		Role:            role,
		AttemptBoundary: boundary,
		InputName:       entry.name,
		InputOrdinal:    entry.ordinal,
		Category:        category,
		Expected: IntegrityObservation{
			SizeBytes: &size,
			Digest:    entry.rawDigest,
			Type:      "regular",
			Mode:      entry.mode,
			Path:      entry.path,
		},
	}
}

func retainedMismatchError(mismatch IntegrityMismatch, cause error) error {
	diagnostic := mismatch.Diagnostic()
	if cause != nil {
		details := diagnostic.Details
		if details == nil {
			details = map[string]any{}
		}
		details["cause_type"] = fmt.Sprintf("%T", cause)
		diagnostic.Details = details
	}
	return contracts.NewDiagnosticError("Retained named input integrity verification failed.", diagnostic)
}

func retainedFilenamePresent(entries []os.DirEntry, filename string) bool {
	for _, entry := range entries {
		if entry.Name() == filename {
			return true
		}
	}
	return false
}

func matchingArtifactRefs(left any, right any) bool {
	leftRef, leftErr := contracts.ValidateArtifactRef(left)
	rightRef, rightErr := contracts.ValidateArtifactRef(right)
	return leftErr == nil && rightErr == nil &&
		leftRef["id"] == rightRef["id"] &&
		leftRef["digest"] == rightRef["digest"]
}

func retainedEntryType(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case mode.IsDir():
		return "directory"
	case mode.IsRegular():
		return "regular"
	default:
		return "other"
	}
}

func readRetainedDirectoryNoFollow(path string) ([]os.DirEntry, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, errors.New("retained input directory is not a real directory")
	}
	handle, err := openNamedInputFileNoFollow(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := handle.Stat()
	if statErr != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		closeErr := handle.Close()
		return nil, errors.Join(statErr, closeErr, errors.New("retained input directory changed before inspection"))
	}
	entries, readErr := handle.ReadDir(-1)
	after, afterErr := handle.Stat()
	closeErr := handle.Close()
	pathAfter, pathErr := os.Lstat(path)
	if readErr != nil || afterErr != nil || closeErr != nil || pathErr != nil {
		return nil, errors.Join(readErr, afterErr, closeErr, pathErr)
	}
	if !after.IsDir() || !os.SameFile(opened, after) || !os.SameFile(opened, pathAfter) ||
		after.Mode() != opened.Mode() || !after.ModTime().Equal(opened.ModTime()) ||
		pathAfter.Mode() != opened.Mode() || !pathAfter.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("retained input directory changed during inspection")
	}
	return entries, nil
}

type retainedContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *retainedContextReader) Read(buffer []byte) (int, error) {
	if err := retainedContextError(r.ctx); err != nil {
		return 0, err
	}
	count, err := r.reader.Read(buffer)
	if contextErr := retainedContextError(r.ctx); contextErr != nil {
		return count, contextErr
	}
	return count, err
}

func retainedContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
