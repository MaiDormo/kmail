// Package preflight holds the validation rules for one outreach email, shared by the builder, the
// reviewer and the sender.
//
// Every rule here exists because it has already gone wrong. campaign.Forbidden in particular holds
// the literal sentences that subagents invented on 2026-08-20, so that exact regression is caught
// rather than described.
//
// A row that fails any check is never written by the builder and never sent by the sender. Failure
// is fatal to the whole run, not to one row: a half-checked batch of real mail is worse than no
// mail.
//
// These rules see structure only. Whether the sentence reads like a human wrote it about a real
// company is what package review is for.
package preflight

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"kmail/campaign"
)

// Row is one queue payload. The field order matches what the Python wrote, so a rewritten queue
// diffs cleanly against the old one.
type Row struct {
	To        []string `json:"to"`
	Subject   string   `json:"subject"`
	Body      string   `json:"body"`
	HTMLBody  string   `json:"htmlBody"`
	Company   string   `json:"company"`
	Title     string   `json:"title"`
	FirstName string   `json:"first_name"`
	HTMLFile  string   `json:"html_file,omitempty"`
	BodyFile  string   `json:"body_file,omitempty"`
	TextFile  string   `json:"text_file,omitempty"`
	Hash      string   `json:"hash,omitempty"`
	Shape     string   `json:"shape,omitempty"`
	Name      string   `json:"name,omitempty"`
}

func (r Row) Addr() string {
	if len(r.To) == 0 {
		return ""
	}
	return r.To[0]
}

