package tools

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// readChunkBytes bounds one read_file result. Deliberately under
// chmctx.ToolOutputCap*4 (24000 bytes): at the cap, Execute's Truncate would
// re fire on the window and drop its middle, silently reinstating the
// unrecoverable dead end that offset/limit exists to remove.
const readChunkBytes = 20000

// maxFileBytes gates read_file and edit_file: both slurp the whole file with
// os.ReadFile, so a multi GB log would OOM kill the TUI: the same hazard
// bash's headTailBuffer capture cap exists to stop, unguarded in the file
// tools. 5MB covers every real source file; anything bigger is a log or an
// artifact, which grep/sed handle without loading it.
const maxFileBytes = 5 << 20

// ReadFile returns a window of path's contents: whole lines from offset
// (counted from 1, 0 meaning the start) up to limit lines, bounded by
// readChunkBytes. When the window stops short, the result names the exact
// offset to continue from, so a big file is read by repeated calls instead of
// guessed at from a truncated middle.
//
// Per the bash/write/edit convention, filesystem errors come back in the
// output string, never as a Go error: the model reacts to them like a nonzero
// exit.
func ReadFile(path string, offset, limit int) string {
	if path == "" {
		return "(empty path)"
	}
	// Refuse non regular files up front: open(2) on a FIFO blocks forever
	// waiting for a writer (leaking the tool goroutine past Ctrl+C, which
	// cancels the turn but can't unblock the read), and an endless device file
	// (/dev/zero) grows ReadFile's buffer without bound. Stat never blocks.
	// The same Stat size gates the whole file read below (see maxFileBytes).
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Sprintf("(read error: %s is not a regular file)", path)
		}
		if info.Mode().IsRegular() && info.Size() > maxFileBytes {
			return fmt.Sprintf("(too large: %s is %d bytes, over read_file's %dMB cap: read slices with bash instead: grep -n pattern, or sed -n '1,200p')", path, info.Size(), maxFileBytes>>20)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(read error: %v)", err)
	}
	lines := strings.Split(string(raw), "\n")
	// A trailing newline splits into a final empty element that is not a line;
	// counting it would report one line too many in every continuation note.
	// Remember it so a window reaching the end restores it: read_file's
	// contract is exact bytes, and a silently stripped final newline comes
	// back as a spurious diff the moment the model writes the content out.
	trailingNewline := len(lines) > 1 && lines[len(lines)-1] == ""
	if trailingNewline {
		lines = lines[:len(lines)-1]
	}
	start := 0
	if offset > 1 {
		start = offset - 1
	}
	if start >= len(lines) {
		return fmt.Sprintf("(read error: offset %d is past the end of %s, which has %d lines)", offset, path, len(lines))
	}
	end := len(lines)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	// Byte bound the window on top of the line bound, cutting whole lines. The
	// i > start guard keeps a single over long line (a minified bundle is one
	// 2MB line) from collapsing the window to nothing; the byte cut below
	// catches that case instead.
	for i, used := start, 0; i < end; i++ {
		used += len(lines[i]) + 1
		if used > readChunkBytes && i > start {
			end = i
			break
		}
	}
	out := strings.Join(lines[start:end], "\n")
	if end == len(lines) && trailingNewline {
		out += "\n"
	}
	// Independent checks, never a switch: a file whose FIRST line is a minified
	// bundle still has ordinary lines after it, and an exclusive branch would
	// print the byte cut note and swallow the continuation offset: reinstating
	// the dead end offset/limit exists to remove.
	if len(out) > readChunkBytes {
		// One line longer than the whole window. Cut bytes rather than return
		// nothing, snapping back to a rune boundary so the cut can't split a
		// multi byte character. Only the boundary is adjusted: sanitising the
		// whole prefix would silently delete invalid bytes from the middle of a
		// file read_file promised to return exactly.
		cut := readChunkBytes
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + fmt.Sprintf("\n\n[cut: line %d alone is longer than one read window. Use grep/sed on this file.]", start+1)
	}
	if end < len(lines) {
		rest := 0
		for _, l := range lines[end:] {
			rest += len(l) + 1
		}
		out += fmt.Sprintf("\n\n[lines %d-%d of %d shown, %d KB left. Continue with read_file offset=%d, or grep/sed instead if you are after something specific.]",
			start+1, end, len(lines), rest/1024, end+1)
	}
	return out
}

// ReadFileSchema is the OpenAI tool definition for read_file. The description
// nudges the model toward read_file over `cat` so it stops piping source
// through the shell just to look at it, and names offset/limit as the way
// through a large file so it never has to guess at a dropped middle.
func ReadFileSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ReadFileName,
			"description": "Read a file and return its contents. Prefer this over `cat`/`sed` in bash for inspecting a file: no shell quoting, exact bytes. Long files come back one window at a time; the result names the exact offset to continue from, so repeated calls read the whole file with no guessing. Reading several files? Put those calls in one message.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute or relative file path.",
					},
					"offset": map[string]any{
						"type":        "integer",
						"description": "Optional starting line, counted from 1. Default 1.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Optional maximum number of lines to return.",
					},
				},
				"required": []string{"path"},
			},
		},
	}
}
