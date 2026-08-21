// Package send is the only thing that talks to SMTP.
//
// Sending needs --send. Running the command to "see what it does" sends nothing, because that is
// the mistake an operator or an agent is most likely to make.
//
// This package prints addresses, counts and reasons. It never prints a subject, a body or any HTML,
// so an agent driving it cannot end up holding email content in its context. On 2026-08-20 sixteen
// subagents were handed the content and 48 of 63 emails went out wrong; the content is not handed
// out any more.
package send

import (
	"bufio"
	"fmt"
	"math/rand"
	"net/smtp"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode"

	"kmail/build"
	"kmail/campaign"
	"kmail/preflight"
	"kmail/review"
)

// Exit codes. A caller needs no judgement: the number says what happened.
const (
	ExitOK      = 0
	ExitConfig  = 1
	ExitRefused = 2 // preflight or the review gate. Nothing was sent
	ExitPartial = 3
	ExitLocked  = 4
	ExitCap     = 5
)

type Options struct {
	Send      bool
	Count     int
	MaxPerDay int // 0 means take it from the warm-up ramp
	Delay     time.Duration
	ToSelf    bool

	// Only sends just this queued address, a real send in every other respect: the gate applies,
	// the ledger records it, the daily cap counts it.
	Only string
	// To redirects one queued row to somewhere else for a look. The gate does not apply and
	// nothing is recorded, so it must never be an address the campaign would really mail.
	To string

	// Transport is replaced in tests. Nil means real SMTP to Gmail.
	Transport func(row preflight.Row, to string) error
}

// ReadLedger returns what settled and what is in doubt. An INTENT with no matching SENT means the
// run crashed mid-send and we cannot know whether the mail left, so the address is never retried.
func ReadLedger() (settled, inDoubt map[string]bool) {
	settled, intent := map[string]bool{}, map[string]bool{}
	f, err := os.Open(campaign.Ledger)
	if err != nil {
		return settled, map[string]bool{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		p := strings.Split(strings.TrimRight(sc.Text(), "\n"), "\t")
		if len(p) < 3 {
			continue
		}
		addr := strings.ToLower(strings.TrimSpace(p[2]))
		switch p[1] {
		case "SENT":
			settled[addr] = true
		case "INTENT":
			intent[addr] = true
		}
	}
	inDoubt = map[string]bool{}
	for a := range intent {
		if !settled[a] {
			inDoubt[a] = true
		}
	}
	return settled, inDoubt
}

// SentToday counts SENT lines stamped with today's UTC date.
func SentToday(now time.Time) int {
	today := now.UTC().Format("2006-01-02")
	n := 0
	f, err := os.Open(campaign.Ledger)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		p := strings.Split(sc.Text(), "\t")
		if len(p) >= 2 && p[1] == "SENT" && strings.HasPrefix(p[0], today) {
			n++
		}
	}
	return n
}

type ledger struct{ f *os.File }

func openLedger() (*ledger, error) {
	f, err := os.OpenFile(campaign.Ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &ledger{f}, nil
}

// append writes and fsyncs before returning. The two-phase INTENT/SENT pair is only at-most-once if
// the INTENT is durable before the socket is written to.
func (l *ledger) append(state, addr, extra string) error {
	if _, err := fmt.Fprintf(l.f, "%s\t%s\t%s\t%s\n",
		campaign.Stamp(time.Now()), state, addr, extra); err != nil {
		return err
	}
	return l.f.Sync()
}

func (l *ledger) Close() error { return l.f.Close() }

func loadSet(path string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if s := strings.ToLower(strings.TrimSpace(sc.Text())); s != "" {
			out[s] = true
		}
	}
	return out
}

