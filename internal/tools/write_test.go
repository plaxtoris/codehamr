package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestWriteFileHappy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	content := "line one\nline two with 'quotes' and $dollar and `backticks`\n"
	s := WriteFile(path, content, false)
	if !strings.Contains(s, "wrote") || !strings.Contains(s, "hello.txt") {
		t.Fatalf("status wrong: %q", s)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestWriteFileCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "file.txt")
	s := WriteFile(path, "x", false)
	if !strings.Contains(s, "wrote 1 bytes") {
		t.Fatalf("status wrong: %q", s)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestWriteFileEmptyPath(t *testing.T) {
	if WriteFile("", "x", false) != "(empty path)" {
		t.Fatal("empty path handling wrong")
	}
}

// TestWriteFileMkdirErrorWhenParentIsFile checks the (mkdir error) branch: a
// MkdirAll failure must surface in the output string (bash convention), never
// as a Go error. A file in the path triggers it; a read only dir would not,
// since tests run as root and root bypasses directory permission bits.
func TestWriteFileMkdirErrorWhenParentIsFile(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "iam-a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// blocker is a file, so MkdirAll(blocker/sub) must fail.
	got := WriteFile(filepath.Join(blocker, "sub", "out.txt"), "data", false)
	if !strings.HasPrefix(got, "(mkdir error:") {
		t.Fatalf("expected (mkdir error: ...) string, got %q", got)
	}
}

// TestWriteFileWriteErrorWhenTargetIsDir checks the (write error) branch:
// writing to an existing directory fails at os.WriteFile, and the error must
// come back in the output string. A directory target is root safe, as above.
func TestWriteFileWriteErrorWhenTargetIsDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "imadir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	got := WriteFile(target, "data", false)
	if !strings.HasPrefix(got, "(write error:") {
		t.Fatalf("expected (write error: ...) string, got %q", got)
	}
}

func TestExecuteWriteFileWrapsResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	call := chmctx.ToolCall{
		ID:   "call_w",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    path,
			"content": "hello",
		},
	}
	msg := Execute(context.Background(), call)
	if msg.Role != chmctx.RoleTool || msg.ToolCallID != "call_w" || msg.ToolName != "write_file" {
		t.Fatalf("bad message: %+v", msg)
	}
	if !strings.Contains(msg.Content, "wrote 5 bytes") {
		t.Fatalf("content missing: %q", msg.Content)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello" {
		t.Fatalf("file content wrong: %q", got)
	}
}

// TestExecuteWriteFileRefusesMissingContent: valid JSON args with no string
// "content" ({"path": ...} alone, or "content": null) must not decode to ""
// and silently truncate an existing file to 0 bytes behind a success shaped
// result. An explicit "content": "" still writes (intentional empty file).
func TestExecuteWriteFileRefusesMissingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keep.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]map[string]any{
		"content key absent": {"path": path},
		"content null":       {"path": path, "content": nil},
	} {
		msg := Execute(context.Background(), chmctx.ToolCall{Name: "write_file", Arguments: args})
		if !strings.HasPrefix(msg.Content, "(missing content argument") {
			t.Fatalf("%s: want refusal, got %q", name, msg.Content)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "package main\n" {
			t.Fatalf("%s: existing file was destroyed: %q", name, got)
		}
	}
	// Explicit empty string is a deliberate empty write and must still pass.
	msg := Execute(context.Background(), chmctx.ToolCall{
		Name: "write_file", Arguments: map[string]any{"path": path, "content": ""},
	})
	if !strings.Contains(msg.Content, "wrote 0 bytes") {
		t.Fatalf("explicit empty content must write, got %q", msg.Content)
	}
}

