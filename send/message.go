package send

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"kmail/campaign"
	"kmail/preflight"
)

// Python's EmailMessage.set_content + add_alternative produced a correct multipart/alternative with
// the right headers and encodings. Go's stdlib has no equivalent, so the message is hand-built —
// and a hand-rolled MIME bug is invisible until a real inbox renders it wrong. MessageRoundTrip in
// the test parses every message back and compares the decoded parts with the inputs.

func boundary() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "==_kmail_" + hex.EncodeToString(b[:]), nil
}

func messageID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	domain := "localhost"
	if _, d, ok := strings.Cut(campaign.Sender, "@"); ok {
		domain = d
	}
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(b[:]), domain), nil
}

// header encodes a value only when it needs it, so an ASCII subject stays readable on the wire.
func header(v string) string {
	if isASCII(v) {
		return v
	}
	return mime.QEncoding.Encode("utf-8", v)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

func address(name, addr string) string {
	a := mail.Address{Name: name, Address: addr}
	return a.String()
}

// BuildMessage renders one row as RFC 5322 bytes: multipart/alternative, both parts
// quoted-printable so the HTML's long lines cannot break the 998-octet line limit.
func BuildMessage(r preflight.Row, to string, now time.Time) ([]byte, error) {
	bnd, err := boundary()
	if err != nil {
		return nil, err
	}
	mid, err := messageID()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	h := []struct{ k, v string }{
		{"From", address(campaign.SenderName, campaign.Sender)},
		{"To", to},
		{"Subject", header(r.Subject)},
		{"Date", now.Format(time.RFC1123Z)},
		{"Message-ID", mid},
		// a reply is the opt-out; declare it in the header the filters read
		{"List-Unsubscribe", "<mailto:" + campaign.Sender + "?subject=unsubscribe>"},
		{"MIME-Version", "1.0"},
		{"Content-Type", "multipart/alternative; boundary=\"" + bnd + "\""},
	}
	for _, kv := range h {
		fmt.Fprintf(&buf, "%s: %s\r\n", kv.k, kv.v)
	}
	buf.WriteString("\r\n")

	w := multipart.NewWriter(&buf)
	if err := w.SetBoundary(bnd); err != nil {
		return nil, err
	}
	for _, part := range []struct{ ctype, body string }{
		{"text/plain; charset=utf-8", r.Body},
		{"text/html; charset=utf-8", r.HTMLBody},
	} {
		ph := textproto.MIMEHeader{}
		ph.Set("Content-Type", part.ctype)
		ph.Set("Content-Transfer-Encoding", "quoted-printable")
		pw, err := w.CreatePart(ph)
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(pw)
		// Python's set_content ends a text part with a newline, and the 76 messages already sent
		// carry it. Matching keeps the bytes identical rather than nearly identical.
		if _, err := qp.Write([]byte(normaliseCRLF(endWithNewline(part.body)))); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func endWithNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// SMTP is a CRLF protocol; a bare LF in a body is what turns one message into two on some servers.
func normaliseCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
