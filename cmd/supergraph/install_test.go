package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// eventArrayLen returns how many hook entries a merged settings file wired for one event.
func eventArrayLen(t *testing.T, settings map[string]any, event string) int {
	t.Helper()
	hooks, _ := settings["hooks"].(map[string]any)
	arr, _ := hooks[event].([]any)
	return len(arr)
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

// M1: a settings file that does not parse is a hard stop — merge returns an error and
// leaves the file byte-for-byte unchanged (no data loss).
func TestMergeHookBlockMalformedIsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte("{ this is not valid json ")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeHookBlock(io.Discard, path); err == nil {
		t.Fatal("merge into malformed settings should error, got nil")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Fatalf("malformed file was modified: %q", string(after))
	}
}

// M1: a valid file gains the hook wiring for every event and keeps its other keys.
func TestMergeHookBlockPreservesOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark","hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeHookBlock(io.Discard, path); err != nil {
		t.Fatalf("merge: %v", err)
	}
	m := readSettings(t, path)
	if m["theme"] != "dark" {
		t.Fatalf("unrelated key lost: theme = %v", m["theme"])
	}
	for _, ev := range claudeHookEvents {
		if n := eventArrayLen(t, m, ev); n != 1 {
			t.Fatalf("event %s array len = %d, want 1", ev, n)
		}
	}
}

// M1: a second merge is idempotent — no duplicate hook entries.
func TestMergeHookBlockIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json") // also exercises MkdirAll
	if err := mergeHookBlock(io.Discard, path); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if err := mergeHookBlock(io.Discard, path); err != nil {
		t.Fatalf("second merge: %v", err)
	}
	m := readSettings(t, path)
	for _, ev := range claudeHookEvents {
		if n := eventArrayLen(t, m, ev); n != 1 {
			t.Fatalf("event %s array len after two merges = %d, want 1", ev, n)
		}
	}
}
