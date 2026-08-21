// Package campaign holds everything about the KAIROS outreach campaign that a human decided: the
// copy, the words that may not appear, the markers that prove an email arrived whole, and how fast
// the domain may send.
//
// This is the only definition of each of those. The subjects previously existed three times across
// two files, one copy dead, and nothing would have caught them drifting apart.
package campaign

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Home and the paths under it. KMAIL_HOME points them somewhere else, which is how `kmail sandbox`
// lets the whole tool be tried without touching the real campaign.
var (
	Home     = home()
	Template = filepath.Join(Home, "kairos-campaign-v2-dark.html")
	Queue    = filepath.Join(Home, "queue.jsonl")
	Ledger   = filepath.Join(Home, "ledger.tsv")

	Contacted = filepath.Join(Home, "contacted.txt")
	Bounced   = filepath.Join(Home, "bounced.txt")
	Held      = filepath.Join(Home, "held.txt")
	Blanked   = filepath.Join(Home, "blanked.txt")

	Approvals = filepath.Join(Home, "approvals.json")
	MXCache   = filepath.Join(Home, "mx-cache.json")

	Drafts = filepath.Join(Home, "drafts")
	Lock   = filepath.Join(Home, ".lock")
)

// RepoTemplate is the design reference. `kmail check` diffs it against Template: if they differ,
// one of them is stale.
var RepoTemplate = expand("~/kairos/ads/email/kairos-campaign-v2-dark.html")

// Lists is where the master CSVs are exported to, and the names looked for in order.
var (
	Lists       = expand("~/Downloads")
	SourceLists []string // from campaign.json
	MasterCSVs  []string // from campaign.json
)

func home() string {
	if v := os.Getenv("KMAIL_HOME"); v != "" {
		return v
	}
	return expand("~/outreach")
}

