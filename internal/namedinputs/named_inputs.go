package namedinputs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/integration"
)

const (
	DiagnosticCodeContractRequired = "named_input_contract_required"
	DiagnosticCodeInvalidBinding   = "invalid_named_input_binding"
	DiagnosticCodeUndeclaredInput  = "undeclared_named_input"
	DiagnosticCodeCardinality      = "named_input_cardinality"
	DiagnosticCodeContextConflict  = "named_input_context_conflict"
	DiagnosticCodeFileUnavailable  = "named_input_file_unavailable"
	DiagnosticCodeFileNotRegular   = "named_input_file_not_regular"
	DiagnosticCodeFileTooLarge     = "named_input_file_too_large"
	DiagnosticCodeTotalTooLarge    = "named_input_total_too_large"
	DiagnosticCodeInvalidMediaType = "invalid_named_input_media_type"
	DiagnosticCodeIntegrity        = "named_input_integrity_failed"
	SchemaStatusValidated          = "validated"
	SchemaStatusNotDeclared        = "not_declared"
	SchemaStatusNotApplicable      = "not_applicable"
)

// Options describes the pure named-input preflight. Binding values use the
// repeatable name=path CLI contract and are resolved relative to SourceAnchor.
type Options struct {
	Contract                *integration.SelectedContract
	Bindings                []string
	SourceAnchor            string
	PositionalContextCount  int
	Context                 context.Context
	NamedInputMaxBytes      int64
	NamedInputTotalMaxBytes int64
}

// Binding preserves the exact opaque input name and everything after the first
// equals sign as the path.
type Binding struct {
	Name string
	Path string
}

// Item is immutable metadata for one prepared input. SourcePath is deliberately
// retained for the internal manifest and must not be sent to providers.
type Item struct {
	Ordinal      int
	Name         string
	NameOrdinal  int
	SourcePath   string
	DisplayName  string
	SizeBytes    int64
	RawDigest    string
	MediaType    string
	SchemaStatus string
}

type preparedItem struct {
	Item
	data      []byte
	jsonValue any
	isJSON    bool
}

// Prepared owns a byte-for-byte snapshot of every input after pure preflight.
// Its private byte slices prevent later source-file changes from altering what
// is persisted or materialized.
type Prepared struct {
	contractID string
	items      []preparedItem
}

func (p *Prepared) ContractID() string {
	if p == nil {
		return ""
	}
	return p.contractID
}

func (p *Prepared) Items() []Item {
	if p == nil {
		return nil
	}
	items := make([]Item, len(p.items))
	for index := range p.items {
		items[index] = p.items[index].Item
	}
	return items
}

// AssertionInputs returns independent JSON values keyed by exact input name.
// Non-JSON inputs are omitted because integration assertions only consume JSON.
func (p *Prepared) AssertionInputs() map[string][]any {
	result := map[string][]any{}
	if p == nil {
		return result
	}
	for _, item := range p.items {
		if !item.isJSON {
			continue
		}
		result[item.Name] = append(result[item.Name], contracts.Materialize(item.jsonValue))
	}
	return result
}

func ParseBinding(raw string) (Binding, error) {
	offset := strings.IndexByte(raw, '=')
	if offset < 0 {
		return Binding{}, diagnosticError(
			nil,
			DiagnosticCodeInvalidBinding,
			contracts.DiagnosticPhasePreflight,
			"",
			"Named input bindings must use name=path.",
			map[string]any{"binding": raw},
		)
	}
	name := raw[:offset]
	path := raw[offset+1:]
	if strings.TrimSpace(name) == "" {
		return Binding{}, diagnosticError(nil, DiagnosticCodeInvalidBinding, contracts.DiagnosticPhasePreflight, "", "Named input names must be non-empty opaque strings.", nil)
	}
	if path == "" {
		return Binding{}, diagnosticError(
			nil,
			DiagnosticCodeInvalidBinding,
			contracts.DiagnosticPhasePreflight,
			"",
			"Named input paths must be non-empty.",
			map[string]any{"name": name},
		)
	}
	return Binding{Name: name, Path: path}, nil
}

