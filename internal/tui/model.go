package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/x/ansi"

	"github.com/codehamr/codehamr/internal/config"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/tools"
)

const (
	defaultWidth = 80              // bootstrap width before the first WindowSizeMsg
	minViewport  = 5               // rows reserved above the prompt for streaming tokens
	popoverCap   = 6               // max rows the popover may claim
	pingTimeout  = 2 * time.Second // backend reachability probe budget
)

// phase is the turn state machine and single source of truth: idle = no turn;
// thinking = awaiting the model, no tokens yet; streaming = content flowing;
// running = a tool is executing.
type phase int

const (
	phaseIdle phase = iota
	phaseThinking
	phaseStreaming
	phaseRunning
)

func (p phase) active() bool { return p != phaseIdle }

func (p phase) label() string {
	switch p {
	case phaseThinking:
		return "thinking"
	case phaseStreaming:
		return "generating"
	case phaseRunning:
		return "running"
	}
	return ""
}

// turnOutcome is how a finished turn ended, frozen into the status bar until the
// next submit. outcomeNone is the zero value (no turn has finished yet, or the
// frozen summary was cleared at the start of a new turn).
type turnOutcome int

const (
	outcomeNone turnOutcome = iota
	outcomeDone
	outcomeStopped
)

// marker is the status bar glyph for the frozen finish: ✓ for a clean finish,
// ✗ for an abort (cancel, error, or a stalled/leaked end). "" suppresses the
// frozen segment for outcomeNone.
func (o turnOutcome) marker() string {
	switch o {
	case outcomeDone:
		return "✓"
	case outcomeStopped:
		return "✗"
	}
	return ""
}

// queuedPrompt is a prompt the user committed while a turn was running, held
// until the turn ends and then auto submitted. send is the chip expanded text
// for the LLM; echo is the collapsed, trimmed form shown in the queued box and
// the scrollback echo. Chip state is not preserved: an unqueued or recalled
// paste reappears expanded (matching disk loaded history), never as a chip, but
// its content is always intact.
type queuedPrompt struct {
	send string
	echo string
}

type Model struct {
	Version string

	cfg *config.Config
	cli *llm.Client

	history []chmctx.Message // full conversation minus the system prompt
	system  string           // embedded system prompt + working directory anchor

	// streaming is the live raw token buffer for the current content block,
	// rendered above the prompt by View() while the model talks. On flush
	// (block end, tool call, cancel, error) it's rendered through glamour,
	// queued into outbox for tea.Println, then reset.
	streaming *strings.Builder

	// outbox holds lines bound for terminal scrollback via tea.Println on the
	// next Update cycle. The Update wrapper drains it every cycle, so handlers
	// can call appendLine / flushStreaming without threading a Cmd back.
	outbox []string

	// scroll is the in memory transcript of every appendLine / flushStreaming
	// line. The real scrollback lives in the user's terminal; this copy is
	// replayed in handleResizeSettle after a width change wipes the terminal,
	// and tests read it to verify what was emitted.
	scroll *strings.Builder

	// reasoning accumulates the current round's chain of thought (EventReasoning)
	// for the debug log only; it never enters history (see llm.EventReasoning).
	// Pointer like streaming/scroll: Model is copied by value across bubbletea
	// and strings.Builder must not be copied after first use. Only written when
	// logging is on; reset every round in applyDone and on abort.
	reasoning *strings.Builder

	ta       promptInput
	renderer *glamour.TermRenderer
	spinner  spinner.Model

	// streaming state
	stream <-chan llm.Event

	// pending tool calls waiting to be executed after an assistant turn
	pending []chmctx.ToolCall

	// turn level stats (reset in finalizeTurn) + session cumulative count
	// (reset only by /clear, so the status bar carries the running session
	// total).
	turnTokens    int
	sessionTokens int
	// turnStart stamps the wall clock start of the current turn, set in beginTurn
	// (the user submit path only; tool reentry bypasses it), so it spans every
	// tool round rather than resetting per round. The status bar ticks
	// liveElapsed(time.Since(turnStart)) while a turn runs.
	turnStart time.Time
	// last* hold the finished turn's frozen footer summary, shown at idle until
	// the next submit: outcome marker, wall clock duration, and the token count
	// the avg tok/s divides by. lastTokens ÷ lastElapsed IS the displayed rate,
	// so it stays self verifying against the shown duration.
	lastElapsed time.Duration
	lastTokens  int
	lastOutcome turnOutcome
	// streamingEstimate is a live bytes/4 estimate of tokens for the current
	// round (reasoning + content). The server reports the authoritative count
	// only in the final usage block, so without this the footer would freeze
	// through the whole reasoning phase then jump. Reset to 0 on
	// EventDone/Error, where the real count takes over.
	streamingEstimate int

	connected bool // last known backend reachability (refreshed on ping / stream error)
	width     int
	height    int

	// View() returns "" while suppressView is on, so bubbletea's async ticker
	// can't commit a stale frame mid drag.
	suppressView bool
	// resizeGen is bumped per width change; settle ticks act only on the
	// matching gen, so older debounces self discard.
	resizeGen int

	// splashShown guards first frame emission; later resizes reemit via the
	// settle handler.
	splashShown bool

	// arrow key history: every successful submit is appended; histIdx tracks
	// the ↑/↓ walker position (-1 = current draft, 0 = newest). Entries carry
	// display text and chip state so ↑ reconstructs the original atomic chip
	// prompt, not just its visible text.
	promptHistory []promptEntry
	histIdx       int
	// histDraft stashes the unsent draft when ↑ first leaves the live line, so
	// ↓ back to histIdx -1 restores it instead of clearing the user's typing.
	histDraft promptEntry

	// queued holds a single prompt the user committed while a turn was still
	// running, auto submitted when the turn finishes naturally (see
	// handleStreamClosed; a Ctrl+C/error abort leaves it untouched for manual
	// send). nil = nothing queued. Enter mid turn fills/appends to it; Backspace
	// on an empty prompt pulls it back for editing (see queuePrompt/unqueuePrompt).
	queued *queuedPrompt

	// slash autocomplete popover state. suggest holds command rows (when
	// suggestArgLevel is false) or argument rows for activeCmd (when true):
	// same renderer, same keybindings.
	suggest         []argOption
	suggestIdx      int
	suggestOpen     bool
	suggestArgLevel bool
	activeCmd       string

	// per turn cancel plumbing: one context + CancelFunc govern the LLM stream
	// and tool calls for the turn. Ctrl+C cancels the whole cascade.
	turnCtx     context.Context
	cancel      context.CancelFunc
	quitArmedAt time.Time // first Ctrl+C in idle arms; second within 3s quits

	status string // transient status bar hint; cleared by the event that obsoletes it (keypress, quit arm timer, endTurn)
	phase  phase  // idle / thinking / streaming / running

	// retrying marks that status currently shows an llm.EventRetry backoff
	// hint; the next non retry stream event clears it (content = the retry
	// succeeded, error = the banner takes over). A flag rather than a text
	// compare because the hint is dynamic ("retry 1/3 in 2s").
	retrying bool

	// Repeated failure nudge, the first of the four deterministic backstops. A
	// turn otherwise ends purely when the model stops calling tools; nothing
	// forces a tool or yields. lastToolKey is the most recently dispatched tool's
	// target identity (set in dispatchNextTool); failKey/failStreak track how
	// often that SAME target failed the SAME way. At maxToolFailStreak we inject
	// one system note to change approach: a nudge, never a hard yield. Keyed on
	// tool+target (not full args) so cosmetic retry differences can't defeat it.
	lastToolKey string
	failKey     string
	failStreak  int

	// Runaway iteration nudge, sibling to the failure nudge. A 30B model can
	// loop on plausible *non failing* calls (reread, re grep, re list) forever;
	// the failure streak only catches repeated *failures*, so that hole stayed
	// open. toolRounds counts tool calls dispatched this turn (reset in endTurn)
	// and is one of the finish nudge's two gates (see maybeVerifyNudge: the
	// batching instruction compresses a substantial turn into few round trips,
	// so either measure of real work must trip it); the runaway check counts
	// llmRounds instead.
	toolRounds int
	// llmRounds counts assistant round trips this turn, the honest measure of
	// "how many times has the model gone around the loop". toolRounds counts
	// CALLS, which the batching instruction deliberately inflates: a healthy
	// turn batching eight reads per message would trip a call based runaway cap
	// in ten round trips. Reset in endTurn.
	llmRounds int
	// lastRunawayRound is the llmRounds value at the last runaway nudge, so the
	// check re fires periodically instead of latching once. A single note at the
	// cap left a turn past it with no supervision at all for the rest of its life.
	lastRunawayRound int

	// streamReplays counts transparent mid stream retries this turn. A dropped
	// socket used to kill the whole turn and wait for a human to reprompt,
	// which is what an unattended run cannot survive. Bounded per turn (reset in
	// endTurn, never in applyDone) so a server dropping every round can't
	// replay without limit.
	streamReplays int
	// ctxPressureWarned keeps the context overflow banner to once per session:
	// the condition holds for every remaining round, so an unlatched warning
	// would paper the transcript.
	ctxPressureWarned bool
	// budgetFloorWarned and ctxTruncationWarned limit configuration warnings
	// to once per session. The first detects little room for history; the
	// second detects a large mismatch between estimated and reported usage.
	budgetFloorWarned   bool
	ctxTruncationWarned bool
	// lastPromptEstimate is the packer's bytes/4 token estimate of the request
	// most recently sent, compared in applyDone against the server reported
	// prompt_tokens to detect silent server side truncation.
	lastPromptEstimate int
	// turnActed is false while a turn has done nothing but read_file: a
	// question answered out of the codebase, with no artifact that could be
	// falsely called green. Deliberately "not read_file" rather than
	// "write_file or edit_file": a file built with a bash heredoc, a `sed -i`,
	// or an `npm init` is just as much an artifact, and telling those apart
	// would mean classifying shell commands. Reset in endTurn.
	turnActed bool

	// Empty reply nudge, the third soft backstop. The two above catch doing too
	// much; this catches a turn ending with nothing said and nothing called. A
	// clean finish always carries a summary and a continuing turn always carries a
	// tool call, so an empty newest assistant message is always an anomaly: the
	// model stopped mid task, or (on a thinking model) its tool call streamed into
	// the reasoning channel and was dropped before reaching us, the dominant
	// silent death we'd otherwise end on with no warning. One reprompt to reissue
	// or finish; emptyNudged bounds CONSECUTIVE empties to a single retry: a
	// round that issues a tool call rearms it (see handleStreamClosed), so a
	// flaky stream earns a fresh reprompt per stall while a server that
	// deterministically swallows every call can't loop. Reset in endTurn.
	emptyNudged bool

	// Finish re grounding nudge, the fourth soft backstop. The three above catch
	// doing too much (failure, runaway) and stopping with nothing said (empty).
	// This catches the false green finish: a turn that did real work ending with a
	// confident summary for something it never actually ran. When a substantial
	// turn (llmRounds >= verifyNudgeMinRounds, or toolRounds >= verifyNudgeMinCalls)
	// is about to finish with a clean,
	// nonempty reply, one reprompt makes the model re walk the original request
	// and run the check that proves each runnable part, or mark it unverified
	// honestly, instead of dressing up a brace count or an HTTP 200 as proof. A
	// nudge, never a hard yield; verifyNudged latches it to once per turn. Reset in
	// endTurn.
	verifyNudged bool
}

