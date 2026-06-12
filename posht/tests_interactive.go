package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// --- mouse reporting ---------------------------------------------------------

type mouseModel struct {
	events int
	wheel  int
	log    []string
}

func (m *mouseModel) Init() tea.Cmd { return tea.EnableMouseAllMotion }

func (m *mouseModel) Update(msg tea.Msg) (TestModel, tea.Cmd) {
	if mouse, ok := msg.(tea.MouseMsg); ok {
		m.events++
		ev := tea.MouseEvent(mouse)
		if ev.Button == tea.MouseButtonWheelUp || ev.Button == tea.MouseButtonWheelDown {
			m.wheel++
		}
		entry := fmt.Sprintf("%-24s at col %d, row %d", ev.String(), ev.X+1, ev.Y+1)
		m.log = append(m.log, entry)
		if len(m.log) > 5 {
			m.log = m.log[len(m.log)-5:]
		}
	}
	return m, nil
}

func (m *mouseModel) View(int) string {
	var b strings.Builder
	b.WriteString("  Mouse reporting is on (all-motion + SGR encoding).\n" +
		"  Click, drag, and scroll the wheel anywhere — events and their\n" +
		"  coordinates must appear below and track your pointer.\n\n")
	if m.events == 0 {
		b.WriteString("  waiting for mouse input…\n")
	} else {
		fmt.Fprintf(&b, "  %d events (%d wheel) — last 5:\n", m.events, m.wheel)
		for _, l := range m.log {
			b.WriteString("    " + l + "\n")
		}
	}
	return b.String()
}

func (m *mouseModel) Cleanup() tea.Cmd { return tea.DisableMouse }

// --- alternate scroll (DECSET 1007) ------------------------------------------

// altScrollModel isolates what governs the wheel→arrow-keys translation on
// the alternate screen — the "wheel becomes arrow up/down" symptom (posh#3/
// #28). It walks four terminal states; in each you scroll the wheel, then
// report WHICH counter moved — a (arrow), m (mouse), or 0 (nothing). The test
// compares your observation to its own expectation and computes pass/fail, so
// you never have to do that mapping yourself (the source of earlier
// misreads). It also auto-counts events independently and records whether your
// report agrees with that tally, flagging perception/reality mismatches.
//
// expectKind names the counter expected to move in each state.
type expectKind int

const (
	expectArrow expectKind = iota // wheel should arrive as arrow up/down
	expectMouse                   // wheel should arrive as SGR mouse events
	expectNone                    // wheel should produce nothing
)

// name is the short token used both in the JSON receipt and to compare an
// expectation against a reported observation.
func (e expectKind) name() string {
	switch e {
	case expectArrow:
		return "arrow"
	case expectMouse:
		return "mouse"
	default:
		return "none"
	}
}

type altScrollState struct {
	name   string
	label  string     // human label for the state
	enter  string     // escape sequence asserting this state
	expect expectKind // which counter is expected to move

	Arrow    int    `json:"arrow"`    // arrow up/down events seen in this state
	Mouse    int    `json:"mouse"`    // SGR mouse events seen in this state
	Other    int    `json:"other"`    // anything else (excluding the report keys)
	Observed string `json:"observed"` // what you reported seeing: arrow/mouse/none
	Verdict  string `json:"verdict"`  // pass/fail, computed: observed vs expected
}

type altScrollModel struct {
	states []*altScrollState
	idx    int
	done   bool
}

func newAltScrollModel() *altScrollModel {
	return &altScrollModel{states: []*altScrollState{
		{name: "1007-off", label: "1007 OFF (\x1b[?1007l)", enter: "\x1b[?1007l",
			expect: expectNone}, // posh's current approach: silence the wheel
		{name: "1007-on", label: "1007 ON (\x1b[?1007h)", enter: "\x1b[?1007h",
			expect: expectArrow}, // xterm/iTerm2 alternate-scroll translation
		{name: "mouse-grab", label: "mouse grab (\x1b[?1000h\x1b[?1006h)",
			enter: "\x1b[?1000h\x1b[?1006h", expect: expectMouse}, // candidate fix
		{name: "all-off", label: "all off (\x1b[?1007l\x1b[?1000l\x1b[?1006l)",
			enter: "\x1b[?1007l\x1b[?1000l\x1b[?1006l", expect: expectNone}, // bare default
	}}
}

