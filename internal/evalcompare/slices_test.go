package evalcompare

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSlices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slices.json")
	content := `{"schema":1,"version":"2026-09-24","tags":{"q1":["person","time"],"q2":["exact_token"]}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write slices file: %v", err)
	}

	s, err := LoadSlices(path)
	if err != nil {
		t.Fatalf("LoadSlices: %v", err)
	}
	if s.Schema != 1 || s.Version != "2026-09-24" {
		t.Fatalf("slices metadata wrong: %+v", s)
	}
	if len(s.Tags["q1"]) != 2 || s.Tags["q1"][0] != "person" {
		t.Fatalf("tags for q1 wrong: %+v", s.Tags["q1"])
	}
	if s.SHA256() == "" {
		t.Fatal("SHA256() empty after a successful load")
	}

	s2, err := LoadSlices(path)
	if err != nil {
		t.Fatalf("LoadSlices (2nd read): %v", err)
	}
	if s2.SHA256() != s.SHA256() {
		t.Fatal("SHA256() must be stable for the same file content")
	}
}

func TestLoadSlices_WrongSchemaRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slices.json")
	if err := os.WriteFile(path, []byte(`{"schema":2,"tags":{}}`), 0o600); err != nil {
		t.Fatalf("write slices file: %v", err)
	}
	if _, err := LoadSlices(path); err == nil {
		t.Fatal("LoadSlices: want error for schema != 1, got nil")
	}
}

func TestLoadSlices_MissingFile(t *testing.T) {
	if _, err := LoadSlices("/nonexistent/slices.json"); err == nil {
		t.Fatal("LoadSlices: want error for a missing file, got nil")
	}
}
