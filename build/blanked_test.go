package build

import (
	"os"
	"strings"
	"testing"

	"kmail/campaign"
)

// Blanking a name is a decision about the campaign, not about one batch. Before this, a rebuild
// rendered every struck-out name straight back into the copy.
func TestBlankedNamesSurviveARebuild(t *testing.T) {
	old := campaign.Home
	campaign.SetHome(t.TempDir())
	defer campaign.SetHome(old)

	body := "YouTube\nTe?t ?edia Group\n  \nNorthgate Media\n"
	if err := os.WriteFile(campaign.Blanked, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadBlanked()
	if len(got) != 3 {
		t.Fatalf("read %d names, want 3: %v", len(got), got)
	}
	// matched case-insensitively, because the list writes the same company three different ways
	for _, v := range []string{"youtube", "YOUTUBE", "YouTube", "northgate media"} {
		if !got[strings.ToLower(v)] {
			t.Errorf("%q not matched", v)
		}
	}
	if got["never blanked"] {
		t.Error("matched a name that was never blanked")
	}
}

func TestNoBlankedFileIsNotAnError(t *testing.T) {
	old := campaign.Home
	campaign.SetHome(t.TempDir())
	defer campaign.SetHome(old)
	if got := LoadBlanked(); len(got) != 0 {
		t.Errorf("got %v from a missing file", got)
	}
}
