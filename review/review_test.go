package review

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"kmail/build"
	"kmail/campaign"
	"kmail/preflight"
)

func TestMain(m *testing.M) {
	_ = campaign.LoadIdentity()
	os.Exit(m.Run())
}

func plan(t *testing.T) *Plan {
	t.Helper()
	rows, err := build.ReadQueue()
	if err != nil {
		t.Fatalf("reading the queue: %v", err)
	}
	if len(rows) == 0 {
		t.Skipf("no queue at %s", campaign.Queue)
	}
	return Collect(rows)
}

func states(p *Plan) ([]Verb, []Verb) {
	sh := make([]Verb, len(p.Shapes))
	for i, s := range p.Shapes {
		sh[i] = s.State
	}
	nm := make([]Verb, len(p.Names))
	for i, n := range p.Names {
		nm[i] = n.State
	}
	return sh, nm
}

func press(m tea.Model, keys ...tea.KeyPressMsg) tea.Model {
	for _, k := range keys {
		m, _ = m.Update(k)
	}
	return m
}

func text(s string) tea.KeyPressMsg {
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func code(c rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: c} }

// The point of one model with two front-ends: the same decisions, whichever way they were made.
func TestTUIAndFileAgree(t *testing.T) {
	viaTUI := plan(t)
	m := tea.Model(model{plan: viaTUI, w: 100, h: 30})
	// shapes: kill the first, keep the rest. names: blank everything flagged, drop the first
	m = press(m, text(" "), code(tea.KeyTab))
	m = press(m, text("a"), text("d"))

	viaFile := plan(t)
	viaFile.Shapes[0].State = Kill
	viaFile.BlankAllFlagged()
	viaFile.Names[0].State = Drop
	rendered := RenderFile(viaFile)

	// round-trip the printed file through the parser onto a clean plan
	roundTripped := plan(t)
	if err := ParseFile(rendered, roundTripped); err != nil {
		t.Fatalf("the file this tool printed did not parse: %v", err)
	}

	tsh, tnm := states(viaTUI)
	fsh, fnm := states(roundTripped)
	for i := range tsh {
		if tsh[i] != fsh[i] {
			t.Errorf("shape %s: TUI %s, file %s", viaTUI.Shapes[i].ID, tsh[i], fsh[i])
		}
	}
	for i := range tnm {
		if tnm[i] != fnm[i] {
			t.Errorf("name %q: TUI %s, file %s", viaTUI.Names[i].Name, tnm[i], fnm[i])
		}
	}
}

// A file that does not describe exactly this queue changes nothing at all.
func TestFileParsingIsFailClosed(t *testing.T) {
	good := RenderFile(plan(t))

	cases := map[string]string{
		// replace a real decision line, not the "keep | kill" inside the header comment
		"unknown verb": strings.Replace(good, firstShapeLine(t, good),
			"yolo "+strings.TrimPrefix(firstShapeLine(t, good), "keep "), 1),
		"missing shape": strings.Replace(good, "\n"+firstShapeLine(t, good), "", 1),
		"unknown name":  good + "\nkeep Not A Company In This Queue  1\n",
		"duplicated":    good + "\n" + lastNameLine(t, good) + "\n",
	}
	for label, text := range cases {
		p := plan(t)
		before, beforeNames := states(p)
		err := ParseFile(text, p)
		if err == nil {
			t.Errorf("%s: accepted", label)
		}
		after, afterNames := states(p)
		for i := range before {
			if before[i] != after[i] {
				t.Errorf("%s: shape state changed despite the error", label)
			}
		}
		for i := range beforeNames {
			if beforeNames[i] != afterNames[i] {
				t.Errorf("%s: name state changed despite the error", label)
			}
		}
	}

	// emptying the file is how you back out, the git rebase convention
	p := plan(t)
	if err := ParseFile("# everything deleted\n", p); err == nil {
		t.Error("an emptied file was not treated as a cancel")
	} else if _, ok := err.(ErrCancelled); !ok {
		t.Errorf("an emptied file gave %v, not a cancel", err)
	}
}

func firstShapeLine(t *testing.T, file string) string {
	t.Helper()
	for _, l := range strings.Split(file, "\n") {
		if strings.HasPrefix(l, "keep ") || strings.HasPrefix(l, "kill ") {
			return l
		}
	}
	t.Fatal("no shape line in the rendered file")
	return ""
}

func lastNameLine(t *testing.T, file string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(file, "\n"), "\n")
	return lines[len(lines)-1]
}

// A name may contain spaces, and the count and flag follow it on the line.
func TestNamesWithSpacesRoundTrip(t *testing.T) {
	p := &Plan{Names: []Name{
		{Name: "Northgate Media", Count: 2, State: Keep},
		{Name: "Northgate Media Group", Count: 1, State: Keep},
		{Name: "Northgate Tech Services, LLC", Count: 1, Why: "shouting", State: Keep},
	}}
	out := RenderFile(p)
	got := &Plan{Names: append([]Name(nil), p.Names...)}
	if err := ParseFile(out, got); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	// the longest match wins, so the shorter prefix must not swallow the longer name
	p.Names[1].State = Drop
	out = RenderFile(p)
	got = &Plan{Names: append([]Name(nil), p.Names...)}
	for i := range got.Names {
		got.Names[i].State = Keep
	}
	if err := ParseFile(out, got); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if got.Names[1].State != Drop || got.Names[0].State != Keep {
		t.Errorf("prefix collision: %v", states2(got))
	}
}

func states2(p *Plan) []Verb {
	_, n := states(p)
	return n
}

func TestFlagMirrorsThePythonRules(t *testing.T) {
	r := preflight.Row{To: []string{"a@x.com"}, Company: "x", Title: "x"}
	for _, junk := range []string{"asdfgh", "YouTube", "Te?t ?edia Group", "PHS"} {
		if Flag(junk, []preflight.Row{r}) == "" {
			t.Errorf("kept %q unflagged", junk)
		}
	}
	ok := preflight.Row{To: []string{"a@alpha.tv"}, Company: "Northgate Media", Title: "Head of Media"}
	if why := Flag("Northgate Media", []preflight.Row{ok}); why != "" {
		t.Errorf("flagged a good name: %s", why)
	}
}

func TestGateRefusesWithoutApproval(t *testing.T) {
	dir := t.TempDir()
	old := campaign.Home
	rows, _ := build.ReadQueue()
	campaign.SetHome(dir)
	defer campaign.SetHome(old)
	if len(rows) == 0 {
		t.Skip("no queue")
	}
	if len(Gate(rows)) == 0 {
		t.Error("the gate opened with no approvals.json")
	}
}