// Run is the whole send. It returns an exit code, never panics on a bad row, and opens no socket
// until preflight and the gate have both passed.
func Run(opt Options, out, errOut *os.File) int {
	p := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	e := func(format string, a ...any) { fmt.Fprintf(errOut, format+"\n", a...) }

	lock, err := campaign.TryLock()
	if err != nil {
		e("\n%v", err)
		return ExitLocked
	}
	defer lock.Close()

	rows, err := build.ReadQueue()
	if err != nil {
		e("\n%v", err)
		return ExitConfig
	}
	if len(rows) == 0 {
		e("\nno queue at %s. Run `kmail build` first.", campaign.Queue)
		return ExitConfig
	}

	// preflight: the entire queue, before any connection is opened
	problems := preflight.CheckQueue(rows)
	for i, r := range rows {
		for _, msg := range preflight.CheckRow(r) {
			problems = append(problems, fmt.Sprintf("row %d (%s): %s", i, r.Addr(), msg))
		}
	}
	if len(problems) > 0 {
		e("PREFLIGHT FAILED — %d problem(s). Nothing sent.\n", len(problems))
		for i, msg := range problems {
			if i == 25 {
				e("  ... and %d more", len(problems)-25)
				break
			}
			e("  %s", msg)
		}
		return ExitRefused
	}

	if opt.ToSelf {
		opt.To = campaign.Reviewer
	}

	// the review gate. a redirected copy goes to you, so it is how you look before approving
	if opt.To == "" {
		if refusals := review.Gate(rows); len(refusals) > 0 {
			e("REFUSED — this copy has not been approved. Nothing sent.\n")
			for i, r := range refusals {
				if i == 15 {
					break
				}
				e("  %s", r)
			}
			return ExitRefused
		}
	}

	settled, inDoubt := ReadLedger()
	skip := map[string]bool{}
	for a := range settled {
		skip[a] = true
	}
	for a := range inDoubt {
		skip[a] = true
	}
	for _, path := range []string{campaign.Contacted, campaign.Held} {
		for a := range loadSet(path) {
			skip[a] = true
		}
	}

	var queue []preflight.Row
	for _, r := range rows {
		if !skip[strings.ToLower(strings.TrimSpace(r.Addr()))] {
			queue = append(queue, r)
		}
	}

	now := time.Now()
	cap_, capNote := opt.MaxPerDay, ""
	if cap_ <= 0 {
		age := campaign.DomainAgeDays(now)
		cap_ = campaign.DailyCap(age)
		if campaign.DailyCapOverride > 0 {
			capNote = fmt.Sprintf(" (daily_cap in campaign.json; the ramp would say %d at %d days old)",
				campaign.RampAt(age), age)
		} else {
			capNote = fmt.Sprintf(" (ramp: %d days since the domain was registered)", age)
		}
	}
	already := SentToday(now)
	room := cap_ - already
	if room < 0 {
		room = 0
	}

	p("queue            : %d", len(rows))
	p("already contacted: %d", len(skip))
	p("eligible         : %d", len(queue))
	p("sent today       : %d of %d%s", already, cap_, capNote)
	if len(inDoubt) > 0 {
		p("needs review     : %d address(es) interrupted mid-send, never retried", len(inDoubt))
		for i, a := range sortedSet(inDoubt) {
			if i == 5 {
				break
			}
			p("                   %s", a)
		}
	}

	var batch []preflight.Row
	var toAddr []string
	switch {
	case opt.To != "":
		// refuse to redirect at someone the campaign would really mail: that would be a way to put
		// unreviewed copy in front of a prospect
		want := strings.ToLower(strings.TrimSpace(opt.To))
		for _, r := range rows {
			if strings.ToLower(strings.TrimSpace(r.Addr())) == want {
				e("\n%s is in the queue. --to is for a copy to yourself; use --only to mail a "+
					"prospect for real.", opt.To)
				return ExitRefused
			}
		}
		src := rows[0]
		if opt.Only != "" {
			found := false
			for _, r := range rows {
				if strings.EqualFold(strings.TrimSpace(r.Addr()), strings.TrimSpace(opt.Only)) {
					src, found = r, true
					break
				}
			}
			if !found {
				e("\n%s is not in the queue.", opt.Only)
				return ExitConfig
			}
		}
		batch = []preflight.Row{src}
		toAddr = []string{opt.To}
		p("\nCOPY: %s's message, redirected to %s. The gate does not apply and nothing is recorded.",
			src.Addr(), opt.To)
	case opt.Only != "":
		want := strings.ToLower(strings.TrimSpace(opt.Only))
		var found *preflight.Row
		for i := range queue {
			if strings.ToLower(strings.TrimSpace(queue[i].Addr())) == want {
				found = &queue[i]
				break
			}
		}
		if found == nil {
			if skip[want] {
				e("\n%s has already been contacted, or is held. Nothing sent.", opt.Only)
			} else {
				e("\n%s is not eligible in this queue. Nothing sent.", opt.Only)
			}
			return ExitConfig
		}
		if room == 0 {
			e("\ndaily cap of %d already reached. Nothing sent.", cap_)
			return ExitCap
		}
		batch = []preflight.Row{*found}
		toAddr = []string{found.Addr()}
		p("\nONLY: one message to %s", found.Addr())
	default:
		if room == 0 && opt.Send {
			e("\ndaily cap of %d already reached. Nothing sent.", cap_)
			return ExitCap
		}
		// one limit, not two: the daily cap is the safety rail, and --count only ever sends fewer.
		// A second default batch size silently won over the cap and surprised everyone.
		n := opt.Count
		if n <= 0 {
			n = len(queue)
		}
		// on a dry run the cap is reported, not enforced: you still want to see what is next
		if opt.Send && n > room {
			n = room
		}
		if n > len(queue) {
			n = len(queue)
		}
		batch = queue[:n]
		for _, r := range batch {
			toAddr = append(toAddr, r.Addr())
		}
	}

	if len(batch) == 0 {
		p("\nnothing eligible to send.")
		return ExitOK
	}

	if !opt.Send {
		p("\nDRY RUN — would send %d. Add --send to transmit.", len(batch))
		if room == 0 {
			p("(the daily cap of %d is reached, so these wait until tomorrow)", cap_)
		}
		for i, a := range toAddr {
			if i == 10 {
				p("  ... and %d more", len(toAddr)-10)
				break
			}
			p("  %s", a)
		}
		return ExitOK
	}

	transport := opt.Transport
	if transport == nil {
		t, code := smtpTransport(e)
		if code != ExitOK {
			return code
		}
		transport = t
	}

	l, err := openLedger()
	if err != nil {
		e("\ncannot write the ledger: %v", err)
		return ExitConfig
	}
	defer l.Close()

	ok, failed, streak := 0, 0, 0
	delay := opt.Delay
	if delay <= 0 {
		delay = 4 * time.Second
	}

	p("\nsending %d...", len(batch))
	for i, row := range batch {
		to := toAddr[i]
		if opt.To == "" {
			if err := l.append("INTENT", to, row.Hash); err != nil {
				e("  cannot write the ledger, stopping before the send: %v", err)
				return ExitPartial
			}
		}
		if err := transport(row, to); err != nil {
			_ = l.append("ERROR", to, errKind(err))
			failed++
			streak++
			e("  %4d/%d  FAILED %s: %v", i+1, len(batch), to, err)
			if streak >= 3 {
				e("  three failures in a row — stopping.")
				p("\nsent %d, failed %d", ok, failed)
				return ExitPartial
			}
			time.Sleep(jitter(delay))
			continue
		}
		if opt.To == "" {
			if err := l.append("SENT", to, row.Hash); err != nil {
				e("  the message left but the ledger did not record it: %v", err)
				return ExitPartial
			}
			if err := appendLine(campaign.Contacted, strings.ToLower(to)); err != nil {
				e("  could not update contacted.txt: %v", err)
			}
		}
		ok++
		streak = 0
		p("  %4d/%d  sent  %s", i+1, len(batch), to)
		time.Sleep(jitter(delay))
	}

	p("\nsent %d, failed %d", ok, failed)
	if ok > 0 && opt.To == "" {
		p("now run `kmail verify` to reconcile against Gmail and update the master CSVs")
	}
	if failed > 0 {
		return ExitPartial
	}
	return ExitOK
}

