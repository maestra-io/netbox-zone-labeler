package main

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSanitizeLabel(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Rack 42", "rack-42"},
		{"UPPER", "upper"},
		{"already-good", "already-good"},
		{"Multiple  Spaces", "multiple--spaces"},
		{"l130-b14", "l130-b14"},
	}
	for _, tt := range tests {
		got := sanitizeLabel(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestIsValidLabel(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"rack-42", true},
		{"l130-b14", true},
		{"a", true},
		{"a.b", true},
		{"a_b", true},
		{"", false},
		{"-start-dash", false},
		{"end-dash-", false},
		{"has space", false},
		{"has#special", false},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
	}
	for _, tt := range tests {
		got := isValidLabel(tt.input)
		if got != tt.valid {
			t.Errorf("isValidLabel(%q) = %v, want %v", tt.input, got, tt.valid)
		}
	}
}

func TestParseExcludeRoles(t *testing.T) {
	tests := []struct {
		input string
		keys  []string
	}{
		{"", nil},
		{"master", []string{"master"}},
		{"master,system", []string{"master", "system"}},
		{" master , system ", []string{"master", "system"}},
		{",,", nil},
	}
	for _, tt := range tests {
		got := parseExcludeRoles(tt.input)
		if len(got) != len(tt.keys) {
			t.Errorf("parseExcludeRoles(%q): got %d keys, want %d", tt.input, len(got), len(tt.keys))
			continue
		}
		for _, k := range tt.keys {
			if _, ok := got[k]; !ok {
				t.Errorf("parseExcludeRoles(%q): missing key %q", tt.input, k)
			}
		}
	}
}

func TestIsExcluded(t *testing.T) {
	masterNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"node-role.kubernetes.io/master": "",
			},
		},
	}
	workerNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"node-role.kubernetes.io/worker": "",
			},
		},
	}

	excluded := map[string]struct{}{"master": {}, "control-plane": {}}

	if !isExcluded(masterNode, excluded) {
		t.Error("expected master node to be excluded")
	}
	if isExcluded(workerNode, excluded) {
		t.Error("expected worker node to not be excluded")
	}
	if isExcluded(masterNode, map[string]struct{}{}) {
		t.Error("expected no exclusion with empty roles")
	}
}

func TestToNode(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
	}

	got, ok := toNode(node)
	if !ok || got.Name != "test" {
		t.Error("expected toNode to extract node directly")
	}

	_, ok = toNode("not a node")
	if ok {
		t.Error("expected toNode to return false for non-node")
	}
}

func TestNegativeCache(t *testing.T) {
	nc := newNegativeCache(100 * time.Millisecond)

	if nc.Has("key") {
		t.Error("expected empty cache to not have key")
	}

	nc.Set("key")
	if !nc.Has("key") {
		t.Error("expected cache to have key after set")
	}

	time.Sleep(150 * time.Millisecond)
	if nc.Has("key") {
		t.Error("expected cache entry to expire")
	}

	nc.Set("key2")
	nc.Clear()
	if nc.Has("key2") {
		t.Error("expected cache to be empty after clear")
	}
}
