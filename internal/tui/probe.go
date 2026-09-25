package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/llm"
)

const probeTimeout = 15 * time.Second

// Client identity prevents a delayed probe from changing connection state
// after a profile switch or another activation of the same profile.
type probeMsg struct {
	profile string
	cli     *llm.Client
	silent  bool
	err     error
}

func probeBackend(cli *llm.Client, profile string, silent bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		return probeMsg{profile: profile, cli: cli, silent: silent, err: cli.Probe(ctx)}
	}
}

func (m Model) handleProbe(msg probeMsg) (tea.Model, tea.Cmd) {
	if msg.cli != m.cli || msg.profile != m.cfg.Active {
		return m, nil
	}
	p, ok := m.cfg.Models[msg.profile]
	if !ok {
		return m, nil
	}
	m.connected = msg.err == nil
	if msg.silent {
		return m, nil
	}
	if msg.err != nil {
		m.appendLine(styleError.Render("⚠ probe " + msg.profile + ": " + probeErrorMessage(msg.err)))
	} else {
		m.appendLine(styleOK.Render(fmt.Sprintf("✓ active: %s · %s @ %s", msg.profile, p.LLM, m.cfg.ActiveURL())))
	}
	return m, nil
}

func probeErrorMessage(err error) string {
	if errors.Is(err, llm.ErrUnauthorized) {
		return "key rejected"
	}
	if un, ok := errors.AsType[llm.ErrUnreachable](err); ok {
		return "unreachable (" + un.Err.Error() + ")"
	}
	return err.Error()
}
