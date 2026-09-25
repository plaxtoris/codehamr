package update

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These two tests close the silent drift gap that the hardcoded
// TestAssetNameCoversEveryReleasedPlatform table cannot: that table only
// restates assetName's own logic, so a divergence between assetName and what
// goreleaser actually publishes passes it. A new goreleaser build target
// assetName doesn't recognise (Check short circuits -> that platform silently
// never updates) or a renamed/reformatted asset (Apply 404s) is exactly the
// regression class that locked Windows out before the Windows fix.

const goreleaserPath = "../../.goreleaser.yaml"

// platformKey is a comparable os/arch pair for set comparison.
type platformKey struct{ os, arch string }

// goreleaserConfig is the slice of .goreleaser.yaml we assert against: the
// goos x goarch cross product goreleaser builds a binary for.
type goreleaserConfig struct {
	Builds []struct {
		Goos   []string `yaml:"goos"`
		Goarch []string `yaml:"goarch"`
	} `yaml:"builds"`
}

// goreleaserMatrix is the source of truth: every os/arch goreleaser builds for.
func goreleaserMatrix(t *testing.T) map[platformKey]bool {
	t.Helper()
	raw, err := os.ReadFile(goreleaserPath)
	if err != nil {
		t.Fatalf("read %s: %v", goreleaserPath, err)
	}
	var cfg goreleaserConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", goreleaserPath, err)
	}
	m := map[platformKey]bool{}
	for _, b := range cfg.Builds {
		for _, goos := range b.Goos {
			for _, goarch := range b.Goarch {
				m[platformKey{goos, goarch}] = true
			}
		}
	}
	if len(m) == 0 {
		t.Fatalf("%s yielded an empty build matrix: parsing broke", goreleaserPath)
	}
	return m
}

// assetNameSupported returns the set of platforms assetName resolves, with
// their names. It probes a generous candidate universe UNION the matrix's own
// values, so every cell goreleaser builds is exercised even if it uses an
// os/arch the static candidate list omits.
func assetNameSupported(matrix map[platformKey]bool) map[platformKey]string {
	osCands := map[string]bool{"linux": true, "darwin": true, "windows": true, "freebsd": true, "openbsd": true, "netbsd": true, "plan9": true, "solaris": true, "js": true, "aix": true}
	archCands := map[string]bool{"amd64": true, "arm64": true, "386": true, "arm": true, "riscv64": true, "ppc64le": true, "s390x": true, "loong64": true, "wasm": true}
	for k := range matrix {
		osCands[k.os] = true
		archCands[k.arch] = true
	}
	out := map[platformKey]string{}
	for goos := range osCands {
		for goarch := range archCands {
			if name, ok := assetName(goos, goarch); ok {
				out[platformKey{goos, goarch}] = name
			}
		}
	}
	return out
}

// TestAssetNameMatchesGoreleaserMatrix asserts the set of platforms assetName
// resolves is EXACTLY the set goreleaser builds: no more, no less. Derived
// from .goreleaser.yaml, not from a hardcoded mirror, so a future matrix edit
// that outpaces assetName fails here loudly instead of after a real release.
func TestAssetNameMatchesGoreleaserMatrix(t *testing.T) {
	matrix := goreleaserMatrix(t)
	supported := assetNameSupported(matrix)

	for k := range matrix {
		if _, ok := supported[k]; !ok {
			t.Errorf("%s/%s: goreleaser builds it but assetName returns ok=false: those users would silently never auto-update; add a case to assetName", k.os, k.arch)
		}
	}
	for k := range supported {
		if !matrix[k] {
			t.Errorf("%s/%s: assetName resolves it but goreleaser doesn't build it: Apply would 404; remove it from assetName or add it to .goreleaser.yaml", k.os, k.arch)
		}
	}
}

// TestPublishedManifestMatchesAssetName is opt in: point CODEHAMR_CHECK_MANIFEST
// at a goreleaser checksums file (CI sets it to dist/codehamr_checksums.txt after
// a release build) and it asserts the published asset names are EXACTLY the set
// assetName produces. This is the only check that exercises real goreleaser
// output, so it catches a name_template edit, an archive format switch
// (binary -> zip/tar.gz), or the implicit windows ".exe" append changing, none
// of which the hermetic test above can see. Skips when the env var is unset, so
// the default `go test ./...` never needs goreleaser or the network.
func TestPublishedManifestMatchesAssetName(t *testing.T) {
	path := os.Getenv("CODEHAMR_CHECK_MANIFEST")
	if path == "" {
		t.Skip("set CODEHAMR_CHECK_MANIFEST=<checksums.txt> to compare published asset names against assetName")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open manifest %s: %v", path, err)
	}
	defer f.Close()

	published := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(strings.TrimSpace(sc.Text()))
		if len(fields) < 2 {
			continue
		}
		published[fields[len(fields)-1]] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(published) == 0 {
		t.Fatalf("manifest %s listed no assets", path)
	}

	want := map[string]bool{}
	for _, name := range assetNameSupported(goreleaserMatrix(t)) {
		want[name] = true
	}
	for name := range want {
		if !published[name] {
			t.Errorf("assetName produces %q but it is NOT in the published manifest: that platform's auto-update would 404 / find no checksum row", name)
		}
	}
	for name := range published {
		if !want[name] {
			t.Errorf("published manifest lists %q which assetName never produces: a stale/renamed asset auto-update can't reach", name)
		}
	}
}
