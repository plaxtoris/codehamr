// Package config owns the .codehamr/ directory: config.yaml plus the
// embedded default system prompt. Changes to the prompt require a rebuild.
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed PROMPT_SYS.md
var DefaultSystemPrompt string

const DirName = ".codehamr"

// defaultContextSize is the initial packing window. Set context_size to the
// window served by the chosen backend.
const defaultContextSize = 262144

// Profile names are user defined. Every endpoint uses the same settings.
type Profile struct {
	LLM         string `yaml:"llm"`
	URL         string `yaml:"url"`
	Key         string `yaml:"key"`
	ContextSize int    `yaml:"context_size"`
}

// Config is the on disk schema at .codehamr/config.yaml. Strict decoding:
// unknown top level keys fail Bootstrap so typos and stale schemas surface
// immediately rather than being silently ignored.
type Config struct {
	Active string              `yaml:"active"`
	Models map[string]*Profile `yaml:"models"`
	// Logging replaces log.txt at startup and records prompts and tool activity.
	Logging bool `yaml:"logging,omitempty"`
	// runtime only (not serialized)
	Dir string `yaml:"-"`
	// URLOverride, if set, wins over ActiveProfile().URL everywhere we dial
	// out. Kept off the Profile map so the runtime CODEHAMR_URL override never
	// round trips into Save().
	URLOverride string `yaml:"-"`
}

func Default() *Config {
	return &Config{
		Active: "local",
		Models: map[string]*Profile{
			"local": {LLM: "qwen3.8:27b", URL: "http://localhost:11434", ContextSize: defaultContextSize},
		},
	}
}

// Bootstrap returns the config for the current project, creating .codehamr/
// and config.yaml on first use. config.yaml is never overwritten; the prompt
// is embedded, never written to disk.
//
// Refuse symlinks so a redirected project directory cannot silently replace
// the configured endpoint or capture its API key.
func Bootstrap(projectRoot string) (*Config, bool, error) {
	dir := filepath.Join(projectRoot, DirName)
	created := false
	info, err := os.Lstat(dir)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("%s: refuses to follow symlink, remove or replace with a real directory", dir)
		}
		if !info.IsDir() {
			return nil, false, fmt.Errorf("%s: exists but is not a directory", dir)
		}
		// Tighten existing directory permissions when possible.
		if info.Mode().Perm() != 0o700 {
			_ = os.Chmod(dir, 0o700)
		}
	case errors.Is(err, os.ErrNotExist):
		// Configuration can contain API keys. Restrict access to the owner.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, false, err
		}
		created = true
	default:
		return nil, false, err
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	// Same symlink defence as the directory check: a symlinked config.yaml
	// could redirect the read (which config we honour) or the write (clobbering
	// an arbitrary user writable file with the seed). Refuse with a clear error.
	if li, err := os.Lstat(cfgPath); err == nil && li.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("%s: refuses to follow symlink, remove or replace with a real file", cfgPath)
	}
	var cfg *Config
	if b, err := os.ReadFile(cfgPath); err == nil {
		cfg = &Config{} // do NOT merge Default here; strict means strict
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, false, fmt.Errorf("config.yaml: %w", err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		cfg = Default()
		if err := writeYAML(cfgPath, cfg); err != nil {
			return nil, false, err
		}
	} else {
		return nil, false, err
	}
	cfg.Dir = dir

	// YAML `models: { name: ~ }` decodes to a nil *Profile that would panic on
	// the ContextSize deref below. Reject up front for a readable error.
	for name, p := range cfg.Models {
		if p == nil {
			return nil, false, fmt.Errorf("config.yaml: profile %q is empty; remove it or fill in the required fields", name)
		}
	}
	// Use the same packing default for every profile when its size is omitted
	// or invalid. Explicit values remain under the user's control.
	for _, p := range cfg.Models {
		if p.ContextSize <= 0 {
			p.ContextSize = defaultContextSize
		}
	}
	// Coerce a dangling Active to the first profile in sorted order
	// (deterministic). With no profiles at all, fail loud, since runtime would
	// otherwise nil deref on the first dial out.
	if _, ok := cfg.Models[cfg.Active]; !ok {
		names := cfg.ModelNames()
		if len(names) == 0 {
			return nil, false, errors.New("config.yaml: no profiles configured; add one under `models:` or delete .codehamr/config.yaml to reseed defaults")
		}
		cfg.Active = names[0]
	}

	return cfg, created, nil
}

// ResolvedKey expands a whole ${VARIABLE_NAME} reference at runtime. Save
// retains the reference. Other values are literal so keys containing dollar
// signs are not corrupted by general environment expansion.
func (p *Profile) ResolvedKey() string {
	key := p.Key
	if name, ok := strings.CutPrefix(key, "${"); ok {
		if name, ok = strings.CutSuffix(name, "}"); ok && isEnvName(name) {
			return os.Getenv(name)
		}
	}
	return key
}

// isEnvName reports whether s is a POSIX environment variable name
// ([A-Za-z_][A-Za-z0-9_]*), the only content ${...} expands.
func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// Save rewrites config.yaml.
func (c *Config) Save() error {
	if c.Dir == "" {
		return errors.New("config: Dir not set")
	}
	return writeYAML(filepath.Join(c.Dir, "config.yaml"), c)
}

func writeYAML(path string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	// Restore the usage header because yaml.Marshal does not retain comments.
	header := []byte(`# codehamr configuration
#
# In a devcontainer with Ollama on the host, use
# http://host.docker.internal:11434 instead of http://localhost:11434.
#
# A key such as ${OPENROUTER_API_KEY} reads the environment at runtime.
# Save preserves the reference. Literal API keys are also supported.
#
# Set context_size to the window your server actually serves.
# For Ollama, match OLLAMA_CONTEXT_LENGTH or the Modelfile num_ctx value.
# The local default is 262144. Lower it when your server serves less.
#
# OpenRouter uses https://openrouter.ai/api/v1 and a provider/model ID.

`)
	// Write a sibling temporary file and rename it into place so failed writes
	// leave the previous configuration intact. CreateTemp sets mode 0o600,
	// restricting the saved keys to the owner even if the old file was permissive.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no operation after a successful rename; cleans up early returns
	if _, err := tmp.Write(append(header, b...)); err != nil {
		tmp.Close()
		return err
	}
	// Flush file contents before publishing the replacement.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// ActiveProfile returns the selected profile. Bootstrap guarantees c.Active
// names a real one, so this is a straight map lookup.
func (c *Config) ActiveProfile() *Profile {
	return c.Models[c.Active]
}

// ActiveURL is the endpoint every dial out uses: the runtime override if set,
// else the active profile's URL. Use this over ActiveProfile().URL so
// CODEHAMR_URL doesn't leak back into Save.
func (c *Config) ActiveURL() string {
	if c.URLOverride != "" {
		return c.URLOverride
	}
	return c.ActiveProfile().URL
}

// ModelNames returns the profile names sorted, so the popover cycles
// deterministically regardless of map iteration order.
func (c *Config) ModelNames() []string {
	return slices.Sorted(maps.Keys(c.Models))
}

// SetActive switches the active profile and persists. Fails on an unknown name,
// no silent coercion. On Save failure it reverts in memory Active so the live
// model and config.yaml stay in lockstep; otherwise the switch would stick this
// session but vanish on the next Bootstrap.
func (c *Config) SetActive(name string) error {
	if _, ok := c.Models[name]; !ok {
		return fmt.Errorf("unknown model: %s", name)
	}
	prev := c.Active
	c.Active = name
	if err := c.Save(); err != nil {
		c.Active = prev
		return err
	}
	return nil
}
