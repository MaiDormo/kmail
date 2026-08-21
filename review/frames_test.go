package review

import (
	"fmt"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// KMAIL_DUMP_FRAMES=1 go test ./review/ -run Frames -v
func TestFrames(t *testing.T) {
	if os.Getenv("KMAIL_DUMP_FRAMES") == "" {
		t.Skip("set KMAIL_DUMP_FRAMES=1 to print the panes")
	}
	p := plan(t)
	m := tea.Model(model{plan: p, w: 96, h: 26})
	dump := func(label string) {
		fmt.Printf("\n=== %s ===\n%s", label, strings.TrimRight(m.View().Content, "\n"))
	}
	dump("shapes")
	m = press(m, code(tea.KeyTab))
	dump("names")
	m = press(m, text("a"))
	dump("names after [a] blank all flagged")
	m = press(m, text("/"))
	m = press(m, text("y"), text("o"), text("u"))
	dump("names filtered by you")
	m = press(m, code(tea.KeyEnter), code(tea.KeyTab))
	dump("summary")
	fmt.Println()
}
