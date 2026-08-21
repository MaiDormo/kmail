package review

import (
	"os"
	"strings"
	"testing"

	"kmail/campaign"
)

// Apply has to record the blanking somewhere durable. approvals.json cannot be it: once a build
// honours the decision, that name never reaches a review again, so the next approvals.json would
// not list it and the decision would evaporate one review later.
func TestApplyRemembersBlankedNames(t *testing.T) {
	old := campaign.Home
	campaign.SetHome(t.TempDir())
	defer campaign.SetHome(old)

	if err := os.WriteFile(campaign.Blanked, []byte("Already Known\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	known := loadLower(campaign.Blanked)
	if !known["already known"] {
		t.Fatal("existing entry not read back")
	}
	state := map[string]Verb{"Already Known": Blank, "Fresh Name": Blank, "Kept Name": Keep}
	var newly []string
	for n, v := range state {
		if v == Blank && !known[strings.ToLower(n)] {
			newly = append(newly, n)
		}
	}
	if len(newly) != 1 || newly[0] != "Fresh Name" {
		t.Fatalf("would append %v, want just the fresh one", newly)
	}
	if err := appendLines(campaign.Blanked, newly); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(campaign.Blanked)
	// the case has to survive: it is a company name, not an address
	if !strings.Contains(string(b), "Fresh Name") {
		t.Errorf("case was mangled: %q", b)
	}
	if strings.Count(string(b), "Already Known") != 1 {
		t.Errorf("duplicated an entry: %q", b)
	}
}
