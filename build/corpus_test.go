package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"kmail/campaign"
	"kmail/preflight"
)

// The 257 rows already in queue.jsonl are the fixture. They were produced by the Python, sealed,
// and reviewed against real prospects — so proving the Go agrees with them proves the port against
// production data rather than against fixtures invented for the occasion.
//
// The python-*.json files live in ~/outreach/fixtures, deliberately outside this source tree: they
// contain real prospect addresses and email bodies, and the tool is published to a repository. They
// are the Python implementation's own answers, captured on 2026-08-21 before it was deleted. They cannot be regenerated: the Python is gone, and that is the
// point — they are the only surviving record of what 76 real recipients were sent under. Changing a
// rule on purpose means re-baselining them from the Go and saying so in the commit.

func TestMain(m *testing.M) {
	_ = campaign.LoadIdentity()
	os.Exit(m.Run())
}

func corpus(t *testing.T) []preflight.Row {
	t.Helper()
	rows, err := ReadQueue()
	if err != nil {
		t.Fatalf("reading the queue: %v", err)
	}
	if len(rows) == 0 {
		t.Skipf("no queue at %s — nothing to check against", campaign.Queue)
	}
	return rows
}

func loadJSON(t *testing.T, name string, into any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(campaign.Home, "fixtures", name))
	if err != nil {
		t.Skipf("no %s fixture: %v", name, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// 1. every seal the Python wrote must verify under the Go hash.
func TestCorpusHashes(t *testing.T) {
	for _, r := range corpus(t) {
		if r.Hash == "" {
			t.Errorf("%s has no hash", r.Addr())
			continue
		}
		if got := preflight.ContentHash(r); got != r.Hash {
			t.Errorf("%s: hash %s, want %s", r.Addr(), got, r.Hash)
		}
	}
}

// 2. the one that matters most: re-deriving the plain text from each row's own HTML must reproduce
// the stored body byte for byte. Everything downstream is hashed off this.
func TestCorpusToText(t *testing.T) {
	bad := 0
	for _, r := range corpus(t) {
		got, err := ToText(r.HTMLBody)
		if err != nil {
			t.Errorf("%s: %v", r.Addr(), err)
			bad++
			continue
		}
		if got != r.Body {
			bad++
			if bad <= 3 {
				t.Errorf("%s: to_text differs\n%s", r.Addr(), firstDiff(r.Body, got))
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of the corpus differ", bad)
	}
}

func firstDiff(want, got string) string {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		if want[i] != got[i] {
			lo := i - 60
			if lo < 0 {
				lo = 0
			}
			return "  at byte " + itoa(i) + "\n  python: " + snippet(want, lo, i) +
				"\n  go    : " + snippet(got, lo, i)
		}
	}
	return "  same prefix, lengths " + itoa(len(want)) + " (python) vs " + itoa(len(got)) + " (go)"
}

func snippet(s string, lo, i int) string {
	hi := i + 60
	if hi > len(s) {
		hi = len(s)
	}
	return strings.ReplaceAll(s[lo:hi], "\n", "\\n")
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// 3. shape and company name, derived, must match what the Python derived.
func TestCorpusAttribute(t *testing.T) {
	rows := corpus(t)
	var want []struct {
		To    string `json:"to"`
		Shape string `json:"shape"`
		Name  string `json:"name"`
		OK    bool   `json:"ok"`
	}
	loadJSON(t, "python-attribute.json", &want)
	byAddr := map[string]preflight.Row{}
	for _, r := range rows {
		byAddr[strings.ToLower(r.Addr())] = r
	}
	// A row blanked in review was legitimately re-rendered after this fixture was captured, so its
	// body no longer carries the name the fixture records. Those are skipped and counted rather
	// than re-baselined away: the point of the fixture is that it predates the Go.
	reRendered, checked := 0, 0
	for _, w := range want {
		r, ok := byAddr[strings.ToLower(w.To)]
		if !ok {
			continue // that contact is no longer queued
		}
		if w.Name != "" && !strings.Contains(r.Body, w.Name) {
			reRendered++
			continue
		}
		checked++
		// the stored shape/name are ignored on purpose: the gate must recompute
		bare := r
		bare.Shape, bare.Name = "", ""
		shape, name, ok := Attribute(bare)
		if shape != w.Shape || name != w.Name || ok != w.OK {
			t.Errorf("%s: go(%s,%q,%v) python(%s,%q,%v)", r.Addr(), shape, name, ok, w.Shape, w.Name, w.OK)
		}
	}
	t.Logf("compared %d rows still in the queue against the pre-Go fixture; %d re-rendered since, "+
		"%d no longer queued", checked, reRendered, len(want)-checked-reRendered)
	if checked == 0 {
		t.Skip("no fixture row is still in the queue — nothing to compare")
	}
}

// clean_company over every distinct value the real list holds.
func TestCorpusCleanCompany(t *testing.T) {
	want := map[string]string{}
	loadJSON(t, "python-cleancompany.json", &want)
	if len(want) == 0 {
		t.Skip("no fixture")
	}
	for in, expect := range want {
		if got := CleanCompany(in); got != expect {
			t.Errorf("CleanCompany(%q) = %q, python said %q", in, got, expect)
		}
	}
}

// 4. the same rules must fire on the same rows.
func TestCorpusPreflight(t *testing.T) {
	rows := corpus(t)
	var want []struct {
		To       string   `json:"to"`
		Problems []string `json:"problems"`
	}
	loadJSON(t, "python-preflight.json", &want)
	byAddr := map[string]preflight.Row{}
	for _, r := range rows {
		byAddr[strings.ToLower(r.Addr())] = r
	}
	checked := 0
	for _, w := range want {
		r, ok := byAddr[strings.ToLower(w.To)]
		if !ok {
			continue
		}
		checked++
		got := ruleKeys(preflight.CheckRow(r))
		expect := norm(w.Problems)
		if !equal(got, expect) {
			t.Errorf("%s: go %v, python %v", r.Addr(), got, expect)
		}
	}
	if checked == 0 {
		t.Skip("no fixture row is still in the queue")
	}
	t.Logf("checked %d rows still in the queue", checked)
}

// and the deliberately broken rows: each mutation must be rejected for the same reason.
func TestCorpusMutations(t *testing.T) {
	var muts map[string]preflight.Row
	var want map[string][]string
	loadJSON(t, "mutations.json", &muts)
	loadJSON(t, "python-mutations.json", &want)
	for label, row := range muts {
		got := ruleKeys(preflight.CheckRow(row))
		expect := norm(want[label])
		if !equal(got, expect) {
			t.Errorf("%s: go %v, python %v", label, got, expect)
		}
	}
}

// The Python and Go phrase a problem differently past the colon (%q against repr, runes against
// characters). The rule that fired is what has to agree, so compare the text before the colon.
func ruleKeys(problems []string) []string {
	var out []string
	for _, p := range problems {
		k := p
		if i := strings.Index(p, ":"); i >= 0 {
			k = p[:i]
		}
		out = append(out, k)
	}
	return norm(out)
}

// Go's %q and Python's repr disagree on the quote character, and a marker such as
// "https://kairosapp.tech" carries the colon the key was cut at. Both sides go through this.
func norm(keys []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		k = strings.ReplaceAll(k, "'", "\"")
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
