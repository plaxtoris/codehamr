package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeRelease serves a goreleaser style manifest plus one binary asset;
// tests plug its URL into the package vars via withReleaseURLs.
type fakeRelease struct {
	srv      *httptest.Server
	manifest string
	binary   []byte
	asset    string
}

func newFakeRelease(t *testing.T, asset string, body []byte, declared string) *fakeRelease {
	t.Helper()
	r := &fakeRelease{binary: body, asset: asset}
	r.manifest = fmt.Sprintf("%s  %s\n", declared, asset)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/" + asset:
			_, _ = w.Write(body)
		case "/codehamr_checksums.txt":
			_, _ = w.Write([]byte(r.manifest))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// withReleaseURLs swaps checksumsURL and releaseBase for one test; safe only
// because these tests don't run in parallel.
func withReleaseURLs(t *testing.T, base string) {
	t.Helper()
	origCS := checksumsURL
	origBase := releaseBase
	checksumsURL = base + "/codehamr_checksums.txt"
	releaseBase = base + "/"
	t.Cleanup(func() {
		checksumsURL = origCS
		releaseBase = origBase
	})
}

// hashOf returns the sha256 hex digest, matching goreleaser's manifest format.
func hashOf(body []byte) string {
	h := sha256.New()
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// platformAsset is the asset name Apply expects for the current runtime,
// via the same assetName helper production uses.
func platformAsset(t *testing.T) string {
	t.Helper()
	asset, ok := assetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("Apply test skipped: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return asset
}

// TestApplyRejectsChecksumMismatch: a binary whose hash doesn't match the
// manifest must NOT replace the local executable, else a corrupted CDN
// response or a swapped asset installs whatever bytes arrived.
func TestApplyRejectsChecksumMismatch(t *testing.T) {
	asset := platformAsset(t)
	good := []byte("genuine binary v1\n")
	tampered := []byte("malicious binary v1\n") // different bytes → different hash

	r := newFakeRelease(t, asset, tampered, hashOf(good))
	withReleaseURLs(t, r.srv.URL)

	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	if err := os.WriteFile(exec, []byte("original\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	beforeHash := hashOf([]byte("original\n"))

	err := Apply(context.Background(), exec)
	if err == nil {
		t.Fatal("Apply must reject a binary that doesn't match the published checksum")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error must explain the mismatch, got: %v", err)
	}
	got, _ := os.ReadFile(exec)
	if hashOf(got) != beforeHash {
		t.Fatalf("local exec was replaced despite checksum mismatch")
	}
}

// TestApplyRestoresBinaryWhenPromoteFails covers the most dangerous failure:
// the running binary is moved aside to execPath+".old", then the promote
// rename fails. Without the restore the user is left with NO executable at
// execPath. Driven through the promoteRename seam (the one step we can't make
// fail deterministically and root safely via the filesystem); the restore
// uses real os.Rename, so this asserts recovery actually happens.
func TestApplyRestoresBinaryWhenPromoteFails(t *testing.T) {
	asset := platformAsset(t)
	body := []byte("verified new binary\n")
	r := newFakeRelease(t, asset, body, hashOf(body))
	withReleaseURLs(t, r.srv.URL)

	orig := promoteRename
	promoteRename = func(string, string) error { return fmt.Errorf("simulated promote failure") }
	t.Cleanup(func() { promoteRename = orig })

	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	const originalBytes = "the original running binary\n"
	if err := os.WriteFile(exec, []byte(originalBytes), 0o755); err != nil {
		t.Fatal(err)
	}

	err := Apply(context.Background(), exec)
	if err == nil {
		t.Fatal("Apply must surface the promote failure")
	}
	// The original binary must be restored from .old, not left missing.
	got, readErr := os.ReadFile(exec)
	if readErr != nil {
		t.Fatalf("execPath is gone after a failed promote: user left with no binary: %v", readErr)
	}
	if string(got) != originalBytes {
		t.Fatalf("execPath not restored to the original binary: got %q", got)
	}
	// No half written temp file should leak.
	if matches, _ := filepath.Glob(filepath.Join(tmpDir, ".codehamr-update-*")); len(matches) != 0 {
		t.Fatalf("temp file leaked after failed promote: %+v", matches)
	}
}

// TestApplyReportsRestoreFailure covers the doubly bad path: the promote
// rename fails AND the restore of the moved aside binary also fails. Apply
// must surface the restore failure (not just the promote one) so the message
// reflects reality: execPath is now empty. Forced by occupying execPath with
// a directory inside the seam, so the restore os.Rename hits EISDIR.
func TestApplyReportsRestoreFailure(t *testing.T) {
	asset := platformAsset(t)
	body := []byte("verified new binary\n")
	r := newFakeRelease(t, asset, body, hashOf(body))
	withReleaseURLs(t, r.srv.URL)

	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	if err := os.WriteFile(exec, []byte("the original running binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := promoteRename
	// Occupy the now vacant execPath with a directory so the restore can't succeed.
	promoteRename = func(_, to string) error {
		_ = os.Mkdir(to, 0o755)
		return fmt.Errorf("simulated promote failure")
	}
	t.Cleanup(func() { promoteRename = orig })

	err := Apply(context.Background(), exec)
	if err == nil {
		t.Fatal("Apply must surface the failure")
	}
	if !strings.Contains(err.Error(), "restore") {
		t.Fatalf("error must name the restore failure (binary may be missing), got: %v", err)
	}
}

// TestApplyAcceptsMatchingChecksum: positive case, a download whose hash
// equals the manifest entry promotes the binary into place.
func TestApplyAcceptsMatchingChecksum(t *testing.T) {
	asset := platformAsset(t)
	body := []byte("legit binary content\n")
	r := newFakeRelease(t, asset, body, hashOf(body))
	withReleaseURLs(t, r.srv.URL)

	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	if err := os.WriteFile(exec, []byte("old\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Apply(context.Background(), exec); err != nil {
		t.Fatalf("Apply on matching checksum should succeed: %v", err)
	}
	got, err := os.ReadFile(exec)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("exec not replaced with downloaded body: got %q", got)
	}
	st, _ := os.Stat(exec)
	if st.Mode()&0o100 == 0 {
		t.Fatalf("exec should be executable, got mode %v", st.Mode())
	}
	// Temp file must be cleaned up.
	matches, _ := filepath.Glob(filepath.Join(tmpDir, ".codehamr-update-*"))
	if len(matches) != 0 {
		t.Fatalf("temp file leaked after successful Apply: %+v", matches)
	}
}

// TestApplyRejectsMissingManifestEntry: a manifest that exists but doesn't
// list our asset (a bad release) must make Apply abort, not skip verification
// and install an unverified binary.
func TestApplyRejectsMissingManifestEntry(t *testing.T) {
	asset := platformAsset(t)
	body := []byte("would-be binary\n")
	// declare a hash for a DIFFERENT asset name so fetchHash returns ""
	other := "codehamr-not-our-asset"
	r := newFakeRelease(t, asset, body, hashOf(body))
	r.manifest = fmt.Sprintf("%s  %s\n", hashOf(body), other) // no entry for `asset`
	withReleaseURLs(t, r.srv.URL)

	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	if err := os.WriteFile(exec, []byte("o\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := Apply(context.Background(), exec)
	if err == nil {
		t.Fatal("Apply must abort when no manifest entry exists for the asset")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error should mention the missing checksum, got: %v", err)
	}
}

// TestApplyCleansTempOnFailure: a failed download (server returns 500)
// must not leave a half written temp file in the install directory.
func TestApplyCleansTempOnFailure(t *testing.T) {
	asset := platformAsset(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "checksums.txt"):
			_, _ = w.Write([]byte(hashOf([]byte{}) + "  " + asset + "\n"))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	withReleaseURLs(t, srv.URL)

	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	if err := os.WriteFile(exec, []byte("o\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), exec); err == nil {
		t.Fatal("Apply must error on download failure")
	}
	matches, _ := filepath.Glob(filepath.Join(tmpDir, ".codehamr-update-*"))
	if len(matches) != 0 {
		t.Fatalf("temp file leaked after failed Apply: %+v", matches)
	}
}

// TestFetchHashHandlesCorruptManifest: a manifest not in `<hash>  <name>`
// form must not crash fetchHash; it just yields an empty hash.
func TestFetchHashHandlesCorruptManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not a real manifest\nrandom text\n"))
	}))
	t.Cleanup(srv.Close)
	origCS := checksumsURL
	checksumsURL = srv.URL + "/codehamr_checksums.txt"
	t.Cleanup(func() { checksumsURL = origCS })

	got, err := fetchHash(context.Background(), "codehamr-linux-amd64")
	if err != nil {
		t.Fatalf("corrupt manifest should not error, got: %v", err)
	}
	if got != "" {
		t.Fatalf("missing entry should yield empty hash, got %q", got)
	}
}

// TestCheckHonoursEnvDisableFlag: CODEHAMR_NO_UPDATE_CHECK=1 must short circuit
// Check before any HTTP work, sparing CI/offline launches the fetch deadline.
func TestCheckHonoursEnvDisableFlag(t *testing.T) {
	t.Setenv("CODEHAMR_NO_UPDATE_CHECK", "1")
	if Check(context.Background(), "/nonexistent/binary") {
		t.Fatal("Check must return false when the env disable flag is set")
	}
}

// TestAssetNameCoversEveryReleasedPlatform: every goos/goarch goreleaser
// publishes a binary for MUST be reachable from assetName. A missing case
// silently locks that platform out of auto updates (Check false → no fetch →
// no update, zero signal). This table mirrors the six published checksum rows.
func TestAssetNameCoversEveryReleasedPlatform(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "codehamr-linux-amd64"},
		{"linux", "arm64", "codehamr-linux-arm64"},
		{"darwin", "amd64", "codehamr-macos-amd64"},
		{"darwin", "arm64", "codehamr-macos-arm64"},
		{"windows", "amd64", "codehamr-windows-amd64.exe"},
		{"windows", "arm64", "codehamr-windows-arm64.exe"},
	}
	for _, c := range cases {
		got, ok := assetName(c.goos, c.goarch)
		if !ok {
			t.Errorf("%s/%s: assetName returned ok=false: every platform goreleaser publishes a binary for must be reachable, or releases are silently broken for that platform", c.goos, c.goarch)
			continue
		}
		if got != c.want {
			t.Errorf("%s/%s: got %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

// TestAssetNameRejectsUnreleasedPlatform: the inverse, anything goreleaser
// doesn't build for must return ok=false so Check short circuits before the
// network, instead of leading Apply down a confusing path on a 404.
func TestAssetNameRejectsUnreleasedPlatform(t *testing.T) {
	cases := [][2]string{
		{"plan9", "amd64"},
		{"plan9", "riscv"},
		{"freebsd", "amd64"},
		{"openbsd", "arm64"},
		{"linux", "386"},
		{"linux", "riscv64"},
		{"darwin", "386"},
	}
	for _, c := range cases {
		if asset, ok := assetName(c[0], c[1]); ok {
			t.Errorf("%s/%s: assetName returned ok=true with %q: goreleaser doesn't publish for this combo, Apply would 404", c[0], c[1], asset)
		}
	}
}

// TestCheckReportsUpToDate: local hash matches the manifest → Check returns
// false. The steady state path that keeps the spinner quiet after a release.
func TestCheckReportsUpToDate(t *testing.T) {
	asset := platformAsset(t)
	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	body := []byte("running binary content\n")
	if err := os.WriteFile(exec, body, 0o755); err != nil {
		t.Fatal(err)
	}
	r := newFakeRelease(t, asset, body, hashOf(body))
	withReleaseURLs(t, r.srv.URL)
	t.Setenv("CODEHAMR_NO_UPDATE_CHECK", "")

	if Check(context.Background(), exec) {
		t.Fatal("Check should return false when local hash matches published")
	}
}

// TestCheckReportsStale: published hash differs from local → Check returns
// true (update available), driving the maybeSelfUpdate trigger.
func TestCheckReportsStale(t *testing.T) {
	asset := platformAsset(t)
	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	if err := os.WriteFile(exec, []byte("local v1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newFakeRelease(t, asset, []byte("remote v2\n"), hashOf([]byte("remote v2\n")))
	withReleaseURLs(t, r.srv.URL)
	t.Setenv("CODEHAMR_NO_UPDATE_CHECK", "")

	if !Check(context.Background(), exec) {
		t.Fatal("Check should return true when local hash differs from published")
	}
}

// TestApplyKeepsPreviousBinaryAsOld is the cross platform parity guard: Apply
// must rename execPath aside to execPath+".old" before moving the new download
// in, never replace it directly. Windows requires this (MoveFileEx with
// REPLACE_EXISTING fails against a running .exe's sharing lock), and the same
// rename aside on linux/macos keeps the flow identical everywhere. CleanupOld
// deletes the stale .old on the next launch.
func TestApplyKeepsPreviousBinaryAsOld(t *testing.T) {
	asset := platformAsset(t)
	newBody := []byte("new release v2\n")
	oldBody := []byte("running binary v1\n")
	r := newFakeRelease(t, asset, newBody, hashOf(newBody))
	withReleaseURLs(t, r.srv.URL)

	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	if err := os.WriteFile(exec, oldBody, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Apply(context.Background(), exec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(exec)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newBody) {
		t.Fatalf("execPath should hold the new binary, got %q", got)
	}
	oldPath := exec + ".old"
	gotOld, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("Apply must preserve previous binary at %s, got %v", oldPath, err)
	}
	if string(gotOld) != string(oldBody) {
		t.Fatalf("%s should hold the previous binary, got %q", oldPath, gotOld)
	}
}

// TestCleanupOldRemovesStaleFile: the previous Apply's .old must be removed at
// the next launch so it doesn't accumulate. On Windows .old stays locked until
// the previous process exits, so cleanup must run at launch, not at Apply's end.
func TestCleanupOldRemovesStaleFile(t *testing.T) {
	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	stale := exec + ".old"
	if err := os.WriteFile(stale, []byte("previous"), 0o755); err != nil {
		t.Fatal(err)
	}
	CleanupOld(exec)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("CleanupOld should remove %s, stat err: %v", stale, err)
	}
}

// TestCleanupOldNoopWhenMissing: cleanup must be silent (no error/log/panic)
// when there is no .old file, the steady state once an update has settled.
func TestCleanupOldNoopWhenMissing(t *testing.T) {
	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	CleanupOld(exec) // must not panic, must not log
}

// TestCleanupOldSweepsOrphanedTempFiles: a Ctrl+C mid download kills the
// process before Apply's deferred Remove runs (no signal handler exists that
// early), stranding a multi MB .codehamr-update-* partial; each retry uses a
// fresh random suffix, so only a launch time sweep ever deletes them. Only
// STALE temps are swept: a fresh one may be another instance's live download.
// The running binary and unrelated files must survive.
func TestCleanupOldSweepsOrphanedTempFiles(t *testing.T) {
	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	unrelated := filepath.Join(tmpDir, "notes.txt")
	live := filepath.Join(tmpDir, ".codehamr-update-live")
	orphans := []string{
		filepath.Join(tmpDir, ".codehamr-update-1111"),
		filepath.Join(tmpDir, ".codehamr-update-2222"),
	}
	for _, p := range append([]string{exec, unrelated, live}, orphans...) {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-2 * orphanSweepAge)
	for _, p := range orphans {
		if err := os.Chtimes(p, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	CleanupOld(exec)
	for _, p := range orphans {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("stale orphan %s must be swept, stat err: %v", p, err)
		}
	}
	for _, p := range []string{exec, unrelated, live} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must survive the sweep: %v", p, err)
		}
	}
}

// TestApplyRespectsContextCancel: a cancelled ctx aborts the download and
// the local exec stays untouched.
func TestApplyRespectsContextCancel(t *testing.T) {
	asset := platformAsset(t)
	body := []byte("matters not\n")
	r := newFakeRelease(t, asset, body, hashOf(body))
	withReleaseURLs(t, r.srv.URL)

	tmpDir := t.TempDir()
	exec := filepath.Join(tmpDir, "codehamr")
	original := []byte("origcontents\n")
	if err := os.WriteFile(exec, original, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if err := Apply(ctx, exec); err == nil {
		t.Fatal("cancelled ctx must propagate as an Apply error")
	}
	got, _ := os.ReadFile(exec)
	if string(got) != string(original) {
		t.Fatalf("exec was replaced after cancelled Apply, got %q", got)
	}
}