func New(cfg *config.Config, cli *llm.Client, projectDir, version string) Model {
	ta := newPromptInput()

	// Fixed dark style: WithAutoStyle queries the terminal (OSC 11) before
	// bubbletea grabs raw stdin, so the reply bytes leak into the textarea as
	// "1;rgb:1e1e/1e1e/1e1e" garbage. Dev containers are dark: no query, no leak.
	r, _ := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(defaultWidth-4))

	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = styleSpinner

	m := Model{
		Version:   version,
		cfg:       cfg,
		cli:       cli,
		system:    buildSystem(projectDir),
		ta:        ta,
		renderer:  r,
		spinner:   sp,
		connected: true, // optimistic until the first ping proves otherwise
		// width/height left at 0; View() returns "" until the first
		// WindowSizeMsg, so we don't flash an 80×24 frame then resize.
		streaming: new(strings.Builder),
		scroll:    new(strings.Builder),
		reasoning: new(strings.Builder),
		histIdx:   -1,
	}
	// Record the active backend + budget once, before any turn, so a shared log
	// names exactly which model/endpoint/context window produced the behaviour.
	// Gated on dbgEnabled so the profile derefs run only when logging is on;
	// off (the default) means New behaves exactly as before.
	if dbgEnabled() {
		dbgWriteSession(version, cfg.Active, cfg.ActiveProfile().LLM, cfg.ActiveURL(),
			m.activeContextSize(), chmctx.Tokens(m.system),
			[]string{tools.BashName, tools.ReadFileName, tools.WriteFileName, tools.EditFileName})
	}
	// Seed prompt history from .codehamr/history so ↑ recalls prompts from
	// earlier sessions. Loaded entries carry no chip metadata (the on disk
	// format stores expanded text only), so a recalled multi line paste
	// appears uncollapsed, the right tradeoff for a cat friendly history file.
	m.promptHistory = loadPromptHistory(cfg.Dir)
	return m
}

// activeContextSize uses the configured window for every backend.
func (m *Model) activeContextSize() int {
	return m.cfg.ActiveProfile().ContextSize
}

// resizeSettleDelay debounces width resize bursts: longer than typical drag
// SIGWINCH cadence (10 50ms) so a continuous drag collapses to one settle,
// short enough that a one off resize feels instant.
const resizeSettleDelay = 150 * time.Millisecond

type resizeSettleMsg struct{ gen int }

// eraseScrollback wipes the terminal's saved lines buffer (DECSED 3); no
// tea.ClearScreen equivalent clears scrollback.
var eraseScrollback tea.Cmd = func() tea.Msg {
	os.Stdout.WriteString(ansi.EraseDisplay(3))
	return nil
}

// pingMsg carries a backend reachability result. baseURL is the URL probed;
// Update drops the message when it no longer matches the live client's URL,
// else a stale ping from the prior profile (a mid flight /models switch) would
// overwrite connected state with the wrong endpoint's reachability.
type pingMsg struct {
	ok      bool
	baseURL string
}

// quitArmResetMsg fires ~3s after Ctrl+C arms the quit: if not already quit or
// rearmed, clear the hint from the status bar.
type quitArmResetMsg struct{}

func (m Model) Init() tea.Cmd {
	// Keyed profiles validate credentials and model support at startup.
	// Keyless profiles only check connectivity to avoid loading a local model.
	connectivity := pingBackend(m.cli.BaseURL)
	if p := m.cfg.ActiveProfile(); p != nil && p.ResolvedKey() != "" {
		connectivity = probeBackend(m.cli, m.cfg.Active, true)
	}
	return tea.Batch(
		textarea.Blink,
		m.spinner.Tick,
		connectivity,
	)
}

