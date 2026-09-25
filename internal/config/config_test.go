package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBootstrapWritesSandboxHintHeader pins the comment header writeYAML
// re prepends on every write. yaml.Marshal drops comments, so this is the only
// place the host.docker.internal hint survives, the #1 first run footgun for
// devcontainer/WSL2 users. Guards against a switch to plain yaml.Marshal.
func TestBootstrapWritesSandboxHintHeader(t *testing.T) {
	dir := t.TempDir()
	if _, created, err := Bootstrap(dir); err != nil || !created {
		t.Fatalf("Bootstrap should create config on first run: created=%v err=%v", created, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, DirName, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codehamr configuration", "host.docker.internal"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("config.yaml header missing %q:\n%s", want, raw)
		}
	}
}

func TestBootstrapCreatesLayout(t *testing.T) {
	dir := t.TempDir()
	cfg, created, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("should report created=true on first bootstrap")
	}
	if _, err := os.Stat(filepath.Join(dir, DirName, "config.yaml")); err != nil {
		t.Errorf("missing config.yaml: %v", err)
	}
	// PROMPT_SYS is embedded, it must never touch disk.
	if _, err := os.Stat(filepath.Join(dir, DirName, "PROMPT_SYS.md")); err == nil {
		t.Errorf("embedded PROMPT_SYS.md must not be written to disk")
	}
	if cfg.Active != "local" {
		t.Fatalf("default Active = %q, want local", cfg.Active)
	}
	p, ok := cfg.Models["local"]
	if !ok {
		t.Fatal("default should include a 'local' profile")
	}
	if p.URL != "http://localhost:11434" || p.LLM != "qwen3.8:27b" || p.ContextSize != 262144 {
		t.Fatalf("default local profile mismatch: %+v", p)
	}
	if len(cfg.Models) != 1 {
		t.Fatalf("expected only the local default, got %d profiles", len(cfg.Models))
	}

}

// TestBootstrapPreservesDeclaredProfiles: once config.yaml exists the user
// owns its profile list. Bootstrap leaves the file and its profiles intact.
func TestBootstrapPreservesDeclaredProfiles(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: local
models:
  local:
    llm: local-model
    url: http://host.docker.internal:11434
    key: ""
    context_size: 262144
  custom:
    llm: foo
    url: http://x
    key: sk-keep
    context_size: 8000
`)
	cfgPath := filepath.Join(cdir, "config.yaml")
	if err := os.WriteFile(cfgPath, yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	beforeStat, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models) != 2 {
		t.Fatalf("expected exactly the two user profiles, got %d: %+v", len(cfg.Models), cfg.Models)
	}
	// User customisations must survive intact.
	if cfg.Models["local"].URL != "http://host.docker.internal:11434" || cfg.Models["local"].ContextSize != 262144 {
		t.Fatalf("local profile was mutated: %+v", cfg.Models["local"])
	}
	if cfg.Models["custom"].Key != "sk-keep" {
		t.Fatalf("custom profile was mutated: %+v", cfg.Models["custom"])
	}
	// No spurious rewrite, Bootstrap's job here is to read, not tidy.
	// mtime is the cheapest signal.
	afterStat, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !afterStat.ModTime().Equal(beforeStat.ModTime()) {
		t.Fatal("config.yaml was rewritten by Bootstrap on a clean read path")
	}
}

// TestBootstrapDoesNotRestoreRenamedLocal: renaming `local` (e.g. to `ollama`)
// must not resurrect a duplicate `local` on next start.
func TestBootstrapDoesNotRestoreRenamedLocal(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: ollama
models:
  ollama:
    llm: local-model
    url: http://localhost:11434
    key: ""
    context_size: 65536
`)
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Models["local"]; ok {
		t.Fatal("renamed-away `local` must not be restored")
	}
	if len(cfg.Models) != 1 || cfg.Active != "ollama" {
		t.Fatalf("expected single 'ollama' profile active, got Active=%q models=%+v", cfg.Active, cfg.Models)
	}
}

