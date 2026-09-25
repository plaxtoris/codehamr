package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestEditFileHappy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("alpha beta gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "beta", "BRAVO")
	if !strings.HasPrefix(s, "edited") {
		t.Fatalf("status wrong: %q", s)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != "alpha BRAVO gamma\n" {
		t.Fatalf("content wrong: %q", got)
	}
}

func TestEditFileEmptyPath(t *testing.T) {
	if got := EditFile("", "x", "y"); got != "(empty path)" {
		t.Fatalf("bad: %q", got)
	}
}

func TestEditFileMissingFile(t *testing.T) {
	s := EditFile(filepath.Join(t.TempDir(), "nope.txt"), "x", "y")
	if !strings.HasPrefix(s, "(read error:") {
		t.Fatalf("bad: %q", s)
	}
}

func TestEditFileOldNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "missing", "x")
	if !strings.Contains(s, "not found") || !strings.Contains(s, path) {
		t.Fatalf("bad: %q", s)
	}
	// File untouched.
	got, _ := os.ReadFile(path)
	if string(got) != "abc" {
		t.Fatalf("file modified on miss: %q", got)
	}
}

// TestEditFileWhitespaceFuzzyApply: a miss whose only difference is
// indentation, matching whole lines exactly once, is APPLIED (the retyped
// indentation failure is the canonical weak model edit_file miss; a unique
// whole line match preserves the exactly once guarantee). new_string goes in
// as given; every surrounding byte survives exactly.
func TestEditFileWhitespaceFuzzyApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	const orig = "func main() {\n\treturn 1\n}\n" // file indents with a tab
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "    return 1", "    return 2") // model supplied spaces
	if !strings.Contains(s, "edited") || !strings.Contains(s, "whitespace") {
		t.Fatalf("want fuzzy apply, got %q", s)
	}
	if !strings.Contains(s, "lines 2-2") {
		t.Fatalf("result must name the matched lines, got %q", s)
	}
	if got, _ := os.ReadFile(path); string(got) != "func main() {\n    return 2\n}\n" {
		t.Fatalf("bad fuzzy apply result: %q", got)
	}
}

// TestEditFileWhitespaceFuzzyApplyMultiline: an indent shifted multi line
// block applies, preserving the lines around it byte exactly.
func TestEditFileWhitespaceFuzzyApplyMultiline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.py")
	const orig = "def f():\n    if x:\n        do(1)\n        do(2)\n    return\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	// Model retyped the block one indent level off.
	s := EditFile(path, "if x:\n    do(1)\n    do(2)", "    if y:\n        do(1)\n        do(2)")
	if !strings.Contains(s, "edited") {
		t.Fatalf("want fuzzy apply, got %q", s)
	}
	want := "def f():\n    if y:\n        do(1)\n        do(2)\n    return\n"
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Fatalf("bad fuzzy apply result: %q", got)
	}
}

// TestEditFileWhitespaceNearMissMidLineStillHints: a whitespace near miss that
// is NOT a run of whole lines (the match covers only part of a line) must not
// fuzzy apply; it keeps the diagnostic hint and the file stays untouched.
func TestEditFileWhitespaceNearMissMidLineStillHints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	const orig = "x := f(a,  b) + g()\n" // double space inside the call
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "f(a, b)", "f(a, b, c)") // fields of the line != fields of old_string
	if !strings.Contains(s, "not found") {
		t.Fatalf("mid-line near-miss must not apply, got %q", s)
	}
	if got, _ := os.ReadFile(path); string(got) != orig {
		t.Fatalf("file modified on mid-line near-miss: %q", got)
	}
}

// TestEditFileWhitespaceFuzzyAmbiguousFails: two whole line fuzzy matches must
// not apply: the exactly once guarantee holds in the fuzzy path too.
func TestEditFileWhitespaceFuzzyAmbiguousFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	const orig = "\tfoo bar\nx\n    foo bar\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "foo  bar", "baz") // double space: no exact match anywhere
	if !strings.Contains(s, "not found") {
		t.Fatalf("ambiguous fuzzy match must fail, got %q", s)
	}
	if got, _ := os.ReadFile(path); string(got) != orig {
		t.Fatalf("file modified on ambiguous fuzzy match: %q", got)
	}
}