// Update is the bubbletea entry point: it dispatches to update()'s typed
// handlers then drains the outbox into a single tea.Println, so lines land in
// scrollback in the exact order appendLine / flushStreaming queued them. One
// Println per cycle, never a Batch; Batch runs children concurrently, leaving
// arrival order undefined, so splash lines and tool call banners would shuffle.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	nm := next.(Model)
	if len(nm.outbox) > 0 {
		printCmd := tea.Println(wrapForScrollback(strings.Join(nm.outbox, "\n"), nm.width))
		nm.outbox = nil
		cmd = tea.Batch(printCmd, cmd)
	}
	return nm, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.FocusMsg, tea.BlurMsg:
		// Terminal focus reports (CSI I / CSI O) arrive as these typed msgs
		// under tea.WithReportFocus. Swallow them so they never reach
		// textarea.Update; otherwise the escape fragments get parsed as
		// printable runes, inserted into the prompt, and bloat textarea height
		// on every focus switch.
		return m, nil

	case tea.KeyMsg:
		// An empty runes key can surface when the parser chokes mid escape.
		// Drop it before recomputeLayout wastes cycles.
		if msg.Type == tea.KeyRunes && len(msg.Runes) == 0 {
			return m, nil
		}
		// Pre grow the textarea before bubbles processes the key. bubbles'
		// repositionView() runs at the end of textarea.Update and scrolls the
		// viewport down whenever the cursor crosses below current Height, but
		// our recomputeLayout() grows Height only AFTER handleKey returns. So
		// a char that wraps to a new visual row leaves YOffset>0 with the first
		// wrap row clipped off the top, which recomputeLayout can't reclaim.
		// Inflating Height to the screen cap first keeps the cursor inside the
		// visible band for any normal keystroke, so repositionView doesn't
		// scroll and YOffset stays 0; recomputeLayout then trims Height back to
		// visualPromptLines so the live region doesn't bloat empty rows.
		m.preGrowTextarea()
		next, cmd := m.handleKey(msg)
		nm := next.(Model)
		nm.recomputeLayout()
		return nm, cmd

	case tea.WindowSizeMsg:
		return m.handleWindowSize(msg)

	case resizeSettleMsg:
		return m.handleResizeSettle(msg)

	case pingMsg:
		// Drop stale pings from a prior backend (a /models switch while a ping
		// was in flight). The live client's URL is the source of truth.
		if msg.baseURL != m.cli.BaseURL {
			return m, nil
		}
		m.connected = msg.ok
		return m, nil

	case probeMsg:
		return m.handleProbe(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case streamEventMsg:
		// Stale event from a stream the current turn no longer owns (Ctrl+C →
		// fresh submit while the prior readEvent was in flight). Keep draining
		// the channel so the producer goroutine exits cleanly, but never let
		// the event mutate the now active turn's state.
		if msg.ch != m.stream {
			return m, readEvent(msg.ch)
		}
		return m.handleStream(msg.e)

	case streamClosedMsg:
		// Stale close from the prior turn's channel; running handleStreamClosed
		// would nil out the live m.stream and, worse, finalizeTurn + endTurn the
		// active turn, killing the user's request out from under them.
		if msg.ch != m.stream {
			return m, nil
		}
		return m.handleStreamClosed()

	case toolResultMsg:
		// Stale result from a turn the user already cancelled. Without this
		// drop, the orphan tool message gets appended to the live turn's history
		// (no preceding assistant.tool_calls → the next /v1 request 400s) and
		// startChat would abandon the in flight stream. The turnCtx tag was
		// captured at runToolCall time; endTurn nils m.turnCtx and a fresh
		// beginTurn installs a new one that can't match.
		if msg.turnCtx != m.turnCtx {
			return m, nil
		}
		dbgWriteMessage("tool_result", msg.Msg)
		m.history = append(m.history, msg.Msg)
		m.recordToolOutcome(msg.Msg.ToolName, msg.Msg.Content)
		// Drain every remaining call before reentering chat: OpenAI rejects an
		// assistant.tool_calls message followed by fewer tool messages than
		// calls issued, so a partial dispatch 400s and loses the rest.
		// Sequential dispatch in emit order keeps the pairing intact.
		if len(m.pending) > 0 {
			return m.dispatchNextTool()
		}
		// Queue drained: only now is it safe to inject a system nudge. A
		// system message wedged between assistant.tool_calls and its tool
		// results would break that pairing and 400 the next request.
		m.maybeFailureNudge()
		m.maybeRunawayNudge()
		m.phase = phaseThinking
		return m, m.startChat()

	case quitArmResetMsg:
		if !m.quitArmedAt.IsZero() && time.Now().After(m.quitArmedAt) {
			m.quitArmedAt = time.Time{}
			if m.status == quitArmText {
				m.status = ""
			}
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.recomputeLayout()
	return m, cmd
}

// handleWindowSize tracks new dimensions, rebuilds the glamour renderer on a
// wrap width change, emits the splash on the first frame, and on a true width
// change starts the debounced resize settle cycle (suppressView until the
// settle tick lands at the matching gen).
func (m Model) handleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	first := !m.splashShown
	widthChanged := m.width > 0 && m.width != msg.Width
	m.width, m.height = msg.Width, msg.Height
	m.ta.SetWidth(msg.Width - 2)
	if first || widthChanged {
		// Glamour compiles a stylesheet + template tree per build, so rebuild
		// only on a real wrap width change; height only events and intra drag
		// duplicates reuse the existing renderer.
		if r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(max(msg.Width-4, 1))); err == nil {
			m.renderer = r
		}
	}
	m.recomputeLayout()
	if first {
		m.splashShown = true
		m.outbox = append(m.outbox, m.splashLines()...)
		return m, nil
	}
	if !widthChanged {
		return m, nil
	}
	m.suppressView = true
	m.resizeGen++
	gen := m.resizeGen
	return m, tea.Tick(resizeSettleDelay, func(time.Time) tea.Msg {
		return resizeSettleMsg{gen: gen}
	})
}

// handleResizeSettle fires once the post resize debounce expires for the
// matching gen. Wipes the terminal (so previous width rows can't soft wrap into
// stair steps), then reemits splash, replayed scroll, and any pending outbox
// at the new width. Older debounces self discard on the gen check.
func (m Model) handleResizeSettle(msg resizeSettleMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.resizeGen {
		return m, nil
	}
	m.suppressView = false
	// tea.Sequence keeps order strict; Batch would race the clears with the
	// writes. After the wipe every line below is emitted at the current width,
	// so no previous width row can soft wrap into stair steps.
	cmds := []tea.Cmd{tea.ClearScreen, eraseScrollback}
	if splash := strings.Join(m.splashLines(), "\n"); splash != "" {
		cmds = append(cmds, tea.Println(wrapForScrollback(splash, m.width)))
	}
	if scroll := strings.TrimRight(m.scroll.String(), "\n"); scroll != "" {
		cmds = append(cmds, tea.Println(wrapForScrollback(scroll, m.width)))
	}
	if len(m.outbox) > 0 {
		cmds = append(cmds, tea.Println(wrapForScrollback(strings.Join(m.outbox, "\n"), m.width)))
		m.outbox = nil
	}
	return m, tea.Sequence(cmds...)
}