// TestBootstrapPreservesExistingAPIKey: an API key must
// round trip untouched. Guards against any future "tidy on read" that would
// mutate existing entries.
func TestBootstrapPreservesExistingAPIKey(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: remote
models:
  local:
    llm: local-model
    url: http://localhost:11434
    key: ""
    context_size: 65536
  remote:
    llm: remote
    url: https://api.example/v1
    key: test-secret-1234567890abcdef
    context_size: 262144
`)
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models["remote"].Key != "test-secret-1234567890abcdef" {
		t.Fatalf("existing key was overwritten: %q", cfg.Models["remote"].Key)
	}
}

// TestBootstrapLoadsMultipleProfiles: a two profile config round trips and
// Bootstrap picks the declared `active`.
func TestBootstrapLoadsMultipleProfiles(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: work
models:
  home:
    llm: local-model
    url: http://llm:11434
    key: ""
    context_size: 65536
  work:
    llm: cloud-model
    url: https://api.example/v1
    key: sk-abc
    context_size: 200000
`)
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Active != "work" {
		t.Fatalf("Active = %q, want work", cfg.Active)
	}
	// Bootstrap must not inject default profiles on top of the declared profiles.
	if len(cfg.Models) != 2 {
		t.Fatalf("expected exactly the two declared profiles, got %d: %+v", len(cfg.Models), cfg.Models)
	}
	for _, name := range []string{"home", "work"} {
		if _, ok := cfg.Models[name]; !ok {
			t.Fatalf("expected profile %q in loaded config", name)
		}
	}
	p := cfg.ActiveProfile()
	if p.LLM != "cloud-model" || p.URL != "https://api.example/v1" || p.Key != "sk-abc" {
		t.Fatalf("active profile wrong: %+v", p)
	}
}

// TestConfigFilePermissionsAreOwnerOnly is the regression for a world readable
// API key. Bootstrap and Save must both write files with mode 0o600.
func TestConfigFilePermissionsAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, DirName, "config.yaml")
	st, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("fresh config.yaml perms = %v, want 0o600 (key may leak to other local users)", got)
	}

	// Save must preserve the restrictive permissions when adding a key.
	cfg.Models["local"].Key = "test-secret-12345678"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.Mode().Perm(); got != 0o600 {
		t.Fatalf("Save() widened config.yaml perms to %v (must stay 0o600)", got)
	}

	// The .codehamr/ dir mustn't be world listable either: even with a 0o600
	// config.yaml, a listable parent leaks the key's existence and invites probing.
	parentSt, err := os.Stat(filepath.Join(dir, DirName))
	if err != nil {
		t.Fatal(err)
	}
	if got := parentSt.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf(".codehamr/ dir perms = %v: must not grant any other-user bits", got)
	}
}

// TestBootstrapTightensLooseDirPerms: a .codehamr/ created loose (older
// release, or by hand at 0o755) must be tightened on the next Bootstrap, the
// directory counterpart of Save's fresh temp inode fix for config.yaml.
func TestBootstrapTightensLooseDirPerms(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // bypass umask
		t.Fatal(err)
	}
	if _, _, err := Bootstrap(root); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Fatalf("pre-existing loose .codehamr/ = %v after Bootstrap, want 0o700", got)
	}
}

// TestSaveIsAtomicAndLeavesNoTemp: writeYAML writes a sibling temp then renames
// it over config.yaml so a torn write can't brick the next launch. Pin that the
// rename leaves no leftover .config-*.yaml temp and the result still decodes.
// A regression here would mean the atomic write path leaks temps or wrote junk.
func TestSaveIsAtomicAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(dir, DirName)
	// Save a few times, each must rename cleanly with no temp accumulation.
	for i := range 3 {
		cfg.Models["local"].Key = fmt.Sprintf("test-key-%d-0000000000", i)
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(cdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Fatalf("Save left a temp file behind: %s", e.Name())
		}
	}
	// The committed file must still be a valid, re decodable config.
	reloaded, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatalf("config.yaml not decodable after atomic Save: %v", err)
	}
	if reloaded.Models["local"].Key != "test-key-2-0000000000" {
		t.Fatalf("last Save not durable: key = %q", reloaded.Models["local"].Key)
	}
}

