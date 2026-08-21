package send

import (
	"errors"
	"strings"
	"testing"

	"kmail/campaign"
)

// The password must never be logged, never be required on the command line, and never be guessed
// at. These are the three ways it can be wrong.
func TestPasswordPrefersTheEnvironmentThenTheKeychain(t *testing.T) {
	old := keychainLookup
	defer func() { keychainLookup = old }()

	asked := 0
	keychainLookup = func(service, account string) (string, error) {
		asked++
		if service != KeychainService || account != campaign.Sender {
			t.Errorf("looked up %q/%q", service, account)
		}
		return "from-keychain", nil
	}

	t.Setenv("KAIROS_SMTP_PASS", "from-env")
	if p, err := Password(); err != nil || p != "from-env" {
		t.Errorf("got %q %v, want the environment to win", p, err)
	}
	if asked != 0 {
		t.Error("the keychain was queried even though the environment had it")
	}

	t.Setenv("KAIROS_SMTP_PASS", "")
	if p, err := Password(); err != nil || p != "from-keychain" {
		t.Errorf("got %q %v, want the keychain", p, err)
	}
}

// An entry that reads fine but holds nothing cost a round trip once: the generic "no app password"
// message sent the operator back round the same loop.
func TestAnEmptyKeychainEntrySaysSo(t *testing.T) {
	old := keychainLookup
	defer func() { keychainLookup = old }()
	keychainLookup = func(string, string) (string, error) { return "", nil }
	t.Setenv("KAIROS_SMTP_PASS", "")

	_, err := Password()
	if err == nil {
		t.Fatal("carried on with an empty password")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exists but is empty") {
		t.Errorf("does not name the real problem:\n%s", msg)
	}
	if !strings.Contains(msg, "-U") {
		t.Errorf("does not give the -U needed to overwrite the existing item:\n%s", msg)
	}
}

// A stray space or a pasted newline must not read as a wrong password: SMTP says only
// "authentication failed" either way, and that is an hour of looking in the wrong place.
func TestPasswordIgnoresTheSpacesGooglePrints(t *testing.T) {
	old := keychainLookup
	defer func() { keychainLookup = old }()
	keychainLookup = func(string, string) (string, error) { return "abcd efgh ijkl mnop\n", nil }

	t.Setenv("KAIROS_SMTP_PASS", "  abcd efgh ijkl mnop  ")
	if p, _ := Password(); p != "abcdefghijklmnop" {
		t.Errorf("env gave %q", p)
	}
	t.Setenv("KAIROS_SMTP_PASS", "")
	if p, _ := Password(); p != "abcdefghijklmnop" {
		t.Errorf("keychain gave %q", p)
	}
	// whitespace only is still empty, and must not be sent as a password
	keychainLookup = func(string, string) (string, error) { return "   \n", nil }
	if _, err := Password(); err == nil {
		t.Error("a whitespace-only entry was accepted as a password")
	}
}

func TestPasswordFailsWithInstructionsNotAPrompt(t *testing.T) {
	old := keychainLookup
	defer func() { keychainLookup = old }()
	keychainLookup = func(string, string) (string, error) { return "", errors.New("not found") }
	t.Setenv("KAIROS_SMTP_PASS", "")

	_, err := Password()
	if err == nil {
		t.Fatal("carried on with no password")
	}
	msg := err.Error()
	for _, want := range []string{"add-generic-password", "-w", "your own Terminal", "shell history"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q:\n%s", want, msg)
		}
	}
	// it must tell you how to store one, never invite you to paste one here
	for _, never := range []string{"paste", "enter your password", "type it here"} {
		if strings.Contains(strings.ToLower(msg), never) {
			t.Errorf("the error invites %q", never)
		}
	}
}
