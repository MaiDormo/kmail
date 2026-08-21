package review

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The decision file. One front-end onto the same *Plan the TUI edits, worth keeping for a flaky ssh
// session, for saving progress, and because it diffs against what was decided last time.
//
// Parsing is fail-closed: a file that does not describe exactly the queue that was written out
// changes nothing at all. It is an input now, and inputs can be malformed.

const fileHeader = `# kmail review — edit the first word on each line, save, quit.
# Delete everything to cancel.
#
#   shapes   keep | kill    kill drops every row using that copy
#   names    keep | blank | drop
#            blank re-renders the row with the no-company wording
#            drop removes the contact and holds it out of every future build
`

// RenderFile prints the plan as the decision file.
func RenderFile(p *Plan) string {
	var b strings.Builder
	b.WriteString(fileHeader)
	s := p.Summary()
	fmt.Fprintf(&b, "#\n# %d rows, %d shapes, %d company names, %d of them doubtful\n\n",
		s.Rows, len(p.Shapes), len(p.Names), countFlagged(p))

	b.WriteString("## shapes\n")
	for _, sh := range p.Shapes {
		named := "no company named"
		if sh.Named {
			named = "with a company name"
		}
		fmt.Fprintf(&b, "%-5s %s  %4d rows  %s\n", orKeep(sh.State), sh.ID, sh.Count, named)
		if sh.Greeting != "" {
			fmt.Fprintf(&b, "#   %s\n", sh.Greeting)
		}
		for _, line := range wrap(sh.Sample, 76) {
			fmt.Fprintf(&b, "#   %s\n", line)
		}
		b.WriteString("#\n")
	}

	b.WriteString("\n## names\n")
	for _, n := range p.Names {
		why := n.Why
		if why != "" {
			why = "  " + why
		}
		fmt.Fprintf(&b, "%-5s %-40s %3d%s\n", orKeep(n.State), n.Name, n.Count, why)
	}
	return b.String()
}

// a zero-value state would print a line with no verb, and that file cannot be parsed back
func orKeep(v Verb) Verb {
	if v == "" {
		return Keep
	}
	return v
}

func countFlagged(p *Plan) int {
	n := 0
	for _, x := range p.Names {
		if x.Why != "" {
			n++
		}
	}
	return n
}

func wrap(s string, width int) []string {
	var out []string
	line := ""
	for _, w := range strings.Fields(s) {
		if line != "" && len(line)+1+len(w) > width {
			out = append(out, line)
			line = ""
		}
		if line == "" {
			line = w
		} else {
			line += " " + w
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// ErrCancelled means the reviewer emptied the file, which is the `git rebase` convention for
// backing out. Nothing is written.
type ErrCancelled struct{}

func (ErrCancelled) Error() string { return "cancelled. Nothing written." }

// ParseFile applies a decision file to the plan. It changes nothing unless the whole file is good.
func ParseFile(text string, p *Plan) error {
	type decision struct {
		verb Verb
		line int
	}
	shapes := map[string]decision{}
	names := map[string]decision{}
	section := ""
	content := false

	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		// "##" first: a section header also starts with "#", and checking the comment rule
		// first swallowed every header, leaving the parser with no section at all
		if strings.HasPrefix(line, "##") {
			section = strings.TrimSpace(strings.TrimPrefix(line, "##"))
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		content = true

		verbStr, rest, ok := strings.Cut(line, " ")
		if !ok {
			return fmt.Errorf("line %d: %q has a verb but nothing after it", lineNo, line)
		}
		rest = strings.TrimSpace(rest)
		verb := Verb(strings.ToLower(verbStr))

		switch section {
		case "shapes":
			if verb != Keep && verb != Kill {
				return fmt.Errorf("line %d: %q is not keep or kill", lineNo, verbStr)
			}
			id, _, _ := strings.Cut(rest, " ")
			if p.shape(id) == nil {
				return fmt.Errorf("line %d: no shape %q in this queue", lineNo, id)
			}
			if prev, dup := shapes[id]; dup {
				return fmt.Errorf("line %d: shape %s already decided on line %d", lineNo, id, prev.line)
			}
			shapes[id] = decision{verb, lineNo}
		case "names":
			if verb != Keep && verb != Blank && verb != Drop {
				return fmt.Errorf("line %d: %q is not keep, blank or drop", lineNo, verbStr)
			}
			name := matchName(p, rest)
			if name == "" {
				return fmt.Errorf("line %d: no company name in this queue matches %q", lineNo, rest)
			}
			if prev, dup := names[name]; dup {
				return fmt.Errorf("line %d: name %q already decided on line %d", lineNo, name, prev.line)
			}
			names[name] = decision{verb, lineNo}
		default:
			return fmt.Errorf("line %d: %q is outside any ## section", lineNo, line)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !content {
		return ErrCancelled{}
	}

	// every item must have been decided, or the file is describing a different queue
	for _, sh := range p.Shapes {
		if _, ok := shapes[sh.ID]; !ok {
			return fmt.Errorf("shape %s is missing from the file — it describes a different queue", sh.ID)
		}
	}
	for _, n := range p.Names {
		if _, ok := names[n.Name]; !ok {
			return fmt.Errorf("company name %q is missing from the file — it describes a different queue", n.Name)
		}
	}

	for i := range p.Shapes {
		p.Shapes[i].State = shapes[p.Shapes[i].ID].verb
	}
	for i := range p.Names {
		p.Names[i].State = names[p.Names[i].Name].verb
	}
	return nil
}

// matchName finds which company the rest of the line names. The count and the flag follow the name
// on the printed line, and a name may itself contain spaces, so the longest match wins.
func matchName(p *Plan, rest string) string {
	best := ""
	for _, n := range p.Names {
		if strings.HasPrefix(rest, n.Name) && len(n.Name) > len(best) {
			best = n.Name
		}
	}
	return best
}

// EditPlan writes the decision file, opens it in $EDITOR, and applies whatever comes back.
func EditPlan(p *Plan) error {
	f, err := os.CreateTemp("", "kmail-review-*.txt")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	before := RenderFile(p)
	if _, err := f.WriteString(before); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if err := openEditor(path); err != nil {
		return err
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	after := string(b)
	if err := ParseFile(after, p); err != nil {
		return err
	}
	if after == before {
		s := p.Summary()
		if s.FlaggedKept > 0 {
			fmt.Printf("\nthe file came back unchanged, so everything is approved as listed —\n"+
				"including all %d flagged company names.\n\n", s.FlaggedKept)
		}
	}
	return nil
}

func openEditor(path string) error {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vi"
	}
	// the editor needs the real terminal, not a pipe
	cmd := exec.Command("sh", "-c", ed+" \"$1\"", "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