// TestSetActivePersists: SetActive flips Active and writes config.yaml.
func TestSetActivePersists(t *testing.T) {
	dir := t.TempDir()
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	// add a second profile so SetActive has somewhere to go
	cfg.Models["other"] = &Profile{LLM: "m", URL: "http://x", ContextSize: 1}
	if err := cfg.SetActive("other"); err != nil {
		t.Fatal(err)
	}
	if cfg.Active != "other" {
		t.Fatalf("Active = %q, want other", cfg.Active)
	}
	reloaded, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Active != "other" {
		t.Fatal("Active did not persist")
	}
}

// TestSetActiveRejectsUnknown: SetActive returns an error for an unknown name.
func TestSetActiveRejectsUnknown(t *testing.T) {
	cfg := &Config{Active: "a", Models: map[string]*Profile{"a": {}}}
	if err := cfg.SetActive("nope"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

// TestSetActiveRevertsOnSaveFailure guards in memory and on disk drift on Save
// failure. If SetActive mutates Active before a failed Save, ActiveProfile()
// reads the wrong endpoint while config.yaml still names the old profile, and
// restart silently undoes the switch. SetActive must roll back on Save failure
// so both views stay in lockstep.
func TestSetActiveRevertsOnSaveFailure(t *testing.T) {
	cfg := &Config{
		Active: "a",
		Models: map[string]*Profile{
			"a": {LLM: "ma"},
			"b": {LLM: "mb"},
		},
		// Dir intentionally empty so Save() fails with "Dir not set".
	}
	err := cfg.SetActive("b")
	if err == nil {
		t.Fatal("precondition: Save with empty Dir must fail")
	}
	if cfg.Active != "a" {
		t.Fatalf("Active mutated to %q despite Save failure: in-memory state diverges from on-disk", cfg.Active)
	}
}

// TestActiveProfileResolvesByName: the helper returns the right struct.
func TestActiveProfileResolvesByName(t *testing.T) {
	cfg := &Config{
		Active: "b",
		Models: map[string]*Profile{
			"a": {LLM: "m-a"},
			"b": {LLM: "m-b"},
		},
	}
	if cfg.ActiveProfile().LLM != "m-b" {
		t.Fatalf("ActiveProfile().LLM = %q, want m-b", cfg.ActiveProfile().LLM)
	}
}

// TestBootstrapCoercesUnknownActive: an unknown `active:` is coerced to the
// first profile in sorted order so ActiveProfile never returns nil.
func TestBootstrapCoercesUnknownActive(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: ghost
models:
  zulu:
    llm: m
    url: http://z
    key: ""
    context_size: 1
  alpha:
    llm: m
    url: http://a
    key: ""
    context_size: 1
`)
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Active != "alpha" {
		t.Fatalf("unknown active should coerce to first sorted, got %q", cfg.Active)
	}
}

// TestBootstrapRejectsEmptyModels: an empty `models:` block leaves Active
// nothing to point at; Bootstrap must error readably (how to recover) rather
// than panic in the Active coercer.
func TestBootstrapRejectsEmptyModels(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte("active: none\nmodels: {}\n")
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Bootstrap(dir)
	if err == nil {
		t.Fatal("empty models map must be rejected, not silently coerced")
	}
	if !strings.Contains(err.Error(), "no profiles configured") {
		t.Fatalf("error should explain the problem, got: %v", err)
	}
}

// TestStrictYAMLRejectsUnknownKey: unknown top level keys in config.yaml
// must fail loud, not be silently ignored: surfaces typos immediately.
func TestStrictYAMLRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := []byte("active: local\nmodels: {local: {llm: m, url: http://x, key: '', context_size: 1}}\nmystery_key: 7\n")
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Bootstrap(dir); err == nil {
		t.Fatal("expected Bootstrap to reject unknown top-level key")
	}
}

// TestBootstrapCoercesBogusContextSize: context_size 0 (or missing) is coerced
// to the default rather than degrading Pack() to "newest message only".
func TestBootstrapCoercesBogusContextSize(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: local
models:
  local:
    llm: m
    url: http://x
    key: ""
    context_size: 0
`)
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveProfile().ContextSize != defaultContextSize {
		t.Fatalf("context_size=0 should be coerced to %d, got %d",
			defaultContextSize, cfg.ActiveProfile().ContextSize)
	}
}

// TestBootstrapRejectsNilProfile: `models: { local: ~ }` decodes to a nil
// *Profile the ContextSize coercion loop would deref panic on. Bootstrap must
// reject it with a readable error instead.
func TestBootstrapRejectsNilProfile(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte("active: local\nmodels:\n  local: ~\n")
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Bootstrap(dir)
	if err == nil {
		t.Fatal("nil YAML profile must be rejected (not panic on deref)")
	}
	if !strings.Contains(err.Error(), "local") {
		t.Fatalf("error should name the offending profile, got: %v", err)
	}
}

// TestBootstrapRefusesSymlinkedDir: a co tenant could plant .codehamr → an
// attacker controlled dir before first run. Bootstrap must Lstat (not Stat) and
// refuse any symlink: even with a 0o600 config.yaml, the attacker owns the
// parent and can swap or read what codehamr writes. Same defence for a planted
// config.yaml symlink.
func TestBootstrapRefusesSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(root, DirName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, _, err := Bootstrap(root)
	if err == nil {
		t.Fatal("Bootstrap accepted a symlinked .codehamr: config-injection vector left open")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error should name the symlink defence: %v", err)
	}
	// Target must stay untouched, nothing dropped into the attacker controlled dir.
	if _, err := os.Stat(filepath.Join(target, "config.yaml")); err == nil {
		t.Fatal("Bootstrap wrote into the symlink target despite the rejection")
	}
}

func TestBootstrapRefusesSymlinkedConfigYAML(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Plant config.yaml as a symlink pointing outside the project.
	target := filepath.Join(t.TempDir(), "external.yaml")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, cfgPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, _, err := Bootstrap(root)
	if err == nil {
		t.Fatal("Bootstrap accepted a symlinked config.yaml")
	}
	// Attacker target must not be clobbered with the seed.
	if _, err := os.Stat(target); err == nil {
		t.Fatal("Bootstrap wrote through the config.yaml symlink: seed bytes landed at attacker target")
	}
}

func TestBootstrapRefusesNonDirectoryAtCodehamrPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, DirName), []byte("oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Bootstrap(root)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Bootstrap should refuse a regular-file at .codehamr, got %v", err)
	}
}

