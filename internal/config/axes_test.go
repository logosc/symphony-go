package config

import (
	"errors"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/logosc/symphony-go/internal/types"
)

func TestOrderedMapPreservesDeclarationOrder(t *testing.T) {
	body := `
"type:code":     "WORKFLOW.code.md"
"type:research": "WORKFLOW.research.md"
"type:catalog":  "WORKFLOW.catalog.md"
default:         "WORKFLOW.code.md"
`
	var m OrderedMap[string]
	if err := yaml.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"type:code", "type:research", "type:catalog", "default"}
	if len(m.Keys) != len(want) {
		t.Fatalf("Keys = %v; want %v", m.Keys, want)
	}
	for i, k := range want {
		if m.Keys[i] != k {
			t.Errorf("Keys[%d] = %q; want %q", i, m.Keys[i], k)
		}
	}
	if m.Values["type:code"] != "WORKFLOW.code.md" {
		t.Errorf("Values[type:code] = %q", m.Values["type:code"])
	}
}

func TestOrderedMapPreservesAlternateOrder(t *testing.T) {
	// Same set of keys, different declaration order: result must reflect input.
	body := `
"type:research": "r"
"type:code":     "c"
default:         "c"
`
	var m OrderedMap[string]
	if err := yaml.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Keys[0] != "type:research" || m.Keys[1] != "type:code" {
		t.Errorf("order not preserved: %v", m.Keys)
	}
}

func TestOrderedMapRejectsSequence(t *testing.T) {
	var m OrderedMap[string]
	if err := yaml.Unmarshal([]byte("- a\n- b\n"), &m); err == nil {
		t.Fatal("expected error for sequence input")
	}
}

func TestOrderedMapRejectsScalar(t *testing.T) {
	var m OrderedMap[string]
	if err := yaml.Unmarshal([]byte("just-a-string\n"), &m); err == nil {
		t.Fatal("expected error for scalar input")
	}
}

func TestOrderedMapIsEmpty(t *testing.T) {
	var m OrderedMap[string]
	if !m.IsEmpty() {
		t.Fatal("zero OrderedMap should be empty")
	}
	body := `key: value`
	if err := yaml.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.IsEmpty() {
		t.Fatal("post-unmarshal map should not be empty")
	}
}

func TestResolveAxisFirstMatchWins(t *testing.T) {
	body := `
"type:code":     "c"
"type:research": "r"
default:         "d"
`
	var m OrderedMap[string]
	if err := yaml.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Issue has both labels — declaration order must win.
	iss := types.Issue{Labels: []string{"type:research", "type:code"}}
	k, v, err := ResolveAxis(iss, m)
	if err != nil {
		t.Fatalf("ResolveAxis: %v", err)
	}
	if k != "type:code" || v != "c" {
		t.Errorf("got (%q,%q); want (type:code,c)", k, v)
	}
}

func TestResolveAxisDefaultFallback(t *testing.T) {
	body := `
"type:code":     "c"
default:         "d"
`
	var m OrderedMap[string]
	_ = yaml.Unmarshal([]byte(body), &m)
	iss := types.Issue{Labels: []string{"unrelated"}}
	k, v, err := ResolveAxis(iss, m)
	if err != nil {
		t.Fatalf("ResolveAxis: %v", err)
	}
	if k != "default" || v != "d" {
		t.Errorf("got (%q,%q); want (default,d)", k, v)
	}
}

func TestResolveAxisNoMatchNoDefault(t *testing.T) {
	body := `
"type:code": "c"
`
	var m OrderedMap[string]
	_ = yaml.Unmarshal([]byte(body), &m)
	iss := types.Issue{Labels: []string{"unrelated"}}
	_, _, err := ResolveAxis(iss, m)
	if !errors.Is(err, ErrNoAxisMatch) {
		t.Fatalf("err = %v; want ErrNoAxisMatch", err)
	}
}

func TestResolveAxisCaseInsensitive(t *testing.T) {
	body := `
"Type:Code": "c"
default:     "d"
`
	var m OrderedMap[string]
	_ = yaml.Unmarshal([]byte(body), &m)
	iss := types.Issue{Labels: []string{"TYPE:code"}}
	k, v, err := ResolveAxis(iss, m)
	if err != nil {
		t.Fatalf("ResolveAxis: %v", err)
	}
	if k != "Type:Code" || v != "c" {
		t.Errorf("got (%q,%q)", k, v)
	}
}

func TestResolveAxisEmptyMap(t *testing.T) {
	var m OrderedMap[string]
	_, _, err := ResolveAxis(types.Issue{Labels: []string{"x"}}, m)
	if !errors.Is(err, ErrNoAxisMatch) {
		t.Fatalf("err = %v; want ErrNoAxisMatch", err)
	}
}

func TestResolveAxisSliceValueType(t *testing.T) {
	body := `
"type:code":     ["go test ./..."]
"type:research": ["test -f docs/research/x.md"]
default:         []
`
	var m OrderedMap[[]string]
	if err := yaml.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	iss := types.Issue{Labels: []string{"type:research"}}
	k, v, err := ResolveAxis(iss, m)
	if err != nil {
		t.Fatalf("ResolveAxis: %v", err)
	}
	if k != "type:research" || len(v) != 1 || v[0] != "test -f docs/research/x.md" {
		t.Errorf("got (%q,%v)", k, v)
	}
}
