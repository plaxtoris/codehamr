package tools

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile writes content to path, creating parent dirs. With appendMode it
// appends instead of overwriting, which is how a file too large for one
// streamed tool call gets built: first part plain, every later part appended.
// Errors return as part of the output string (bash convention), never as a Go
// error, so the model sees a write failure the way it sees a nonzero bash exit.
func WriteFile(path, content string, appendMode bool) string {
	if path == "" {
		return "(empty path)"
	}
	// Refuse an existing non regular target: open(2) with O_WRONLY on a FIFO
	// with no reader blocks forever, leaking the tool goroutine past Ctrl+C
	// (which cancels the turn but can't unblock the open). Stat never blocks;
	// directories fall through to os.WriteFile's immediate EISDIR.
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Sprintf("(write error: %s is not a regular file)", path)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Sprintf("(mkdir error: %v)", err)
		}
	}
	if appendMode {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Sprintf("(write error: %v)", err)
		}
		defer f.Close()
		if _, err := f.WriteString(content); err != nil {
			return fmt.Sprintf("(write error: %v)", err)
		}
		return fmt.Sprintf("appended %d bytes to %s", len(content), path)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("(write error: %v)", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path)
}

// WriteFileSchema is the OpenAI tool definition for write_file. The description
// steers the model away from bash heredocs (shell quoting failure mode) and
// names append as the recovery for a body too large for one streamed call,
// so the truncation dead end is a one line fix instead of a tool switch.
func WriteFileSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        WriteFileName,
			"description": "Write content bytes to a file at path. Creates parent directories. Overwrites unless append is true. Use this instead of bash heredocs for multi line content or content with quotes, dollar signs, or backticks: no shell quoting issues. For an existing file prefer edit_file: a full rewrite risks a one character regression. A very large body can be cut off mid stream by the server; if that happens, send it in parts of at most ~200 lines: first part plain, each later part with append true.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute or relative file path. Relative paths resolve against the working directory.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Exact bytes to write to the file.",
					},
					"append": map[string]any{
						"type":        "boolean",
						"description": "Append to the file instead of overwriting it. Use for each part after the first when building a large file in parts.",
					},
				},
				"required": []string{"path", "content"},
			},
		},
	}
}
