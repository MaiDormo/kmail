// kmail — the KAIROS outreach campaign, one command.
//
//	kmail sandbox                 a throwaway copy, to try all of this on
//	kmail status                  where the campaign stands
//	kmail build --count 300       rebuild the queue from the master CSV
//	kmail review                  the human gate. TUI, or --file for $EDITOR
//	kmail preview --n 3           render at phone and desktop width, and open it
//	kmail check                   template, preflight, links, unit tests
//	kmail send                    dry run unless --send is given
//	kmail one <address>           one message to somebody not on the list
//	kmail verify                  reconcile the ledger against Gmail
//
// Nothing is sent that `kmail review` has not approved, and review needs a terminal.
package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"kmail/addresses"
	"kmail/build"
	"kmail/campaign"
	"kmail/preflight"
	"kmail/review"
	"kmail/send"
)

func main() { os.Exit(run(os.Args[1:])) }

func logf(format string, a ...any) { fmt.Printf(format+"\n", a...) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}
	// the identity is campaign data, not source: without it the tool still reports and checks,
	// but there is nobody to send as
	idErr := campaign.LoadIdentity()
	cmd, rest := args[0], args[1:]
	if idErr != nil {
		switch cmd {
		case "send", "review", "build", "verify", "one":
			fmt.Fprintf(os.Stderr, "no campaign identity: %v\n\n"+
				"Copy campaign.example.json to %s/campaign.json and fill it in.\n", idErr, campaign.Home)
			return 1
		}
	}
	switch cmd {
	case "status":
		return cmdStatus()
	case "build":
		return cmdBuild(rest)
	case "review":
		return cmdReview(rest)
	case "preview":
		return cmdPreview(rest)
	case "check":
		return cmdCheck()
	case "send":
		return cmdSend(rest)
	case "verify":
		return cmdVerify(rest)
	case "one":
		return cmdOne(rest)
	case "sandbox":
		return cmdSandbox(rest)
	case "-h", "--help", "help":
		usage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
	usage()
	return 1
}

func usage() {
	fmt.Print(`kmail — the KAIROS outreach campaign, one command.

  kmail sandbox                 a throwaway copy, to try all of this on
  kmail status                  where the campaign stands
  kmail build --count 300       rebuild the queue from the master CSV
  kmail review [--file]         the human gate. TUI, or $EDITOR with --file
  kmail preview --n 3           render at phone and desktop width, and open it
  kmail check                   template, preflight, links
  kmail send [--send]           dry run unless --send is given
  kmail one <address>           one message to somebody not on the list
  kmail verify [--write]        reconcile the ledger against Gmail

Exit codes: 0 clean · 1 config · 2 refused, nothing sent · 3 part-way · 4 locked · 5 cap reached
`)
}

// ---------------------------------------------------------------- status

func cmdStatus() int {
	rows, err := build.ReadQueue()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logf("queue            : %d rows", len(rows))
	if len(rows) == 0 {
		logf("  run `kmail build` first.")
		return 0
	}
	p := review.Collect(rows)
	flagged := 0
	for _, n := range p.Names {
		if n.Why != "" {
			flagged++
		}
	}
	logf("opener shapes    : %d", len(p.Shapes))
	logf("company names    : %d, %d doubtful", len(p.Names), flagged)
	shown := 0
	for _, n := range p.Names {
		if n.Why == "" || shown == 6 {
			continue
		}
		logf("    %-34s %3d  %s", trunc(n.Name, 34), n.Count, n.Why)
		shown++
	}
	if flagged > 6 {
		logf("    ... and %d more", flagged-6)
	}

	if a, _ := review.LoadApprovals(); a != nil {
		logf("\napproved         : %s  %d shapes, %d names", a.ApprovedAt, len(a.Shapes), len(a.Names))
	}
	refusals := review.Gate(rows)
	state := "OPEN"
	if len(refusals) > 0 {
		state = "CLOSED"
	}
	logf("gate             : %s", state)
	for i, r := range refusals {
		if i == 5 {
			break
		}
		logf("    %s", r)
	}

	settled, inDoubt := send.ReadLedger()
	age := campaign.DomainAgeDays(time.Now())
	logf("\nsent, all time   : %d", len(settled))
	logf("in doubt         : %d   (interrupted mid-send, never retried)", len(inDoubt))
	logf("daily cap        : %d   (domain is %d days old)", campaign.DailyCap(age), age)
	return 0
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---------------------------------------------------------------- build

func cmdBuild(args []string) int {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	count := fs.Int("count", 300, "how many rows to build")
	allRows := fs.Bool("all-rows", false,
		"skip the relevance gate — will happily tell a venture fund their archive is expensive to search")
	fs.Parse(args)

	err := build.Build(build.BuildOptions{Target: *count, RequireVideo: !*allRows}, logf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n%v\n", err)
		if err == campaign.ErrLocked {
			return 4
		}
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- review

func cmdReview(args []string) int {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	useFile := fs.Bool("file", false, "review in $EDITOR instead of the TUI")
	fs.Parse(args)

	// an agent must not be able to approve copy on your behalf
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintln(os.Stderr, "review needs a terminal. It is the one step a human has to do.")
		return 1
	}

	lock, err := campaign.TryLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	defer lock.Close()

	rows, err := build.ReadQueue()
	if err != nil || len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no queue. Run `kmail build` first.")
		return 1
	}
	p := review.Collect(rows)
	if un := p.Unattributable(); len(un) > 0 {
		fmt.Fprintf(os.Stderr, "%d row(s) carry copy this campaign did not produce, so no approval "+
			"can cover them:\n", len(un))
		for i, a := range un {
			if i == 10 {
				break
			}
			fmt.Fprintf(os.Stderr, "  %s\n", a)
		}
		fmt.Fprintln(os.Stderr, "rebuild the queue with `kmail build`.")
		return 1
	}

	if *useFile {
		if err := review.EditPlan(p); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			if _, cancelled := err.(review.ErrCancelled); cancelled {
				return 1
			}
			return 2
		}
	} else {
		approved, err := review.RunTUI(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if !approved {
			fmt.Println("cancelled. Nothing written.")
			return 1
		}
	}

	if err := review.Apply(p, logf); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}
	return 0
}

