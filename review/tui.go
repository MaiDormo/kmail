package review

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The Bubble Tea front-end. It edits the same *Plan the decision file edits and produces nothing
// else, so the two can be — and are — tested for identical output.
//
// Colours are the ANSI 0-15 palette on purpose: those resolve against whatever theme the terminal
// is set to, so the same code reads correctly on a light background and a dark one without any
// adaptive logic. Nothing here paints a full-width background; a reverse-video bar looks cheap and
// fights every colour scheme it lands in.

type pane int

const (
	paneShapes pane = iota
	paneNames
	paneSummary
)

type model struct {
	plan      *Plan
	pane      pane
	cursor    [3]int
	top       [3]int
	filter    string
	filtering bool
	w, h      int
	approved  bool
	done      bool
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	brandStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	metaStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	ruleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	keepMark  = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("✓")
	blankMark = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render("·")
	dropMark  = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("✗")
	warnMark  = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render("⚠")

	gutterOn  = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Render("▸")
	rowSelect = lipgloss.NewStyle().Bold(true)
	rowDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	whyStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	countCol  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	keyStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	helpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	sampleBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("8")).
			Padding(0, 1)
	quoteStyle = lipgloss.NewStyle().Italic(true)
	greetStyle = lipgloss.NewStyle().Bold(true)
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
)

func (m model) Init() tea.Cmd { return nil }

// visibleNames is the name list after the filter, as indices into plan.Names.
func (m model) visibleNames() []int {
	var out []int
	f := strings.ToLower(m.filter)
	for i, n := range m.plan.Names {
		if f == "" || strings.Contains(strings.ToLower(n.Name), f) || strings.Contains(strings.ToLower(n.Why), f) {
			out = append(out, i)
		}
	}
	return out
}

func (m *model) clampCursor(n int) {
	p := int(m.pane)
	if n == 0 {
		m.cursor[p], m.top[p] = 0, 0
		return
	}
	if m.cursor[p] < 0 {
		m.cursor[p] = 0
	}
	if m.cursor[p] >= n {
		m.cursor[p] = n - 1
	}
	rows := m.rows()
	if m.cursor[p] < m.top[p] {
		m.top[p] = m.cursor[p]
	}
	if m.cursor[p] >= m.top[p]+rows {
		m.top[p] = m.cursor[p] - rows + 1
	}
	if m.top[p] < 0 {
		m.top[p] = 0
	}
}

// rows is how many list lines fit, leaving room for the header, the help line and — on the shapes
// pane — the sample box.
func (m model) rows() int {
	r := m.h - 7
	if m.pane == paneShapes {
		r = m.h - 14
	}
	if r < 1 {
		return 1
	}
	return r
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := msg.String()
	text := msg.Text

	// the filter box swallows typing until it is closed
	if m.filtering {
		switch s {
		case "esc":
			m.filter, m.filtering = "", false
		case "enter":
			m.filtering = false
		case "backspace":
			if m.filter != "" {
				m.filter = m.filter[:len(m.filter)-1]
			}
		default:
			if text != "" {
				m.filter += text
			}
		}
		m.clampCursor(len(m.visibleNames()))
		return m, nil
	}

	switch s {
	case "ctrl+c", "q", "esc":
		m.done = true
		return m, tea.Quit
	}

	switch m.pane {
	case paneShapes:
		switch {
		case s == "down" || text == "j":
			m.cursor[0]++
		case s == "up" || text == "k":
			m.cursor[0]--
		case text == " ":
			sh := &m.plan.Shapes[m.cursor[0]]
			if sh.State == Keep {
				sh.State = Kill
			} else {
				sh.State = Keep
			}
		case s == "tab" || s == "enter":
			m.pane = paneNames
		}
		m.clampCursor(len(m.plan.Shapes))

	case paneNames:
		vis := m.visibleNames()
		switch {
		case s == "down" || text == "j":
			m.cursor[1]++
		case s == "up" || text == "k":
			m.cursor[1]--
		case s == "pgdown":
			m.cursor[1] += m.rows()
		case s == "pgup":
			m.cursor[1] -= m.rows()
		case text == "/":
			m.filtering = true
		case text == "a":
			m.plan.BlankAllFlagged()
		case len(vis) > 0 && text == " ":
			m.plan.Names[vis[m.cursor[1]]].State = Blank
			m.cursor[1]++
		case len(vis) > 0 && text == "d":
			m.plan.Names[vis[m.cursor[1]]].State = Drop
			m.cursor[1]++
		case len(vis) > 0 && s == "enter":
			m.plan.Names[vis[m.cursor[1]]].State = Keep
			m.cursor[1]++
		case s == "shift+tab":
			m.pane = paneShapes
		case s == "tab":
			m.pane = paneSummary
		}
		m.clampCursor(len(m.visibleNames()))

	case paneSummary:
		switch {
		case text == "A":
			m.approved, m.done = true, true
			return m, tea.Quit
		case s == "shift+tab" || s == "tab":
			m.pane = paneNames
		}
	}
	return m, nil
}

