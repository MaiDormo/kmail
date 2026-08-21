package send

import (
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"strings"
	"testing"
	"time"

	"kmail/build"
	"kmail/campaign"
	"kmail/preflight"
)

func TestMain(m *testing.M) {
	// the identity is campaign data; without it Sender is empty and every address check is wrong
	_ = campaign.LoadIdentity()
	os.Exit(m.Run())
}

func corpusRow(t *testing.T) preflight.Row {
	t.Helper()
	rows, err := build.ReadQueue()
	if err != nil || len(rows) == 0 {
		t.Skip("no queue to take a real row from")
	}
	return rows[0]
}

// A hand-rolled MIME bug is invisible until a real inbox renders it wrong, so the message is parsed
// back and the decoded parts compared with what went in.
func TestMessageRoundTrip(t *testing.T) {
	row := corpusRow(t)
	raw, err := BuildMessage(row, "someone@example.com", time.Now())
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("the message does not parse as RFC 5322: %v", err)
	}
	if got := msg.Header.Get("Subject"); got != row.Subject {
		t.Errorf("subject %q, want %q", got, row.Subject)
	}
	if got := msg.Header.Get("To"); got != "someone@example.com" {
		t.Errorf("to %q", got)
	}
	if !strings.Contains(msg.Header.Get("List-Unsubscribe"), campaign.Sender) {
		t.Error("no List-Unsubscribe pointing at the sender")
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || from.Address != campaign.Sender {
		t.Errorf("from %q (%v)", msg.Header.Get("From"), err)
	}

	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content-type %q (%v)", msg.Header.Get("Content-Type"), err)
	}

	parts := map[string]string{}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading a part: %v", err)
		}
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		var r io.Reader = p
		if strings.EqualFold(p.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			r = quotedprintable.NewReader(p)
		}
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("decoding %s: %v", ct, err)
		}
		parts[ct] = strings.ReplaceAll(string(b), "\r\n", "\n")
	}

	if len(parts) != 2 {
		t.Fatalf("got %d parts, want text and html", len(parts))
	}
	if parts["text/plain"] != row.Body {
		t.Errorf("the plain part did not survive the round trip")
	}
	// a text part ends with a newline: Python's set_content added one and the messages already
	// sent carry it, so the port keeps it rather than shipping nearly-identical bytes
	if parts["text/html"] != row.HTMLBody+"\n" {
		t.Errorf("the html part did not survive the round trip")
	}
	// text before html: clients render the last part they understand
	if i, j := strings.Index(string(raw), "text/plain"), strings.Index(string(raw), "text/html"); i > j {
		t.Error("html comes before text, so a plain-text client would show nothing")
	}
}

// No line may exceed the RFC 5322 limit, which is what quoted-printable is there to guarantee.
func TestNoOverlongLines(t *testing.T) {
	row := corpusRow(t)
	raw, err := BuildMessage(row, "someone@example.com", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line %d is %d octets", i+1, len(line))
		}
	}
}

func TestBareLFBecomesCRLF(t *testing.T) {
	if got := normaliseCRLF("a\nb\r\nc"); got != "a\r\nb\r\nc" {
		t.Errorf("%q", got)
	}
}

func sandbox(t *testing.T) {
	t.Helper()
	old := campaign.Home
	campaign.SetHome(t.TempDir())
	t.Cleanup(func() { campaign.SetHome(old) })
}