// ---------------------------------------------------------------- preview

func cmdPreview(args []string) int {
	fs := flag.NewFlagSet("preview", flag.ExitOnError)
	n := fs.Int("n", 3, "how many to render")
	noOpen := fs.Bool("no-open", false, "write the files but do not open a browser")
	fs.Parse(args)

	rows, err := build.ReadQueue()
	if err != nil || len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no queue.")
		return 1
	}
	out := "/tmp/kmail-preview"
	if err := os.MkdirAll(out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *n > len(rows) {
		*n = len(rows)
	}
	var frames strings.Builder
	for i, r := range rows[:*n] {
		doc := ""
		if r.HTMLFile != "" {
			if b, err := os.ReadFile(filepath.Join(campaign.Drafts, r.HTMLFile)); err == nil {
				doc = string(b)
			}
		}
		if doc == "" {
			doc = `<!doctype html><html><head><meta charset="utf-8"></head><body>` + r.HTMLBody + `</body></html>`
		}
		if err := os.WriteFile(fmt.Sprintf("%s/%d.html", out, i), []byte(doc), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		shape, name, _ := build.Attribute(r)
		if name == "" {
			name = "no company named"
		}
		label := fmt.Sprintf("%s  ·  shape %s  ·  %s", r.Addr(), shape, name)
		for _, w := range []struct {
			px   int
			kind string
		}{{375, "phone"}, {600, "desktop"}} {
			fmt.Fprintf(&frames,
				`<figure><figcaption>%s %dpx — %s</figcaption>`+
					`<iframe src="%d.html" width="%d" height="900" loading="lazy"></iframe></figure>`,
				w.kind, w.px, label, i, w.px)
		}
	}
	index := `<!doctype html><meta charset="utf-8"><title>kmail preview</title>` +
		`<style>body{background:#222;color:#ddd;font:13px/1.5 system-ui;margin:20px}` +
		`figure{display:inline-block;margin:0 20px 30px 0;vertical-align:top}` +
		`figcaption{margin-bottom:6px;font-size:11px;color:#9ab}` +
		`iframe{border:1px solid #444;background:#fff}</style>` +
		`<h1>kmail preview</h1>` + frames.String()
	idx := out + "/index.html"
	if err := os.WriteFile(idx, []byte(index), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logf("%d rendered to %s", *n, idx)
	if !*noOpen {
		exec.Command("open", idx).Run()
	}
	return 0
}

// ---------------------------------------------------------------- check

func cmdCheck() int {
	fails := 0

	tplBytes, err := os.ReadFile(campaign.Template)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tpl := string(tplBytes)
	problems := preflight.CheckTemplate(tpl)
	logf("template         : %s", okFail(len(problems) == 0))
	for _, p := range problems {
		logf("    %s", p)
	}
	fails += len(problems)

	if repo, err := os.ReadFile(campaign.RepoTemplate); err == nil {
		same := string(repo) == tpl
		if same {
			logf("repo copy        : identical")
		} else {
			logf("repo copy        : DIFFERS — one of them is stale")
			fails++
		}
	} else {
		logf("repo copy        : not found, skipped")
	}

	rows, _ := build.ReadQueue()
	if len(rows) > 0 {
		problems = preflight.CheckQueue(rows)
		for i, r := range rows {
			for _, p := range preflight.CheckRow(r) {
				problems = append(problems, fmt.Sprintf("row %d (%s): %s", i, r.Addr(), p))
			}
		}
		logf("preflight        : %d rows, %d problem(s)", len(rows), len(problems))
		for i, p := range problems {
			if i == 10 {
				break
			}
			logf("    %s", p)
		}
		fails += len(problems)

		var links []string
		seen := map[string]bool{}
		for i, r := range rows {
			if i == 20 {
				break
			}
			for _, l := range preflight.CheckLinks(r.HTMLBody) {
				if !seen[l] {
					seen[l] = true
					links = append(links, l)
				}
			}
		}
		logf("links            : %s", okFail(len(links) == 0))
		for _, l := range links {
			logf("    %s", l)
		}
		fails += len(links)

		// the plain text is what gets hashed, so it must still derive from the html it ships with
		drift := 0
		for _, r := range rows {
			if got, err := build.ToText(r.HTMLBody); err != nil || got != r.Body {
				drift++
			}
		}
		logf("text matches html: %s", okFail(drift == 0))
		fails += drift
	}

	logf("\n%s", map[bool]string{true: "PASS", false: fmt.Sprintf("FAIL — %d problem(s)", fails)}[fails == 0])
	if fails == 0 {
		logf("(the unit and corpus tests live in `go test ./...`)")
		return 0
	}
	return 1
}

func okFail(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAIL"
}

// ---------------------------------------------------------------- send

func cmdSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	doSend := fs.Bool("send", false, "actually transmit")
	count := fs.Int("count", 50, "")
	maxPerDay := fs.Int("max-per-day", 0, "default comes from the warm-up ramp for the domain age")
	delay := fs.Float64("delay", 4.0, "seconds between messages, before jitter")
	toSelf := fs.Bool("to-self", false, "one message to the reviewer instead, for a rendering check")
	only := fs.String("only", "", "send to just this queued address — a real send in every respect")
	to := fs.String("to", "", "redirect a copy somewhere else for a look; not recorded, and refused "+
		"if the address is in the queue")
	fs.Parse(args)

	return send.Run(send.Options{
		Send: *doSend, Count: *count, MaxPerDay: *maxPerDay,
		Delay: time.Duration(*delay * float64(time.Second)), ToSelf: *toSelf,
		Only: *only, To: *to,
	}, os.Stdout, os.Stderr)
}

// ---------------------------------------------------------------- one

func cmdOne(args []string) int {
	fs := flag.NewFlagSet("one", flag.ExitOnError)
	first := fs.String("first-name", "", "greet them by name; omitted gives \"Hi there,\"")
	title := fs.String("title", "", "their job title, which picks the opener")
	company := fs.String("company", "", "only accepted if that name was already approved")
	subject := fs.Int("subject", 0, "which approved subject to use, 0-3")
	doSend := fs.Bool("send", false, "actually transmit")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: kmail one [flags] <address>")
		return 1
	}
	addr := strings.TrimSpace(fs.Arg(0))

	tpl, err := build.LoadTemplate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	domain := ""
	if _, d, ok := strings.Cut(addr, "@"); ok {
		domain = d
	}
	// the same free gate the queue gets: a domain with no MX cannot receive anything
	if alive := addresses.MXAlive([]string{domain}, logf); !alive[domain] {
		fmt.Fprintf(os.Stderr, "\n%s has no MX record — nothing can be delivered there.\n", domain)
		return 1
	}
	if *subject < 0 || *subject >= len(campaign.Subjects) {
		fmt.Fprintf(os.Stderr, "--subject must be 0..%d\n", len(campaign.Subjects)-1)
		return 1
	}

	c := build.Contact{
		Email: addr, FirstName: *first, Company: *company,
		SafeCompany: build.CleanCompany(*company), Title: *title, Domain: domain,
	}
	row, _, err := build.RenderRow(tpl, c, campaign.Subjects[*subject], nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return send.One(row, send.Options{Send: *doSend}, os.Stdout, os.Stderr)
}

// ---------------------------------------------------------------- verify

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	write := fs.Bool("write", false, "also update the master CSVs")
	fs.Parse(args)

	settled, inDoubt := send.ReadLedger()
	bounced := loadSet(campaign.Bounced)
	contacted := loadSet(campaign.Contacted)
	logf("ledger SENT        : %d", len(settled))
	logf("ledger in doubt    : %d   (interrupted mid-send, never retried)", len(inDoubt))
	logf("contacted.txt      : %d", len(contacted))
	logf("bounced            : %d", len(bounced))

	gmailPath := filepath.Join(campaign.Home, "gmail_sent.txt")
	if gmail := loadSet(gmailPath); len(gmail) > 0 {
		var phantom, unlogged []string
		for a := range settled {
			if !gmail[a] {
				phantom = append(phantom, a)
			}
		}
		for a := range gmail {
			if !settled[a] && a != strings.ToLower(campaign.Reviewer) {
				unlogged = append(unlogged, a)
			}
		}
		sort.Strings(phantom)
		sort.Strings(unlogged)
		logf("\ngmail_sent.txt     : %d", len(gmail))
		logf("logged, never sent : %d", len(phantom))
		for i, a := range phantom {
			if i == 10 {
				break
			}
			logf("   %s", a)
		}
		logf("sent, never logged : %d", len(unlogged))
		for i, a := range unlogged {
			if i == 10 {
				break
			}
			logf("   %s", a)
		}
		if len(phantom) > 0 {
			logf("\n  phantom entries must be removed from ledger.tsv or those people are never mailed")
		}
	} else {
		logf("\nno %s — skipping the Gmail comparison.", gmailPath)
		logf("  export recipients of in:sent to that file to enable it")
	}

	if !*write {
		logf("\nreport only. Add --write to update the master CSVs.")
		return 0
	}

	today := time.Now().UTC().Format("2006-01-02")
	known := map[string]bool{}
	for a := range settled {
		known[a] = true
	}
	for a := range contacted {
		known[a] = true
	}
	for _, name := range campaign.MasterCSVs {
		path := filepath.Join(campaign.Lists, name)
		n, err := markCSV(path, known, bounced, today)
		if err != nil {
			logf("%s: %v", name, err)
			continue
		}
		logf("%s: %d rows marked", name, n)
	}
	return 0
}

