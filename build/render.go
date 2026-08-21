// Package build turns the master CSV into a sealed queue, and holds the renderer that the reviewer
// re-uses when a company name is blanked.
package build

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
	"unicode"

	"kmail/campaign"
	"kmail/preflight"
)

var (
	bodyRe    = regexp.MustCompile(`(?is)<body[^>]*>(.*)</body>`)
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	hiddenRe  = regexp.MustCompile(`(?is)<div[^>]*display:\s*none[^>]*>.*?</div>`)
	panelRe   = regexp.MustCompile(`(?is)<td[^>]*background-color:\s*#0[c8][0-9a-f]*[^>]*>.*?</td>\s*</tr>`)
	// RE2 has no backreference, so the Python's <(script|style)...</\1> becomes two passes
	scriptRe   = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleTagRe = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	brRe       = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockEndRe = regexp.MustCompile(`(?i)</(td|p|div|h1|h2|h3)>`)
	anyTagRe   = regexp.MustCompile(`<[^>]+>`)
	// U+034F, U+200B, U+200C, U+200D, U+00AD, U+FEFF
	invisibleRe = regexp.MustCompile("[\u034F\u200B\u200C\u200D\u00AD\uFEFF]")
	spacesRe    = regexp.MustCompile(`[ \t]+`)
	blankRunRe  = regexp.MustCompile(`\n{3,}`)
	greetingRe  = regexp.MustCompile(`(?m)^(Hi|Hello)[ ,]`)
	styleRe     = regexp.MustCompile(`(?s)<style>.*?</style>`)
)

// LoadTemplate reads the template and refuses it if it has lost a slot or regained a forbidden word.
func LoadTemplate() (string, error) {
	return LoadTemplateFor(campaign.Audiences[campaign.DefaultAudience])
}

func LoadTemplateFor(a *campaign.Audience) (string, error) {
	b, err := os.ReadFile(a.Template())
	if err != nil {
		return "", err
	}
	tpl := string(b)
	if problems := preflight.CheckTemplate(tpl); len(problems) > 0 {
		return "", fmt.Errorf("template rejected:\n  %s", strings.Join(problems, "\n  "))
	}
	return tpl, nil
}

// CleanCompany decides whether a name enters the copy. The column also holds mail domains, bare
// hostnames and, often, the contact's own name - and "At Jordan Fairweather the archive keeps growing"
// is worse than saying nothing.
func CleanCompany(name string) string {
	n := strings.TrimSpace(name)
	if n == "" || n == "?" || n == "-" || len([]rune(n)) < 3 {
		return ""
	}
	if campaign.Mojibake.MatchString(n) {
		return ""
	}
	if campaign.ConsumerMail.MatchString(n) {
		return ""
	}
	if bareHost.MatchString(n) {
		return ""
	}
	if shortCaps.MatchString(n) {
		return ""
	}
	// the column sometimes holds a sentence: "a chinese company which develop video compress card".
	// That is a description, and it cannot go after "At". A lowercase first letter is a brand
	// (adRom, iMedia); a lowercase first letter on a long phrase is a sentence.
	fields := strings.Fields(n)
	if len([]rune(n)) > 42 || len(fields) > 6 {
		return ""
	}
	if firstIsLower(n) && len(fields) > 3 {
		return ""
	}
	// a plain "Firstname Lastname" with nothing corporate about it is a person
	if campaign.PersonName.MatchString(n) && !campaign.Corp.MatchString(n) {
		return ""
	}
	return n
}

var (
	bareHost  = regexp.MustCompile(`(?i)^[a-z0-9-]+(\.[a-z]{2,})+$`)
	shortCaps = regexp.MustCompile(`^[A-Z]{1,2}$`)
)

// firstIsLower mirrors Python's n[:1].islower(): true only when the first character is a cased
// letter that is lowercase. An uncased first character (a digit, a symbol) is false in both.
func firstIsLower(s string) bool {
	for _, r := range s {
		return unicode.IsLower(r)
	}
	return false
}

// ToText turns the HTML fragment into the plain-text alternative. Its output is what gets hashed,
// so any drift here changes the seal on every row built afterwards.
func ToText(fragment string) (string, error) {
	t := commentRe.ReplaceAllString(fragment, "")
	t = hiddenRe.ReplaceAllString(t, "")
	t = panelRe.ReplaceAllString(t, "")
	t = scriptRe.ReplaceAllString(t, "")
	t = styleTagRe.ReplaceAllString(t, "")
	t = brRe.ReplaceAllString(t, "\n")
	t = blockEndRe.ReplaceAllString(t, "\n\n")
	t = anyTagRe.ReplaceAllString(t, "")
	t = html.UnescapeString(t)
	t = invisibleRe.ReplaceAllString(t, "")
	// the Python also had .replace(' ', ' '), which is U+0020 to U+0020 and does nothing
	t = spacesRe.ReplaceAllString(t, " ")

	lines := strings.Split(t, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(ln)
	}
	t = strings.Join(lines, "\n")
	t = strings.TrimSpace(blankRunRe.ReplaceAllString(t, "\n\n"))

	paras := strings.Split(t, "\n\n")
	for i, p := range paras {
		var kept []string
		for _, ln := range strings.Split(p, "\n") {
			if ln != "" {
				kept = append(kept, ln)
			}
		}
		paras[i] = strings.Join(kept, " ")
	}
	t = strings.Join(paras, "\n\n")

	// the wordmark, headline and timeline strip are picture, not prose: the letter starts at the
	// greeting
	loc := greetingRe.FindStringIndex(t)
	if loc == nil {
		return "", fmt.Errorf("greeting not found in plain text — cannot trim the hero")
	}
	return strings.TrimSpace(t[loc[0]:]) + "\n", nil
}