// submit commits a user prompt. sendText is the expanded form sent to the LLM
// (chip labels replaced by their original paste content); echoText is the
// collapsed form shown in scrollback so the chat doesn't swallow 80 lines of
// pasted log every turn; entry is the history snapshot replayed by ↑/↓,
// including chip state.
func (m Model) submit(sendText, echoText string, entry promptEntry) (tea.Model, tea.Cmd) {
	// Echo to scrollback with the same accent ▌ the textarea uses, one visual
	// language for "your voice" across live input and history.
	m.appendLine(stylePrompt.Render("▌ ") + styleUser.Render(echoText))
	m.promptHistory = append(m.promptHistory, entry)
	m.histIdx = -1
	// Persist the prompt so ↑ finds it after a restart. Errors are
	// swallowed: a transient failure isn't worth derailing submit, and a
	// permanent one (read only .codehamr/) would just be noise on every prompt.
	_ = appendPromptHistory(m.cfg.Dir, sendText)

	if strings.HasPrefix(sendText, "/") {
		dbgWritef("user_slash", "%s", sendText)
		return m.runSlash(sendText)
	}
	// A new user message is a new goal: drop any in progress failure streak so
	// a stale count can't trip the nudge early. History persists; only the
	// counter resets.
	m.failKey, m.failStreak = "", 0
	dbgWritef("user", "%s", sendText)
	return m, m.appendUserTurn(sendText)
}

func (m *Model) startChat() tea.Cmd {
	m.llmRounds++
	msgs := m.buildMessages()
	ch := m.cli.Chat(m.turnCtx, msgs, m.buildTools())
	m.stream = ch
	return readEvent(ch)
}

// installTurnContext cancels any in flight turn context and installs a fresh
// per turn root on m.turnCtx / m.cancel. Cancel old then install new keeps
// Ctrl+C consistent: one m.cancel() always unwinds the whole current cascade.
func (m *Model) installTurnContext() {
	if m.cancel != nil {
		m.cancel()
	}
	m.turnCtx, m.cancel = context.WithCancel(context.Background())
}

// beginTurn installs a fresh per turn context, flips phase to thinking, and
// returns the chat stream reader Cmd. Every path starting a new LLM round
// funnels through here so one m.cancel() cancels the whole cascade.
func (m *Model) beginTurn() tea.Cmd {
	m.installTurnContext()
	m.turnStart = time.Now()
	m.lastOutcome = outcomeNone // the new run replaces the prior frozen summary
	m.phase = phaseThinking
	return m.startChat()
}

// appendUserTurn appends a user role message to history and starts a turn.
// The only path that does so; used by submit.
func (m *Model) appendUserTurn(content string) tea.Cmd {
	m.history = append(m.history, chmctx.Message{Role: chmctx.RoleUser, Content: content})
	return m.beginTurn()
}

// endTurn zeroes per turn state after a turn finishes or aborts. Pair to
// beginTurn. Cancels the per turn context unconditionally to release the
// CancelFunc; Background rooted contexts otherwise leak one child cancelCtx
// per turn until the process exits. Drops pending tool calls so a turn cut
// short mid dispatch (Ctrl+C or error) can't leak a leftover call into the next
// turn, which would dispatch with stale args and append an orphan tool_result
// whose tool_call_id no longer pairs the latest assistant message. Does NOT
// touch scrollback; callers decide whether to flush streaming or emit a banner.
func (m *Model) endTurn() {
	if m.cancel != nil {
		m.cancel()
	}
	m.phase = phaseIdle
	m.cancel = nil
	m.turnCtx = nil
	m.pending = nil
	m.toolRounds = 0
	m.turnActed = false
	m.llmRounds = 0
	m.lastRunawayRound = 0
	m.streamReplays = 0
	m.emptyNudged = false
	m.verifyNudged = false
	// The queue refusal hint says "send it when the turn ends"; that moment is
	// now, so the advice would be stale from the next render on.
	if m.status == queueSlashHint {
		m.status = ""
	}
	// A retry hint dies with its turn: a Ctrl+C during the backoff wait would
	// otherwise leave "retry 1/3 in 15s" stranded in the idle status bar.
	if m.retrying {
		m.retrying = false
		m.status = ""
	}
}

// minUsableBudget is the history budget below which the agent is effectively
// amnesiac: Pack keeps little beyond the newest message, so every round
// forgets the last. Budget(8192) is already 0 and Budget(16384) ~5k, so this
// fires only on genuinely misconfigured small windows.
const minUsableBudget = 4096

func (m *Model) buildMessages() []chmctx.Message {
	ctxSize := m.activeContextSize()
	budget := chmctx.Budget(ctxSize)
	if budget < minUsableBudget && !m.budgetFloorWarned {
		m.budgetFloorWarned = true
		m.appendLine(styleError.Render(fmt.Sprintf(
			"⚠ context_size %d leaves only %d tokens for history after fixed reservations: the agent will forget almost everything between rounds. Raise context_size in .codehamr/config.yaml (and the server's real window) to 32768+.",
			ctxSize, budget)))
	}
	r := chmctx.Pack(m.history, budget)
	out := make([]chmctx.Message, 0, len(r.Messages)+1)
	out = append(out, chmctx.Message{Role: chmctx.RoleSystem, Content: m.system})
	out = append(out, r.Messages...)
	est := 0
	for i := range out {
		est += out[i].Tokens()
	}
	m.lastPromptEstimate = est
	dbgWriteRequest(m.cfg.ActiveProfile().LLM, ctxSize, budget, len(m.history), out)
	return out
}

// buildTools exposes the four local tools every turn: bash, read_file,
// write_file, edit_file. No loop/control tool; a turn ends when the model
// stops emitting tool calls (see handleStreamClosed).
func (m *Model) buildTools() []llm.Tool {
	return []llm.Tool{
		schemaToTool(tools.BashSchema()),
		schemaToTool(tools.ReadFileSchema()),
		schemaToTool(tools.WriteFileSchema()),
		schemaToTool(tools.EditFileSchema()),
	}
}

// schemaToTool unwraps a tool schema (the map[string]any shape shared by bash
// and the file tools) into the flat llm.Tool the Responses payload expects.
func schemaToTool(s map[string]any) llm.Tool {
	fn := s["function"].(map[string]any)
	return llm.Tool{
		Type:        s["type"].(string),
		Name:        fn["name"].(string),
		Description: fn["description"].(string),
		Parameters:  fn["parameters"].(map[string]any),
	}
}

// handleStream dispatches one llm.Event to the matching apply* helper and
// rearms the stream reader; EventError unwinds the turn instead of looping.
// Events arriving after cancellation are drained quietly; acting on them would
// corrupt scroll (EventContent), re populate pending (EventToolCall), or credit
// a dead turn's tokens (EventDone).
func (m Model) handleStream(e llm.Event) (tea.Model, tea.Cmd) {
	if !m.phase.active() {
		return m, readEvent(m.stream)
	}
	// A retry hint is obsolete the moment anything else arrives: content means
	// the resend succeeded, an error means the banner takes over.
	if m.retrying && e.Kind != llm.EventRetry {
		m.retrying = false
		m.status = ""
	}
	switch e.Kind {
	case llm.EventRetry:
		// The client is waiting out a backoff before resending a transiently
		// failed request. Surface the wait in the status bar so it doesn't
		// read as a frozen turn; history is untouched.
		m.retrying = true
		m.status = e.Content
		dbgWritef("retry", "%s (%v)", e.Content, e.Err)
	case llm.EventContent:
		m.applyContent(e)
	case llm.EventReasoning:
		// Reasoning streams while phase stays "thinking": still deliberating,
		// no user facing content yet. Hidden from the transcript (not written
		// to scroll); only the live token estimate ticks up in the status bar.
		// When logging, accumulate it so the round's chain of thought lands in
		// the debug log, the highest signal record for understanding why the
		// model chose a tool or went wrong.
		m.streamingEstimate += len(e.Content) / 4
		if dbgEnabled() {
			m.reasoning.WriteString(e.Content)
		}
	case llm.EventToolArgs:
		// Tool call arguments stream as the model writes a file (write_file /
		// edit_file) or a bash command. Count them live like content so the
		// counter doesn't freeze through a long file write, and flip to
		// "generating": the model is producing output, not thinking. The
		// resolved call still arrives whole as EventToolCall; this only feeds
		// the estimate, nothing reaches history here.
		if m.phase == phaseThinking {
			m.phase = phaseStreaming
		}
		m.streamingEstimate += len(e.Content) / 4
	case llm.EventToolCall:
		m.applyToolCall(e)
	case llm.EventDone:
		m.applyDone(e)
	case llm.EventError:
		return m, m.applyError(e)
	}
	return m, readEvent(m.stream)
}

