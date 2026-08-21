package build

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"kmail/addresses"
	"kmail/campaign"
	"kmail/preflight"
)

// The list pipeline: dedupe against everything already mailed or held -> drop the roles and
// industries that make the copy false -> cap at three people per company -> MX-check the surviving
// domains -> render, seal and validate.
//
// The MX pass is the cheap half of the deliverability fix: 57% of the last run's bounces were
// domains with no DNS at all.

// LoadExclusions is every address that must not be mailed again: sent, held in review, or bounced. An empty set would re-mail the entire campaign, so the caller asserts it.
func LoadExclusions() map[string]bool {
	out := map[string]bool{}
	for _, path := range []string{campaign.Contacted, campaign.Ledger, campaign.Held,
		campaign.Bounced} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\n")
			if strings.TrimSpace(line) == "" {
				continue
			}
			parts := strings.Split(line, "\t")
			v := parts[0]
			if len(parts) >= 3 {
				v = parts[2]
			}
			out[strings.ToLower(strings.TrimSpace(v))] = true
		}
		f.Close()
	}
	return out
}

type csvRow map[string]string

func pick(r csvRow, names ...string) string {
	for _, n := range names {
		for k, v := range r {
			if strings.ReplaceAll(strings.ToLower(strings.TrimSpace(k)), " ", "_") == n {
				if s := strings.TrimSpace(v); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func readCSV(path string) ([]csvRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	head := recs[0]
	// strip a UTF-8 BOM off the first header, the way Python's utf-8-sig did
	if len(head) > 0 {
		head[0] = strings.TrimPrefix(head[0], "\uFEFF")
	}
	var out []csvRow
	for _, rec := range recs[1:] {
		m := csvRow{}
		for i, h := range head {
			if i < len(rec) {
				m[h] = rec[i]
			}
		}
		out = append(out, m)
	}
	return out, nil
}

func SourceList() (string, error) {
	for _, name := range campaign.SourceLists {
		p := filepath.Join(campaign.Lists, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no source list in %s — export the sheet tab as CSV first", campaign.Lists)
}

var firstNameRe = regexp.MustCompile(`^[A-Za-zÀ-ÿ][A-Za-zÀ-ÿ'-]+$`)

// Candidates applies every filter that does not cost anything.
func Candidates(rows []csvRow, exclude map[string]bool, requireVideo bool) []Contact {
	var out []Contact
	seen := map[string]bool{}
	for _, r := range rows {
		if strings.TrimSpace(r["status"]) != "" {
			continue
		}
		email := strings.ToLower(pick(r, "email", "email_address", "work_email"))
		if email == "" || !strings.Contains(email, "@") || exclude[email] || seen[email] {
			continue
		}
		local, domain, _ := strings.Cut(email, "@")
		if campaign.DropEmail.MatchString(email) || campaign.NoiseLocal.MatchString(local) {
			continue
		}
		company := pick(r, "company", "company_name", "organization", "account_name")
		title := pick(r, "title", "job_title", "position")
		switch strings.ToLower(title) {
		case "(no value)", "none", "n/a", "-":
			title = ""
		}
		// only a name the list actually carries; never one guessed from the address
		first := pick(r, "first_name", "firstname", "given_name")
		if !firstNameRe.MatchString(first) {
			first = ""
		}
		if campaign.DropTitle.MatchString(title) || campaign.DropDomain.MatchString(domain) ||
			campaign.DropCompany.MatchString(company) {
			continue
		}
		if requireVideo && !campaign.VideoSignal.MatchString(company+" "+domain+" "+title) {
			continue
		}
		seen[email] = true
		name := company
		if name == "" {
			name = domain
		}
		out = append(out, Contact{
			Email: email, FirstName: first, Company: name,
			SafeCompany: CleanCompany(company), Title: title, Domain: domain,
		})
	}
	return out
}

// LoadBlanked is every company name a review has ever struck out of the copy. Append-only and
// matched case-insensitively, because the list writes the same company three different ways.
func LoadBlanked() map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(campaign.Blanked)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if s := strings.ToLower(strings.TrimSpace(sc.Text())); s != "" {
			out[s] = true
		}
	}
	return out
}

type BuildOptions struct {
	Target       int
	RequireVideo bool
}

// Build replaces the queue from the master CSV. It takes the lock, so it cannot run while a send is
// in flight — rebuilding from a stale view of what had gone out is what double-mailed a real
// prospect on 2026-08-20.
func Build(opt BuildOptions, log func(string, ...any)) error {
	lock, err := campaign.TryLock()
	if err != nil {
		return err
	}
	defer lock.Close()

	tpl, err := LoadTemplate()
	if err != nil {
		return err
	}

	exclude := LoadExclusions()
	if len(exclude) == 0 {
		return fmt.Errorf("contacted.txt, ledger.tsv, held.txt and bounced.txt are all empty — " +
			"refusing to build a queue with no exclusion list, that would re-mail the entire campaign")
	}
	log("excluding %d already mailed or held; rebuilding the queue from scratch", len(exclude))

	src, err := SourceList()
	if err != nil {
		return err
	}
	log("source: %s", src)
	rows, err := readCSV(src)
	if err != nil {
		return err
	}
	log("source rows: %d", len(rows))

	// a contact already mailed still occupies one of its company's three slots
	prior := map[string]int{}
	for _, r := range rows {
		switch strings.TrimSpace(r["status"]) {
		case "SENT", "BOUNCED", "HELD":
			if k := strings.ToLower(strings.TrimSpace(pick(r, "company", "company_name", "organization"))); k != "" {
				prior[k]++
			}
		}
	}

	cands := Candidates(rows, exclude, opt.RequireVideo)
	log("after dedupe + relevance: %d", len(cands))

	// a name struck out in an earlier review stays struck out: review is a decision about the
	// campaign, not about one batch
	if blanked := LoadBlanked(); len(blanked) > 0 {
		n := 0
		for i := range cands {
			if cands[i].SafeCompany != "" && blanked[strings.ToLower(cands[i].SafeCompany)] {
				cands[i].SafeCompany = ""
				n++
			}
		}
		log("blanked by earlier reviews: %d of %d contacts keep the no-company wording", n, len(cands))
	}

	per := map[string]int{}
	for k, v := range prior {
		per[k] = v
	}
	var capped []Contact
	for _, c := range cands {
		key := strings.ToLower(strings.TrimSpace(orDomain(c)))
		if per[key] >= campaign.CapPerCompany {
			continue
		}
		per[key]++
		capped = append(capped, c)
	}
	log("after %d-per-company cap: %d", campaign.CapPerCompany, len(capped))

	// gate one, free: a domain with no MX cannot receive anything
	pool := capped
	if n := opt.Target * 3; len(pool) > n {
		pool = pool[:n]
	}
	domSet := map[string]bool{}
	var doms []string
	for _, c := range pool {
		if !domSet[c.Domain] {
			domSet[c.Domain] = true
			doms = append(doms, c.Domain)
		}
	}
	sort.Strings(doms)
	alive := addresses.MXAlive(doms, log)
	dead := 0
	for _, ok := range alive {
		if !ok {
			dead++
		}
	}
	log("  no MX: %d domains dropped", dead)
	var survived []Contact
	for _, c := range pool {
		if alive[c.Domain] {
			survived = append(survived, c)
		}
	}

	final := survived
	if len(final) > opt.Target {
		final = final[:opt.Target]
	}
	log("final batch: %d", len(final))
	if len(final) == 0 {
		return fmt.Errorf("nothing to build")
	}

	// a rebuild replaces the queue rather than adding to it, so last run's drafts are cleared first.
	// Excluding them instead made the second build return four rows out of 259.
	if err := os.MkdirAll(campaign.Drafts, 0o755); err != nil {
		return err
	}
	for _, pat := range []string{"*.html", "*.txt"} {
		matches, _ := filepath.Glob(filepath.Join(campaign.Drafts, pat))
		for _, m := range matches {
			os.Remove(m)
		}
	}

	var payloads []preflight.Row
	used := map[string]bool{}
	for i, c := range final {
		row, doc, err := RenderRow(tpl, c, campaign.Subjects[i%len(campaign.Subjects)], nil)
		if err != nil {
			return err
		}
		base := slug(c.Company) + "__" + slug(strings.Split(c.Email, "@")[0])
		n, k := base, 2
		for used[n] {
			n = fmt.Sprintf("%s-%d", base, k)
			k++
		}
		used[n] = true
		row.HTMLFile = n + ".html"
		if err := os.WriteFile(filepath.Join(campaign.Drafts, n+".html"), []byte(doc), 0o644); err != nil {
			return err
		}
		payloads = append(payloads, row)
	}

	problems := preflight.CheckQueue(payloads)
	for i, p := range payloads {
		for _, msg := range preflight.CheckRow(p) {
			problems = append(problems, fmt.Sprintf("row %d (%s): %s", i, p.Addr(), msg))
		}
	}
	if len(problems) > 0 {
		log("\nPREFLIGHT FAILED — %d problem(s). queue.jsonl NOT written.", len(problems))
		for i, msg := range problems {
			if i == 25 {
				break
			}
			log("   %s", msg)
		}
		return fmt.Errorf("preflight rejected the build")
	}

	if err := WriteQueue(payloads); err != nil {
		return err
	}
	named, shapes := 0, map[string]bool{}
	greeted := 0
	for _, p := range payloads {
		if p.Name != "" {
			named++
		}
		shapes[p.Shape] = true
		if !strings.HasPrefix(p.Body, "Hi there,") {
			greeted++
		}
	}
	log("queue.jsonl written and sealed: %d rows", len(payloads))
	log("greeted by name : %d", greeted)
	log("company named   : %d", named)
	log("shapes in use   : %d", len(shapes))
	log("\nnothing is sendable until `kmail review` approves the copy.")
	return nil
}

func orDomain(c Contact) string {
	if c.Company != "" {
		return c.Company
	}
	return c.Domain
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	out := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(out) > 40 {
		out = out[:40]
	}
	if out == "" {
		return "contact"
	}
	return out
}