func (m model) View() tea.View {
	if m.w == 0 {
		return tea.NewView("")
	}
	switch m.pane {
	case paneShapes:
		return tea.NewView(m.viewShapes())
	case paneNames:
		return tea.NewView(m.viewNames())
	default:
		return tea.NewView(m.viewSummary())
	}
}

// header is the same two lines on every pane: what this is on the left, where you are on the right.
func (m model) header(right string) string {
	left := brandStyle.Render("kairos") + titleStyle.Render(" outreach review")
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(right) - 4
	if gap < 1 {
		gap = 1
	}
	rule := strings.Repeat("─", max(4, m.w-4))
	return "  " + left + strings.Repeat(" ", gap) + metaStyle.Render(right) + "\n" +
		"  " + ruleStyle.Render(rule) + "\n"
}

func help(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, keyStyle.Render(pairs[i])+" "+helpStyle.Render(pairs[i+1]))
	}
	return "  " + strings.Join(parts, helpStyle.Render(" · "))
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%4d %s ", n, word)
	}
	return fmt.Sprintf("%4d %ss", n, word)
}

func mark(v Verb) string {
	switch v {
	case Kill, Drop:
		return dropMark
	case Blank:
		return blankMark
	}
	return keepMark
}

// scrollHint tells you there is more above or below without drawing a scrollbar.
func scrollHint(top, shown, total int) string {
	if total <= shown {
		return ""
	}
	switch {
	case top == 0:
		return "▼ more"
	case top+shown >= total:
		return "▲ more"
	}
	return "▲▼ more"
}

func (m model) viewShapes() string {
	var b strings.Builder
	kept := 0
	for _, s := range m.plan.Shapes {
		if s.State == Keep {
			kept++
		}
	}
	b.WriteString(m.header(fmt.Sprintf("shapes %d/%d · keeping %d",
		m.cursor[0]+1, len(m.plan.Shapes), kept)))
	b.WriteString("\n")

	rows := m.rows()
	shown := 0
	for i := m.top[0]; i < len(m.plan.Shapes) && shown < rows; i++ {
		sh := m.plan.Shapes[i]
		named := "no company named"
		if sh.Named {
			named = "with a company name"
		}
		gutter := "  "
		body := fmt.Sprintf("%-9s %s   %s", sh.ID, countCol.Render(plural(sh.Count, "row")), named)
		switch {
		case i == m.cursor[0]:
			gutter = " " + gutterOn
			body = rowSelect.Render(body)
		case sh.State == Kill:
			body = rowDim.Render(body)
		}
		b.WriteString(fmt.Sprintf("%s %s %s\n", gutter, mark(sh.State), body))
		shown++
	}
	if h := scrollHint(m.top[0], shown, len(m.plan.Shapes)); h != "" {
		b.WriteString("    " + metaStyle.Render(h) + "\n")
	}

	if len(m.plan.Shapes) > 0 {
		cur := m.plan.Shapes[m.cursor[0]]
		box := min(78, max(28, m.w-6))
		inner := greetStyle.Render(cur.Greeting) + "\n" + quoteStyle.Render(cur.Sample)
		b.WriteString("\n  " + strings.ReplaceAll(
			sampleBox.Width(box).Render(inner), "\n", "\n  ") + "\n")
	}

	b.WriteString("\n" + help("j/k", "move", "space", "keep or kill", "tab", "names", "q", "cancel"))
	return b.String()
}

