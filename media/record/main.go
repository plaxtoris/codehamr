// Command record prepares an isolated project for a real Qwen demo recording.
package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codehamr/codehamr/internal/config"
)

//go:embed project/*.py
var project embed.FS

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: record SOURCE_PROJECT DEMO_PROJECT")
		os.Exit(1)
	}
	if err := prepare(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func prepare(source, destination string) error {
	original, _, err := config.Bootstrap(source)
	if err != nil {
		return fmt.Errorf("load project configuration: %w", err)
	}
	provider, ok := original.Models["openrouter"]
	if !ok || provider.ResolvedKey() == "" {
		return fmt.Errorf("recording requires an openrouter profile with an API key in %s", filepath.Join(source, config.DirName, "config.yaml"))
	}
	cfg := &config.Config{
		Active: "qwen3.8",
		Models: map[string]*config.Profile{
			"qwen3.8": {LLM: "qwen/qwen3.8-27b", URL: provider.URL, Key: provider.ResolvedKey(), ContextSize: 262144},
		},
		Logging: true,
		Dir:     filepath.Join(destination, config.DirName),
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	entries, err := project.ReadDir("project")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		data, err := project.ReadFile("project/" + entry.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