func expand(p string) string {
	if len(p) > 1 && p[0] == '~' {
		h, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// SetHome re-points every path. Tests and `sandbox` use it; nothing else should.
func SetHome(dir string) {
	Home = dir
	Template = filepath.Join(dir, "kairos-campaign-v2-dark.html")
	Queue = filepath.Join(dir, "queue.jsonl")
	Ledger = filepath.Join(dir, "ledger.tsv")
	Contacted = filepath.Join(dir, "contacted.txt")
	Bounced = filepath.Join(dir, "bounced.txt")
	Held = filepath.Join(dir, "held.txt")
	Blanked = filepath.Join(dir, "blanked.txt")
	Approvals = filepath.Join(dir, "approvals.json")
	MXCache = filepath.Join(dir, "mx-cache.json")
	Drafts = filepath.Join(dir, "drafts")
	Lock = filepath.Join(dir, ".lock")
}

// Identity is who the mail comes from and who reviews it. It is deliberately not in this source:
// the tool is published and the campaign is not. Put it in $KMAIL_HOME/campaign.json — see
// campaign.example.json — and it is loaded at startup.
type Identity struct {
	Sender           string            `json:"sender"`
	SenderName       string            `json:"sender_name"`
	Signature        string            `json:"signature"`
	Reviewer         string            `json:"reviewer"`
	SMTPHost         string            `json:"smtp_host"`
	DomainRegistered string            `json:"domain_registered"`
	Postal           string            `json:"postal"`             // required in the HTML, legally
	ForbiddenExtra   []string          `json:"forbidden_extra"`    // names this campaign must never print
	SourceLists      []string          `json:"source_lists"`       // CSVs to build from, first match wins
	MasterCSVs       []string          `json:"master_csvs"`        // CSVs `verify --write` marks up
	DailyCapOverride int               `json:"daily_cap"`          // 0 uses the warm-up ramp
	AudienceBySource map[string]string `json:"audience_by_source"` // CSV source value -> audience
	DefaultAudience  string            `json:"default_audience"`
}

var (
	Sender           string
	SenderName       string
	Signature        string
	Reviewer         string
	SMTPHost         = "smtp.gmail.com"
	DomainRegistered string
)

// LoadIdentity reads $KMAIL_HOME/campaign.json. Absent, the tool still builds and tests but will
// not send: there is nobody to send as.
// generalOpeners never assume the reader owns footage: most of this audience does not.
var generalOpeners = []RoleOpener{
	{
		Role: "production",
		Match: regexp.MustCompile(`(?i)\b(producer|production|post-production|editor|editing|camera|videographer|` +
			`cinematographer|dop|colou?rist)\b`),
		WithCo:    "If you cut video at {company}, the slow part isn’t the edit. It’s finding the moment worth cutting to.",
		WithoutCo: "If you cut video, the slow part isn’t the edit. It’s finding the moment worth cutting to.",
	},
	{
		Role: "engineering",
		Match: regexp.MustCompile(`(?i)\b(engineer|engineering|architect|cto|technical|developer|technology|` +
			`systems|devops|platform|infrastructure)\b`),
		WithCo:    "If {company} stores video, this turns it into something you can query instead of something you pay to keep.",
		WithoutCo: "If you store video, this turns it into something you can query instead of something you pay to keep.",
	},
	{
		Role: "content",
		Match: regexp.MustCompile(`(?i)\b(content|editorial|programming|journalist|news|archive|archivist|librarian|` +
			`marketing|brand|social|communications|audience)\b`),
		WithCo:    "Anything long that {company} records gets more useful when you can ask it a question instead of watching it again.",
		WithoutCo: "Anything long you record gets more useful when you can ask it a question instead of watching it again.",
	},
	{
		Role:      "generic",
		Match:     nil,
		WithCo:    "If {company} has hours of video sitting somewhere, this is worth two minutes of your time.",
		WithoutCo: "If you have hours of video sitting somewhere, this is worth two minutes of your time.",
	},
}

// the sport-specific questions make no sense to this audience
var generalExamples = []searchExample{
	{regexp.MustCompile(`(?i)football|soccer|tennis|cricket|basket|rugby|motor|racing|sport`),
		"“show me every goal scored with a header”"},
	{regexp.MustCompile(`(?i)news|journal|bulletin|press|editorial|publish`),
		"“find every mention of the budget in last night’s bulletin”"},
	{regexp.MustCompile(`(?i)teach|learn|train|course|academy|school|univers|webinar|confer`),
		"“find the part where they explain the pricing model”"},
}

func init() {
	Audiences["broadcast"] = &Audience{
		Name: "broadcast", TemplateFile: "kairos-campaign-v2-dark.html",
		Subjects: Subjects, RequiredHTML: RequiredHTML,
		Openers: RoleOpeners, Examples: searchExamples, Example: SearchDefault,
	}
	Audiences["general"] = &Audience{
		Name: "general", TemplateFile: "kairos-general-dark.html",
		Subjects: []string{
			"KAIROS: turn a long video into its highlights",
			"Hours of video, and the minutes that matter",
			"A tool for anyone sitting on hours of video",
			"Find the moments worth keeping, automatically",
		},
		RequiredHTML: []string{
			"Hours of video, and the few minutes that matter", // preheader
			"NEXT-GEN AISA",
			"https://kairosapp.tech",
			"unsubscribe",
		},
		Openers: generalOpeners, Examples: generalExamples,
		Example: "“find the part where they explain the pricing model”",
	}
}

func LoadIdentity() error {
	Sender, SenderName, Signature, Reviewer = "", "", "", ""
	b, err := os.ReadFile(filepath.Join(Home, "campaign.json"))
	if err != nil {
		return err
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return fmt.Errorf("campaign.json: %w", err)
	}
	if id.Sender == "" {
		return fmt.Errorf("campaign.json has no sender")
	}
	Sender, SenderName = id.Sender, id.SenderName
	Signature, Reviewer = id.Signature, id.Reviewer
	if Signature == "" {
		Signature = id.SenderName
	}
	if Reviewer == "" {
		Reviewer = id.Sender
	}
	if id.SMTPHost != "" {
		SMTPHost = id.SMTPHost
	}
	DomainRegistered = id.DomainRegistered
	if id.Postal != "" {
		RequiredHTML = append(RequiredHTML, id.Postal)
		for _, a := range Audiences {
			a.RequiredHTML = append(a.RequiredHTML, id.Postal)
		}
	}
	Forbidden = append(Forbidden, id.ForbiddenExtra...)
	TemplateForbidden = append(TemplateForbidden, id.ForbiddenExtra...)
	SourceLists, MasterCSVs = id.SourceLists, id.MasterCSVs
	if len(id.AudienceBySource) > 0 {
		AudienceBySource = id.AudienceBySource
	}
	if id.DefaultAudience != "" {
		DefaultAudience = id.DefaultAudience
	}
	DailyCapOverride = id.DailyCapOverride
	return nil
}

var Slots = []string{"{{ greeting }}", "{{ opener }}", "{{ search_example }}"}

// Subjects are the only subjects that may leave. An agent inventing "Broadcast breakdown,
// automatically" is how the 2026-08-20 audit found the first fabricated email.
var Subjects = []string{
	"Automatic video highlight generation with KAIROS",
	"Two hours of broadcast, ten minutes worth watching",
	"Find the moments worth keeping, automatically",
	"Turn a full broadcast into highlights",
}

// RequiredHTML are the markers that prove the HTML arrived whole. The postal identity that cold
// B2B mail into the EU has to carry is appended from campaign.json.
var RequiredHTML = []string{
	"Two hours of broadcast, ten minutes worth watching", // preheader
	"NEXT-GEN AISA",          // wordmark
	"https://kairosapp.tech", // CTA target
	"unsubscribe",            // opt-out, legally required
}

// TemplateForbidden are strings the template must NOT carry: an unresolved placeholder, plus
// anything in forbidden_extra. The footer surgery that used to run on every build is baked into the
// template file now, and these checks are what stops it coming back.
var TemplateForbidden = []string{"{{ unsubscribe }}"}

// Forbidden strings, extended by forbidden_extra in campaign.json — a campaign usually has one or
// two names it must never print.
var Forbidden = []string{
	"unlimited",
	"guarantee",
	"free forever",
	"no limits",
	"{{",
	// fabricated by subagents on 2026-08-20; kept verbatim so the regression is caught, not described
	"typically contains only minutes of content",
	"growing faster than you can search",
	"broadcast breakdown, automatically",
}

// The hero panel is ~7KB of the fragment; anything much shorter lost it, which is what happened to
// 32 emails on 2026-08-20.
const (
	HTMLMin = 14000
	HTMLMax = 26000
	BodyMin = 400
)

var Address = regexp.MustCompile(`(?i)^[^\s@,;<>"]+@[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// An Audience is a template and the copy that goes with it. The list is not one population: some
// contacts came from a prospect search of sports organisations, and the rest from a marketing
// newsletter with no video business at all. One pitch cannot be true for both, and a false premise
// is worse than a generic one.
type Audience struct {
	Name         string
	TemplateFile string
	Subjects     []string
	RequiredHTML []string
	Openers      []RoleOpener
	Examples     []searchExample
	Example      string // the fallback
}

func (a *Audience) Template() string { return filepath.Join(Home, a.TemplateFile) }

// Audiences are keyed by name; campaign.json maps a CSV source column value onto one.
var Audiences = map[string]*Audience{}

var (
	AudienceBySource map[string]string
	DefaultAudience  = "broadcast"
)

// AudienceFor picks the copy for one contact. An unmapped source gets the default rather than an
// error: a new source value in the CSV must not stop a build, it must just be unsurprising.
func AudienceFor(source string) *Audience {
	if name, ok := AudienceBySource[strings.TrimSpace(source)]; ok {
		if a := Audiences[name]; a != nil {
			return a
		}
	}
	return Audiences[DefaultAudience]
}

// RoleOpener is one shape of the per-contact line: a role to match, and the copy with and without a
// company name.
type RoleOpener struct {
	Role      string
	Match     *regexp.Regexp // nil is the catch-all, and must be last
	WithCo    string
	WithoutCo string
}

// RoleOpeners: order matters, the most specific role wins. Every alternative is anchored with \b,
// because an unanchored one matches inside another word - "cto" inside Director, "vice" inside
// Service - and silently hands a producer the engineering line.
var RoleOpeners = []RoleOpener{
	{
		Role: "production",
		Match: regexp.MustCompile(`(?i)\b(producer|production|post-production|editor|editing|camera|videographer|` +
			`cinematographer|dop|colou?rist)\b`),
		WithCo:    "If you cut video at {company}, you already know the slow part isn’t the edit. It’s finding the moment worth cutting to.",
		WithoutCo: "If you cut video for a living, you already know the slow part isn’t the edit. It’s finding the moment worth cutting to.",
	},
	{
		Role: "engineering",
		Match: regexp.MustCompile(`(?i)\b(engineer|engineering|architect|cto|technical|developer|technology|` +
			`systems|devops|platform|infrastructure)\b`),
		WithCo:    "You keep {company}’s video running and pay to store all of it. What’s usually missing is a way to query what’s actually in there.",
		WithoutCo: "You keep the video running and pay to store all of it. What’s usually missing is a way to query what’s actually in there.",
	},
	{
		Role: "content",
		Match: regexp.MustCompile(`(?i)\b(content|editorial|programming|journalist|news|archive|archivist|librarian|` +
			`media manager|media services)\b`),
		WithCo:    "The footage {company} already owns gets a lot more useful when you can ask it a question instead of watching it again.",
		WithoutCo: "The footage you already own gets a lot more useful when you can ask it a question instead of watching it again.",
	},
	{
		Role:      "marketing",
		Match:     regexp.MustCompile(`(?i)\b(marketing|brand|social|communications|digital|audience|growth)\b`),
		WithCo:    "Getting clips out of {company}’s own footage means someone has to find them first, and that’s most of the work.",
		WithoutCo: "Getting clips out of your own footage means someone has to find them first, and that’s most of the work.",
	},
	{
		Role: "leadership",
		Match: regexp.MustCompile(`(?i)\b(ceo|founder|co-founder|president|managing director|chief|owner|` +
			`head of|vp|vice president|director|manager)\b`),
		WithCo:    "The video library at {company} keeps growing, and the cost of finding anything in it grows with it.",
		WithoutCo: "A video library keeps growing, and the cost of finding anything in it grows with it.",
	},
	{
		Role:      "generic",
		Match:     nil,
		WithCo:    "If {company} works with long-form video, this is worth two minutes of your time.",
		WithoutCo: "If you work with long-form video, this is worth two minutes of your time.",
	},
}

// SearchExample quotes a question the recipient might plausibly ask.
type searchExample struct {
	match   *regexp.Regexp
	example string
}

var searchExamples = []searchExample{
	{regexp.MustCompile(`(?i)tennis|atp|wta|racquet|racket`), "“find all the passing shot winners”"},
	{regexp.MustCompile(`(?i)football|soccer|fc\b|united|city|liga|serie|bundesliga|premier`),
		"“show me all the goals scored with a header”"},
	{regexp.MustCompile(`(?i)cricket|ipl|wicket`), "“show me every wicket taken before lunch”"},
	{regexp.MustCompile(`(?i)basket|nba|hoop`), "“show me every three-pointer in the fourth quarter”"},
	{regexp.MustCompile(`(?i)rugby|nrl|scrum`), "“find every try in the second half”"},
	{regexp.MustCompile(`(?i)motor|racing|formula|f1|speedway|rally`), "“show me every overtake on the main straight”"},
	{regexp.MustCompile(`(?i)news|journal|broadcast news|bulletin|press|editorial`),
		"“find every mention of the budget in last night’s bulletin”"},
}

const SearchDefault = "“show me all the goals scored with a header”"

func SearchExample(a *Audience, company, domain, title string) string {
	hay := company + " " + domain + " " + title
	for _, s := range a.Examples {
		if s.match.MatchString(hay) {
			return s.example
		}
	}
	return a.Example
}

// ShapeID identifies one piece of copy, independent of which company is interpolated into it.
// Reviewing 12 of these covers all 257 rows; reviewing the rendered paragraphs would be 231.
func ShapeID(audience, role string, named bool) string {
	which := "plain"
	if named {
		which = "named"
	}
	sum := sha1.Sum([]byte(audience + "|" + role + "|" + which))
	return hex.EncodeToString(sum[:])[:8]
}

// Opener picks the per-contact line: role first, company second, generic last - and never a line
// naming a company the list did not give us a usable name for.
func Opener(a *Audience, title, company string) (line, role string, named bool) {
	for _, o := range a.Openers {
		if o.Match != nil && !o.Match.MatchString(title) {
			continue
		}
		if company != "" {
			return replaceCompany(o.WithCo, company), o.Role, true
		}
		return o.WithoutCo, o.Role, false
	}
	panic("RoleOpeners must end with a catch-all")
}

func replaceCompany(s, company string) string {
	return strings.ReplaceAll(s, "{company}", company)
}

// ---------------------------------------------------------------- list filters

// ConsumerMail: a company name only goes in the copy when it reads as one. The list carries mail
// domains and bare hostnames in that column, and "The footage gmail already owns" is worse than
// saying nothing.
var (
	ConsumerMail = regexp.MustCompile(`(?i)^(gmail|googlemail|yahoo|ymail|hotmail|outlook|live|msn|aol|icloud|me\.com|` +
		`protonmail|proton|gmx|web\.de|qq|163|126|naver|daum|mail|email|yandex)\b`)
	Corp = regexp.MustCompile(`(?i)media|video|broadcast|\btv\b|television|studio|film|cinema|sport|stream|channel|` +
		`production|news|radio|entertain|content|network|vision|pictures|creative|group|` +
		`ltd|llc|inc\b|gmbh|\bbv\b|\bsa\b|\bag\b|corp|company|systems|digital|labs|` +
		`partners|solutions|technolog|university|college|records|press|publish`)
	PersonName = regexp.MustCompile(`^[A-ZÀ-Þ][a-zà-ÿ]+(?: [A-ZÀ-Þ][a-zà-ÿ]+){1,2}$`)
	// Mojibake is a name that survived a bad decode: a literal "?" or replacement character, or a
	// UTF-8 lead byte read as Latin-1 and followed by another high byte. Neither is a string to put
	// in front of a stranger. It catches the common double-encode; a single stray high byte
	// followed by ASCII reads as a legitimate accented name and is deliberately left alone.
	Mojibake = regexp.MustCompile("[?\uFFFD]|[\u00c3\u00c2\u00d0\u00d1\u00fe\u00fd][\u0080-\u00bf\u00a0-\u00ff]")

	// signup junk: disposable mailboxes and plus-tagged trial addresses
	DropEmail = regexp.MustCompile(`(?i)\+[^@]*@|@(mailinator|muck\.net|yopmail|guerrillamail|10minutemail|tempmail|` +
		`trashmail|sharklasers|dispostable|maildrop|getnada|throwaway)`)
	NoiseLocal = regexp.MustCompile(`(?i)^(test|demo|noreply|no-reply|postmaster|abuse|webmaster)\b`)

	// KAIROS reads footage the recipient owns. Someone with no video of their own is not a prospect,
	// and the copy is false for them.
	VideoSignal = regexp.MustCompile(`(?i)media|video|broadcast|\btv\b|television|studio|film|cinema|sport|stream|channel|` +
		`production|post-produc|news|radio|entertain|content|network|vision|pictures|` +
		`creative|footage|highlight|archive`)
	DropTitle = regexp.MustCompile(`(?i)recruit|talent|human resour|\bhr\b|payroll|account(ant|s payable|s receivable)|` +
		`legal counsel|paralegal|facilit|janitor|driver|nurse|physician|dental|therapist|` +
		`real estate|mortgage|insurance agent|financial advisor|teacher|professor|lecturer`)
	DropDomain = regexp.MustCompile(`(?i)\.(edu|gov|mil)$|dental|clinic|hospital|\blaw\b|bank|insur`)
	// "radiology" contains "radio", so a medical board reads as a broadcaster
	DropCompany = regexp.MustCompile(`(?i)radiolog|oncolog|cardiolog|patholog|pharma|medical|healthcare|\bdental\b|` +
		`hospital|clinic`)
)

const CapPerCompany = 3

// ---------------------------------------------------------------- address quality

// DNS changes, so an MX answer is not kept forever.
const MXCacheDays = 30

// ---------------------------------------------------------------- deliverability

// 603 contacted, 35 hard bounces, 5.8%. Google throttles a young domain around 2%, and this one was
// registered on 2026-08-17. Age in days -> the most that may leave in a UTC day.
var ramp = []struct {
	Days int
	Cap  int
}{{7, 40}, {14, 80}, {30, 150}, {60, 300}}

const RampMax = 500

// DailyCapOverride replaces the ramp entirely when campaign.json sets daily_cap. The ramp exists
// because a young domain sending at volume is how a domain gets filtered rather than throttled;
// overriding it is a deliberate choice about reputation, so it lives in config and not in a flag
// default.
var DailyCapOverride int

func DailyCap(ageDays int) int {
	if DailyCapOverride > 0 {
		return DailyCapOverride
	}
	return RampAt(ageDays)
}

// RampAt is what the warm-up ramp would allow, whether or not an override is in force. Worth
// printing next to an override so the gap is visible rather than forgotten.
func RampAt(ageDays int) int {
	for _, r := range ramp {
		if ageDays < r.Days {
			return r.Cap
		}
	}
	return RampMax
}

// DomainAgeDays drives the warm-up ramp. With no registration date configured the domain is
// treated as brand new, which is the cautious answer.
func DomainAgeDays(today time.Time) int {
	reg, err := time.Parse("2006-01-02", DomainRegistered)
	if err != nil {
		return 0
	}
	y, m, d := today.Date()
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return int(t.Sub(reg).Hours() / 24)
}

func TemplateSHA() (string, error) {
	b, err := os.ReadFile(Template)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ---------------------------------------------------------------- the lock

// ErrLocked is returned when another build, review or send holds the campaign lock.
var ErrLocked = errors.New("another build, review or send holds the lock. Nothing written")

// TryLock takes an exclusive advisory lock over the whole campaign directory. Rebuilding the queue
// from a stale view of what had already gone out is what double-mailed a real prospect on
// 2026-08-20.
//
// The file is returned, not closed: the lock lives as long as the caller keeps it open.
func TryLock() (*os.File, error) {
	if err := os.MkdirAll(Home, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(Lock, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrLocked
	}
	return f, nil
}

// Stamp is the timestamp format the Python wrote, kept so old and new ledger lines parse the same.
func Stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000-07:00")
}

func MustSHA() string {
	s, err := TemplateSHA()
	if err != nil {
		panic(fmt.Sprintf("template unreadable: %v", err))
	}
	return s
}
