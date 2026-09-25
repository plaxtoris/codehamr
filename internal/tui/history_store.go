package tui

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
)

// Prompt history persists in .codehamr/history, so recall is per project
// (cd stable) and /clear wipes it with the rest of the reset. One
// strconv quoted entry per line keeps the format cat friendly, handles
// multi line prompts without a separator, and lets a corrupt line be
// skipped without poisoning the rest.
const (
	historyFileName   = "history"
	historyMaxEntries = 500
	// Unquoted value size cap. History stores chips' expanded text, so without
	// this a multi megabyte log paste would balloon the file every submit;
	// longer pastes simply aren't recalled.
	historyMaxEntryBytes = 256 * 1024
	// Per line token ceiling for the bufio scanners that read the file back. A
	// line at or above this is dropped with ErrTooLong AND halts the scan,
	// losing every later entry, so appendPromptHistory must never write a
	// quoted line this long.
	historyScannerMax = 1024 * 1024
)

func historyPath(dir string) string { return filepath.Join(dir, historyFileName) }

// loadPromptHistory returns every saved prompt oldest first, matching the
// in memory append order so historyUp/Down walk the same direction for
// typed and on disk entries. A missing file is first run, not an error.
func loadPromptHistory(dir string) []promptEntry {
	f, err := os.Open(historyPath(dir))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []promptEntry
	sc := bufio.NewScanner(f)
	// A prompt may carry a pasted log of tens of KB; raise the per line cap
	// past Scanner's 64KB default so we don't drop a long entry's tail.
	sc.Buffer(make([]byte, 64*1024), historyScannerMax)
	for sc.Scan() {
		v, err := strconv.Unquote(sc.Text())
		if err != nil {
			continue
		}
		out = append(out, promptEntry{display: v})
	}
	return out
}

// appendPromptHistory writes value as one more entry, trimming to
// historyMaxEntries to bound growth. Empty prompts are skipped so stray ↵
// presses don't pollute recall; entries over historyMaxEntryBytes are
// dropped (recall would fail at the scanner cap anyway).
//
// O_APPEND so two codehamr processes in the same project can each add a
// line without one's load+rewrite eating the other's submit. The trim is
// best effort: an IO error during rewrite leaves the appended file as is.
// The next start trims it back down.
func appendPromptHistory(dir, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > historyMaxEntryBytes {
		return nil
	}
	// Quote expands control/invalid bytes to \xNN (4× each), so a value under
	// the unquoted cap can still quote past historyScannerMax, and such a line
	// halts the scan, losing every newer entry. Decline to store what the
	// loader can't read back. (bufio needs the token strictly below the buffer
	// max, hence >=.)
	quoted := strconv.Quote(value)
	if len(quoted) >= historyScannerMax {
		return nil
	}
	line := quoted + "\n"
	path := historyPath(dir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Lazy trim: rewrite only once the count exceeds historyMaxEntries.
	count, err := countHistoryLines(path)
	if err != nil || count <= historyMaxEntries {
		return nil
	}
	all := loadPromptHistory(dir)
	if len(all) > historyMaxEntries {
		all = all[len(all)-historyMaxEntries:]
	}
	var buf []byte
	for _, e := range all {
		buf = append(buf, strconv.Quote(e.display)...)
		buf = append(buf, '\n')
	}
	// Best effort: a failure keeps the over cap but correct file rather than
	// reporting an error that would obscure the successful append above.
	_ = os.WriteFile(path, buf, 0o600)
	return nil
}

// countHistoryLines tallies lines without parsing them: fast path for the
// trim decision in appendPromptHistory.
func countHistoryLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), historyScannerMax)
	n := 0
	for sc.Scan() {
		n++
	}
	return n, sc.Err()
}

// clearPromptHistory removes the on disk file so /clear also wipes recall.
// A missing file is not an error: the empty history intent already holds.
func clearPromptHistory(dir string) error {
	err := os.Remove(historyPath(dir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
