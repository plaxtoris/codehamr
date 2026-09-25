package tools

import (
	"fmt"
	"os"
	"strings"
)

// EditFile replaces one unambiguous occurrence of old_string with new_string.
// It tries an exact match, then a match of whole lines ignoring whitespace.
// An empty search string or an unchanged replacement is rejected. Failures
// are returned as result text for the model.
func EditFile(path, oldString, newString string) string {
	if path == "" {
		return "(empty path)"
	}
	if oldString == "" {
		return "(empty old_string)"
	}
	if oldString == newString {
		return "(no change: old_string equals new_string)"
	}
	// Reject special files and oversized input before reading. This avoids
	// blocking on a FIFO or allocating memory for an unbounded log file.
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Sprintf("(read error: %s is not a regular file)", path)
		}
		if info.Mode().IsRegular() && info.Size() > maxFileBytes {
			return fmt.Sprintf("(too large: %s is %d bytes, over edit_file's %dMB cap: edit a file this size with bash instead: sed -i or a short Python script)", path, info.Size(), maxFileBytes>>20)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(read error: %v)", err)
	}
	content := string(raw)
	n := strings.Count(content, oldString)
	if n == 0 {
		// A near miss that differs only in whitespace (wrong indentation, tabs vs
		// spaces) is the most common edit_file failure for an LLM; each one costs
		// a reread round plus a failure streak entry. When the near miss is a
		// run of WHOLE lines matching exactly once, apply it: the uniqueness gate
		// preserves the exactly once guarantee, and the spliced bytes are the
		// model's own new_string, exactly what an exact match would have written.
		// Anything looser (mid line fragments, 0 or 2+ fuzzy matches) still fails
		// with a message that names the recovery.
		if out, ok := fuzzyWhitespaceEdit(path, content, oldString, newString); ok {
			return out
		}
		if differsOnlyInWhitespace(content, oldString) {
			return fmt.Sprintf("(not found: no exact match in %s: a block there differs only in whitespace (indentation/tabs/newlines); copy the exact bytes, including indentation)", path)
		}
		return fmt.Sprintf("(not found: old_string does not appear in %s: read the exact bytes back with read_file before retrying, don't retype them from memory)", path)
	}
	if n > 1 {
		return fmt.Sprintf("(ambiguous: old_string appears %d times (lines %s): provide more context to make it unique)", n, matchLines(content, oldString))
	}
	// strings.Count skips overlapping matches. Check again after the first
	// match so overlapping candidates cannot make a replacement ambiguous.
	if idx := strings.Index(content, oldString); strings.Contains(content[idx+1:], oldString) {
		return "(ambiguous: old_string overlaps itself: provide more context to make it unique)"
	}
	updated := strings.Replace(content, oldString, newString, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Sprintf("(write error: %v)", err)
	}
	return fmt.Sprintf("edited %s: -%d +%d bytes", path, len(oldString), len(newString))
}

// differsOnlyInWhitespace reports whether oldString matches content at exactly
// one spot once every whitespace run is collapsed: i.e. the sole mismatch is
// indentation/tabs/newlines. Bounded with spaces so a match can't straddle a
// token boundary and mislabel an unrelated near miss.
func differsOnlyInWhitespace(content, oldString string) bool {
	norm := func(s string) string { return " " + strings.Join(strings.Fields(s), " ") + " " }
	return strings.Count(norm(content), norm(oldString)) == 1
}

// fuzzyWhitespaceEdit accepts one whole sequence of lines whose words match
// old_string. Requiring a unique match and whole line boundaries preserves
// surrounding content. new_string is written exactly as supplied.
func fuzzyWhitespaceEdit(path, content, oldString, newString string) (string, bool) {
	want := strings.Fields(oldString)
	if len(want) == 0 {
		return "", false
	}
	lines := strings.Split(content, "\n")
	matches, matchStart, matchEnd := 0, -1, -1
	for i := range lines {
		if len(strings.Fields(lines[i])) == 0 {
			continue // a match starts on a non blank line, keeping the region tight
		}
		if end := fuzzyMatchAt(lines, i, want); end >= 0 {
			matches++
			matchStart, matchEnd = i, end
		}
	}
	if matches != 1 {
		return "", false
	}
	oldLen := len(strings.Join(lines[matchStart:matchEnd+1], "\n"))
	var b strings.Builder
	if matchStart > 0 {
		b.WriteString(strings.Join(lines[:matchStart], "\n"))
		b.WriteString("\n")
	}
	b.WriteString(newString)
	if matchEnd+1 < len(lines) {
		b.WriteString("\n")
		b.WriteString(strings.Join(lines[matchEnd+1:], "\n"))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Sprintf("(write error: %v)", err), true
	}
	return fmt.Sprintf("edited %s: -%d +%d bytes (old_string matched lines %d-%d only after ignoring whitespace differences; new_string was written exactly as given: reread the region if surrounding indentation matters)",
		path, oldLen, len(newString), matchStart+1, matchEnd+1), true
}

// fuzzyMatchAt reports the end line of a whole line fuzzy match of want
// starting at lines[start], or -1. Every field of every consumed line must
// belong to want in order (old_string covered those lines entirely, modulo
// whitespace); blank lines inside the run contribute nothing and are consumed.
func fuzzyMatchAt(lines []string, start int, want []string) int {
	k := 0
	for j := start; j < len(lines); j++ {
		fields := strings.Fields(lines[j])
		for _, f := range fields {
			if k >= len(want) || want[k] != f {
				return -1
			}
			k++
		}
		if k == len(want) {
			return j
		}
	}
	return -1
}

// matchLines names the counted from 1 line of each non overlapping occurrence of
// sub in content (capped at 10), so the ambiguous failure carries the
// locations the scan already visited instead of forcing a reread round to
// find them.
func matchLines(content, sub string) string {
	var out []string
	line, from := 1, 0
	for len(out) < 10 {
		i := strings.Index(content[from:], sub)
		if i < 0 {
			break
		}
		line += strings.Count(content[from:from+i], "\n")
		out = append(out, fmt.Sprint(line))
		line += strings.Count(sub, "\n")
		from += i + len(sub)
	}
	if strings.Contains(content[from:], sub) {
		out = append(out, "...")
	}
	return strings.Join(out, ", ")
}

// EditFileSchema is the OpenAI tool definition for edit_file. The description
// steers the model toward edit_file over write_file for small changes so it
// stops rewriting whole documents to fix a typo.
func EditFileSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        EditFileName,
			"description": "Surgically replace a single occurrence of old_string with new_string in an existing file. old_string must appear EXACTLY ONCE: include enough surrounding context to make it unique. Prefer this over write_file for any change to an existing file short of a full rewrite. To change several places, put several edit_file calls in the SAME message: they run in order against the file on disk, so they compose. Errors (not found, ambiguous, file missing) come back in the result string, same as bash.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute or relative file path. Relative paths resolve against the working directory.",
					},
					"old_string": map[string]any{
						"type":        "string",
						"description": "Exact substring to find. Must be nonempty and appear exactly once.",
					},
					"new_string": map[string]any{
						"type":        "string",
						"description": "Replacement string. Empty deletes the match.",
					},
				},
				"required": []string{"path", "old_string", "new_string"},
			},
		},
	}
}