func TestEditFileOldNotUnique(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo bar foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "foo", "qux")
	if !strings.Contains(s, "appears") || !strings.Contains(s, "2") {
		t.Fatalf("bad: %q", s)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "foo bar foo" {
		t.Fatalf("file modified on ambiguity: %q", got)
	}
}

// TestEditFileOverlappingOldString: strings.Count sees only one
// non overlapping "==" in "a === b", but it matches at two positions with
// different results; the exactly once guarantee must reject it.
func TestEditFileOverlappingOldString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("a === b"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "==", "XX")
	if !strings.Contains(s, "(ambiguous") {
		t.Fatalf("self-overlapping old_string must be ambiguous, got %q", s)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "a === b" {
		t.Fatalf("file modified on ambiguity: %q", got)
	}
}

func TestEditFileNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "x", "x")
	if !strings.Contains(s, "no change") {
		t.Fatalf("bad: %q", s)
	}
}

func TestEditFileEmptyOldString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "", "x")
	if !strings.Contains(s, "empty") {
		t.Fatalf("bad: %q", s)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "abc" {
		t.Fatalf("file modified on empty old_string: %q", got)
	}
}

func TestEditFileDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha beta gamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "beta ", "")
	if !strings.HasPrefix(s, "edited") {
		t.Fatalf("bad: %q", s)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "alpha gamma" {
		t.Fatalf("content wrong: %q", got)
	}
}

func TestEditFileMultilineOldString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	src := "func foo() {\n\treturn 1\n}\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "\treturn 1\n", "\treturn 42\n")
	if !strings.HasPrefix(s, "edited") {
		t.Fatalf("bad: %q", s)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "func foo() {\n\treturn 42\n}\n" {
		t.Fatalf("content wrong: %q", got)
	}
}

func TestExecuteEditFileWrapsResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	call := chmctx.ToolCall{
		ID:   "call_e",
		Name: "edit_file",
		Arguments: map[string]any{
			"path":       path,
			"old_string": "world",
			"new_string": "earth",
		},
	}
	msg := Execute(context.Background(), call)
	if msg.Role != chmctx.RoleTool || msg.ToolCallID != "call_e" || msg.ToolName != "edit_file" {
		t.Fatalf("bad message: %+v", msg)
	}
	if !strings.HasPrefix(msg.Content, "edited") {
		t.Fatalf("content missing: %q", msg.Content)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello earth" {
		t.Fatalf("file content wrong: %q", got)
	}
}

func TestInlineStatusEditFile(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{
		Name: "edit_file",
		Arguments: map[string]any{
			"path":       "/tmp/x.txt",
			"old_string": "a",
			"new_string": "b",
		},
	})
	if !strings.HasPrefix(s, "▶ edit_file: /tmp/x.txt") {
		t.Fatalf("bad inline status: %q", s)
	}
}

func TestEditFileSchemaShape(t *testing.T) {
	sch := EditFileSchema()
	fn, ok := sch["function"].(map[string]any)
	if !ok {
		t.Fatal("missing function")
	}
	if fn["name"] != "edit_file" {
		t.Fatalf("name wrong: %v", fn["name"])
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatal("missing parameters")
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	for _, key := range []string{"path", "old_string", "new_string"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("missing property %q", key)
		}
	}
}

// TestEditFileTooLargeRefused: edit_file slurps whole files like read_file;
// the same Stat gate refuses oversized ones with a bash recovery.
func TestEditFileTooLargeRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got := EditFile(path, "a", "b")
	if !strings.Contains(got, "too large") || !strings.Contains(got, "sed") {
		t.Fatalf("want too-large refusal naming a bash recovery, got %q", got)
	}
}

// TestEditFileAmbiguousNamesLines: the ambiguous failure names the line of
// each occurrence the scan already visited, so the model can disambiguate
// without a full reread round.
func TestEditFileAmbiguousNamesLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo\nbar\nfoo\nbaz\nfoo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := EditFile(path, "foo", "qux")
	if !strings.Contains(s, "lines 1, 3, 5") {
		t.Fatalf("ambiguous failure must name lines 1, 3, 5, got %q", s)
	}
}