// applyContent writes one streamed text chunk to the live buffer and promotes
// phase from thinking to streaming on the first chunk so the status bar shows
// tokens flowing. View() renders the buffer live above the prompt; once the
// block ends it's flushed through glamour into scrollback via tea.Println.
func (m *Model) applyContent(e llm.Event) {
	if m.phase == phaseThinking {
		m.phase = phaseStreaming
	}
	// Expand tabs on the way into the display buffer: terminals advance a
	// literal tab to the next 8 column stop while every width computation
	// downstream (View's live ansi.Wrap, glamour's code fence padding,
	// wrapForScrollback) counts it as one cell, so a tab indented code block
	// passes the width checks yet physically overflows and drifts the
	// renderer's cursor math. Display only: history keeps e.Final untouched.
	m.streaming.WriteString(strings.ReplaceAll(e.Content, "\t", "    "))
	m.streamingEstimate += len(e.Content) / 4
}

// applyToolCall queues a streamed tool call for later dispatch. flushStreaming
// up front commits this round's text to scroll before the inline tool call
// status lands, so the user sees styled text *before* the "▶ bash: ..." line,
// not all at once at turn end.
func (m *Model) applyToolCall(e llm.Event) {
	m.flushStreaming()
	m.pending = append(m.pending, *e.ToolCall)
}

// applyDone records one assistant response and adds its output tokens to
// the turn and session totals. When usage is absent, the live byte estimate
// is used instead. Tool rounds accumulate until the turn ends.
func (m *Model) applyDone(e llm.Event) {
	delta := e.Tokens
	if delta == 0 {
		delta = m.streamingEstimate
	}
	m.turnTokens += delta
	m.sessionTokens += delta
	m.streamingEstimate = 0
	m.connected = true
	// Round level reasoning first (it preceded the answer), then the assistant
	// message, then the round metrics, so the log reads in causal order.
	if r := m.reasoning.String(); r != "" {
		dbgWritef("reasoning", "%s", r)
	}
	m.reasoning.Reset()
	if e.Final != nil {
		dbgWriteMessage("assistant", *e.Final)
		m.history = append(m.history, *e.Final)
	}
	// Log actual usage beside the request estimate to help diagnose packing.
	dbgWritef("round_done", "tokens=%d (counted=%d) · prompt_tokens=%d · elapsed=%s",
		e.Tokens, delta, e.PromptTokens, e.Elapsed.Round(time.Millisecond))
	// Warn when reported input usage approaches the configured window. The
	// provider may reject or truncate subsequent requests.
	if ctxSize := m.activeContextSize(); e.PromptTokens > 0 && e.PromptTokens >= ctxSize-ctxSize/20 {
		dbgWritef("ctx_pressure", "prompt_tokens=%d at >=95%% of ctx=%d; real prompt has outgrown the packer's estimate, next request risks silent server side truncation", e.PromptTokens, ctxSize)
		// Show the warning once so it remains visible when logging is disabled.
		if !m.ctxPressureWarned {
			m.ctxPressureWarned = true
			m.appendLine(styleError.Render(fmt.Sprintf(
				"⚠ prompt is at %d of %d context tokens. Your server may be silently truncating it (the system prompt goes first). Lower context_size in .codehamr/config.yaml to match what the server really serves, or /clear.",
				e.PromptTokens, ctxSize)))
		}
	}
	// A much smaller reported input count can indicate server truncation, but
	// tokenization and accounting also vary. Keep this diagnostic conditional
	// and avoid small prompts where fixed overhead dominates the estimate.
	if e.PromptTokens > 0 && m.lastPromptEstimate > 12000 && e.PromptTokens < m.lastPromptEstimate/2 && !m.ctxTruncationWarned {
		m.ctxTruncationWarned = true
		dbgWritef("ctx_truncation", "server prompt_tokens=%d vs packed estimate=%d; possible server side truncation", e.PromptTokens, m.lastPromptEstimate)
		m.appendLine(styleError.Render(fmt.Sprintf(
			"⚠ the server processed only %d tokens of a ~%d token prompt: this may indicate context truncation. Raise the server's window (e.g. Ollama num_ctx) or lower context_size in .codehamr/config.yaml to what it really serves.",
			e.PromptTokens, m.lastPromptEstimate)))
	}
	m.flushStreaming()
}

// maxStreamReplays bounds transparent mid stream retries per turn. Two is
// enough to ride out a flaky proxy without letting a server that drops every
// round spin forever; past it the error surfaces as it always did.
const maxStreamReplays = 2

// applyError unwinds the turn on a stream error: preserve content streamed
// before the error (so the user keeps failure context), emit the one line hint,
// drop the pending queue, reset turn state.
//
// Unless the drop is replayable. llm sets MidStream for a socket that died
// after delivering at least one frame and that is not the server's own refusal.
// Only applyDone writes an assistant message to history, so a round that never
// reached EventDone left history untouched: reissuing the identical request
// duplicates nothing. Discard this round's partial buffers first: they are
// display and queue state, not context: and go again. Without this, one
// transient drop three hours into an unattended run ends it and waits for a
// human.
func (m *Model) applyError(e llm.Event) tea.Cmd {
	dbgWritef("error", "%v", e.Err)
	if e.MidStream && m.phase.active() && m.streamReplays < maxStreamReplays {
		m.streamReplays++
		dbgWritef("replay", "mid stream drop, replaying request (%d/%d): %v", m.streamReplays, maxStreamReplays, e.Err)
		m.streaming.Reset()
		m.streamingEstimate = 0
		m.reasoning.Reset()
		m.pending = nil
		// Ride the retry hint machinery so the hint is cleared by the next
		// non retry event and by endTurn; without it the idle status bar keeps
		// reading "retrying" long after the turn finished cleanly.
		m.retrying = true
		m.status = fmt.Sprintf("stream dropped, retrying (%d/%d)", m.streamReplays, maxStreamReplays)
		m.phase = phaseThinking
		return m.startChat()
	}
	if isUnreachable(e.Err) {
		m.connected = false
	}
	m.abortTurn(styleError.Render(m.errorMessage(e)))
	return nil
}

