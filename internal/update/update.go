// Package update compares the running binary with GitHub release checksums
// and installs verified replacement binaries. Check returns false on failure;
// Apply returns errors to the caller. CODEHAMR_NO_UPDATE_CHECK=1 skips Check.
// The caller handles relaunching and skips checks for local development builds.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// checksumsURL points to the release checksum asset without using the GitHub
// API. Tests replace it with a local server.
var checksumsURL = "https://github.com/codehamr/codehamr/releases/latest/download/codehamr_checksums.txt"

// releaseBase is the "latest" redirect for individual binary assets; combined
// with an assetName to form the download URL in Apply.
var releaseBase = "https://github.com/codehamr/codehamr/releases/latest/download/"

// fetchTimeout bounds the checksums.txt GET so a silent network can't extend
// startup.
const fetchTimeout = 2 * time.Second

// promoteRename indirects Apply's final rename of the verified download onto
// execPath. var not const purely so tests can drive the restore on failure
// branch (which can leave the user with no executable) deterministically.
var promoteRename = os.Rename

// Check reports whether the local binary's sha256 differs from the remote
// asset's recorded hash. ctx bounds the HTTP request (the caller's update
// budget). Returns false on any failure; see package doc.
func Check(ctx context.Context, execPath string) bool {
	if os.Getenv("CODEHAMR_NO_UPDATE_CHECK") == "1" {
		return false
	}
	asset, ok := assetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return false
	}
	local, err := hashFile(execPath)
	if err != nil {
		return false
	}
	remote, err := fetchHash(ctx, asset)
	if err != nil || remote == "" {
		return false
	}
	return !strings.EqualFold(local, remote)
}

// assetName mirrors the name_template in .goreleaser.yaml. Every goos goreleaser
// builds for must have a case here; a missing one silently locks that
// platform's users out of auto updates (the Windows regression);
// TestAssetNameCoversEveryReleasedPlatform pins the contract.
//
// Unsupported platforms (freebsd, plan9, linux/386, …) return ok=false so Check
// short circuits before touching the network.
func assetName(goos, goarch string) (string, bool) {
	ext := ""
	switch goos {
	case "linux":
		// keep as is
	case "darwin":
		goos = "macos"
	case "windows":
		// Manifest row reads `<hash>  codehamr-windows-<arch>.exe`; match the
		// .exe or the asset 404s on download.
		ext = ".exe"
	default:
		return "", false
	}
	if goarch != "amd64" && goarch != "arm64" {
		return "", false
	}
	return fmt.Sprintf("codehamr-%s-%s%s", goos, goarch, ext), true
}

