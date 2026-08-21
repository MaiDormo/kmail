package send

import (
	"os"
	"testing"

	"kmail/build"
	"kmail/campaign"
	"kmail/preflight"
)

// --to is a copy for your own eyes: it bypasses the gate, so it must never be usable to put
// unreviewed copy in front of someone the campaign would really mail.
func TestToRefusesAnAddressInTheQueue(t *testing.T) {
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
	sent := 0
	tr := func(preflight.Row, string) error { sent++; return nil }
	code := Run(Options{Send: true, To: rows[0].Addr(), Transport: tr}, devNull(t), devNull(t))
	if code != ExitRefused {
		t.Errorf("exit %d, want %d", code, ExitRefused)
	}
	if sent != 0 {
		t.Errorf("%d messages went out", sent)
	}
}

// --only is a real send, so the gate still applies.
func TestOnlyStillObeysTheGate(t *testing.T) {
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
	sent := 0
	tr := func(preflight.Row, string) error { sent++; return nil }
	code := Run(Options{Send: true, Only: rows[0].Addr(), Transport: tr}, devNull(t), devNull(t))
	if code != ExitRefused || sent != 0 {
		t.Errorf("exit %d, sent %d — --only walked past a closed gate", code, sent)
	}
}

// an address that is not in the queue is a typo, not a silent no-op.
func TestOnlyRejectsAnUnknownAddress(t *testing.T) {
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
	copyFile(t, old+"/approvals.json", campaign.Approvals)
	sent := 0
	tr := func(preflight.Row, string) error { sent++; return nil }
	code := Run(Options{Send: true, Only: "nobody@nowhere.example", Transport: tr}, devNull(t), devNull(t))
	if code != ExitConfig || sent != 0 {
		t.Errorf("exit %d, sent %d", code, sent)
	}
}

var _ = os.DevNull