// abortTurn winds down a turn that did not complete normally: flush in flight
// text so the partial block lands in scrollback, post the explanatory banner,
// drop pending tool calls, reset per turn counters and context. Pair to
// applyDone for the happy path.
func (m *Model) abortTurn(banner string) {
	m.flushStreaming()
	m.reasoning.Reset()
	if banner != "" {
		m.appendLine(banner)
	}
	// A prompt queued mid turn must NOT auto fire on an abort: the user took back
	// control, so its follow up may no longer be wanted. Restore it to the
	// textarea instead (editable, one Enter to send), which also avoids leaving an
	// idle "queued" box that would orphan fire after the next turn. Only when the
	// textarea is empty, so a draft typed mid turn isn't clobbered.
	if m.queued != nil {
		if m.ta.Value() == "" {
			m.setPromptText(m.queued.send)
		}
		m.queued = nil
	}
	// finalizeTurn folds the in flight estimate into the counters and zeroes it,
	// so the avg counts what was generated up to the interrupt; don't drop it here.
	m.finalizeTurn(outcomeStopped)
	m.endTurn() // drops pending tool calls along with the rest of the turn state
}

// finalizeTurn freezes the finished turn's wall clock summary into the status
// bar (shown at idle until the next submit) and logs the totals. outcome is
// the finish glyph: ✓ clean, ✗ abort/stall. The bar's avg tok/s divides
// lastTokens by lastElapsed (wall clock), so it stays self verifying against
// the duration shown right beside it. There is no scrollback banner: the footer
// owns the run summary, and the precise wall time lands in the turn_end log.
// Common to every wind down (handleStreamClosed and abortTurn both call it).
func (m *Model) finalizeTurn(outcome turnOutcome) {
	if m.turnStart.IsZero() {
		return // defensive: finalizeTurn only runs inside a turn beginTurn started
	}
	// Commit the in flight round's live estimate before measuring. On a clean
	// finish it's already 0 (applyDone folded the round into turnTokens at
	// EventDone). But a Ctrl+C or error mid stream interrupts before EventDone,
	// so the cancelled round's tokens (often the whole generation) sit only in
	// streamingEstimate; without this they'd vanish from the avg and the session
	// total would drop backward. bytes/4 is the best count for a round that never
	// reported usage.
	m.turnTokens += m.streamingEstimate
	m.sessionTokens += m.streamingEstimate
	m.streamingEstimate = 0
	wall := time.Since(m.turnStart)
	m.lastElapsed = wall
	m.lastTokens = m.turnTokens
	m.lastOutcome = outcome
	avg := humanRate(m.turnTokens, wall)
	if avg != "" {
		avg = " · " + avg + " avg"
	}
	dbgWritef("turn_end", "%s · %s wall%s · session_total=%s",
		humanTokens(m.turnTokens), wall.Round(time.Millisecond), avg, humanTokens(m.sessionTokens))
	m.turnStart = time.Time{}
	m.turnTokens = 0
}

// handleStreamClosed drives what happens after one round's stream finishes:
// dispatch the next pending tool call, or, if none, finalize the turn and
// hand control back. A turn ends precisely when the assistant emits no tool
// calls; there is no loop tool to land on.
func (m Model) handleStreamClosed() (tea.Model, tea.Cmd) {
	m.stream = nil
	// Stale close from a cancelled turn (handleCtrlC / EventError reset phase
	// to idle).
	if !m.phase.active() {
		return m, nil
	}
	if len(m.pending) > 0 {
		// The model issued a tool call, genuine progress. Rearm the empty reply
		// latch so a LATER transient empty on this same (long) turn earns its own
		// reprompt instead of hitting the leak and die branch below. The latch
		// exists to stop a server that deterministically swallows EVERY call, not
		// to cap recoveries on a turn that keeps advancing: a flaky stream that
		// drops the occasional call must not abandon a half built file (the galaxy1
		// failure: empty → nudge → recovered with a write → empty again → died).
		// Two CONSECUTIVE empties still terminate: pending is 0 on that path, so
		// this never rearms there.
		m.emptyNudged = false
		return m.dispatchNextTool()
	}
	// The turn is ending with no tool calls. If the model said nothing and called
	// nothing, it either stopped mid task or its tool call was swallowed (a
	// thinking model streams the call into the reasoning channel, which never
	// reaches us as content or a structured call). Reprompt once to reissue or
	// finish; the emptyNudged latch bounds it to a single retry so a server that
	// deterministically swallows the call can't loop. If it persists, surface it
	// rather than dying silently: the prior behaviour left a half done artifact
	// with no banner at all.
	outcome := outcomeDone // a clean, nonempty finish; stall/leak below downgrade it
	if newestAssistantEmpty(m.history) {
		if !m.emptyNudged {
			m.emptyNudged = true
			dbgWritef("nudge", "empty reply nudge injected (turn ended with no content and no tool call)")
			m.history = append(m.history, chmctx.Message{
				Role:    chmctx.RoleSystem,
				Content: nudgeOrigin + "Your last turn ended with no reply and no tool call. If you meant to call a tool and it did not run, issue it again now as a proper tool call. If you are still working, continue. If the task is done, check it against the original request: actually run or drive what proves it works: then reply with a one line summary.",
			})
			m.phase = phaseThinking
			return m, m.startChat()
		}
		m.appendLine(styleError.Render("⚠ the model ended its turn with no reply and no tool call: it stalled, or your server dropped the call. If thinking is on, its reasoning parser may be swallowing calls: enable one (e.g. vLLM `--reasoning-parser`) or disable thinking for tool turns."))
		dbgWritef("leak", "turn ended with an empty assistant message after a reprompt (model stalled or the call was swallowed server side)")
		outcome = outcomeStopped
	} else if w := toolCallLeakWarning(m.history); w != "" {
		// The model meant to call a tool but its server's parser leaked the raw
		// call into the reply text instead. The fix is server side.
		m.appendLine(w)
		dbgWritef("leak", "turn ended with tool call text leaked into the reply (server side parser misconfigured)")
		outcome = outcomeStopped
	} else if m.maybeVerifyNudge() {
		// A substantial turn is finishing with a clean, nonempty summary. Re ground
		// it once to the original request and let the model verify (or honestly mark
		// unverified) before it hands control back. Mirrors the empty reply reprompt:
		// applyDone already flushed this summary to scrollback and appended it to
		// history, so the streaming buffer is clean and startChat resumes safely.
		m.phase = phaseThinking
		return m, m.startChat()
	}
	m.finalizeTurn(outcome)
	m.endTurn()
	return m.fireQueued()
}

// fireQueued auto submits a prompt the user queued mid turn, once the turn has
// wound down to idle. Reached only from the natural finish path (here, after
// finalizeTurn/endTurn); a Ctrl+C or stream error abort routes through abortTurn
// and never gets here, so an interrupt leaves the slot for a manual send. No operation
// when nothing is queued. The expanded send goes to the LLM, the collapsed echo
// to scrollback, exactly as a typed submit; the recall entry carries the expanded
// text (no chip), matching disk loaded history.
func (m Model) fireQueued() (tea.Model, tea.Cmd) {
	if m.queued == nil {
		return m, nil
	}
	q := m.queued
	m.queued = nil
	return m.submit(q.send, q.echo, promptEntry{display: q.send})
}

// newestAssistant returns the newest assistant role message in history: the
// turn's final reply, which all three finish checks below inspect.
func newestAssistant(history []chmctx.Message) (chmctx.Message, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == chmctx.RoleAssistant {
			return history[i], true
		}
	}
	return chmctx.Message{}, false
}

// newestAssistantEmpty reports whether the turn's final assistant message
// carried neither text nor a structured tool call. A clean finish always has a
// summary and a continuing turn always has a tool call, so an empty newest
// assistant message is always an anomaly: the model stopped mid task, or its
// call streamed into the reasoning channel and was dropped before reaching us.
func newestAssistantEmpty(history []chmctx.Message) bool {
	msg, ok := newestAssistant(history)
	return ok && strings.TrimSpace(msg.Content) == "" && len(msg.ToolCalls) == 0
}