// Prepare performs all binding, file, encoding, media, and schema validation
// without creating a session or writing any artifacts.
func Prepare(options Options) (*Prepared, error) {
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := retainedContextError(ctx); err != nil {
		return nil, err
	}
	if options.PositionalContextCount < 0 {
		return nil, diagnosticError(nil, DiagnosticCodeContextConflict, contracts.DiagnosticPhasePolicy, "", "Positional context count cannot be negative.", nil)
	}
	if options.Contract == nil {
		if len(options.Bindings) > 0 {
			return nil, diagnosticError(nil, DiagnosticCodeContractRequired, contracts.DiagnosticPhasePreflight, "", "Named inputs require a selected integration contract.", nil)
		}
		return &Prepared{}, nil
	}
	contract := options.Contract.Contract()
	if contract == nil {
		return nil, diagnosticError(nil, DiagnosticCodeContractRequired, contracts.DiagnosticPhasePreflight, "", "Named inputs require a selected integration contract.", nil)
	}
	if len(contract.Inputs) > 0 && options.PositionalContextCount > 0 {
		return nil, diagnosticError(
			nil,
			DiagnosticCodeContextConflict,
			contracts.DiagnosticPhasePolicy,
			"",
			"Positional context is not allowed when the selected contract declares named inputs.",
			map[string]any{"context_count": options.PositionalContextCount},
		)
	}

	bindings := make([]Binding, len(options.Bindings))
	counts := map[string]int{}
	for index, raw := range options.Bindings {
		binding, err := ParseBinding(raw)
		if err != nil {
			return nil, prefixBindingError(err, index)
		}
		declaration, exists := contract.Inputs[binding.Name]
		if !exists || declaration == nil {
			return nil, diagnosticError(
				nil,
				DiagnosticCodeUndeclaredInput,
				contracts.DiagnosticPhasePreflight,
				bindingPointer(index),
				"Named input is not declared by the selected contract.",
				map[string]any{"name": binding.Name},
			)
		}
		counts[binding.Name]++
		if declaration.Cardinality == integration.CardinalityOne && counts[binding.Name] > 1 {
			return nil, cardinalityError(binding.Name, declaration, counts[binding.Name])
		}
		bindings[index] = binding
	}
	if err := validateCardinalities(contract.Inputs, counts); err != nil {
		return nil, err
	}

	runtimeMaxBytes, totalMaxBytes, err := effectiveRuntimeLimits(options)
	if err != nil {
		return nil, err
	}
	prepared := &Prepared{contractID: options.Contract.ID(), items: make([]preparedItem, 0, len(bindings))}
	nameOrdinals := map[string]int{}
	var totalBytes int64
	for index, binding := range bindings {
		if err := retainedContextError(ctx); err != nil {
			return nil, err
		}
		declaration := contract.Inputs[binding.Name]
		nameOrdinals[binding.Name]++
		item, nextTotal, err := prepareFile(
			ctx,
			binding,
			declaration,
			options.SourceAnchor,
			index+1,
			nameOrdinals[binding.Name],
			runtimeMaxBytes,
			totalBytes,
			totalMaxBytes,
		)
		if err != nil {
			return nil, err
		}
		prepared.items = append(prepared.items, item)
		totalBytes = nextTotal
	}
	return prepared, nil
}

