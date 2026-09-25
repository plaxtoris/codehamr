package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestReadFileHappy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	content := "line one\nline two with 'quotes' and $dollar and `backticks`\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadFile(path, 0, 0)
	if got != content {
		t.Fatalf("read content mismatch:\n got %q\nwant %q", got, content)
	}
}

func TestReadFileEmptyPath(t *testing.T) {
	if got := ReadFile("", 0, 0); got != "(empty path)" {
		t.Fatalf("empty path handling wrong: %q", got)
	}
}

func TestReadFileMissingFile(t *testing.T) {
	s := ReadFile(filepath.Join(t.TempDir(), "nope.txt"), 0, 0)
	if !strings.HasPrefix(s, "(read error:") {
		t.Fatalf("expected (read error: ...) string, got %q", s)
	}
}

// TestReadFileWindowsOversizeContent: a file past one window comes back as a
// contiguous HEAD plus the exact offset to continue from: never Truncate's
// head+tail with the middle dropped, which is the unrecoverable dead end
// offset/limit exists to remove. The result must also stay under
// ToolOutputCap*4 so Execute's Truncate can't re fire on the window.
func TestReadFileWindowsOversizeContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	var b strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&b, "line %d padding padding padding\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadFile(path, 0, 0)
	if !strings.Contains(got, "Continue with read_file offset=") {
		t.Fatalf("windowed read should name the next offset, got %d bytes without it", len(got))
	}
	if strings.Contains(got, "───── truncated") {
		t.Fatalf("windowed read must not fall through to head+tail truncation: %q", got[:200])
	}
	if len(got) >= chmctx.ToolOutputCap*4 {
		t.Fatalf("window must stay under Truncate's cap, got %d bytes", len(got))
	}
	// The head is contiguous: line 0 present, and no gap before the last line
	// shown. Anything else means the middle was dropped.
	if !strings.HasPrefix(got, "line 0 ") {
		t.Fatalf("window should start at line 0: %q", got[:60])
	}
}

// TestReadFileOffsetLimitPagesWholeFile: following the continuation offsets
// reconstructs the file exactly, with no gap and no overlap. This is the
// property the whole change exists for.
func TestReadFileOffsetLimitPagesWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paged.txt")
	var b strings.Builder
	for i := range 900 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	want := b.String()
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for offset := 1; offset <= 900; offset += 100 {
		page := ReadFile(path, offset, 100)
		if i := strings.Index(page, "\n\n[lines "); i >= 0 {
			page = page[:i] + "\n"
		}
		got.WriteString(page)
	}
	if got.String() != want {
		t.Fatalf("paged read did not reconstruct the file: got %d bytes, want %d", got.Len(), len(want))
	}
}

// TestReadFileOffsetPastEnd: a bad offset must say so, not return an empty
// string the model would read as "the file is empty".
func TestReadFileOffsetPastEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadFile(path, 99, 0)
	if !strings.Contains(got, "past the end") {
		t.Fatalf("expected a past-the-end error, got %q", got)
	}
}

// TestReadFileCoercesStringOffset: weak local tool call parsers emit integers
// as strings. A string offset silently read as 0 would reread the head
// forever, exactly the loop the continuation note invites.
func TestReadFileCoercesStringOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "coerce.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := Execute(context.Background(), chmctx.ToolCall{
		Name:      ReadFileName,
		Arguments: map[string]any{"path": path, "offset": "3", "limit": "1"},
	})
	if msg.Content != "c\n" {
		t.Fatalf("string offset/limit not coerced: got %q, want %q", msg.Content, "c\n")
	}
}

