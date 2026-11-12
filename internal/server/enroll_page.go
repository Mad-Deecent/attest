package server

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mad-deecent/attest/internal/assurance"
	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/identity"
)

//go:embed enroll
var enrollFiles embed.FS

var (
	enrollStatic, _ = fs.Sub(enrollFiles, "enroll/static")
	enrollTemplate  = template.Must(template.ParseFS(enrollFiles, "enroll/enroll.html"))
)

// voicePrompts are the lines offered, one at a time, while someone records
// their voice: sentences to read mixed with the kind of thing people say to an
// assistant, so the sample covers both reading and talking. About 40 seconds
// of speech in all; the page wraps around if more is needed.
var voicePrompts = []voicePrompt{
	{Read: true, Text: "On a quiet morning, I asked the kitchen speaker to play some jazz and to tell me how warm it is outside."},
	{Text: "Turn the living room lights down to thirty percent."},
	{Text: "What's on my calendar for tomorrow afternoon?"},
	{Read: true, Text: "The old bridge over the river was painted bright yellow, and on clear evenings you can see it from the hill behind the school."},
	{Text: "Set a timer for twelve minutes and remind me to check the oven."},
	{Text: "Lock the front door and turn on the porch light."},
	{Read: true, Text: "We packed sandwiches, two thermoses of tea, and a map we never once looked at."},
	{Text: "Play the next episode of the podcast I was listening to yesterday."},
	{Text: "Add oat milk, lemons, and a loaf of bread to the shopping list."},
	{Read: true, Text: "If it rains on Saturday, we'll move the picnic indoors and play board games instead."},
	{Text: "How long will it take to drive to the airport right now?"},
	{Text: "Call my sister and tell her I'm running ten minutes late."},
}

// voiceTargetSeconds is how much speech the page aims for. The voice service
// decides what is enough (its own minimum, 15 seconds by default) and the
// page treats its answer as the truth; aiming higher leaves room for the
// page's rough estimate of when someone is speaking.
const voiceTargetSeconds = 30

// voiceMinFallback is the voice service's default minimum. The page asks the
// voice service for its configured minimum and uses this only until it
// answers, or if it cannot be asked.
const voiceMinFallback = 15

// Where the front door's project links go.
const (
	repoURL = "https://github.com/Mad-Deecent/attest"
	docsURL = repoURL + "#readme"
)

type voicePrompt struct {
	Read bool
	Text string
}

// doneMessages are the fixed confirmations a redirect can ask for, so nothing
// from the URL is echoed into the page.
var doneMessages = map[string]string{
	"saved":        "Your choices are saved.",
	"revoked":      "Removed. Your assistant can no longer do that for you.",
	"signedout":    "You're signed out.",
	"voicedeleted": "Your voice is deleted. The voice service no longer holds it, and attest no longer lists it.",
}

// The screens of the enrollment page, in the order a newcomer meets them.
// Returning people land on "manage".
var steps = []struct{ screen, label string }{{"you", "You"}, {"voice", "Voice"}, {"choices", "Choices"}}

type enrollView struct {
	Screen, Title   string
	Configured      bool
	SignedIn        bool
	SignInAvailable bool
	Status, Error   string
	IdPHost         string
	DocsURL         string
	RepoURL         string

	CSRF          string
	Name, Account string
	Roles         []string
	Steps         []stepView
	Returning     bool

	VoiceURL   string
	VoiceDone  string // the date in full, on the voice step
	VoiceSaved string // the date in short, on the manage screen
	Prompts    []voicePrompt
	Target     int
	MinSeconds int
	Next, Skip string

	NoRoles   bool
	Agents    []agentView
	SaveLabel string
	CanChoose bool
	May       []mayView
}

type stepView struct {
	Label, Class string
	Current      bool
}

type agentView struct {
	ID, Name string
	Perms    []permView
}

type permView struct {
	Name, Label string
	Checked     bool
	Level       levelView
}