func effectiveRuntimeLimits(options Options) (int64, int64, error) {
	runtimeMaxBytes := options.NamedInputMaxBytes
	totalMaxBytes := options.NamedInputTotalMaxBytes
	if runtimeMaxBytes < 0 || totalMaxBytes < 0 {
		return 0, 0, diagnosticError(
			nil,
			contracts.DiagnosticCodeResourceAccountingOverflow,
			contracts.DiagnosticPhasePreflight,
			"/runtime_config/limits",
			"Named input runtime limits must be positive integers.",
			map[string]any{
				"named_input_max_bytes":       runtimeMaxBytes,
				"named_input_total_max_bytes": totalMaxBytes,
			},
		)
	}
	// Zero preserves the package-level API for callers that do not own runtime
	// configuration. Root execution always supplies its positive effective
	// limits.
	if runtimeMaxBytes == 0 {
		runtimeMaxBytes = math.MaxInt64
	}
	if totalMaxBytes == 0 {
		totalMaxBytes = math.MaxInt64
	}
	return runtimeMaxBytes, totalMaxBytes, nil
}

func validateCardinalities(declarations map[string]*integration.InputDeclaration, counts map[string]int) error {
	names := make([]string, 0, len(declarations))
	for name := range declarations {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		declaration := declarations[name]
		if declaration == nil {
			return diagnosticError(nil, DiagnosticCodeCardinality, contracts.DiagnosticPhasePreflight, inputPointer(name), "Named input declaration is missing.", map[string]any{"name": name})
		}
		count := counts[name]
		switch declaration.Cardinality {
		case integration.CardinalityOne:
			if count > 1 || (declaration.Required && count != 1) {
				return cardinalityError(name, declaration, count)
			}
		case integration.CardinalityMany:
			if declaration.Required && count < 1 {
				return cardinalityError(name, declaration, count)
			}
		default:
			return cardinalityError(name, declaration, count)
		}
	}
	return nil
}

func cardinalityError(name string, declaration *integration.InputDeclaration, count int) error {
	required := false
	cardinality := ""
	if declaration != nil {
		required = declaration.Required
		cardinality = declaration.Cardinality
	}
	return diagnosticError(
		nil,
		DiagnosticCodeCardinality,
		contracts.DiagnosticPhasePreflight,
		inputPointer(name),
		"Named input values do not satisfy the declared cardinality.",
		map[string]any{"name": name, "required": required, "cardinality": cardinality, "value_count": count},
	)
}

func prepareFile(
	ctx context.Context,
	binding Binding,
	declaration *integration.InputDeclaration,
	sourceAnchor string,
	ordinal int,
	nameOrdinal int,
	runtimeMaxBytes int64,
	totalBytes int64,
	totalMaxBytes int64,
) (preparedItem, int64, error) {
	effectiveMaxBytes := declaration.MaxBytes
	if runtimeMaxBytes < effectiveMaxBytes {
		effectiveMaxBytes = runtimeMaxBytes
	}
	sourcePath, data, digest, nextTotal, err := readRegularFile(
		ctx,
		binding.Path,
		sourceAnchor,
		effectiveMaxBytes,
		totalBytes,
		totalMaxBytes,
	)
	if err != nil {
		return preparedItem{}, totalBytes, prefixFileError(
			err,
			binding.Name,
			nameOrdinal,
			binding.Path,
			declaration.MaxBytes,
			runtimeMaxBytes,
			effectiveMaxBytes,
		)
	}
	if err := retainedContextError(ctx); err != nil {
		return preparedItem{}, totalBytes, err
	}
	jsonValue, isJSON, schemaStatus, err := validateContent(binding.Name, nameOrdinal, data, declaration)
	if err != nil {
		return preparedItem{}, totalBytes, err
	}
	if err := retainedContextError(ctx); err != nil {
		return preparedItem{}, totalBytes, err
	}
	return preparedItem{
		Item: Item{
			Ordinal:      ordinal,
			Name:         binding.Name,
			NameOrdinal:  nameOrdinal,
			SourcePath:   sourcePath,
			DisplayName:  filepath.Base(sourcePath),
			SizeBytes:    int64(len(data)),
			RawDigest:    digest,
			MediaType:    declaration.MediaType,
			SchemaStatus: schemaStatus,
		},
		data:      append([]byte(nil), data...),
		jsonValue: contracts.Materialize(jsonValue),
		isJSON:    isJSON,
	}, nextTotal, nil
}