// newestAssistantUnverified reports whether the turn's final assistant message
// already carries an "unverified" marker, the honest self assessment the finish
// nudge exists to elicit. Case insensitive: the model writes "unverified" /
// "Unverified" interchangeably. Used to suppress the finish nudge on a finish
// that already named what it couldn't prove (see maybeVerifyNudge).
func newestAssistantUnverified(history []chmctx.Message) bool {
	msg, ok := newestAssistant(history)
	return ok && strings.Contains(strings.ToLower(msg.Content), "unverified")
}

// toolCallLeakWarning returns a user facing diagnostic when the newest assistant
// message carries a tool call opener (`<tool_call>`) in its text instead of
// structured tool_calls, the dominant local hosting failure: a
// misconfigured/missing server parser leaks the call as output text and the
// response completes normally, so the turn ends silently with the tool intent
// stranded.
// The bare `<tool_call>` opener covers both shapes the target servers emit: the
// XML body (`<function=…`) and the general JSON body
// (`{"name":…`): gating on the literal tag alone catches both while staying
// specific enough that ordinary prose can't trip it. A message that carried a
// real structured call never leaked, even if its prose quotes the tag, so a
// nonempty ToolCalls short circuits to clean. codehamr stays wire only (it does
// not parse or run the leaked call); it points the user at the server side fix.
// Empty string when there is nothing to warn.
func toolCallLeakWarning(history []chmctx.Message) string {
	msg, ok := newestAssistant(history)
	if !ok || len(msg.ToolCalls) > 0 {
		return "" // no assistant yet, or it called a tool properly; the prose tag is incidental
	}
	if strings.Contains(msg.Content, "<tool_call>") {
		return styleError.Render("⚠ a tool call leaked into the reply as text instead of running: your model server isn't parsing tool calls. Enable its OpenAI tool call parser server side (e.g. vLLM `--tool-call-parser`, llama.cpp `--jinja`).")
	}
	return "" // newest assistant message is clean
}

// dispatchNextTool pops the next pending tool call and runs it. Every tool
// flows through runToolCall; none are special cased. lastToolKey records this
// call's target so the failure nudge can tell when the model keeps retrying the
// same failing operation (see recordToolOutcome).
func (m Model) dispatchNextTool() (tea.Model, tea.Cmd) {
	call := m.pending[0]
	m.pending = m.pending[1:]
	m.appendLine(styleDim.Render(tools.InlineStatus(call)))
	m.lastToolKey = toolTargetKey(call)
	m.toolRounds++
	if call.Name != tools.ReadFileName {
		m.turnActed = true
	}
	m.phase = phaseRunning
	return m, runToolCall(m.turnCtx, call)
}

// nudgeOrigin prefixes every deterministic backstop note. A weak (30B) model
// reads a bare mid turn system message as an empty/absent user turn ("the user
// hasn't given me a new task, I'll just stop"), the exact misread that turned the
// finish nudge net negative on the galaxy run (it reprompted an honest
// `unverified` finish into a confident, caveat free "it works"). Naming the note
// as codehamr's own automated check, not the user's, keeps the model oriented.
// Deliberately says nothing about whether to stop or keep going (each nudge body
// owns that), so it can't induce the premature completion failure the runaway /
// verify wording fights.
const nudgeOrigin = "[Automated codehamr check: not a message from your user.] "

// maxToolFailStreak is how many consecutive same target failures trigger the
// nudge. Generous on purpose: a model iterating on a hard edit gets several
// attempts before being told it's stuck; catches genuine loops without
// interrupting honest trial and error.
const maxToolFailStreak = 5

// toolTargetKey is the stable identity used to detect a repeated failure loop:
// tool name + its target (the path for file tools, the command's first line for
// bash). Deliberately NOT the full argument set: a full args key is defeated by
// any cosmetic change between retries (a regenerated file body, a reworded
// command). Keying on the target catches a model hammering the same operation
// while leaving varied exploration alone.
func toolTargetKey(call chmctx.ToolCall) string {
	switch call.Name {
	case tools.WriteFileName, tools.EditFileName, tools.ReadFileName:
		path, _ := call.Arguments["path"].(string)
		return call.Name + "|" + path
	case tools.BashName:
		cmd, _ := call.Arguments["cmd"].(string)
		if i := strings.IndexByte(cmd, '\n'); i >= 0 {
			cmd = cmd[:i]
		}
		return call.Name + "|" + strings.TrimSpace(cmd)
	}
	return call.Name
}

// toolResultFailed reports whether a tool result is an error the model should
// react to. File tools wrap errors in parens ("(write error: ...)", "(not
// found: ...)") and report success as plain text ("wrote N bytes"); bash
// appends "(exit: N)" / "(timeout after ...)" on failure. A user Ctrl+C
// ("(cancelled)") never counts as a failure.
func toolResultFailed(name, result string) bool {
	if strings.Contains(result, "(cancelled)") {
		return false
	}
	// Router level failures arrive under any tool name and bypass the per tool
	// shapes below: truncated/invalid JSON args (the failure that makes a model
	// reemit the same too large write for minutes) and a hallucinated tool
	// name. Both must count as failures or the repeated failure nudge never
	// fires on exactly the loops it was built for.
	t := strings.TrimSpace(result)
	if strings.HasPrefix(t, "(tool arguments were not valid JSON") || strings.HasPrefix(t, "(unknown tool:") {
		return true
	}
	switch name {
	case tools.WriteFileName, tools.EditFileName:
		// write/edit report success as plain text ("wrote N bytes", "edited …")
		// and every error in parens, so a leading "(" is the failure signal.
		return strings.HasPrefix(t, "(")
	case tools.ReadFileName:
		// read_file returns the file's RAW content on success, which can
		// legitimately start with "(" (Lisp, S expressions, a leading paren
		// expr). Match only its two real failure outputs so a successful read
		// isn't counted as a failure and made to feed the repeated failure nudge.
		return strings.HasPrefix(t, "(read error:") || t == "(empty path)"
	case tools.BashName:
		// "(empty command)" is bash's malformed call outcome (missing/blank
		// cmd), exact matched like read_file's "(empty path)": successful bash
		// output can legitimately start with "(", so no prefix match here. It
		// must count as a failure or a model looping on empty calls never
		// builds a streak and slips past the backstop to the runaway cap.
		return strings.Contains(result, "\n(exit: ") || strings.Contains(result, "(timeout after ") ||
			t == "(empty command)"
	}
	return false
}

// recordToolOutcome updates the failure streak from one finished tool result.
// Any success EXCEPT a read resets the streak; a same target failure extends
// it. lastToolKey was stamped in dispatchNextTool for the call this result
// belongs to.
func (m *Model) recordToolOutcome(name, content string) {
	if !toolResultFailed(name, content) {
		// A successful READ clears nothing: reading a file changes no state, so
		// pairing a failing edit_file with a succeeding read_file of the same
		// file: the most natural batch there is: must not reset the streak, or
		// the loop breaker never fires and the first supervision arrives at the
		// runaway cap. Every other success IS progress and resets it, including
		// on a different target: a healthy build loop batches a succeeding edit
		// with a still failing `go test`, and letting that build a streak hands
		// the model a stop shaped note five errors into ten: the premature stop
		// failure, arrived at from the other direction.
		if name != tools.ReadFileName {
			m.failKey, m.failStreak = "", 0
		}
		return
	}
	if m.lastToolKey == m.failKey && m.failKey != "" {
		m.failStreak++
	} else {
		m.failKey = m.lastToolKey
		m.failStreak = 1
	}
	// Log only failures: a success leaves the streak at 0 and is already visible
	// as a tool_result. The climbing streak is the nudge machinery's state, the
	// part the per message records can't show.
	dbgWritef("tool_outcome", "tool=%s FAILED · same target streak=%d/%d · key=%s", name, m.failStreak, maxToolFailStreak, m.failKey)
}