// Password finds the app password without it ever being typed where it can be recorded.
//
// The Keychain is the preferred home: `security add-generic-password -w` prompts without echoing,
// so the secret misses the shell history, the environment and any transcript. KAIROS_SMTP_PASS is
// still read first, because it is what the habit is.
func Password() (string, error) {
	if p := stripSpace(os.Getenv("KAIROS_SMTP_PASS")); p != "" {
		return p, nil
	}
	out, err := keychainLookup(KeychainService, campaign.Sender)
	out = stripSpace(out)
	if err == nil && out != "" {
		return out, nil
	}
	// an entry that reads fine but holds nothing is the likely case, because the prompt echoes
	// nothing and an empty Enter is accepted silently. Saying "no app password" there sends you
	// round the same loop again.
	if err == nil {
		return "", fmt.Errorf(
			"the keychain entry %s/%s exists but is empty — the prompt echoes nothing, so an\n"+
				"empty Enter looks the same as a paste. Store it again, note the -U:\n\n"+
				"  security add-generic-password -U -s %s -a %s -w\n\n"+
				"Paste the 16-character app password at the prompt. You will see no characters.",
			KeychainService, campaign.Sender, KeychainService, campaign.Sender)
	}
	return "", fmt.Errorf(
		"no app password. Store it once, in your own Terminal — the -w with no value prompts\n"+
			"without echoing, so it stays out of your shell history:\n\n"+
			"  security add-generic-password -s %s -a %s -w\n\n"+
			"Get the value from Google Account > Security > 2-Step Verification > App passwords.\n"+
			"KAIROS_SMTP_PASS still works if you prefer an environment variable.",
		KeychainService, campaign.Sender)
}

// Google prints an app password as four groups of four; the spaces are display only. A stray space
// or a trailing newline from a paste is otherwise indistinguishable from a wrong password, because
// all SMTP says back is "authentication failed".
func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// KeychainService is the service name the password is filed under.
const KeychainService = "kmail-smtp"

// keychainLookup is replaced in tests. It never returns the value anywhere but to Password.
var keychainLookup = func(service, account string) (string, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-s", service, "-a", account, "-w").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// smtpTransport is the real one. Anything else is a test.
func smtpTransport(e func(string, ...any)) (func(preflight.Row, string) error, int) {
	password, err := Password()
	if err != nil {
		e("\n%v", err)
		return nil, ExitConfig
	}
	auth := smtp.PlainAuth("", campaign.Sender, password, campaign.SMTPHost)
	return func(row preflight.Row, to string) error {
		msg, err := BuildMessage(row, to, time.Now())
		if err != nil {
			return err
		}
		return smtp.SendMail(campaign.SMTPHost+":587", auth, campaign.Sender, []string{to}, msg)
	}, ExitOK
}

// a fixed interval is a signature; ±40% is still slow enough for a young domain
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.6 + 0.8*rand.Float64()))
}

func errKind(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