func readRegularFile(
	ctx context.Context,
	rawPath string,
	sourceAnchor string,
	maxBytes int64,
	totalBytes int64,
	totalMaxBytes int64,
) (string, []byte, string, int64, error) {
	if maxBytes <= 0 {
		return "", nil, "", totalBytes, fmt.Errorf("maximum byte limit must be positive")
	}
	resolved := rawPath
	if !filepath.IsAbs(resolved) {
		anchor := sourceAnchor
		if anchor == "" {
			var err error
			anchor, err = os.Getwd()
			if err != nil {
				return "", nil, "", totalBytes, err
			}
		}
		resolved = filepath.Join(anchor, resolved)
	}
	absolutePath, err := filepath.Abs(resolved)
	if err != nil {
		return "", nil, "", totalBytes, err
	}
	if err := retainedContextError(ctx); err != nil {
		return "", nil, "", totalBytes, err
	}
	info, err := os.Lstat(absolutePath)
	if err != nil {
		return "", nil, "", totalBytes, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, "", totalBytes, errNotRegular
	}
	if err := contracts.CheckResourceLimit(
		contracts.DiagnosticCodeNamedInputMaxBytes,
		"named input bytes",
		info.Size(),
		maxBytes,
	); err != nil {
		return "", nil, "", totalBytes, err
	}
	handle, openedInfo, err := openNamedInputFile(absolutePath)
	if err != nil {
		return "", nil, "", totalBytes, err
	}
	defer handle.Close()
	if !os.SameFile(info, openedInfo) {
		return "", nil, "", totalBytes, errNotRegular
	}
	if err := contracts.CheckResourceLimit(
		contracts.DiagnosticCodeNamedInputMaxBytes,
		"named input bytes",
		openedInfo.Size(),
		maxBytes,
	); err != nil {
		return "", nil, "", totalBytes, err
	}
	nextTotal, err := contracts.CheckedAddResource(
		totalBytes,
		openedInfo.Size(),
		totalMaxBytes,
		contracts.DiagnosticCodeNamedInputTotalMaxBytes,
		"aggregate named input bytes",
	)
	if err != nil {
		return "", nil, "", totalBytes, err
	}
	data, digest, err := readNamedInputBytes(ctx, handle, maxBytes)
	if err != nil {
		return "", nil, "", totalBytes, err
	}
	openedAfter, statErr := handle.Stat()
	pathAfter, pathErr := os.Lstat(absolutePath)
	if statErr != nil || pathErr != nil ||
		!openedAfter.Mode().IsRegular() ||
		!os.SameFile(openedInfo, openedAfter) ||
		!os.SameFile(openedInfo, pathAfter) ||
		openedAfter.Size() != openedInfo.Size() ||
		pathAfter.Size() != openedInfo.Size() ||
		openedAfter.Mode() != openedInfo.Mode() ||
		pathAfter.Mode() != openedInfo.Mode() ||
		!openedAfter.ModTime().Equal(openedInfo.ModTime()) ||
		!pathAfter.ModTime().Equal(openedInfo.ModTime()) {
		return "", nil, "", totalBytes, errors.Join(statErr, pathErr, errFileChanged)
	}
	if int64(len(data)) != openedInfo.Size() {
		return "", nil, "", totalBytes, errFileChanged
	}
	if err := retainedContextError(ctx); err != nil {
		return "", nil, "", totalBytes, err
	}
	return filepath.Clean(absolutePath), data, digest, nextTotal, nil
}

