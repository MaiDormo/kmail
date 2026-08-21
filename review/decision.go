// Package review is the human gate.
//
// Preflight can prove an email is structurally intact. It cannot tell you that "If asdfgh works with
// long-form video" is not a sentence you want to send a stranger. Twelve opener shapes and a list of
// company names decide that, and both are small enough to read.
//
// There are two front-ends — a Bubble Tea TUI and a decision file opened in $EDITOR — and neither is
// an implementation. Both produce the same *Plan, and only the Plan reaches Apply and Gate.
package review

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"kmail/build"
	"kmail/campaign"
	"kmail/preflight"
)

// Verb is what a human decided about one shape or one name.
type Verb string

const (
	Keep  Verb = "keep"  // shapes and names
	Kill  Verb = "kill"  // shapes: drop every row using this copy
	Blank Verb = "blank" // names: re-render the row with the no-company wording
	Drop  Verb = "drop"  // names: drop the contact entirely
)

var (
	ShapeVerbs = []Verb{Keep, Kill}
	NameVerbs  = []Verb{Keep, Blank, Drop}
)

// Shape is one piece of copy, independent of the company interpolated into it.
type Shape struct {
	ID       string
	Count    int
	Greeting string
	Sample   string
	Named    bool
	State    Verb
}

// Name is one company name that reaches the copy.
type Name struct {
	Name  string
	Count int
	Why   string // empty means it reads fine
	State Verb
}

// Item ties a queue row to the shape and name it is made of.
type Item struct {
	Row   preflight.Row
	Shape string
	Name  string
	OK    bool
}

// Plan is everything a review decides. Both front-ends edit one of these.
type Plan struct {
	Shapes []Shape
	Names  []Name
	Items  []Item
}

func (p *Plan) shape(id string) *Shape {
	for i := range p.Shapes {
		if p.Shapes[i].ID == id {
			return &p.Shapes[i]
		}
	}
	return nil
}

func (p *Plan) name(n string) *Name {
	for i := range p.Names {
		if p.Names[i].Name == n {
			return &p.Names[i]
		}
	}
	return nil
}

// Unattributable lists rows whose copy this campaign did not produce, which no approval can cover.
func (p *Plan) Unattributable() []string {
	var out []string
	for _, it := range p.Items {
		if !it.OK {
			out = append(out, it.Row.Addr())
		}
	}
	return out
}

// Summary is what the confirm step shows, and what the file prints at the top.
type Summary struct {
	Rows, ShapesKilled, RowsFromKilledShapes, NamesBlanked, Dropped, Sendable, FlaggedKept int
}

func (p *Plan) Summary() Summary {
	s := Summary{Rows: len(p.Items)}
	killed := map[string]bool{}
	for _, sh := range p.Shapes {
		if sh.State == Kill {
			killed[sh.ID] = true
			s.ShapesKilled++
			s.RowsFromKilledShapes += sh.Count
		}
	}
	dropped := map[string]bool{}
	for _, n := range p.Names {
		switch n.State {
		case Blank:
			s.NamesBlanked++
		case Drop:
			dropped[n.Name] = true
		case Keep:
			if n.Why != "" {
				s.FlaggedKept++
			}
		}
	}
	for _, it := range p.Items {
		if killed[it.Shape] || (it.Name != "" && dropped[it.Name]) {
			s.Dropped++
		}
	}
	s.Sendable = s.Rows - s.Dropped
	return s
}

// ---------------------------------------------------------------- flags

var (
	platformRe = regexp.MustCompile(`(?i)^(my |the |our )?(youtube|yt|steam|twitch|facebook|instagram|tiktok|vimeo|netflix|` +
		`spotify|google|apple|social media|channel)\b`)
	vowelRe = regexp.MustCompile(`(?i)[aeiouyàèéìòùáéíóúäöü]`)
	oddPunc = regexp.MustCompile(`[_/\\|!]`)
)

// Flag says why a company name is doubtful in the copy. An empty string means it reads fine.
func Flag(name string, rows []preflight.Row) string {
	n := strings.TrimSpace(name)
	switch {
	case campaign.Mojibake.MatchString(n):
		return "mojibake"
	case platformRe.MatchString(n):
		return "a platform, not a prospect"
	case !vowelRe.MatchString(n):
		return "not a word"
	case len([]rune(n)) <= 3:
		return "acronym"
	case isUpper(n) && len([]rune(n)) > 6:
		return "shouting"
	case isLower(n) && !strings.Contains(n, " "):
		return "a handle, not a name"
	case oddPunc.MatchString(n):
		return "punctuation a name would not have"
	}
	// "no video signal" asks whether the contact is in the video business. That matters only where
	// the copy claims something about their industry; for an audience whose pitch claims nothing,
	// every row would be flagged and the screen would be noise.
	claimsIndustry := false
	for _, r := range rows {
		if r.Aud().Name == campaign.DefaultAudience {
			claimsIndustry = true
			break
		}
	}
	if !claimsIndustry {
		return ""
	}
	for _, r := range rows {
		if campaign.VideoSignal.MatchString(r.Company + " " + r.Title + " " + r.Addr()) {
			return ""
		}
	}
	return "no video signal"
}

