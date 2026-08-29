// Package proof owns the operator-supplied guidance snapshot used by the
// proof gate. Repository configuration never participates in this snapshot.
package proof

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	MaxGuidanceFiles = 16
	MaxGuidanceBytes = 256 * 1024
)

type GuidanceSnapshot struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	Text   string `json:"text"`
}

// SnapshotGuidance reads and hashes each operator guidance file. It rejects
// missing, unreadable, non-regular, invalid UTF-8, and oversized files before
// a proof agent is launched.
func SnapshotGuidance(paths []string) ([]GuidanceSnapshot, error) {
	if len(paths) > MaxGuidanceFiles {
		return nil, fmt.Errorf("proof guidance has %d files, maximum is %d", len(paths), MaxGuidanceFiles)
	}
	seen := make(map[string]struct{}, len(paths))
	out := make([]GuidanceSnapshot, 0, len(paths))
	for i, raw := range paths {
		path := filepath.Clean(strings.TrimSpace(raw))
		if path == "." || path == "" || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("proof guidance file %d must be an absolute path", i)
		}
		if _, ok := seen[path]; ok {
			return nil, fmt.Errorf("proof guidance file %q is duplicated", path)
		}
		seen[path] = struct{}{}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("proof guidance file %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("proof guidance file %q is not regular", path)
		}
		if info.Size() > MaxGuidanceBytes {
			return nil, fmt.Errorf("proof guidance file %q is %d bytes, maximum is %d", path, info.Size(), MaxGuidanceBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read proof guidance file %q: %w", path, err)
		}
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("proof guidance file %q is not valid UTF-8", path)
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			return nil, fmt.Errorf("proof guidance file %q is empty", path)
		}
		sum := sha256.Sum256(data)
		out = append(out, GuidanceSnapshot{Path: path, SHA256: fmt.Sprintf("%x", sum[:]), Bytes: len(data), Text: text})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
