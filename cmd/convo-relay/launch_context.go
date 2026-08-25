package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	maxLaunchContextFileBytes  = int64(1 << 20)
	maxLaunchContextTotalBytes = int64(2 << 20)
)

type launchContext struct {
	Label       string
	DisplayName string
	SourcePath  string
	Digest      string
	Content     string
}

func preflightLaunchContexts(paths []string) ([]launchContext, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	type checkedContext struct {
		displayName string
		sourcePath  string
		digest      string
		content     string
	}
	seen := map[string]bool{}
	var checked []checkedContext
	var total int64
	for _, rawPath := range paths {
		rawPath = strings.TrimSpace(rawPath)
		if rawPath == "" {
			return nil, fmt.Errorf("context path is empty")
		}
		absPath, err := filepath.Abs(rawPath)
		if err != nil {
			return nil, fmt.Errorf("context %q: %w", rawPath, err)
		}
		info, err := os.Stat(absPath)
		if err != nil {
			return nil, fmt.Errorf("context %q is unreadable: %w", rawPath, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("context %q is a directory; provide a text file", rawPath)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("context %q is not a regular file", rawPath)
		}
		normalizedPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return nil, fmt.Errorf("context %q cannot be normalized: %w", rawPath, err)
		}
		if seen[normalizedPath] {
			return nil, fmt.Errorf("duplicate context input after path normalization: %s", rawPath)
		}
		seen[normalizedPath] = true
		if info.Size() > maxLaunchContextFileBytes {
			return nil, fmt.Errorf("context %q is %d bytes; limit is %d bytes per file", rawPath, info.Size(), maxLaunchContextFileBytes)
		}
		total += info.Size()
		if total > maxLaunchContextTotalBytes {
			return nil, fmt.Errorf("context inputs total %d bytes; limit is %d bytes", total, maxLaunchContextTotalBytes)
		}
		data, err := os.ReadFile(normalizedPath)
		if err != nil {
			return nil, fmt.Errorf("context %q is unreadable: %w", rawPath, err)
		}
		if !supportedLaunchContextText(data) {
			return nil, fmt.Errorf("context %q appears to be binary or unsupported text; only UTF-8 text files are supported", rawPath)
		}
		sum := sha256.Sum256(data)
		checked = append(checked, checkedContext{
			displayName: filepath.Base(normalizedPath),
			sourcePath:  normalizedPath,
			digest:      "sha256:" + hex.EncodeToString(sum[:]),
			content:     string(data),
		})
	}
	contexts := make([]launchContext, 0, len(checked))
	for index, item := range checked {
		contexts = append(contexts, launchContext{
			Label:       fmt.Sprintf("ctx%d", index+1),
			DisplayName: item.displayName,
			SourcePath:  item.sourcePath,
			Digest:      item.digest,
			Content:     item.content,
		})
	}
	return contexts, nil
}

func supportedLaunchContextText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, value := range data {
		if value == 0 {
			return false
		}
	}
	return true
}

func buildTaskWithLaunchContext(task string, contexts []launchContext) string {
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