// levelView is a level as the page draws it: the glyph's filled steps (0 to
// 3) and the word beside it.
type levelView struct {
	Word  string
	Glyph int
	// Desc says in plain words what the level asks, for a tooltip.
	Desc string
}

// mayView is what one agent may do for the person, one row per action.
type mayView struct {
	Agent, Name string
	Rows        []rowView
}

type rowView struct {
	Agent, Perm, Label string
	Level              levelView
	// Managed marks an action an administrator set up. The person sees it,
	// with its limits, but cannot change it here.
	Managed bool
	Limits  string
}

func (s *Server) enrollPage(w http.ResponseWriter, r *http.Request) {
	l := s.cur()
	if l.enrolling() && !s.enrollSignedIn(r) {
		// Signed out, enrollment starts at the front door.
		target := "/"
		if done := r.URL.Query().Get("done"); doneMessages[done] != "" {
			target += "?done=" + done
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	s.renderEnroll(w, r, http.StatusOK, "")
}

// renderFront renders the sign-in page.
func (s *Server) renderFront(w http.ResponseWriter, r *http.Request, status int, problem string) {
	l := s.cur()
	v := enrollView{Screen: "front", Title: "Sign in · attest", Error: problem, Status: doneMessages[r.URL.Query().Get("done")]}
	v.Configured = l.enrolling()
	v.SignInAvailable = v.Configured
	s.writePage(w, l, status, &v)
}

// renderEnroll renders the enrollment page for the request's session, or the
// sign-in page with a problem when there is no session.
func (s *Server) renderEnroll(w http.ResponseWriter, r *http.Request, status int, problem string) {
	l := s.cur()
	v := enrollView{Configured: l.enrolling(), Error: problem, Status: doneMessages[r.URL.Query().Get("done")]}
	if !v.Configured {
		v.Screen, v.Title = "unconfigured", "Enrollment · attest"
		s.writePage(w, l, status, &v)
		return
	}
	_, sess, ok := s.enroll.session(r, s.now())
	ident, known := s.registry.Get(sess.identityID)
	if !ok || !known {
		s.renderFront(w, r, status, problem)
		return
	}
	s.signedInView(&v, l, ident, sess, r.URL.Query().Get("step"))
	s.writePage(w, l, status, &v)
}

func (s *Server) writePage(w http.ResponseWriter, l *live, status int, v *enrollView) {
	v.DocsURL, v.RepoURL = docsURL, repoURL
	if l.oidc != nil {
		if u, err := url.Parse(l.oidc.cfg.Issuer); err == nil {
			v.IdPHost = u.Host
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; "+
		"connect-src 'self'"+origin(v.VoiceURL)+"; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	if err := enrollTemplate.Execute(w, v); err != nil {
		log.Printf("enroll: render: %v", err)
	}
}

func (s *Server) signedInView(v *enrollView, l *live, ident identity.Identity, sess session, step string) {
	policy := l.policy()
	now := s.now()
	v.SignedIn = true
	v.CSRF = sess.csrf
	v.Name, v.Account = ident.DisplayName, sess.account
	v.Roles = s.rbac.Roles(ident.ID)
	v.NoRoles = len(v.Roles) == 0
	v.VoiceURL = s.enroll.voiceURL
	if ident.VoiceEnrolledAt != nil {
		v.VoiceDone = day(*ident.VoiceEnrolledAt)
		v.VoiceSaved = shortDay(*ident.VoiceEnrolledAt, now)
	}
	live := s.liveDelegations(ident.ID, "", now)
	// Someone who has a voice on record or anything allowed has been here
	// before, and starts from what they have.
	v.Returning = ident.VoiceEnrolledAt != nil || len(live) > 0

	switch step {
	case "you", "voice", "choices", "done", "manage":
	default:
		step = "you"
		if v.Returning {
			step = "manage"
		}
	}
	if step == "voice" && v.VoiceURL == "" {
		step = "choices"
	}
	v.Screen = step
	v.Title = map[string]string{
		"you": "You · Enroll · attest", "voice": "Voice · Enroll · attest", "choices": "Choices · Enroll · attest",
		"done": "Enrolled · attest", "manage": "Your enrollment · attest",
	}[step]
	if !v.Returning || step == "you" {
		phase := map[string]int{"you": 0, "voice": 1, "choices": 2, "done": 2}[step]
		if step != "manage" {
			for i, st := range steps {
				if st.screen == "voice" && v.VoiceURL == "" {
					continue
				}
				sv := stepView{Label: st.label, Current: i == phase}
				if i <= phase {
					sv.Class = "reached"
				}
				v.Steps = append(v.Steps, sv)
			}
		}
	}

	v.Prompts = voicePrompts
	v.Target = voiceTargetSeconds
	v.MinSeconds = voiceMinFallback
	v.Next, v.Skip = "/enroll?step=choices", "/enroll?step=choices"
	if v.Returning {
		v.Next, v.Skip = "/enroll", "/enroll"
	}

	levels := levelsOf(policy)
	checked := 0
	for _, o := range s.offers(policy, ident.ID) {
		// What the person allowed before stays ticked, so changing one choice
		// never drops the others. A newcomer has nothing ticked.
		own := covered(o.permissions, s.ownConsent(ident.ID, o.agent, now))
		agent := agentView{ID: o.agent, Name: label(o.agent, true)}
		for _, p := range o.permissions {
			pv := permView{Name: p, Label: permLabel(policy, p, true), Checked: slices.Contains(own, p), Level: levels.of(policy.Permissions[p])}
			if pv.Checked {
				checked++
			}
			agent.Perms = append(agent.Perms, pv)
		}
		// Weakest level first, then by label.
		slices.SortStableFunc(agent.Perms, func(a, b permView) int {
			if a.Level.Glyph != b.Level.Glyph {
				return a.Level.Glyph - b.Level.Glyph
			}
			return strings.Compare(a.Label, b.Label)
		})
		v.Agents = append(v.Agents, agent)
	}
	v.CanChoose = len(v.Agents) > 0
	v.SaveLabel = saveLabel(checked)
	v.May = s.mayViews(policy, levels, ident.ID, live, now)
}

func saveLabel(n int) string {
	switch n {
	case 0:
		return "Save, allowing nothing"
	case 1:
		return "Save 1 choice"
	default:
		return "Save " + strconv.Itoa(n) + " choices"
	}
}

// mayViews lists, per agent, every action the person's live delegations
// allow: their own first, each with Revoke, then what an administrator set up.
func (s *Server) mayViews(policy decision.Policy, levels ladder, identityID string, live []decision.Delegation, now time.Time) []mayView {
	byAgent := map[string]*mayView{}
	var order []string
	seen := map[string]bool{}
	for _, d := range live {
		mv := byAgent[d.Agent]
		if mv == nil {
			mv = &mayView{Agent: d.Agent, Name: label(d.Agent, true)}
			byAgent[d.Agent] = mv
			order = append(order, d.Agent)
		}
		managed := d.GrantedBy != identityID
		for _, p := range d.Permissions {
			key := fmt.Sprintf("%s\x00%s\x00%t", d.Agent, p, managed)
			if seen[key] {
				continue
			}
			seen[key] = true
			row := rowView{Agent: d.Agent, Perm: p, Label: permLabel(policy, p, true), Level: levels.of(policy.Permissions[p]), Managed: managed}
			if p == "*" {
				row.Label, row.Level = "Anything your roles allow", levelView{}
			}
			if managed {
				row.Limits = limits(d, levels, now)
			}
			mv.Rows = append(mv.Rows, row)
		}
	}
	slices.Sort(order)
	out := make([]mayView, 0, len(order))
	for _, a := range order {
		mv := byAgent[a]
		slices.SortStableFunc(mv.Rows, func(x, y rowView) int {
			if x.Managed != y.Managed {
				if x.Managed {
					return 1
				}
				return -1
			}
			if x.Level.Glyph != y.Level.Glyph {
				return x.Level.Glyph - y.Level.Glyph
			}
			return strings.Compare(x.Label, y.Label)
		})
		out = append(out, *mv)
	}
	return out
}

// limits says, in plain words, who set up a delegation and how far it goes.
func limits(d decision.Delegation, levels ladder, now time.Time) string {
	var parts []string
	if len(d.Zones) > 0 {
		zones := make([]string, len(d.Zones))
		for i, z := range d.Zones {
			zones[i] = label(z, false)
		}
		parts = append(parts, strings.Join(zones, " and ")+" only")
	}
	if d.Ceiling != "" {
		parts = append(parts, "up to "+levels.of(d.Ceiling).Word)
	}
	if d.ExpiresAt != nil {
		parts = append(parts, "until "+shortDay(*d.ExpiresAt, now))
	}
	out := "Set up by an administrator"
	if len(parts) > 0 {
		out += ": " + strings.Join(parts, ", ")
	}
	return out + ". Ask them to change it."
}

// ladder maps band names to the words and glyphs people see. The shipped
// bands read Likely, Sure and Confirmed; a deployment's own band names are
// shown as they are. The glyph has three steps, and longer or shorter
// ladders spread over them, as in the console.
type ladder map[string]levelView

var levelWords = map[string]string{"floor": "Anyone", "presumed": "Likely", "confident": "Sure", "corroborated": "Confirmed"}

func levelsOf(policy decision.Policy) ladder {
	out := ladder{"": {Word: "Anyone"}, "floor": {Word: "Anyone"}}
	n := len(policy.Bands)
	for i, b := range policy.Bands {
		word := levelWords[b.Name]
		if word == "" {
			word = b.Name
		}
		out[b.Name] = levelView{Word: word, Glyph: min(3, max(1, int(math.Round(float64((i+1)*3)/float64(n))))), Desc: describeBand(b)}
	}
	return out
}

func (l ladder) of(band string) levelView {
	if v, ok := l[band]; ok {
		return v
	}
	return levelView{Word: band}
}

// describeBand says, in plain words, how sure attest must be that it is you.
func describeBand(b assurance.Band) string {
	need := "It must be able to tell it's you"
	if b.MinConfidence > 0 {
		need = "It must be at least " + strconv.FormatFloat(b.MinConfidence*100, 'g', 4, 64) + "% sure it's you"
	}
	if b.MinModalities > 1 {
		need += ", recognizing you in " + count(b.MinModalities) + " separate ways, such as your face and your voice"
	}
	if b.MaxAgeSeconds > 0 {
		need += ", within the last " + duration(time.Duration(b.MaxAgeSeconds)*time.Second)
	}
	for _, role := range b.CoPresence {
		need += ", with someone who is a " + label(role, false) + " there too"
	}
	return need + "."
}

func count(n int) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	return strconv.Itoa(n)
}

func duration(d time.Duration) string {
	unit := func(n int, one string) string {
		if n == 1 {
			return one
		}
		return strconv.Itoa(n) + " " + one + "s"
	}
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return unit(int(d/time.Hour), "hour")
	case d >= time.Minute && d%time.Minute == 0:
		return unit(int(d/time.Minute), "minute")
	default:
		return unit(int(d/time.Second), "second")
	}
}

// permLabel is the tenant's label for a permission (policy.labels), else one
// derived from its name.
func permLabel(policy decision.Policy, name string, capital bool) string {
	if l := policy.Labels[name]; l != "" {
		return l
	}
	return label(name, capital)
}

// label turns a policy name such as "door.unlock" into "door unlock".
func label(name string, capital bool) string {
	out := strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(name)
	if capital {
		if r, n := utf8.DecodeRuneInString(out); n > 0 {
			out = string(unicode.ToUpper(r)) + out[n:]
		}
	}
	return out
}

func day(t time.Time) string { return t.Format("2 January 2006") }

// shortDay is "Nov 10", with the year when it is not this year's.
func shortDay(t, now time.Time) string {
	if t.Year() == now.Year() {
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

// origin is the scheme and host of a URL, for the page's connect-src.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Host == "" {
		return ""
	}
	return fmt.Sprintf(" %s://%s", u.Scheme, u.Host)
}