// Capture input so wheel-generated arrows land here (and the per-state y/n
// keys reach us) instead of driving the chrome's verdict/navigation.
func (m *altScrollModel) Capturing() bool { return !m.done }

func (m *altScrollModel) cur() *altScrollState { return m.states[m.idx] }

// assert emits the current state's escape sequence. It rides a tea.Cmd (like
// the cursor test's shape writes) so the bytes reach the terminal once.
func (m *altScrollModel) assert() tea.Cmd {
	seq := m.cur().enter
	return func() tea.Msg {
		fmt.Print(seq)
		return nil
	}
}

func (m *altScrollModel) Init() tea.Cmd { return m.assert() }

// record stores your reported observation for the current state, computes the
// verdict (observation vs expectation), and advances. After the last state the
// test is done and the chrome takes the overall y/n/s. The receipt also emits
// the raw arrow/mouse tallies, so a reader can independently check your report
// against what posht auto-counted.
func (m *altScrollModel) record(observed expectKind) tea.Cmd {
	s := m.cur()
	s.Observed = observed.name()
	if observed == s.expect {
		s.Verdict = "pass"
	} else {
		s.Verdict = "fail"
	}
	if m.idx+1 >= len(m.states) {
		m.done = true
		return nil
	}
	m.idx++
	return m.assert()
}

func (m *altScrollModel) Update(msg tea.Msg) (TestModel, tea.Cmd) {
	if m.done {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.MouseMsg:
		m.cur().Mouse++
		return m, nil
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyUp, tea.KeyDown:
			m.cur().Arrow++
			return m, nil
		}
		// Report keys: which counter did YOU see move? The test judges.
		switch msg.String() {
		case "a":
			return m, m.record(expectArrow)
		case "m":
			return m, m.record(expectMouse)
		case "0":
			return m, m.record(expectNone)
		}
		// Any other key while scrolling is noise we still want to see.
		m.cur().Other++
	}
	return m, nil
}

func (m *altScrollModel) View(int) string {
	var b strings.Builder
	s := m.cur()
	fmt.Fprintf(&b, "  State %d of %d — %s\n\n", m.idx+1, len(m.states), s.label)
	b.WriteString("  Scroll the wheel up/down over this screen, then report what\n" +
		"  you saw happen. Live counts (auto-detected) below:\n\n")

	fmt.Fprintf(&b, "      [a] arrow up/down: %d\n", s.Arrow)
	fmt.Fprintf(&b, "      [m] mouse events:  %d\n", s.Mouse)
	fmt.Fprintf(&b, "          other input:   %d\n", s.Other)

	b.WriteString("\n  Which did you observe? Press the key:\n" +
		"    a = the cursor/screen moved as ARROW keys\n" +
		"    m = mouse events (coordinates) registered\n" +
		"    0 = nothing happened (wheel silent)\n\n" +
		"  The test scores it for you (you report, it judges) and advances.\n")
	if m.done {
		b.WriteString("\n  all four states recorded — press y/n/s for the overall verdict.\n")
	}
	return b.String()
}

// Report contributes per-state expectation, raw auto-counted tally, your
// observation, and the computed verdict to the JSON receipt. Report and tally
// are both recorded so a reader can cross-check perception against what posht
// actually counted.
func (m *altScrollModel) Report() any {
	out := make(map[string]any, len(m.states))
	for _, s := range m.states {
		out[s.name] = map[string]any{
			"expected": s.expect.name(),
			"arrow":    s.Arrow,
			"mouse":    s.Mouse,
			"other":    s.Other,
			"observed": s.Observed,
			"verdict":  s.Verdict,
		}
	}
	return out
}

