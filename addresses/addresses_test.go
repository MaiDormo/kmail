package addresses

import (
	"os"
	"testing"

	"kmail/campaign"
)

func sandbox(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	old := campaign.Home
	campaign.SetHome(dir)
	t.Cleanup(func() { campaign.SetHome(old) })
}

func quiet(string, ...any) {}

// MX is free, so this one talks to real DNS. It is the cache behaviour being checked, not the DNS.
func TestMXCacheAnswersTwiceAndOnlyLooksUpOnce(t *testing.T) {
	sandbox(t)
	doms := []string{"gmail.com", "this-domain-does-not-exist-kmail-test.tv"}

	first := MXAlive(doms, quiet)
	if !first["gmail.com"] {
		t.Skip("no DNS in this environment")
	}
	if first["this-domain-does-not-exist-kmail-test.tv"] {
		t.Error("a domain with no DNS was reported alive")
	}

	info, err := os.Stat(campaign.MXCache)
	if err != nil {
		t.Fatalf("no cache written: %v", err)
	}
	before := info.ModTime()

	second := MXAlive(doms, quiet)
	for d, v := range first {
		if second[d] != v {
			t.Errorf("%s: %v then %v", d, v, second[d])
		}
	}
	info, _ = os.Stat(campaign.MXCache)
	if !info.ModTime().Equal(before) {
		t.Error("the cache was rewritten on a run that had nothing to look up")
	}
}

// A corrupt cache costs a few DNS lookups, never a run.
func TestCorruptCacheNeverStopsARun(t *testing.T) {
	sandbox(t)
	if err := os.WriteFile(campaign.MXCache, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadCache(campaign.MXCache); len(got) != 0 {
		t.Errorf("a corrupt cache produced %v", got)
	}
}

func TestMXCacheExpires(t *testing.T) {
	sandbox(t)
	stale := `{"old.example":{"mx":true,"at":"2020-01-01T00:00:00Z"}}`
	if err := os.WriteFile(campaign.MXCache, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	c := loadCache(campaign.MXCache)
	if fresh(c["old.example"].At, campaign.MXCacheDays) {
		t.Error("a six-year-old MX answer was treated as current")
	}
}