// TestExecuteEditFileRefusesMissingNewString: same guard as write_file's
// content: a dropped new_string must not decode to "" and silently delete
// the matched text. An explicit "new_string": "" still deletes.
func TestExecuteEditFileRefusesMissingNewString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("keep THIS bit"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := Execute(context.Background(), chmctx.ToolCall{
		Name: "edit_file", Arguments: map[string]any{"path": path, "old_string": "THIS "},
	})
	if !strings.HasPrefix(msg.Content, "(missing new_string argument") {
		t.Fatalf("want refusal, got %q", msg.Content)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "keep THIS bit" {
		t.Fatalf("file was edited despite refusal: %q", got)
	}

	msg = Execute(context.Background(), chmctx.ToolCall{
		Name: "edit_file", Arguments: map[string]any{"path": path, "old_string": "THIS ", "new_string": ""},
	})
	if !strings.Contains(msg.Content, "edited") {
		t.Fatalf("explicit empty new_string must delete the match, got %q", msg.Content)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "keep bit" {
		t.Fatalf("explicit deletion wrong: %q", got)
	}
}

func TestInlineStatusWriteFile(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{
		Name:      "write_file",
		Arguments: map[string]any{"path": "/tmp/foo.txt", "content": "x"},
	})
	if !strings.HasPrefix(s, "▶ write_file: /tmp/foo.txt") {
		t.Fatalf("bad inline status: %q", s)
	}
}

// TestWriteFileAppendCoercesStringFlag: a weak backend emitting `"append":
// "true"` must still APPEND. A failed bool assertion would silently overwrite,
// destroying the earlier parts of a chunked write behind a success shaped
// "wrote N bytes": the worst possible outcome on the exact recovery path
// append exists for.
func TestWriteFileAppendCoercesStringFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parts.txt")
	for _, appendArg := range []any{nil, "true", true} {
		args := map[string]any{"path": path, "content": "part\n"}
		if appendArg != nil {
			args["append"] = appendArg
		}
		Execute(context.Background(), chmctx.ToolCall{Name: WriteFileName, Arguments: args})
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "part\npart\npart\n" {
		t.Fatalf("append lost a part: got %q", got)
	}
}

// TestWriteFileAppendCoercesEveryTruthyShape: `append` reaches us through a
// weak local tool call parser, so it arrives as a bool, as the JSON number 1
// (float64 after decoding), or as "True"/"true"/"1". Every one of those must
// APPEND. Missing any shape silently OVERWRITES behind a success shaped "wrote
// N bytes": destroying the earlier parts of a chunked write, or a pre existing
// user file, with nothing in the transcript to show for it.
func TestWriteFileAppendCoercesEveryTruthyShape(t *testing.T) {
	for _, truthy := range []any{true, "true", "True", "TRUE", "1", float64(1)} {
		path := filepath.Join(t.TempDir(), "parts.txt")
		Execute(context.Background(), chmctx.ToolCall{
			Name:      WriteFileName,
			Arguments: map[string]any{"path": path, "content": "one\n"},
		})
		Execute(context.Background(), chmctx.ToolCall{
			Name:      WriteFileName,
			Arguments: map[string]any{"path": path, "content": "two\n", "append": truthy},
		})
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "one\ntwo\n" {
			t.Fatalf("append=%#v (%T) overwrote instead of appending: file is %q", truthy, truthy, got)
		}
	}
	// And the falsy shapes must still overwrite.
	for _, falsy := range []any{false, "false", float64(0)} {
		path := filepath.Join(t.TempDir(), "parts.txt")
		Execute(context.Background(), chmctx.ToolCall{
			Name:      WriteFileName,
			Arguments: map[string]any{"path": path, "content": "one\n"},
		})
		Execute(context.Background(), chmctx.ToolCall{
			Name:      WriteFileName,
			Arguments: map[string]any{"path": path, "content": "two\n", "append": falsy},
		})
		got, _ := os.ReadFile(path)
		if string(got) != "two\n" {
			t.Fatalf("append=%#v (%T) should overwrite, file is %q", falsy, falsy, got)
		}
	}
}