func markCSV(path string, known, bounced map[string]bool, today string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("not found, skipped")
	}
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	f.Close()
	if err != nil || len(recs) == 0 {
		return 0, err
	}
	head := recs[0]
	head[0] = strings.TrimPrefix(head[0], "\uFEFF")
	col := map[string]int{}
	for i, h := range head {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	iEmail, ok := col["email"]
	if !ok {
		return 0, fmt.Errorf("no email column")
	}
	n := 0
	for _, rec := range recs[1:] {
		if iEmail >= len(rec) {
			continue
		}
		e := strings.ToLower(strings.TrimSpace(rec[iEmail]))
		if !known[e] {
			continue
		}
		set := func(name, value string) {
			if i, ok := col[name]; ok && i < len(rec) {
				if name != "date_sent" || strings.TrimSpace(rec[i]) == "" {
					rec[i] = value
				}
			}
		}
		status := "SENT"
		if bounced[e] {
			status = "BOUNCED"
		}
		set("status", status)
		set("date_sent", today)
		set("sent_from", campaign.Sender)
		n++
	}
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	w := csv.NewWriter(out)
	if err := w.WriteAll(recs); err != nil {
		out.Close()
		return 0, err
	}
	w.Flush()
	out.Close()
	return n, os.Rename(tmp, path)
}

