package review

import (
	"testing"

	"kmail/campaign"
	"kmail/preflight"
)

// The video-signal flag is a question about the broadcast pitch, not about the tool. Applying it to
// an audience whose copy claims nothing about the reader's industry flags every single row.
func TestVideoSignalFlagOnlyAppliesWhereTheCopyClaimsIt(t *testing.T) {
	notVideo := func(aud string) []preflight.Row {
		return []preflight.Row{{To: []string{"a@acme.com"}, Company: "Acme Bricks",
			Title: "Head of Operations", Audience: aud}}
	}
	if why := Flag("Acme Bricks", notVideo(campaign.DefaultAudience)); why != "no video signal" {
		t.Errorf("broadcast audience gave %q, want the flag", why)
	}
	if why := Flag("Acme Bricks", notVideo("general")); why != "" {
		t.Errorf("general audience gave %q, want no flag", why)
	}
	// the flags that are about the name itself still apply to every audience
	for _, junk := range []string{"YouTube", "werew", "Th? ?\u00f4 Multimedia"} {
		if Flag(junk, notVideo("general")) == "" {
			t.Errorf("general audience kept %q unflagged", junk)
		}
	}
}