func readNamedInputBytes(ctx context.Context, reader io.Reader, maxBytes int64) ([]byte, string, error) {
	contextReader := &retainedContextReader{ctx: ctx, reader: reader}
	var source io.Reader = contextReader
	if maxBytes < math.MaxInt64 {
		source = &io.LimitedReader{R: contextReader, N: maxBytes + 1}
	}
	var buffer bytes.Buffer
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(&buffer, hasher), source)
	if err != nil {
		return nil, "", err
	}
	if err := contracts.CheckResourceLimit(
		contracts.DiagnosticCodeNamedInputMaxBytes,
		"named input bytes",
		size,
		maxBytes,
	); err != nil {
		return nil, "", err
	}
	if err := retainedContextError(ctx); err != nil {
		return nil, "", err
	}
	return buffer.Bytes(), contracts.DigestPrefix + hex.EncodeToString(hasher.Sum(nil)), nil
}

func openNamedInputFile(path string) (*os.File, os.FileInfo, error) {
	handle, err := openNamedInputFileNoFollow(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := handle.Stat()
	if err != nil {
		_ = handle.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = handle.Close()
		return nil, nil, errNotRegular
	}
	return handle, info, nil
}

var (
	errNotRegular  = errors.New("not a regular file")
	errFileChanged = errors.New("named input file changed while it was read")
)

func validateContent(name string, nameOrdinal int, data []byte, declaration *integration.InputDeclaration) (any, bool, string, error) {
	mediaType, err := parseNamedInputMediaType(declaration.MediaType)
	if err != nil {
		return nil, false, "", diagnosticError(
			err,
			DiagnosticCodeInvalidMediaType,
			contracts.DiagnosticPhasePreflight,
			inputValuePointer(name, nameOrdinal),
			"Named input media type is invalid.",
			map[string]any{"name": name, "media_type": declaration.MediaType},
		)
	}
	mediaType = strings.ToLower(mediaType)
	isJSON := mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
	if declaration.Schema != nil && !isJSON {
		return nil, false, "", diagnosticError(
			nil,
			DiagnosticCodeInvalidMediaType,
			contracts.DiagnosticPhasePreflight,
			inputValuePointer(name, nameOrdinal),
			"Named input schemas require a JSON media type.",
			map[string]any{"name": name, "media_type": declaration.MediaType},
		)
	}
	if isJSON {
		value, err := contracts.DecodeStrictJSONBytes(data)
		if err != nil {
			return nil, true, "", contextualDiagnosticError(err, name, nameOrdinal, "Named input is not valid strict JSON.")
		}
		status := SchemaStatusNotDeclared
		if declaration.Schema != nil {
			if err := declaration.Schema.Validate(value); err != nil {
				return nil, true, "", contextualDiagnosticError(err, name, nameOrdinal, "Named input does not match its declared JSON Schema.")
			}
			status = SchemaStatusValidated
		}
		return value, true, status, nil
	}
	if strings.HasPrefix(mediaType, "text/") && !utf8.Valid(data) {
		return nil, false, "", diagnosticError(
			nil,
			contracts.DiagnosticCodeInvalidUTF8,
			contracts.DiagnosticPhaseDecode,
			inputValuePointer(name, nameOrdinal),
			"Text named input must be valid UTF-8.",
			map[string]any{"name": name, "input_index": nameOrdinal - 1},
		)
	}
	return nil, false, SchemaStatusNotApplicable, nil
}

func parseNamedInputMediaType(value string) (string, error) {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil {
		return "", fmt.Errorf("invalid named input media type: %w", err)
	}
	if strings.Contains(mediaType, "*") {
		return "", fmt.Errorf("wildcard named input media types are not supported")
	}
	mediaType = strings.ToLower(mediaType)
	isTextual := strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
	if charset, exists := parameters["charset"]; isTextual && exists && !strings.EqualFold(charset, "utf-8") {
		return "", fmt.Errorf("named input media type requires UTF-8, not %q", charset)
	}
	return mediaType, nil
}

func contextualDiagnosticError(cause error, name string, nameOrdinal int, message string) error {
	var structured *contracts.DiagnosticError
	if errors.As(cause, &structured) && len(structured.Diagnostics) > 0 {
		diagnostics := make([]contracts.Diagnostic, 0, len(structured.Diagnostics))
		for _, source := range structured.Diagnostics {
			details := map[string]any{"name": name, "input_index": nameOrdinal - 1}
			for key, value := range source.Details {
				details[key] = value
			}
			diagnostics = append(diagnostics, contracts.NewDiagnostic(
				source.Code,
				source.Phase,
				inputValuePointer(name, nameOrdinal)+source.Path,
				source.Message,
				details,
			))
		}
		return contracts.WrapDiagnosticError(cause, message, diagnostics...)
	}
	return diagnosticError(cause, DiagnosticCodeIntegrity, contracts.DiagnosticPhasePreflight, inputValuePointer(name, nameOrdinal), message, map[string]any{"name": name, "input_index": nameOrdinal - 1})
}

func prefixBindingError(err error, index int) error {
	var structured *contracts.DiagnosticError
	if !errors.As(err, &structured) || len(structured.Diagnostics) == 0 {
		return err
	}
	diagnostics := make([]contracts.Diagnostic, 0, len(structured.Diagnostics))
	for _, source := range structured.Diagnostics {
		diagnostics = append(diagnostics, contracts.NewDiagnostic(source.Code, source.Phase, bindingPointer(index)+source.Path, source.Message, source.Details))
	}
	return contracts.WrapDiagnosticError(err, structured.Error(), diagnostics...)
}

func prefixFileError(
	err error,
	name string,
	ordinal int,
	rawPath string,
	contractMaxBytes int64,
	runtimeMaxBytes int64,
	effectiveMaxBytes int64,
) error {
	code := DiagnosticCodeFileUnavailable
	message := "Named input file could not be read."
	path := inputValuePointer(name, ordinal)
	details := map[string]any{
		"name":                name,
		"source_path":         rawPath,
		"contract_max_bytes":  contractMaxBytes,
		"runtime_max_bytes":   runtimeMaxBytes,
		"effective_max_bytes": effectiveMaxBytes,
	}
	var limitErr *contracts.ResourceLimitError
	switch {
	case errors.Is(err, errNotRegular):
		code = DiagnosticCodeFileNotRegular
		message = "Named input path must identify a regular file and must not be a symlink."
	case errors.As(err, &limitErr):
		if limitErr.Code == contracts.DiagnosticCodeNamedInputTotalMaxBytes ||
			limitErr.Code == contracts.DiagnosticCodeResourceAccountingOverflow {
			code = DiagnosticCodeTotalTooLarge
			path = "/runtime_config/limits/named_input_total_max_bytes"
			message = "Named inputs exceed the configured aggregate raw-byte budget."
		} else {
			code = DiagnosticCodeFileTooLarge
			message = "Named input file exceeds its effective raw-byte budget."
		}
		for key, value := range resourceLimitDetails(limitErr) {
			details[key] = value
		}
	}
	return diagnosticError(
		err,
		code,
		contracts.DiagnosticPhasePreflight,
		path,
		message,
		details,
	)
}

func resourceLimitDetails(limitErr *contracts.ResourceLimitError) map[string]any {
	if limitErr == nil {
		return map[string]any{}
	}
	return map[string]any{
		"resource":  limitErr.Resource,
		"limit":     limitErr.Limit,
		"observed":  limitErr.Observed,
		"current":   limitErr.Current,
		"increment": limitErr.Increment,
	}
}

func diagnosticError(cause error, code string, phase string, path string, message string, details map[string]any) error {
	diagnostic := contracts.NewDiagnostic(code, phase, path, message, details)
	return contracts.WrapDiagnosticError(cause, message, diagnostic)
}

func rawDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return contracts.DigestPrefix + hex.EncodeToString(sum[:])
}

func bindingPointer(index int) string {
	return fmt.Sprintf("/bindings/%d", index)
}

func inputPointer(name string) string {
	return "/inputs/" + escapePointer(name)
}

func inputValuePointer(name string, ordinal int) string {
	return fmt.Sprintf("%s/%d", inputPointer(name), ordinal-1)
}

func escapePointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
