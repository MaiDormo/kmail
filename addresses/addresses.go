// Package addresses answers one question, for free: can this domain receive mail at all?
//
// An MX lookup removed 57% of the bad addresses on the last measurement, costs nothing, and is
// cached for campaign.MXCacheDays because DNS does change — a domain that dropped its MX three
// months ago may have one now.
//
// There is deliberately no paid validator here. One was built and removed on 2026-08-21: it would
// have cost about 31 cents for the remaining queue, and the answer was that the campaign does not
// pay per address. If that changes, the shape to add back is a cache keyed by address, a hard spend
// ceiling checked before any request, and a verdict that is never cached when the check itself
// failed.
package addresses

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"kmail/campaign"
)

type entry struct {
	MX *bool  `json:"mx,omitempty"`
	At string `json:"at"`
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func loadCache(path string) map[string]entry {
	out := map[string]entry{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	// a corrupt cache costs a few DNS lookups but must never stop a run
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]entry{}
	}
	return out
}

func saveCache(path string, data map[string]entry) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fresh(stamp string, days int) bool {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return false
	}
	return time.Since(t) < time.Duration(days)*24*time.Hour
}

// Logf is how progress reaches the caller. Nothing here prints on its own.
type Logf func(format string, a ...any)

// lookupMX returns nil when the lookup itself failed, which is not the same as "no MX" and so is
// never cached.
func lookupMX(domain string) *bool {
	recs, err := net.LookupMX(domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			no := false
			return &no
		}
		return nil
	}
	ok := len(recs) > 0
	return &ok
}

// MXAlive reports, for every domain given, whether it can receive mail at all.
func MXAlive(domains []string, log Logf) map[string]bool {
	cache := loadCache(campaign.MXCache)
	var todo []string
	for _, d := range domains {
		e, ok := cache[d]
		if !ok || e.MX == nil || !fresh(e.At, campaign.MXCacheDays) {
			todo = append(todo, d)
		}
	}
	if len(todo) > 0 {
		log("MX-checking %d domains (%d already known)...", len(todo), len(domains)-len(todo))
		type res struct {
			d  string
			ok *bool
		}
		ch := make(chan res, len(todo))
		sem := make(chan struct{}, 32)
		var wg sync.WaitGroup
		for _, d := range todo {
			wg.Add(1)
			go func(d string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				ch <- res{d, lookupMX(d)}
			}(d)
		}
		wg.Wait()
		close(ch)
		for r := range ch {
			if r.ok != nil {
				cache[r.d] = entry{MX: r.ok, At: now()}
			}
		}
		if err := saveCache(campaign.MXCache, cache); err != nil {
			log("could not save the MX cache: %v", err)
		}
	} else {
		log("MX: all %d domains already known", len(domains))
	}

	out := make(map[string]bool, len(domains))
	for _, d := range domains {
		// a domain whose lookup failed is given the benefit of the doubt
		if e, ok := cache[d]; ok && e.MX != nil {
			out[d] = *e.MX
		} else {
			out[d] = true
		}
	}
	return out
}