func loadSet(path string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		s := strings.ToLower(strings.TrimSpace(strings.Split(sc.Text(), "\t")[0]))
		if s != "" {
			out[s] = true
		}
	}
	return out
}

// ---------------------------------------------------------------- sandbox

func cmdSandbox(args []string) int {
	fs := flag.NewFlagSet("sandbox", flag.ExitOnError)
	dir := fs.String("dir", "/tmp/kmail-sandbox", "where to put it")
	fs.Parse(args)

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var copied []string
	for _, name := range []string{"queue.jsonl", "kairos-campaign-v2-dark.html", "contacted.txt",
		"ledger.tsv", "bounced.txt", "held.txt", "invalid.txt"} {
		b, err := os.ReadFile(filepath.Join(campaign.Home, name))
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(*dir, name), b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		copied = append(copied, name)
	}
	logf("sandbox at %s", *dir)
	logf("copied: %s", strings.Join(copied, ", "))
	logf("not copied: approvals.json — the gate starts closed, which is the part worth trying\n")
	logf("  export KMAIL_HOME=%s", *dir)
	logf("  kmail status && kmail review && kmail send")
	logf("\nunset KMAIL_HOME to go back to the real campaign. Sending from a sandbox still uses")
	logf("the real mailbox, so keep it to dry runs and --to-self.")
	return 0
}