// ContentHash binds subject, body and HTML together. Any later edit to any of them breaks the seal.
func ContentHash(r Row) string {
	h := sha256.New()
	for _, f := range []string{r.Subject, r.Body, r.HTMLBody} {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var ctaAnchor = regexp.MustCompile(`(?s)<a[^>]+kairosapp\.tech.*?>\s*Try KAIROS for free\s*</a`)

// CheckRow returns the problems with one row. An empty slice means it may be sent.
func CheckRow(r Row) []string {
	var bad []string
	to := strings.TrimSpace(r.Addr())

	if !campaign.Address.MatchString(to) {
		bad = append(bad, fmt.Sprintf("address not sendable: %q", to))
	}
	if strings.EqualFold(to, campaign.Sender) {
		bad = append(bad, "addressed to the sending mailbox")
	}

	if !contains(campaign.Subjects, r.Subject) {
		bad = append(bad, fmt.Sprintf("subject not in the approved set: %q", r.Subject))
	}
	if strings.ContainsAny(r.Subject, "\n\r") {
		bad = append(bad, "subject contains a newline")
	}

	if !strings.HasPrefix(r.Body, "Hi ") {
		bad = append(bad, fmt.Sprintf("body does not open with a greeting: %q", head(r.Body, 40)))
	}
	if !strings.Contains(r.Body, campaign.Signature) {
		bad = append(bad, "body has no sign-off")
	}
	if strings.Contains(r.Body, "—") || strings.Contains(r.Body, "–") {
		bad = append(bad, "body contains a dash we do not use")
	}
	if len(r.Body) < campaign.BodyMin {
		bad = append(bad, fmt.Sprintf("body suspiciously short (%d chars)", len(r.Body)))
	}

	lowerHTML := strings.ToLower(r.HTMLBody)
	for _, marker := range campaign.RequiredHTML {
		if !strings.Contains(lowerHTML, strings.ToLower(marker)) {
			bad = append(bad, fmt.Sprintf("html is missing %q", marker))
		}
	}
	if n := len(r.HTMLBody); n < campaign.HTMLMin || n > campaign.HTMLMax {
		bad = append(bad, fmt.Sprintf("html length %d outside %d-%d — truncated or bloated",
			n, campaign.HTMLMin, campaign.HTMLMax))
	}
	// the button a human clicks: an anchor pointing at the app whose text is the label. A check for
	// the bare URL passed once while the anchor itself had been deleted.
	if !ctaAnchor.MatchString(r.HTMLBody) {
		bad = append(bad, "no clickable CTA anchor pointing at kairosapp.tech")
	}

	for _, tag := range []string{"table", "td"} {
		open := strings.Count(r.HTMLBody, "<"+tag)
		closed := strings.Count(r.HTMLBody, "</"+tag+">")
		if open != closed {
			bad = append(bad, fmt.Sprintf("<%s> tags unbalanced: %d open, %d close", tag, open, closed))
		}
	}

	haystack := strings.ToLower(r.Subject + "\n" + r.Body + "\n" + r.HTMLBody)
	for _, word := range campaign.Forbidden {
		if strings.Contains(haystack, word) {
			bad = append(bad, fmt.Sprintf("forbidden string present: %q", word))
		}
	}

	if r.Hash != "" && r.Hash != ContentHash(r) {
		bad = append(bad, "content hash mismatch — the queue was edited after it was built")
	}

	return bad
}

// CheckQueue holds the whole-queue rules.
func CheckQueue(rows []Row) []string {
	var bad []string
	seen := map[string]int{}
	for i, r := range rows {
		to := strings.ToLower(strings.TrimSpace(r.Addr()))
		if j, ok := seen[to]; ok {
			bad = append(bad, fmt.Sprintf("duplicate address %s at rows %d and %d", to, j, i))
		}
		seen[to] = i
	}
	return bad
}

// AllowedHosts is every host a link may point at. A tracking pixel or a shortener showing up here
// is a change nobody reviewed, and it is what turns a 10/10 mail-tester score into a spam folder.
var AllowedHosts = []string{"kairosapp.tech"}

var (
	hrefRe = regexp.MustCompile(`(?i)href="([^"]+)"`)
	// RE2 has no lookahead, so the Python's <img src="https?://(?!kairosapp\.tech) becomes: find
	// every img src, then compare its host the same way a link is compared.
	imgSrcRe = regexp.MustCompile(`(?i)<img[^>]+src="([^"]+)"`)
)

func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func hostAllowed(host string) bool {
	for _, a := range AllowedHosts {
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

func CheckLinks(html string) []string {
	var bad []string
	seen := map[string]bool{}
	for _, m := range hrefRe.FindAllStringSubmatch(html, -1) {
		h := strings.TrimSpace(m[1])
		if seen[h] {
			continue
		}
		seen[h] = true
		if h == "" || strings.HasPrefix(h, "#") || strings.HasPrefix(strings.ToLower(h), "mailto:") {
			continue
		}
		// a substring test passes https://evil.example/?ref=kairosapp.tech. Compare the host.
		if !hostAllowed(hostOf(h)) {
			bad = append(bad, "link to an unapproved host: "+h)
		}
		if strings.HasPrefix(strings.ToLower(h), "http://") {
			bad = append(bad, "plaintext http link: "+h)
		}
	}
	for _, m := range imgSrcRe.FindAllStringSubmatch(html, -1) {
		src := strings.TrimSpace(m[1])
		low := strings.ToLower(src)
		if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
			continue
		}
		if !hostAllowed(hostOf(src)) {
			bad = append(bad, "remote image from an unapproved host — reads as a tracking pixel")
			break
		}
	}
	return bad
}

var styleBlock = regexp.MustCompile(`(?s)<style>.*?</style>`)

// CheckTemplate validates the template itself, before anything is rendered from it.
func CheckTemplate(tpl string) []string {
	var bad []string
	for _, slot := range campaign.Slots {
		if !strings.Contains(tpl, slot) {
			bad = append(bad, fmt.Sprintf("template is missing %s — the per-contact line would silently vanish", slot))
		}
	}
	for _, word := range campaign.TemplateForbidden {
		if strings.Contains(tpl, word) {
			bad = append(bad, fmt.Sprintf("template still carries %q", word))
		}
	}
	if !styleBlock.MatchString(tpl) {
		bad = append(bad, "template has no <style> block to carry into the fragment")
	}
	return bad
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
