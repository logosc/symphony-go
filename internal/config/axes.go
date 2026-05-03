// Package config — per-axis (label-driven) configuration helpers.
//
// This file implements the resolver foundation used by every `*_by_label`
// configuration field introduced by Proposal 0001 (per-axis configuration).
// See docs/proposals/0001-per-axis-config.md §5 for design details.
package config

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/logosc/symphony-go/internal/types"
)

// ErrNoAxisMatch is returned by ResolveAxis when no key in the OrderedMap
// matches any label on the issue and no "default" key is present.
var ErrNoAxisMatch = errors.New("config: no axis match and no default")

// OrderedMap is a YAML map that preserves declaration order. It is used by
// every `*_by_label` config field so axis resolution can honor the
// "first match in declared order wins" rule (same shape as auto.rules).
//
// Keys lists every key in the declared YAML order, INCLUDING the
// reserved "default" key when present. Values maps each key to its
// associated value.
type OrderedMap[T any] struct {
	// Keys is the declared YAML order of keys, including "default" if
	// the document specified one.
	Keys []string
	// Values maps each key to the value parsed at that key.
	Values map[string]T
}

// IsEmpty reports whether the map carries no entries. An OrderedMap that
// was never set in YAML (and thus never unmarshaled) is also empty.
func (m *OrderedMap[T]) IsEmpty() bool {
	if m == nil {
		return true
	}
	return len(m.Keys) == 0
}

// UnmarshalYAML implements yaml.Unmarshaler. It accepts only mapping
// nodes; sequence or scalar nodes produce an error. The yaml.Node
// mapping content slice is walked pairwise so the original key order is
// captured exactly.
func (m *OrderedMap[T]) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	// Allow alias resolution to a mapping.
	resolved := node
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		resolved = node.Alias
	}
	if resolved.Kind != yaml.MappingNode {
		return fmt.Errorf("config: OrderedMap expects a YAML mapping at line %d", node.Line)
	}
	if len(resolved.Content)%2 != 0 {
		return fmt.Errorf("config: OrderedMap mapping has odd content length at line %d", node.Line)
	}
	keys := make([]string, 0, len(resolved.Content)/2)
	values := make(map[string]T, len(resolved.Content)/2)
	for i := 0; i < len(resolved.Content); i += 2 {
		kNode := resolved.Content[i]
		vNode := resolved.Content[i+1]
		var key string
		if err := kNode.Decode(&key); err != nil {
			return fmt.Errorf("config: OrderedMap key at line %d: %w", kNode.Line, err)
		}
		var v T
		if err := vNode.Decode(&v); err != nil {
			return fmt.Errorf("config: OrderedMap value for key %q: %w", key, err)
		}
		if _, dup := values[key]; dup {
			return fmt.Errorf("config: OrderedMap duplicate key %q at line %d", key, kNode.Line)
		}
		keys = append(keys, key)
		values[key] = v
	}
	m.Keys = keys
	m.Values = values
	return nil
}

// HasDefault reports whether m carries a "default" key.
func (m *OrderedMap[T]) HasDefault() bool {
	if m == nil || m.Values == nil {
		return false
	}
	_, ok := m.Values["default"]
	return ok
}

// ResolveAxis returns the first key in m.Keys (in declared order, with
// "default" excluded) that appears as a label on the issue, plus the
// value at that key. Label match is case-insensitive.
//
// If no concrete key matched but "default" is in the map, ResolveAxis
// returns ("default", m.Values["default"], nil). Otherwise it returns
// ("", zero-value, ErrNoAxisMatch).
func ResolveAxis[T any](issue types.Issue, m OrderedMap[T]) (string, T, error) {
	var zero T
	if len(m.Keys) == 0 {
		return "", zero, ErrNoAxisMatch
	}
	// Build a case-insensitive set of issue labels for O(n+m) lookup.
	have := make(map[string]struct{}, len(issue.Labels))
	for _, l := range issue.Labels {
		have[strings.ToLower(l)] = struct{}{}
	}
	for _, k := range m.Keys {
		if k == "default" {
			continue
		}
		if _, ok := have[strings.ToLower(k)]; ok {
			return k, m.Values[k], nil
		}
	}
	if v, ok := m.Values["default"]; ok {
		return "default", v, nil
	}
	return "", zero, ErrNoAxisMatch
}