// hashFile streams a file through sha256.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Apply downloads the current platform's binary, verifies its sha256 against the
// published codehamr_checksums.txt, and atomically replaces execPath. The caller
// is expected to reExec afterwards (unix execve / Windows spawn and wait) so the
// session continues on the new binary with no user visible restart.
//
// Checksum verification closes the supply chain hole an unchecked download
// leaves open: a corrupted CDN response, a TLS MITM proxy, or a swapped binary
// would otherwise install whatever bytes arrived. Any mismatch errors before the
// binary is promoted.
//
// The temp file lives in execPath's directory so os.Rename stays an atomic
// intra filesystem move. A read only dir (e.g. /usr/local/bin without sudo)
// surfaces os.CreateTemp's EACCES verbatim, so the caller can hint about sudo or
// a user local PREFIX.
//
// ctx governs both fetches; the binary download sets no http.Client.Timeout, so
// ctx's deadline is the only budget.
func Apply(ctx context.Context, execPath string) error {
	asset, ok := assetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	expected, err := fetchHash(ctx, asset)
	if err != nil {
		return fmt.Errorf("checksum lookup: %w", err)
	}
	if expected == "" {
		return fmt.Errorf("checksum lookup: no entry for %s in published manifest", asset)
	}
	tmp, err := os.CreateTemp(filepath.Dir(execPath), ".codehamr-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Deferred Close/Remove clean up on any early return below. After a
	// successful rename tmpPath is gone (Remove's ENOENT ignored); a second
	// Close on *os.File is harmless.
	defer os.Remove(tmpPath)
	defer tmp.Close()

	req, err := http.NewRequestWithContext(ctx, "GET", releaseBase+asset, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: status %d", resp.StatusCode)
	}
	// Stream hash while writing so verification needs no second read of the
	// temp file.
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return err
	}
	// Sync before the rename: rename is metadata only, so without the flush a
	// power loss right after Apply can journal the rename while the data never
	// hit disk, leaving a truncated binary at execPath that CleanupOld can
	// never reach (recoverable only by hand from .old).
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("checksum mismatch: downloaded %s, expected %s", got, expected)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	// Rename aside, not overwrite in place: Windows blocks replacing a running
	// .exe but allows renaming one. Move the current binary to execPath+".old",
	// then move the download into the vacant execPath. Unix needs no dance (the
	// kernel keeps the running inode alive across an overwriting rename) but
	// runs the same path for one cross platform codepath. CleanupOld removes the
	// .old at next launch, once the prior process has released its handle.
	oldPath := execPath + ".old"
	// A stale .old from a failed cleanup makes the next Rename fail on Windows
	// (locked file). Remove eagerly; ENOENT is fine.
	_ = os.Remove(oldPath)
	if err := os.Rename(execPath, oldPath); err != nil {
		return err
	}
	if err := promoteRename(tmpPath, execPath); err != nil {
		// Promote failed after the running binary was moved aside; restore it
		// so the caller still has something to exec. If restore ALSO fails,
		// execPath is empty: the one outcome worth shouting about, so surface
		// both errors.
		if restoreErr := os.Rename(oldPath, execPath); restoreErr != nil {
			return fmt.Errorf("promote failed (%w); restore of %s also failed, binary may be missing: %v", err, execPath, restoreErr)
		}
		return err
	}
	return nil
}

// CleanupOld removes the execPath+".old" left by a previous Apply. Run at the
// start of the next launch: on Windows the .old is locked for the prior
// process's lifetime, so unlink at Apply time fails but unlink at next launch
// wins. Failure is silent: a leftover .old wastes disk but never breaks the
// session.
//
// It also sweeps orphaned .codehamr-update-* temp files: a Ctrl+C mid download
// (no signal handler exists that early, so the default disposition kills the
// process outright) skips Apply's deferred Remove, and each retry uses a fresh
// random suffix, so without the sweep every interrupted update strands another
// multi MB partial in the install dir forever. Only files older than
// orphanSweepAge are removed: a second instance launched during a pending
// update would otherwise unlink the first instance's in flight download and
// fail that update spuriously; a genuinely orphaned partial just waits one
// more launch.
func CleanupOld(execPath string) {
	_ = os.Remove(execPath + ".old")
	// Interrupted downloads, plus the tool output spill files tools.Execute
	// writes when a result outgrows the context budget. A long unattended run
	// can strand hundreds of megabytes of those, and a devcontainer's /tmp has
	// no reaper of its own.
	sweepOrphans(filepath.Join(filepath.Dir(execPath), ".codehamr-update-*"))
	sweepOrphans(filepath.Join(os.TempDir(), "codehamr-out-*.txt"))
}

// sweepOrphans deletes files matching pattern that nothing has touched for
// orphanSweepAge, so a live session's files are never pulled out from under it.
func sweepOrphans(pattern string) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && time.Since(info.ModTime()) > orphanSweepAge {
			_ = os.Remove(m)
		}
	}
}

// orphanSweepAge is how old a .codehamr-update-* temp must be before the
// launch sweep treats it as orphaned rather than another instance's live
// download; comfortably past any Apply budget.
const orphanSweepAge = time.Hour

// fetchHash downloads codehamr_checksums.txt and returns the hash for asset.
// The manifest is one line per asset, "<hex sha256>  <filename>"; we match the
// last field so a future prefix tweak still works.
//
// A scanner read error is surfaced, not dropped as "", nil: Apply treats "no
// entry" as a fatal mismatch, so silently turning a network glitch into "asset
// doesn't exist" would be a confusing error.
func fetchHash(ctx context.Context, asset string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", checksumsURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == asset {
			return fields[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", nil
}
