package send

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"kmail/campaign"
)

// ~/outreach/fixtures/python-message.json is what the Python actually put on the wire for queue row 0,
// captured on 2026-08-21 before it was deleted. It cannot be regenerated. It is the only record of the format 76 real recipients received,
// so this test is the one thing standing between a port and a silently different email.
func TestMatchesThePythonMessage(t *testing.T) {
	var want struct {
		Headers []string `json:"headers"`
		Ctype   string   `json:"ctype"`
		Text    string   `json:"text"`
		HTML    string   `json:"html"`
	}
	b, err := os.ReadFile(filepath.Join(campaign.Home, "fixtures", "python-message.json"))
	if err != nil {
		t.Skipf("no captured Python message: %v", err)
	}
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}

	row := corpusRow(t)
	raw, err := BuildMessage(row, "someone@example.com", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for k := range msg.Header {
		got = append(got, strings.ToLower(k))
	}
	sort.Strings(got)
	sort.Strings(want.Headers)
	if strings.Join(got, ",") != strings.Join(want.Headers, ",") {
		t.Errorf("headers\n  go     %v\n  python %v", got, want.Headers)
	}

	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != want.Ctype {
		t.Fatalf("content-type %q, python had %q", mt, want.Ctype)
	}

	parts := map[string]string{}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		var r io.Reader = p
		if strings.EqualFold(p.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			r = quotedprintable.NewReader(p)
		}
		body, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		parts[ct] = strings.ReplaceAll(string(body), "\r\n", "\n")
	}

	if parts["text/plain"] != want.Text {
		t.Errorf("the plain part differs from what the Python sent (%d vs %d bytes)",
			len(parts["text/plain"]), len(want.Text))
	}
	if parts["text/html"] != want.HTML {
		t.Errorf("the html part differs from what the Python sent (%d vs %d bytes)",
			len(parts["text/html"]), len(want.HTML))
	}
}
