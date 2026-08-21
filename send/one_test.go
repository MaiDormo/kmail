package send

import (
	"encoding/json"
	"os"
	"testing"

	"kmail/build"
	"kmail/campaign"
	"kmail/preflight"
	"kmail/review"
)

// approve exactly these rows' copy, so a test never depends on whatever the last real review
// happened to seal.
func approve(t *testing.T, rows ...preflight.Row) {
	t.Helper()
	sha, err := campaign.TemplateSHA()
	if err != nil {
		t.Fatal(err)
	}
	rec := review.Approval{ApprovedAt: "test", TemplateSHA: sha}
	seenShape, seenName := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		shape, name, _ := build.Attribute(r)
		if !seenShape[shape] {
			seenShape[shape] = true
			rec.Shapes = append(rec.Shapes, shape)
		}
		if name != "" && !seenName[name] {
			seenName[name] = true
			rec.Names = append(rec.Names, name)
		}
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(campaign.Approvals, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A one-off is a real send, so every guarantee the queue gets applies to it too.
func oneRow(t *testing.T, addr, company string) preflight.Row {
	t.Helper()
	tpl, err := build.LoadTemplate()
	if err != nil {
		t.Skipf("no template: %v", err)
	}
	c := build.Contact{Email: addr, FirstName: "Ada", Company: company,
		SafeCompany: build.CleanCompany(company), Title: "Head of Content", Domain: "example.com"}
	row, _, err := build.RenderRow(tpl, c, campaign.Subjects[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func oneSandbox(t *testing.T) string {
	t.Helper()
	old := campaign.Home
	campaign.SetHome(t.TempDir())
	t.Cleanup(func() { campaign.SetHome(old) })
	copyFile(t, old+"/kairos-campaign-v2-dark.html", campaign.Template)
	return old
}

func TestOneObeysTheGate(t *testing.T) {
	oneSandbox(t) // no approvals.json copied: the gate is shut
	row := oneRow(t, "stranger@example.com", "")
	sent := 0
	code := One(row, Options{Send: true, Transport: func(preflight.Row, string) error {
		sent++
		return nil
	}}, devNull(t), devNull(t))
	if code != ExitRefused || sent != 0 {
		t.Errorf("exit %d, sent %d — a one-off walked past a closed gate", code, sent)
	}
}

func TestOneWillNotMailSomebodyTwice(t *testing.T) {
	oneSandbox(t)
	row := oneRow(t, "stranger@example.com", "")
	approve(t, row)
	if err := os.WriteFile(campaign.Contacted, []byte("stranger@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sent := 0
	code := One(row, Options{Send: true, Transport: func(preflight.Row, string) error {
		sent++
		return nil
	}}, devNull(t), devNull(t))
	if code != ExitConfig || sent != 0 {
		t.Errorf("exit %d, sent %d — mailed somebody already contacted", code, sent)
	}
}

// The whole reason One exists rather than --to: the send has to be recorded, or it can happen again.
func TestOneRecordsWhatItSent(t *testing.T) {
	oneSandbox(t)
	row := oneRow(t, "stranger@example.com", "")
	approve(t, row)
	code := One(row, Options{Send: true, Transport: func(preflight.Row, string) error { return nil }},
		devNull(t), devNull(t))
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	settled, _ := ReadLedger()
	if !settled["stranger@example.com"] {
		t.Error("the ledger has no SENT for it")
	}
	if !loadSet(campaign.Contacted)["stranger@example.com"] {
		t.Error("contacted.txt was not updated, so a later build could queue them again")
	}
	// and a second attempt must now refuse
	if code := One(row, Options{Send: true,
		Transport: func(preflight.Row, string) error { t.Error("sent twice"); return nil }},
		devNull(t), devNull(t)); code != ExitConfig {
		t.Errorf("second attempt exited %d", code)
	}
}

// The cap stops a send, not a preview.
func TestOneDryRunPreviewsPastTheCap(t *testing.T) {
	oneSandbox(t)
	row := oneRow(t, "stranger@example.com", "")
	approve(t, row)
	if code := One(row, Options{MaxPerDay: 0}, devNull(t), devNull(t)); code != ExitOK {
		t.Errorf("dry run exited %d", code)
	}
}