func (m model) viewNames() string {
	var b strings.Builder
	vis := m.visibleNames()
	flagged, blanked, dropped := 0, 0, 0
	for _, n := range m.plan.Names {
		if n.Why != "" {
			flagged++
		}
		switch n.State {
		case Blank:
			blanked++
		case Drop:
			dropped++
		}
	}
	right := fmt.Sprintf("names %d/%d · %d flagged · %d blanked · %d dropped",
		min(m.cursor[1]+1, len(vis)), len(vis), flagged, blanked, dropped)
	b.WriteString(m.header(right))

	if m.filtering {
		b.WriteString("  " + keyStyle.Render("/") + " " + m.filter + lipgloss.NewStyle().
			Foreground(lipgloss.Color("6")).Render("▌") + "\n")
	} else if m.filter != "" {
		b.WriteString("  " + metaStyle.Render("filtered by ") + m.filter + "\n")
	} else {
		b.WriteString("\n")
	}

	rows := m.rows()
	shown := 0
	for i := m.top[1]; i < len(vis) && shown < rows; i++ {
		n := m.plan.Names[vis[i]]
		warn := " "
		if n.Why != "" {
			warn = warnMark
		}
		name := n.Name
		if len([]rune(name)) > 36 {
			name = string([]rune(name)[:35]) + "…"
		}
		count := countCol.Render(fmt.Sprintf("%3d", n.Count))
		gutter := "  "
		var body string
		switch {
		case i == m.cursor[1]:
			gutter = " " + gutterOn
			body = rowSelect.Render(fmt.Sprintf("%-36s", name)) + " " + count + "  " + whyStyle.Render(n.Why)
		case n.State != Keep:
			body = rowDim.Render(fmt.Sprintf("%-36s %3d  %s", name, n.Count, n.Why))
		default:
			body = fmt.Sprintf("%-36s %s  %s", name, count, whyStyle.Render(n.Why))
		}
		b.WriteString(fmt.Sprintf("%s %s %s %s\n", gutter, mark(n.State), warn, body))
		shown++
	}
	if h := scrollHint(m.top[1], shown, len(vis)); h != "" {
		b.WriteString("    " + metaStyle.Render(h) + "\n")
	}

	b.WriteString("\n" + help("space", "blank", "d", "drop", "enter", "keep",
		"a", "blank all flagged", "/", "filter", "tab", "done"))
	return b.String()
}

func (m model) viewSummary() string {
	s := m.plan.Summary()
	var b strings.Builder
	b.WriteString(m.header("summary"))
	b.WriteString("\n")

	for _, row := range [][2]string{
		{"rows in the queue", fmt.Sprint(s.Rows)},
		{"shapes killed", fmt.Sprintf("%d (%d rows)", s.ShapesKilled, s.RowsFromKilledShapes)},
		{"names blanked", fmt.Sprintf("%d", s.NamesBlanked)},
		{"contacts dropped", fmt.Sprint(s.Dropped)},
	} {
		b.WriteString(fmt.Sprintf("    %s %s\n",
			metaStyle.Render(fmt.Sprintf("%-22s", row[0])), row[1]))
	}
	b.WriteString(fmt.Sprintf("    %s %s\n",
		metaStyle.Render(fmt.Sprintf("%-22s", "sendable after this")),
		okStyle.Render(fmt.Sprint(s.Sendable))))

	if s.FlaggedKept > 0 {
		b.WriteString("\n  " + warnMark + whyStyle.Render(fmt.Sprintf(
			" keeping %d flagged company names as they are", s.FlaggedKept)) + "\n")
	}

	b.WriteString("\n" + help("A", "approve, rewrite the queue and seal it",
		"shift+tab", "back", "q", "cancel") + "\n")
	return b.String()
}

// RunTUI edits the plan in place. It reports whether the reviewer approved; on false, nothing has
// been decided and the caller must write nothing.
func RunTUI(p *Plan) (bool, error) {
	m := model{plan: p, w: 80, h: 24}
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return false, err
	}
	fm, ok := final.(model)
	if !ok {
		return false, fmt.Errorf("the review ended in an unexpected state")
	}
	return fm.approved, nil
}
