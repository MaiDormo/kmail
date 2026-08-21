# kmail

An outbound email tool that refuses to send anything a human has not read.

One static Go binary. Builds personalised emails from a CSV, seals each one, makes you review the
copy, then talks to SMTP itself — with nothing in between that can rewrite a message.

```
kmail sandbox                 a throwaway copy of the data, to try this on
kmail status                  where things stand
kmail build --count 300       rebuild the queue from the CSV
kmail review [--file]         the human gate. A TUI, or $EDITOR with --file
kmail preview --n 3           render at phone and desktop width, and open it
kmail check                   template, preflight, links
kmail send [--send]           dry run unless --send is given
kmail send --only ADDR        just that one queued address, a real send
kmail send --to ADDR          a copy to yourself; refused if ADDR is in the queue
kmail one ADDR                one message to somebody not on the list
kmail verify [--write]        reconcile the ledger against what actually sent
```

Exit codes are the contract: `0` clean, `1` config, `2` refused with nothing sent, `3` stopped
part-way, `4` locked, `5` daily cap reached.

## The two rules

**Content never passes through a language model.** `send` reads the queue and writes bytes to a
socket. It prints addresses and counts, never a subject or body, so an agent can run it and read its
exit code but can never hold the content. There is no flag to make it print more.

**Nothing sends that a person has not read.** Structural checks cannot tell you that
`If asdfgh works with long-form video…` is not a sentence to send a stranger. `kmail review` shows
the opener shapes — one queue of 257 rows had 231 distinct paragraphs but only 12 shapes — then
every company name that reaches the copy, doubtful ones flagged: *mojibake · a platform, not a
prospect · a handle, not a name · shouting · acronym*. `space` blanks a name and re-renders that row
with the no-company wording, `d` drops the contact, `a` blanks everything flagged, `/` filters.

Review needs a terminal, so an agent cannot approve on your behalf. The TUI and the `$EDITOR`
decision file both produce the same `[]Decision`; only that reaches `Apply` and the gate, and a test
drives both and asserts they agree.

## What send refuses

Always with nothing sent: the queue was edited after it was built (sha256 over subject+body+html),
any row fails preflight, nothing has been reviewed, the template changed since it was approved, a
row uses an unapproved opener shape or company name, another run holds the lock, or the daily cap is
reached.

Approval is stored as the set of shapes and names, not row IDs, so a later build with the same copy
passes and a new company name does not.

A name you strike out is struck out for good: `review` appends it to `blanked.txt` and every later
`build` renders those contacts with the no-company wording. Review is a decision about the campaign,
not about one batch — without this, a rebuild puts every bad name straight back. That is also what lets `kmail one` mail a referral who was
never in the CSV: the copy is rendered from an approved shape, so it needs no new approval — and it
is recorded like any other send, which `--to` deliberately is not.

**Delivery is at most once.** The ledger is written twice per message — `INTENT` and fsync before
the send, `SENT` and fsync after. A crash between them leaves an address that is never retried.

## Audiences

A list is rarely one population. Ours was a prospect search of sports organisations plus a marketing
newsletter with no video business at all, and one pitch cannot be true for both — a false premise is
worse than a generic one.

An audience is a template plus the copy that goes with it: subjects, opener shapes, the search
example, and the markers that prove the HTML arrived whole. `campaign.json` maps a value in the
CSV's `source` column onto one:

```json
"audience_by_source": { "apollo-sport": "broadcast", "marketing-newsletter": "general" },
"default_audience": "broadcast"
```

Shape IDs are namespaced by audience, so approving copy for one can never cover the other. An
unmapped source falls back to the default rather than failing: a new value in the CSV should be
unsurprising, not fatal.

## Setup

Copy `campaign.example.json` to `$KMAIL_HOME/campaign.json` (default `~/outreach`) and fill in the
sender, the postal identity your jurisdiction requires in cold B2B mail, and the CSVs to read.

The SMTP app password comes from the macOS Keychain, or `KAIROS_SMTP_PASS`:

```bash
security add-generic-password -U -s kmail-smtp -a you@example.com -w
```

`-w` with no value prompts without echoing, so it misses your shell history. Whitespace is stripped.

```bash
go build -o kmail .
go test ./...
```

No campaign data is in this repository — queue, ledger, contacted list, template and test fixtures
all live under `$KMAIL_HOME`. The corpus tests read real sealed rows from there and skip when it is
absent, so a fresh clone tests clean.

## Layout

| Package | What |
| --- | --- |
| `campaign` | the copy, the forbidden strings, the ramp, every path |
| `preflight` | the structural rules |
| `build` | CSV to sealed queue, the renderer, attributing a row back to its copy |
| `addresses` | the MX gate and its cache |
| `review` | the decision model, the TUI, the decision file, the gate |
| `send` | MIME, SMTP, the two-phase ledger |
