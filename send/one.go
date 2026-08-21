package send

import (
	"fmt"
	"os"
	"strings"
	"time"

	"kmail/build"
	"kmail/campaign"
	"kmail/preflight"
	"kmail/review"
)

// One sends a single message to somebody who is not on the list — a referral, someone you met — with
// every guarantee a queued send has.
//
// It is not `--to`. That one bypasses the gate and records nothing, which is right for a copy to
// yourself and wrong for a real person: an unrecorded send is one that can happen twice, because
// nothing stops the same address arriving in a later CSV.
//
// The row is rendered by the same renderer, sealed, preflighted, put through the review gate,
// checked against everything already contacted, counted against the daily cap, and written to the
// ledger and contacted.txt.
func One(row preflight.Row, opt Options, out, errOut *os.File) int {
	p := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	e := func(format string, a ...any) { fmt.Fprintf(errOut, format+"\n", a...) }

	lock, err := campaign.TryLock()
	if err != nil {
		e("\n%v", err)
		return ExitLocked
	}
	defer lock.Close()

	addr := strings.ToLower(strings.TrimSpace(row.Addr()))

	if problems := preflight.CheckRow(row); len(problems) > 0 {
		e("PREFLIGHT FAILED — %d problem(s). Nothing sent.\n", len(problems))
		for _, msg := range problems {
			e("  %s", msg)
		}
		return ExitRefused
	}

	// the gate applies exactly as it does to the queue: this has to be copy a human approved
	if refusals := review.Gate([]preflight.Row{row}); len(refusals) > 0 {
		e("REFUSED — this copy has not been approved. Nothing sent.\n")
		for _, r := range refusals {
			e("  %s", r)
		}
		e("\nA one-off reuses the shapes the queue was approved with. If you passed --company, leave")
		e("it off: a name nobody reviewed is exactly what the gate is there to stop.")
		return ExitRefused
	}

	settled, inDoubt := ReadLedger()
	if settled[addr] || inDoubt[addr] {
		e("\n%s is already in the ledger. Nothing sent.", addr)
		return ExitConfig
	}
	for _, path := range []string{campaign.Contacted, campaign.Held, campaign.Bounced} {
		if loadSet(path)[addr] {
			e("\n%s is already in %s. Nothing sent.", addr, path)
			return ExitConfig
		}
	}

	now := time.Now()
	cap_ := opt.MaxPerDay
	if cap_ <= 0 {
		cap_ = campaign.DailyCap(campaign.DomainAgeDays(now))
	}
	already := SentToday(now)
	p("sent today       : %d of %d", already, cap_)

	shape, name, _ := build.Attribute(row)
	named := "none"
	if name != "" {
		named = name
	}
	p("\nONE-OFF: %s", addr)
	p("  opener shape   : %s", shape)
	p("  company named  : %s", named)

	// the cap stops a send, not a look: being unable to preview once you have hit it is useless
	if !opt.Send {
		p("\nDRY RUN — would send 1. Add --send to transmit.")
		if already >= cap_ {
			p("(the daily cap of %d is reached, so this would have to wait until tomorrow)", cap_)
		}
		return ExitOK
	}
	if already >= cap_ {
		e("\ndaily cap of %d already reached. Nothing sent.", cap_)
		return ExitCap
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

	if err := l.append("INTENT", addr, row.Hash); err != nil {
		e("  cannot write the ledger, stopping before the send: %v", err)
		return ExitPartial
	}
	if err := transport(row, addr); err != nil {
		_ = l.append("ERROR", addr, errKind(err))
		e("  FAILED %s: %v", addr, err)
		return ExitPartial
	}
	if err := l.append("SENT", addr, row.Hash); err != nil {
		e("  the message left but the ledger did not record it: %v", err)
		return ExitPartial
	}
	if err := appendLine(campaign.Contacted, addr); err != nil {
		e("  could not update contacted.txt: %v", err)
	}
	p("  sent  %s", addr)
	return ExitOK
}
