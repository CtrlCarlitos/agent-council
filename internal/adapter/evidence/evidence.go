// Package evidence holds the shared, provider-free evidence-root
// contract reused by every harness adapter (AC-008/AC-009/AC-010 rules,
// applied verbatim): symlink-safe containment of a repo-relative path
// inside a trusted evidence root, a digest-bound re-hash of its raw
// bytes, a token-level strict JSON decode (single value, no unknown or
// duplicate keys, no trailing content), and the canonical re-encoding
// used to compare a decoded value against committed bytes.
//
// ReadFile was moved verbatim out of internal/adapter/codex/profile.go
// (readEvidenceFile) so every adapter — codex today, agy from this
// package on — shares one implementation instead of forking it.
package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ReadFile resolves a repo-relative evidence path inside the trusted
// evidence root (symlink-safe containment: every path component must be
// a real directory, the final entry a regular file whose symlink-
// resolved location stays under the resolved root), re-hashes the raw
// bytes, requires an exact digest match, and returns the bytes.
func ReadFile(evidenceRoot, relPath, wantDigest, label, field string) ([]byte, error) {
	rel := strings.TrimSpace(relPath)
	if rel == "" {
		return nil, fmt.Errorf("%s is empty", field)
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if filepath.IsAbs(rel) || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return nil, fmt.Errorf("%s %q escapes the evidence root", field, relPath)
	}

	resolvedRoot, err := filepath.EvalSymlinks(evidenceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve evidence root: %w", err)
	}
	parts := strings.Split(rel, "/")
	cur := resolvedRoot
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil, fmt.Errorf("evidence path component: %w", err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return nil, fmt.Errorf("evidence path component %q is not a real directory", part)
		}
	}
	final := filepath.Join(cur, parts[len(parts)-1])
	fi, err := os.Lstat(final)
	if err != nil {
		return nil, fmt.Errorf("read %s evidence: %w", label, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s evidence must be a regular file", label)
	}
	real, err := filepath.EvalSymlinks(final)
	if err != nil {
		return nil, fmt.Errorf("resolve %s evidence: %w", label, err)
	}
	if !strings.HasPrefix(real, resolvedRoot+string(os.PathSeparator)) {
		return nil, fmt.Errorf("%s evidence %q resolves outside the evidence root", label, relPath)
	}

	raw, err := os.ReadFile(final)
	if err != nil {
		return nil, fmt.Errorf("read %s evidence: %w", label, err)
	}
	got := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	if got != wantDigest {
		return nil, fmt.Errorf("%s digest mismatch: got %s want %s", label, got, wantDigest)
	}
	return raw, nil
}

// DecodeStrictObject decodes raw as exactly one JSON value into into:
// DisallowUnknownFields on the typed target, a token-walk rejecting any
// duplicate key at any nesting level (encoding/json's own map/struct
// decoding silently collapses duplicate keys, which would let an
// evidence file carry two conflicting meanings), and trailing content
// after the value rejected.
func DecodeStrictObject(raw []byte, into any) error {
	if err := checkNoDuplicateKeys(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("trailing content after json value")
	}
	return nil
}

// checkNoDuplicateKeys walks raw at the token level and rejects a
// duplicate object key at ANY nesting level, plus trailing content
// after the single top-level value — the generalized form of the
// codex token-walk idea (decodeNativeToolInventory), not specialized to
// one evidence shape.
func checkNoDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := walkJSONValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing content after json value")
	}
	return nil
}

// walkJSONValue consumes exactly one JSON value from dec, recursing
// into objects and arrays and rejecting a duplicate key within any one
// object.
func walkJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		// A scalar (string, json.Number, bool, nil): nothing more to walk.
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("object key must be a string, got %v", keyTok)
			}
			if _, dup := seen[key]; dup {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // consume closing '}'
			return err
		}
	case '[':
		for dec.More() {
			if err := walkJSONValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // consume closing ']'
			return err
		}
	default:
		return fmt.Errorf("unexpected json delimiter %v", delim)
	}
	return nil
}

// Canonical re-encodes v as canonical JSON: sorted object keys (a
// property of Go's encoder over map[string]any / struct field order —
// callers that need alphabetical key order pass a map), no
// insignificant whitespace, and ensure_ascii NOT applied (SetEscapeHTML
// false), mirroring exactly how storage.ComputeProfileDigest encodes
// the canonical profile.
func Canonical(v any) ([]byte, error) {
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode canonical json: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
