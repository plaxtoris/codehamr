package tui

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/config"
	"github.com/codehamr/codehamr/internal/llm"
)

// argOption is one popover entry, used at command level (one row per command)
// and argument level (one row per accepted value for the active command).
type argOption struct {
	value       string // what gets inserted / committed
	description string // right aligned help text
	current     bool   // rendered bold; default selected when the popover opens
}

// command is one row in the popover, --help, and the dispatch table.
// args, if nonnil, supplies the argument level popover entries.
type command struct {
	name        string
	description string
	handler     func(Model, []string) (tea.Model, tea.Cmd)
	args        func(Model) []argOption
}

// commands lists every slash command, in popover/--help order. Keep it short.
var commands = []command{
	{
		name:        "/clear",
		description: "reset the conversation",
		handler:     (Model).cmdClear,
	},
	{
		name:        "/models",
		description: "list · <name> set (Tab cycles in the popover)",
		handler:     (Model).cmdModel,
		args: func(m Model) []argOption {
			out := make([]argOption, 0, len(m.cfg.Models))
			for _, n := range m.cfg.ModelNames() {
				p := m.cfg.Models[n]
				out = append(out, argOption{
					value:       n,
					description: p.LLM + " @ " + p.URL,
					current:     n == m.cfg.Active,
				})
			}
			return out
		},
	},
}

// commandByName returns the registered command for a slash name, or nil.
// Centralises the linear scan shared by completion, dispatch, and runSlash.
func commandByName(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// runSlash dispatches a slash prefixed submission; unknown commands produce a
// quiet hint, not an error. config.yaml is reread before every slash so
// hand edits take effect without a restart (see reloadConfigFromDisk).
func (m Model) runSlash(text string) (tea.Model, tea.Cmd) {
	if err := m.reloadConfigFromDisk(); err != nil {
		m.appendLine(styleWarn.Render("⚠ " + err.Error()))
	}
	fields := strings.Fields(text)
	if c := commandByName(fields[0]); c != nil {
		return c.handler(m, fields[1:])
	}
	m.appendLine(styleWarn.Render("unknown command: type / to see options"))
	return m, nil
}

// reloadConfigFromDisk reruns config.Bootstrap and replaces m.cfg so hand edits
// to config.yaml between slash commands take effect immediately. URLOverride
// (from CODEHAMR_URL) is carried across the swap so the env var keeps applying.
//
// Returns the Bootstrap error verbatim; callers decide whether to surface it
// (runSlash warns on submit; the popover refresh path ignores it so a broken
// file doesn't spam a warning on every keystroke).
//
// Rebuilds the llm.Client when the active profile's resolved (URL, model, key)
// triple changed: covers both within profile edits and a moved active.
func (m *Model) reloadConfigFromDisk() error {
	projectRoot := filepath.Dir(m.cfg.Dir)
	fresh, _, err := config.Bootstrap(projectRoot)
	if err != nil {
		return err
	}
	fresh.URLOverride = m.cfg.URLOverride

	prevURL := m.cfg.ActiveURL()
	prevProfile := m.cfg.ActiveProfile()
	prevLLM, prevKey := prevProfile.LLM, prevProfile.ResolvedKey()

	m.cfg = fresh

	newProfile := m.cfg.ActiveProfile()
	if prevURL != m.cfg.ActiveURL() || prevLLM != newProfile.LLM || prevKey != newProfile.ResolvedKey() {
		m.rebuildClient()
	}
	return nil
}

// PrintHelp writes the canonical human readable command list. Used by --help.
func PrintHelp(out io.Writer) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(w, "  %s\t%s\n", c.name, c.description)
	}
	w.Flush()
}

// handlers

// cmdModel: `/models` lists, `/models <name>` sets. Cycling is Tab/Shift+Tab
// in the popover, no separate "next" command.
func (m Model) cmdModel(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.printModelList()
		return m, nil
	}
	if err := m.cfg.SetActive(args[0]); err != nil {
		m.appendLine(styleError.Render("⚠ " + err.Error()))
		return m, nil
	}
	m.rebuildClient()
	return m, m.confirmActive(args[0])
}

// printModelList writes the "▸ active, name, llm @ url" rollup to scroll.
func (m *Model) printModelList() {
	m.appendLine(styleDim.Render("models (▸ active, /models <name> to switch):"))
	for _, n := range m.cfg.ModelNames() {
		mark := "  "
		if n == m.cfg.Active {
			mark = "▸ "
		}
		p := m.cfg.Models[n]
		m.appendLine(fmt.Sprintf("%s%s  %s",
			mark, n, styleDim.Render(p.LLM+" @ "+p.URL)))
	}
}

// confirmActive verifies keyed profiles with a small model request. Keyless
// profiles use a connectivity check without starting inference.
func (m *Model) confirmActive(profile string) tea.Cmd {
	p := m.cfg.ActiveProfile()
	// ActiveURL, not p.URL: under a CODEHAMR_URL override the banner must name
	// the endpoint actually dialed, not the config value the override displaced.
	if p.ResolvedKey() != "" {
		m.appendLine(styleDim.Render(fmt.Sprintf("▶ probing %s · %s @ %s", profile, p.LLM, m.cfg.ActiveURL())))
		return probeBackend(m.cli, profile, false)
	}
	m.appendLine(styleOK.Render(fmt.Sprintf("✓ active: %s · %s @ %s", profile, p.LLM, m.cfg.ActiveURL())))
	return pingBackend(m.cli.BaseURL)
}

// rebuildClient swaps in a fresh llm.Client for the now active profile.
// Replacing the pointer (not mutating fields) drops the prior Client's sticky
// state (noReasoning, keep alive pool tied to the old URL): new
// endpoint, fresh slate.
func (m *Model) rebuildClient() {
	p := m.cfg.ActiveProfile()
	m.cli = llm.New(m.cfg.ActiveURL(), p.LLM, p.ResolvedKey())

}

func (m Model) cmdClear(_ []string) (tea.Model, tea.Cmd) {
	m.history = nil
	m.scroll.Reset()
	m.sessionTokens = 0
	m.streamingEstimate = 0
	// Drop any queued follow up: it targeted the conversation just wiped.
	m.queued = nil
	// Reset the repeated failure streak so the next turn starts clean.
	m.failKey, m.failStreak = "", 0
	// The banner offers /clear as one of its two remedies, so clearing must
	// rearm it: a re filled window is a new occurrence, not the same one.
	m.ctxPressureWarned = false
	// Wipe prompt recall too: in memory ring and on disk .codehamr/history,
	// or leftover history would contradict the "fresh start" promise.
	m.promptHistory = nil
	m.histIdx = -1
	_ = clearPromptHistory(m.cfg.Dir)
	// Full wipe (unlike Ctrl+L, which redraws but keeps scrollback).
	// tea.ClearScreen emits \x1b[2J, which only wipes the viewport; the
	// saved lines buffer needs eraseScrollback (DECSED 3) too, or old replies
	// stay scrollable above the reset line. tea.Sequence keeps the print from
	// racing past the clear (tea.Batch runs both concurrently and the print
	// could land first, then get wiped). scroll keeps the line for resize
	// replay; outbox is cleared because the Sequence owns the print now.
	line := styleOK.Render("✓ conversation reset")
	m.scroll.WriteString(line + "\n")
	m.outbox = nil
	// Wrap like the outbox drain would: this Println bypasses it (the Sequence
	// owns the print), and every string handed to tea.Println must be wrapped
	// or an over width line drifts the renderer's cursor math.
	return m, tea.Sequence(tea.ClearScreen, eraseScrollback, tea.Println(wrapForScrollback(line, m.width)))
}