func TestReadFileSchemaShape(t *testing.T) {
	sch := ReadFileSchema()
	fn, ok := sch["function"].(map[string]any)
	if !ok {
		t.Fatal("missing function")
	}
	if fn["name"] != "read_file" {
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
	if _, ok := props["path"]; !ok {
		t.Fatal("missing property \"path\"")
	}
	req, ok := params["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "path" {
		t.Fatalf("required should be [\"path\"], got %v", params["required"])
	}
}

func TestExecuteReadFileWrapsResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in.txt")
	content := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	call := chmctx.ToolCall{
		ID:        "call_r",
		Name:      ReadFileName,
		Arguments: map[string]any{"path": path},
	}
	msg := Execute(context.Background(), call)
	if msg.Role != chmctx.RoleTool || msg.ToolCallID != "call_r" || msg.ToolName != ReadFileName {
		t.Fatalf("bad message: %+v", msg)
	}
	if msg.Content != content {
		t.Fatalf("content mismatch:\n got %q\nwant %q", msg.Content, content)
	}
}

func TestInlineStatusReadFile(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{
		Name:      ReadFileName,
		Arguments: map[string]any{"path": "/tmp/foo.go"},
	})
	if !strings.HasPrefix(s, "▶ read_file: /tmp/foo.go") {
		t.Fatalf("bad inline status: %q", s)
	}
}

// TestReadFileCoercesFloatShapedOffset: a model echoing back the continuation
// note can type the offset as "411.0". Dropped to 0, read_file returns the
// byte identical head window with the byte identical note, forever: and every
// one of those is a SUCCESS, so no failure streak can ever form to break it.
func TestReadFileCoercesFloatShapedOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []any{"3", "3.0", float64(3), float64(3.0)} {
		msg := Execute(context.Background(), chmctx.ToolCall{
			Name:      ReadFileName,
			Arguments: map[string]any{"path": path, "offset": offset, "limit": 1},
		})
		if msg.Content != "c\n" {
			t.Fatalf("offset=%#v (%T) not coerced: got %q", offset, offset, msg.Content)
		}
	}
}

// TestReadFileOverLongLineStillPages: a file whose FIRST line is a minified
// bundle still has ordinary lines after it. Printing only the byte cut note and
// swallowing the continuation offset strands the rest of the file behind a dead
// end: the exact failure offset/limit exists to remove.
func TestReadFileOverLongLineStillPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.js")
	body := strings.Repeat("x", 30000) + "\n" + strings.Repeat("ordinary line\n", 500)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadFile(path, 0, 0)
	if !strings.Contains(got, "longer than one read window") {
		t.Fatalf("expected the byte-cut note: %q", got[len(got)-200:])
	}
	if !strings.Contains(got, "Continue with read_file offset=2") {
		t.Fatalf("over-long first line stranded the remaining 500 lines with no offset: %q", got[len(got)-300:])
	}
	// And that offset actually delivers them.
	rest := ReadFile(path, 2, 0)
	if strings.Count(rest, "ordinary line") != 500 {
		t.Fatalf("continuation returned %d of 500 remaining lines", strings.Count(rest, "ordinary line"))
	}
}

// TestReadFileKeepsExactBytesAcrossAnOverLongCut: the cut snaps to a rune
// boundary and touches nothing before it. Sanitising the whole prefix would
// delete invalid bytes out of the MIDDLE of a file read_file promised to return
// exactly.
func TestReadFileKeepsExactBytesAcrossAnOverLongCut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binaryish.txt")
	body := append([]byte("head\xff\xfetail"), []byte(strings.Repeat("z", 30000))...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadFile(path, 0, 0)
	if !strings.HasPrefix(got, "head\xff\xfetail") {
		t.Fatalf("bytes before the cut must survive verbatim, got %q", got[:20])
	}
}

// TestReadFileTooLargeRefused: read_file slurps whole files, so a multi GB
// log would OOM the process; the Stat gate refuses it with a recovery string.
// Sparse file: Truncate allocates no blocks, so the test costs no real disk.
func TestReadFileTooLargeRefused(t *testing.T) {
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
	got := ReadFile(path, 0, 0)
	if !strings.Contains(got, "too large") || !strings.Contains(got, "grep") {
		t.Fatalf("want too-large refusal naming a bash recovery, got %q", got[:min(len(got), 120)])
	}
}
