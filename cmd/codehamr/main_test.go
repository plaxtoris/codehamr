package main

import (
	"strings"
	"testing"
)

// TestIsLocalBuild pins the contract: `go run` ("dev"), dirty tree builds,
// clean tree builds past the last tag (git describe shape), and tag less
// clones (bare short sha) are all local and skip self update; else the
// updater downgrades unreleased work to the last published release (a
// non release hash always reads as "stale"). Only exact release tags
// self update.
func TestIsLocalBuild(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"dev", true},
		{"v1.2.3-dirty", true},
		{"v0.1.0-5-g1a2b3c4-dirty", true},
		{"v0.1.0-5-g1a2b3c4", true}, // clean tree, 5 commits past the tag: unreleased work
		{"5290930", true},           // tag less clone: `git describe --always` bare sha
		{"v1.2.3", false},
		{"v1.2.3-gamma", false}, // prerelease tag: "-g" but not hex, still a release
		{"", false},
	}
	for _, c := range cases {
		if got := isLocalBuild(c.in); got != c.want {
			t.Errorf("isLocalBuild(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestReexecGuardOverridesPreexistingValue pins the loop guard env semantics
// maybeSelfUpdate relies on: the environment handed to reExec must carry
// CODEHAMR_NO_UPDATE_CHECK=1 exactly once even when a user already exported a
// different value. The old append(os.Environ(), …) left the stale value
// first, which Unix execve resolves first, defeating the guard; update.Check
// short circuits only on exactly "1".
func TestReexecGuardOverridesPreexistingValue(t *testing.T) {
	t.Setenv("CODEHAMR_NO_UPDATE_CHECK", "0") // user set it wrong; restored after test
	var got []string
	for _, kv := range reexecEnv() {
		if strings.HasPrefix(kv, "CODEHAMR_NO_UPDATE_CHECK=") {
			got = append(got, kv)
		}
	}
	if len(got) != 1 || got[0] != "CODEHAMR_NO_UPDATE_CHECK=1" {
		t.Fatalf("re exec env must carry exactly one guard entry set to 1, got %v", got)
	}
}