// TestResolvedKeyExpandsEnvVar: `key: ${MY_KEY}` in config.yaml must expand
// the env var at read time while the raw reference round trips on Save. This
// is the core of the "no plaintext secret on disk" path.
func TestResolvedKeyExpandsEnvVar(t *testing.T) {
	p := &Profile{Key: "${TEST_CODEHAMR_KEY}"}
	if err := os.Setenv("TEST_CODEHAMR_KEY", "sk-resolved-123"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("TEST_CODEHAMR_KEY")

	if got := p.ResolvedKey(); got != "sk-resolved-123" {
		t.Fatalf("ResolvedKey() = %q, want sk-resolved-123", got)
	}
	// Raw Key must be untouched: Save writes this, not the expanded value.
	if p.Key != "${TEST_CODEHAMR_KEY}" {
		t.Fatalf("ResolvedKey() mutated raw Key to %q: must stay ${TEST_CODEHAMR_KEY} for Save", p.Key)
	}
}

// TestResolvedKeyPassesLiteralThrough: a plaintext key with no $-references
// must pass through unchanged so existing configs keep working.
func TestResolvedKeyPassesLiteralThrough(t *testing.T) {
	p := &Profile{Key: "sk-literal-abc"}
	if got := p.ResolvedKey(); got != "sk-literal-abc" {
		t.Fatalf("ResolvedKey() = %q, want sk-literal-abc", got)
	}
}

// TestResolvedKeyUnsetEnvYieldsEmpty: ${VAR} with VAR unset expands to "",
// matching the "no key" branch (keyless local Ollama) without a panic.
func TestResolvedKeyUnsetEnvYieldsEmpty(t *testing.T) {
	os.Unsetenv("TEST_CODEHAMR_MISSING")
	p := &Profile{Key: "${TEST_CODEHAMR_MISSING}"}
	if got := p.ResolvedKey(); got != "" {
		t.Fatalf("ResolvedKey() = %q, want empty for unset env var", got)
	}
}

// TestResolvedKeyLiteralDollarSurvives: expansion applies ONLY when the whole
// key is a ${VAR} reference. A literal proxy key containing '$' (llama.cpp
// --api-key, LiteLLM master keys) must pass through byte identical:
// os.ExpandEnv would silently corrupt it ("pa$$word" -> "paword") and every
// request 401s with nothing anywhere hinting the key was rewritten.
func TestResolvedKeyLiteralDollarSurvives(t *testing.T) {
	for _, key := range []string{
		"pa$$word123",
		"sk-abc$def",
		"trailing$",
		"$UPFRONT-rest",
		"${not-a-valid-name}", // ${...} but not an env var name: literal
		"prefix-${REAL_VAR}",  // reference not the whole key: literal
	} {
		p := &Profile{Key: key}
		if got := p.ResolvedKey(); got != key {
			t.Errorf("ResolvedKey(%q) = %q, want the literal back", key, got)
		}
	}
}

// TestURLOverrideDoesNotPersist: a CODEHAMR_URL override lives in
// cfg.URLOverride and ActiveURL reflects it, but Save writes only the stored
// URL, so re bootstrapping without the env var restores the original endpoint.
func TestURLOverrideDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	originalURL := cfg.ActiveProfile().URL
	cfg.URLOverride = "http://override:9999"
	if got := cfg.ActiveURL(); got != "http://override:9999" {
		t.Fatalf("ActiveURL() ignored override: %q", got)
	}
	if got := cfg.ActiveProfile().URL; got != originalURL {
		t.Fatalf("override leaked into stored profile: %q", got)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ActiveProfile().URL != originalURL {
		t.Fatalf("Save persisted the override: %q", reloaded.ActiveProfile().URL)
	}
	if reloaded.URLOverride != "" {
		t.Fatalf("URLOverride round-tripped through YAML: %q", reloaded.URLOverride)
	}
}

// TestSaveTightensPreexistingLoosePerms covers the upgrade path fresh bootstrap
// misses: a config.yaml from an older world readable codehamr (or a hand edit)
// starts at 0o644, and os.WriteFile preserves an existing file's mode, so Save
// would rewrite the bytes (including a fresh API key) while leaving it
// world readable. Save must tighten a pre existing loose file to 0o600.
func TestSaveTightensPreexistingLoosePerms(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cdir, "config.yaml")
	// World readable file an older codehamr would have written.
	loose := []byte("active: local\nmodels:\n  local:\n    llm: m\n    url: http://x\n    key: \"\"\n    context_size: 1\n")
	if err := os.WriteFile(cfgPath, loose, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Save persists the API key with restrictive permissions.
	cfg.ActiveProfile().Key = "test-secret-1234567890abcdef"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("Save() must tighten a pre-existing 0o644 config.yaml to 0o600, got %v: API key stays world-readable across an upgrade", got)
	}
}