// Render fills the three slots and returns the whole document, the body fragment and the plain text.
func Render(tpl, greeting, openerLine, example string) (doc, frag, text string, err error) {
	doc = tpl
	for _, sv := range []struct{ slot, value string }{
		{"{{ greeting }}", greeting},
		{"{{ opener }}", openerLine},
		{"{{ search_example }}", example},
	} {
		slot, value := sv.slot, sv.value
		if !strings.Contains(doc, slot) {
			return "", "", "", fmt.Errorf("template lost the %s slot — fix the template, not this code", slot)
		}
		doc = strings.ReplaceAll(doc, slot, value)
	}
	if strings.Contains(doc, "{{") {
		return "", "", "", fmt.Errorf("unresolved placeholder left in the render")
	}

	frag = doc
	if m := bodyRe.FindStringSubmatch(doc); m != nil {
		frag = m[1]
	}
	frag = strings.TrimSpace(frag)
	// the <style> block lives in <head>, which the fragment drops. Without it the responsive rules
	// and the [data-ogsc] dark-mode overrides never reach the recipient, so carry it into the
	// fragment: clients that support embedded CSS read it there too.
	if s := styleRe.FindString(tpl); s != "" {
		frag = s + "\n" + frag
	}
	text, err = ToText(frag)
	return doc, frag, text, err
}

// Contact is one row of the master list, after filtering.
type Contact struct {
	Audience    *campaign.Audience
	Email       string
	FirstName   string
	Company     string // raw, as the list has it — used for the search example
	SafeCompany string // the cleaned name, or "" when it must not enter the copy
	Title       string
	Domain      string
}

// RenderRow builds one queue payload. nameOverride set to a non-nil empty string forces the
// no-company wording, which is what the reviewer does when a company name reads badly.
func RenderRow(tpl string, c Contact, subject string, nameOverride *string) (preflight.Row, string, error) {
	safe := c.SafeCompany
	if nameOverride != nil {
		safe = *nameOverride
	}
	greeting := "Hi there,"
	if c.FirstName != "" {
		greeting = "Hi " + c.FirstName + ","
	}
	aud := c.Audience
	if aud == nil {
		aud = campaign.Audiences[campaign.DefaultAudience]
	}
	line, role, named := campaign.Opener(aud, c.Title, safe)
	example := campaign.SearchExample(aud, c.Company, c.Domain, c.Title)
	doc, frag, text, err := Render(tpl, greeting, line, example)
	if err != nil {
		return preflight.Row{}, "", err
	}
	name := ""
	if named {
		name = safe
	}
	row := preflight.Row{
		To: []string{c.Email}, Subject: subject, Body: text, HTMLBody: frag,
		Company: c.Company, Title: c.Title, FirstName: c.FirstName,
		Shape: campaign.ShapeID(aud.Name, role, named), Name: name, Audience: aud.Name,
	}
	row.Hash = preflight.ContentHash(row)
	return row, doc, nil
}

// Attribute reports which shape and which company name a queued row is actually made of.
//
// Always recomputed, never read back from the row: the seal covers subject, body and html, so a
// "shape" field written by hand would be believed without the hash ever breaking. The candidates
// are tried in order because a filter tightened later must not orphan a row an earlier build wrote
// with the looser one. ok is false when no candidate reproduces the body, which means the row was
// not produced by this campaign's copy and no approval can honestly cover it.
func Attribute(r preflight.Row) (shape, name string, ok bool) {
	aud := r.Aud()
	raw := strings.TrimSpace(r.Company)
	seen := map[string]bool{}
	for _, cand := range []string{CleanCompany(raw), raw, ""} {
		if seen[cand] {
			continue
		}
		seen[cand] = true
		line, role, named := campaign.Opener(aud, r.Title, cand)
		if strings.Contains(r.Body, line) {
			n := ""
			if named {
				n = cand
			}
			return campaign.ShapeID(aud.Name, role, named), n, true
		}
	}
	_, role, named := campaign.Opener(aud, r.Title, CleanCompany(raw))
	return campaign.ShapeID(aud.Name, role, named), "", false
}

// ---------------------------------------------------------------- the queue file

func ReadQueue() ([]preflight.Row, error) {
	b, err := os.ReadFile(campaign.Queue)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []preflight.Row
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r preflight.Row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("queue.jsonl is not readable: %w", err)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// WriteQueue replaces the queue atomically: a half-written queue is a queue whose hashes no longer
// describe it.
func WriteQueue(rows []preflight.Row) error {
	tmp := campaign.Queue + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	// Go escapes <, > and & by default and htmlBody is nothing but those. Python did not.
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, campaign.Queue)
}