// isUpper and isLower mirror Python's str.isupper/islower: at least one cased character, and every
// cased character in that case.
func isUpper(s string) bool { return cased(s, unicode.IsUpper, unicode.IsLower) }
func isLower(s string) bool { return cased(s, unicode.IsLower, unicode.IsUpper) }

func cased(s string, want, other func(rune) bool) bool {
	any := false
	for _, r := range s {
		if other(r) {
			return false
		}
		if want(r) {
			any = true
		}
	}
	return any
}

// ---------------------------------------------------------------- collect

// Collect groups the queue by opener shape and by the company name each row interpolates.
func Collect(rows []preflight.Row) *Plan {
	p := &Plan{}
	byName := map[string][]preflight.Row{}
	index := map[string]int{}

	for _, r := range rows {
		sid, name, ok := build.Attribute(r)
		p.Items = append(p.Items, Item{Row: r, Shape: sid, Name: name, OK: ok})
		if _, seen := index[sid]; !seen {
			paras := strings.Split(r.Body, "\n\n")
			sh := Shape{ID: sid, Named: name != "", State: Keep}
			if len(paras) > 0 {
				sh.Greeting = paras[0]
			}
			if len(paras) > 1 {
				sh.Sample = paras[1]
			}
			index[sid] = len(p.Shapes)
			p.Shapes = append(p.Shapes, sh)
		}
		p.Shapes[index[sid]].Count++
		if name != "" {
			byName[name] = append(byName[name], r)
		}
	}

	for n, rs := range byName {
		p.Names = append(p.Names, Name{Name: n, Count: len(rs), Why: Flag(n, rs), State: Keep})
	}
	// doubtful first, then the crowded ones: the order you would read them in anyway
	sort.Slice(p.Names, func(i, j int) bool {
		a, b := p.Names[i], p.Names[j]
		if (a.Why != "") != (b.Why != "") {
			return a.Why != ""
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	sort.SliceStable(p.Shapes, func(i, j int) bool { return p.Shapes[i].Count > p.Shapes[j].Count })
	return p
}

// BlankAllFlagged is the bulk action both front-ends offer.
func (p *Plan) BlankAllFlagged() int {
	n := 0
	for i := range p.Names {
		if p.Names[i].Why != "" && p.Names[i].State == Keep {
			p.Names[i].State = Blank
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- apply

// Approval is what a human approved, and against which template. Shapes and names, never a row
// list — so a later build producing the same copy for new contacts passes, and a new company name
// does not.
type Approval struct {
	ApprovedAt  string   `json:"approved_at"`
	TemplateSHA string   `json:"template_sha"`
	Shapes      []string `json:"shapes"`
	Names       []string `json:"names"`
	Blanked     []string `json:"blanked"`
	Dropped     []string `json:"dropped"`
}

// Apply rewrites the queue to match the decisions, then records what was approved.
func Apply(p *Plan, log func(string, ...any)) error {
	templates := map[string]string{}
	for name, a := range campaign.Audiences {
		t, err := build.LoadTemplateFor(a)
		if err != nil {
			return fmt.Errorf("audience %q: %w", name, err)
		}
		templates[name] = t
	}
	killed := map[string]bool{}
	for _, s := range p.Shapes {
		if s.State == Kill {
			killed[s.ID] = true
		}
	}
	state := map[string]Verb{}
	for _, n := range p.Names {
		state[n.Name] = n.State
	}

	var kept []preflight.Row
	var dropped []string
	reblanked := 0
	empty := ""

	for _, it := range p.Items {
		r := it.Row
		if killed[it.Shape] || (it.Name != "" && state[it.Name] == Drop) {
			dropped = append(dropped, strings.ToLower(r.Addr()))
			continue
		}
		if it.Name != "" && state[it.Name] == Blank {
			addr := r.Addr()
			domain := ""
			if i := strings.Index(addr, "@"); i >= 0 {
				domain = addr[i+1:]
			}
			aud := r.Aud()
			c := build.Contact{
				Audience: aud, Email: addr, FirstName: r.FirstName, Company: r.Company,
				SafeCompany: it.Name, Title: r.Title, Domain: domain,
			}
			nr, doc, err := build.RenderRow(templates[aud.Name], c, r.Subject, &empty)
			if err != nil {
				return fmt.Errorf("re-rendering %s: %w", addr, err)
			}
			if r.HTMLFile != "" {
				nr.HTMLFile = r.HTMLFile
				path := campaign.Drafts + "/" + r.HTMLFile
				if _, err := os.Stat(path); err == nil {
					if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
						return err
					}
				}
			}
			kept = append(kept, nr)
			reblanked++
			continue
		}
		r.Shape, r.Name = it.Shape, it.Name
		kept = append(kept, r)
	}

	problems := preflight.CheckQueue(kept)
	for i, r := range kept {
		for _, p := range preflight.CheckRow(r) {
			problems = append(problems, fmt.Sprintf("row %d (%s): %s", i, r.Addr(), p))
		}
	}
	if len(problems) > 0 {
		log("PREFLIGHT FAILED after review — %d problem(s). Nothing written.", len(problems))
		for i, p := range problems {
			if i == 20 {
				break
			}
			log("  %s", p)
		}
		return fmt.Errorf("preflight rejected the reviewed queue")
	}

	if err := build.WriteQueue(kept); err != nil {
		return err
	}
	if len(dropped) > 0 {
		if err := appendLines(campaign.Held, dropped); err != nil {
			return err
		}
	}
	// remember the blanking, or the next build renders these names all over again
	var newlyBlanked []string
	known := loadLower(campaign.Blanked)
	for n, v := range state {
		if v == Blank && !known[strings.ToLower(n)] {
			newlyBlanked = append(newlyBlanked, n)
		}
	}
	sort.Strings(newlyBlanked)
	if len(newlyBlanked) > 0 {
		if err := appendLines(campaign.Blanked, newlyBlanked); err != nil {
			return err
		}
	}

	sha, err := campaign.TemplateSHA()
	if err != nil {
		return err
	}
	rec := Approval{
		ApprovedAt:  time.Now().UTC().Format(time.RFC3339),
		TemplateSHA: sha,
		Shapes: uniqueSorted(func(add func(string)) {
			for _, r := range kept {
				add(r.Shape)
			}
		}),
		Names: uniqueSorted(func(add func(string)) {
			for _, r := range kept {
				if r.Name != "" {
					add(r.Name)
				}
			}
		}),
		Blanked: uniqueSorted(func(add func(string)) {
			for n, v := range state {
				if v == Blank {
					add(n)
				}
			}
		}),
		Dropped: uniqueSorted(func(add func(string)) {
			for _, d := range dropped {
				add(d)
			}
		}),
	}
	if err := writeJSON(campaign.Approvals, rec); err != nil {
		return err
	}

	log("approved  : %d rows, %d shapes, %d company names", len(kept), len(rec.Shapes), len(rec.Names))
	log("blanked   : %d rows re-rendered without a company name", reblanked)
	log("dropped   : %d contacts, written to held.txt", len(dropped))
	if len(newlyBlanked) > 0 {
		log("remembered: %d company names, so no future build renders them again", len(newlyBlanked))
	}
	log("queue.jsonl rewritten and re-sealed. `kmail send` is now open for these rows.")
	return nil
}

func uniqueSorted(collect func(add func(string))) []string {
	seen := map[string]bool{}
	var out []string
	collect(func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	})
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func loadLower(path string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		if s := strings.ToLower(strings.TrimSpace(l)); s != "" {
			out[s] = true
		}
	}
	return out
}

func appendLines(path string, lines []string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", " ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------- the gate

func LoadApprovals() (*Approval, error) {
	b, err := os.ReadFile(campaign.Approvals)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a Approval
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// Gate says why these rows may not be sent. An empty slice means a human approved this exact copy.
func Gate(rows []preflight.Row) []string {
	a, err := LoadApprovals()
	if err != nil {
		return []string{fmt.Sprintf("approvals.json is not readable: %v", err)}
	}
	if a == nil {
		return []string{"no approvals.json — run `kmail review` first. Nothing has been reviewed."}
	}
	sha, err := campaign.TemplateSHA()
	if err != nil {
		return []string{fmt.Sprintf("the template is not readable: %v", err)}
	}
	if a.TemplateSHA != sha {
		return []string{"the template changed since it was reviewed. Run `kmail review` again."}
	}

	shapes := set(a.Shapes)
	names := set(a.Names)
	badShape := map[string]int{}
	badName := map[string]int{}
	var unattributable []string

	for _, r := range rows {
		// recomputed, never read back from the row: the seal does not cover the shape field
		sid, name, ok := build.Attribute(r)
		if !ok {
			unattributable = append(unattributable, r.Addr())
			continue
		}
		if !shapes[sid] {
			badShape[sid]++
		}
		if name != "" && !names[name] {
			badName[name]++
		}
	}

	var out []string
	if len(unattributable) > 0 {
		n := len(unattributable)
		if n > 3 {
			unattributable = unattributable[:3]
		}
		out = append(out, fmt.Sprintf("%d row(s) carry copy this campaign did not produce: %s",
			n, strings.Join(unattributable, ", ")))
	}
	for _, sid := range sortedKeys(badShape) {
		out = append(out, fmt.Sprintf("%d row(s) use opener shape %s, which was never approved", badShape[sid], sid))
	}
	shown := 0
	for _, n := range sortedKeys(badName) {
		if shown == 5 {
			out = append(out, fmt.Sprintf("... and %d more unapproved names", len(badName)-5))
			break
		}
		out = append(out, fmt.Sprintf("company name not approved: %q (%d row(s))", n, badName[n]))
		shown++
	}
	return out
}

func set(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}