// maybeFailureNudge appends one system role note once the same target has
// failed maxToolFailStreak times running, then resets the streak so it fires at
// most once per run of failures. A nudge, not a yield: the model stays in
// control and decides whether to pivot or stop and tell the user.
func (m *Model) maybeFailureNudge() {
	if m.failStreak < maxToolFailStreak {
		return
	}
	dbgWritef("nudge", "repeated failure nudge injected after %d same target failures (key=%s)", m.failStreak, m.failKey)
	m.history = append(m.history, chmctx.Message{
		Role: chmctx.RoleSystem,
		Content: nudgeOrigin + fmt.Sprintf(
			"The last %d tool calls to the same target failed the same way. Stop repeating it: read the error, change your approach, or tell the user what's blocking you.",
			m.failStreak),
	})
	m.failKey, m.failStreak = "", 0
}

// maxLLMRounds caps assistant round trips per turn before the runaway
// self check fires, and runawayNudgeInterval is how often it re fires past that
// cap. Counted in round trips rather than tool calls because batching makes a
// call count meaningless: an honest large build is well under 60 round trips,
// a genuine runaway sails past it.
const (
	// 40 round trips is ~20 minutes of model time. The old cap was 75 tool
	// calls, which equalled 75 round trips only because nothing batched; at a
	// realistic 3 calls per message 60 would have let ~180 calls run before the
	// first word, three times looser than the cap it replaced.
	maxLLMRounds         = 40
	runawayNudgeInterval = 25
)

// maybeRunawayNudge appends one soft system note when a turn crosses
// maxLLMRounds round trips without finishing, then again every
// runawayNudgeInterval rounds after that. Comparing against lastRunawayRound
// rather than latching keeps supervision alive for the rest of a long turn:
// under the old once per turn latch, everything past the cap ran unwatched. The
// gap test (not equality) survives a check being skipped, since this is only
// consulted when the pending queue drains. Framed as a self check, not a stop order:
// telling a 30B to "stop" mid task is the premature completion failure we
// otherwise fight, so the model decides whether it is still converging.
func (m *Model) maybeRunawayNudge() {
	if m.llmRounds < maxLLMRounds || m.llmRounds-m.lastRunawayRound < runawayNudgeInterval {
		return
	}
	m.lastRunawayRound = m.llmRounds
	dbgWritef("nudge", "runaway iteration nudge injected at %d round trips this turn", m.llmRounds)
	m.history = append(m.history, chmctx.Message{
		Role: chmctx.RoleSystem,
		Content: nudgeOrigin + fmt.Sprintf(
			"%d round trips so far this turn without finishing. If you're still making real progress, keep going: reread the original request in this conversation and state in one line what remains, then continue. If you're repeating a step that can't work here: a blocked install, a missing tool, a path failing the same way: stop chasing it (that loop burns the turn); verify another way. If you're stuck or unsure you're converging, tell the user where things stand and what's blocking you.",
			m.llmRounds),
	})
}

// verifyNudgeMinRounds is how many Round trips a turn must have taken before the
// finish re grounding nudge can fire. Counted in round trips for the same reason
// maxLLMRounds is: batching makes a call count meaningless, and a "fix the typo"
// turn that batches six reads then edits and builds would clear a call based
// gate of 8 in two round trips: taxing exactly the quick turns this is meant to
// skip. Set so only a turn that did real, multi step work trips it; the galaxy
// runs that shipped broken but claimed done artifacts each took dozens.
//
// verifyNudgeMinCalls is the second gate, in CALLS, closing the hole the
// round trip gate opens: the same batching that inflates call counts also
// COMPRESSES a substantial build and claim turn below 8 round trips (five
// rounds of three calls each is real work with a real false green risk), and
// that turn would otherwise skip re grounding entirely. Either measure of
// substantial work trips the nudge; a quick turn clears both.
const (
	verifyNudgeMinRounds = 8
	verifyNudgeMinCalls  = 15
)

// maybeVerifyNudge appends one re grounding system note when a substantial turn
// (>= verifyNudgeMinRounds round trips, or >= verifyNudgeMinCalls tool calls) is
// about to finish with a clean, nonempty
// reply, then latches so it fires at most once per turn. Returns true when it
// nudged, so the caller reprompts and the model can verify before its final
// summary. The false green finish (a confident summary for an artifact that was
// never actually run) is invisible to the other three backstops, which only see
// repeated failures, runaway counts, or an empty reply. Framed as re grounding +
// honest verification, never a stop order: telling a 30B to "stop" mid task is the
// premature completion failure we otherwise fight.
func (m *Model) maybeVerifyNudge() bool {
	if m.verifyNudged || (m.llmRounds < verifyNudgeMinRounds && m.toolRounds < verifyNudgeMinCalls) {
		return false
	}
	// A turn that only read has no artifact to falsely call green; re grounding
	// a code reading answer is a wasted round trip.
	if !m.turnActed {
		return false
	}
	// The nudge targets the false green finish: a confident summary for work that
	// was never run. A finish that already marks something `unverified` has done
	// exactly the honest self assessment the nudge would ask for; it is the
	// OPPOSITE of a false green. Reprompting it is a wasted round at best, and on
	// a weak model a regression: the galaxy run's honest "unverified: browser
	// runtime" got reprompted into a confident, caveat free "it works". Let an
	// honest finish stand. A true false green carries no such marker, so it still
	// gets nudged.
	if newestAssistantUnverified(m.history) {
		return false
	}
	m.verifyNudged = true
	dbgWritef("nudge", "finish re grounding nudge injected at %d round trips this turn", m.llmRounds)
	m.history = append(m.history, chmctx.Message{
		Role:    chmctx.RoleSystem,
		Content: nudgeOrigin + "Before you finish: walk the original request part by part. For each part, name the check you actually ran and what it showed. Anything runnable is proven only by running it: build it, run the test, execute it, or load the page and drive the interaction. Fix what breaks and rerun. If a check genuinely could not run here, write `unverified: <what>: <why>` and lead with it instead of a confident \"works\". Then reply with your summary.",
	})
	return true
}

// cursorOnFirstLine: true when ↑ should walk prompt history instead of moving
// the textarea's own cursor. cursorOnLastLine is the mirror for ↓.
func (m Model) cursorOnFirstLine() bool { return m.ta.Line() == 0 }
func (m Model) cursorOnLastLine() bool  { return m.ta.Line() == m.ta.LineCount()-1 }

// buildSystem appends the working directory anchor to the embedded system
// prompt so "hier" / "here" resolves to a concrete path.
func buildSystem(projectDir string) string {
	return config.DefaultSystemPrompt + "\n\nWorking directory: " + projectDir
}

// pingBackend issues a short GET to baseURL/v1/models via llm.Reachable. Any
// HTTP response counts as reachable; transport errors and timeouts mean
// disconnected. The result carries the URL it was issued against so Update can
// drop late results arriving after a /models switch.
func pingBackend(baseURL string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
		defer cancel()
		return pingMsg{ok: llm.Reachable(ctx, baseURL) == nil, baseURL: baseURL}
	}
}