// Cleanup resets every mode this test touched so nothing leaks past it.
func (m *altScrollModel) Cleanup() tea.Cmd {
	return func() tea.Msg {
		fmt.Print("\x1b[?1007l\x1b[?1000l\x1b[?1006l")
		return nil
	}
}

// --- keyboard input ----------------------------------------------------------

type keysModel struct {
	log  []string
	done bool
}

func (m *keysModel) Capturing() bool { return !m.done }

func (m *keysModel) Init() tea.Cmd { return nil }

func (m *keysModel) Update(msg tea.Msg) (TestModel, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	if m.done {
		return m, nil
	}
	if key.Type == tea.KeyEsc {
		m.done = true
		return m, nil
	}
	m.log = append(m.log, key.String())
	if len(m.log) > 8 {
		m.log = m.log[len(m.log)-8:]
	}
	return m, nil
}

func (m *keysModel) View(int) string {
	var b strings.Builder
	b.WriteString("  Type keys with modifiers — ctrl+arrows, alt+letters,\n" +
		"  shift+tab, F-keys, home/end/pgup — and check each echoes back\n" +
		"  as the key you pressed (not garbage, not a plain variant).\n\n")
	if m.done {
		b.WriteString("  capture finished — judge the log, then y/n/s.\n\n")
	}
	if len(m.log) == 0 {
		b.WriteString("  waiting for keys…\n")
	} else {
		for _, l := range m.log {
			b.WriteString("    " + l + "\n")
		}
	}
	return b.String()
}

// --- bracketed paste ---------------------------------------------------------

type pasteModel struct {
	pastes []int // rune counts of received paste events
	loose  int   // non-paste keys received while capturing
	done   bool
}

func (m *pasteModel) Capturing() bool { return !m.done }

func (m *pasteModel) Init() tea.Cmd { return nil }

func (m *pasteModel) Update(msg tea.Msg) (TestModel, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || m.done {
		return m, nil
	}
	switch {
	case key.Paste:
		m.pastes = append(m.pastes, len(key.Runes))
	case key.Type == tea.KeyEsc:
		m.done = true
	default:
		m.loose++
	}
	return m, nil
}

func (m *pasteModel) View(int) string {
	var b strings.Builder
	b.WriteString("  Paste some multi-character text now (clipboard paste, not\n" +
		"  typing). With bracketed paste (mode 2004) working, the whole\n" +
		"  paste arrives as ONE atomic event:\n\n")
	for i, n := range m.pastes {
		fmt.Fprintf(&b, "    paste %d: one event, %d characters ✓\n", i+1, n)
	}
	if m.loose > 0 {
		fmt.Fprintf(&b, "    %d loose keystrokes — if these came from your paste,\n"+
			"    bracketed paste is BROKEN (that's a fail)\n", m.loose)
	}
	if len(m.pastes) == 0 && m.loose == 0 {
		b.WriteString("    waiting for a paste…\n")
	}
	return b.String()
}

// --- window resize -----------------------------------------------------------

type resizeModel struct {
	w, h    int
	changes int
}

func (m *resizeModel) Init() tea.Cmd { return nil }

func (m *resizeModel) Update(msg tea.Msg) (TestModel, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		if m.w != 0 {
			m.changes++
		}
		m.w, m.h = size.Width, size.Height
	}
	return m, nil
}

func (m *resizeModel) View(w int) string {
	cur := fmt.Sprintf("%d × %d", m.w, m.h)
	if m.w == 0 {
		cur = fmt.Sprintf("%d × ? (no size event yet)", w)
	}
	ruler := "  ├" + strings.Repeat("─", max(0, w-6)) + "┤"
	return fmt.Sprintf("  Resize your terminal window. The size below must track it\n"+
		"  live, and the ruler must always span the full width:\n\n"+
		"      current size: %s   (%d resize events)\n\n%s\n",
		cur, m.changes, ruler)
}