func TestAnIntentWithNoSentIsNeverRetried(t *testing.T) {
	sandbox(t)
	body := "2026-08-20T10:00:00\tINTENT\tcrashed@example.com\tabc\n" +
		"2026-08-20T10:00:01\tINTENT\tfine@example.com\tdef\n" +
		"2026-08-20T10:00:02\tSENT\tfine@example.com\tdef\n"
	if err := os.WriteFile(campaign.Ledger, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	settled, inDoubt := ReadLedger()
	if !settled["fine@example.com"] || len(settled) != 1 {
		t.Errorf("settled %v", settled)
	}
	if !inDoubt["crashed@example.com"] || len(inDoubt) != 1 {
		t.Errorf("in doubt %v", inDoubt)
	}
}

// daily_cap in campaign.json replaces the ramp; 0 falls back to it.
// --count 0 means "whatever the cap allows", so there is one limit rather than two.
func TestCountDefaultsToTheCap(t *testing.T) {
	rows, err := build.ReadQueue()
	if err != nil || len(rows) == 0 {
		t.Skip("no queue")
	}
	old := campaign.Home
	dir := t.TempDir()
	campaign.SetHome(dir)
	defer campaign.SetHome(old)
	copyFile(t, old+"/queue.jsonl", campaign.Queue)
	copyFile(t, old+"/kairos-campaign-v2-dark.html", campaign.Template)
	copyFile(t, old+"/kairos-general-dark.html", campaign.Home+"/kairos-general-dark.html")
	queued, _ := build.ReadQueue()
	approve(t, queued...)

	sent := 0
	tr := func(preflight.Row, string) error { sent++; return nil }
	// a cap of 7 with no --count must send exactly 7, not a hidden default of 50
	if code := Run(Options{Send: true, MaxPerDay: 7, Delay: time.Nanosecond, Transport: tr},
		devNull(t), devNull(t)); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if sent != 7 {
		t.Errorf("sent %d, want the cap of 7", sent)
	}
}

func TestDailyCapOverrideBeatsTheRamp(t *testing.T) {
	old := campaign.DailyCapOverride
	defer func() { campaign.DailyCapOverride = old }()
	campaign.DailyCapOverride = 0
	if got := campaign.DailyCap(0); got != 40 {
		t.Errorf("ramp gave %d for a new domain, want 40", got)
	}
	campaign.DailyCapOverride = 1000
	if got := campaign.DailyCap(0); got != 1000 {
		t.Errorf("override gave %d, want 1000", got)
	}
}

func TestSentTodayCountsOnlyToday(t *testing.T) {
	sandbox(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	body := "2026-08-20T10:00:00+00:00\tSENT\ta@x\t\n" +
		"2026-08-21T09:00:00+00:00\tSENT\tb@x\t\n" +
		"2026-08-21T09:30:00+00:00\tINTENT\tc@x\t\n"
	os.WriteFile(campaign.Ledger, []byte(body), 0o644)
	if got := SentToday(now); got != 1 {
		t.Errorf("counted %d, want 1", got)
	}
}

// The ledger format has to stay readable by anything that read the Python's lines.
func TestStampMatchesThePythonShape(t *testing.T) {
	s := campaign.Stamp(time.Date(2026, 8, 21, 9, 22, 27, 123456000, time.UTC))
	if !strings.HasPrefix(s, "2026-08-21T09:22:27.123456") {
		t.Errorf("stamp %q", s)
	}
	if !strings.HasSuffix(s, "+00:00") {
		t.Errorf("stamp %q does not end +00:00 the way the Python's isoformat did", s)
	}
}

// The gate, the cap and the dry run, with no socket anywhere near it.
func TestRunRefusesWithoutApproval(t *testing.T) {
	rows, err := build.ReadQueue()
	if err != nil || len(rows) == 0 {
		t.Skip("no queue")
	}
	dir := t.TempDir()
	old := campaign.Home
	campaign.SetHome(dir)
	defer campaign.SetHome(old)
	// copy the queue and the template into the sandbox so preflight passes
	copyFile(t, old+"/queue.jsonl", campaign.Queue)
	copyFile(t, old+"/kairos-campaign-v2-dark.html", campaign.Template)

	sent := 0
	code := Run(Options{Send: true, Count: 5, Transport: func(preflight.Row, string) error {
		sent++
		return nil
	}}, devNull(t), devNull(t))
	if code != ExitRefused {
		t.Errorf("exit %d, want %d", code, ExitRefused)
	}
	if sent != 0 {
		t.Errorf("%d messages went out through a closed gate", sent)
	}
}

func TestLockIsExclusive(t *testing.T) {
	sandbox(t)
	held, err := campaign.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if code := Run(Options{}, devNull(t), devNull(t)); code != ExitLocked {
		t.Errorf("exit %d, want %d", code, ExitLocked)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Skipf("no %s", from)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
