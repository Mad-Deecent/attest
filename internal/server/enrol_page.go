package server

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
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
)

//go:embed enrol
var enrolFiles embed.FS

var (
	enrolStatic, _ = fs.Sub(enrolFiles, "enrol/static")
	enrolTemplate  = template.Must(template.ParseFS(enrolFiles, "enrol/enrol.html"))
)

// readAloud is the sentence offered for a voice recording.
const readAloud = "On a quiet morning, I asked the kitchen speaker to play some jazz and to tell me how warm it is outside."

// doneMessages are the fixed confirmations a redirect can ask for, so nothing
// from the URL is echoed into the page.
var doneMessages = map[string]string{
	"saved":     "Your choices are saved.",
	"revoked":   "Removed. Your assistant can no longer do that for you.",
	"signedout": "You're signed out.",
}

type enrolView struct {
	Configured bool
	SignedIn   bool
	Status     string
	Error      string
	Name       string
	CSRF       string
	Current    int
	Sentence   string
	VoiceURL   string
	VoiceDone  string
	NoRoles    bool
	Agents     []agentView
	Granted    []grantView
}

type agentView struct {
	ID, Name string
	Groups   []groupView
}

type groupView struct {
	Need        string
	Permissions []permView
}

type permView struct {
	Name, Label string
	Checked     bool
}

type grantView struct {
	ID, Agent, What, Since string
	// Managed marks a delegation an administrator made. The person sees it
	// but cannot change it here; Limits says where and how far it applies.
	Managed bool
	Limits  string
}

func (s *Server) enrolPage(w http.ResponseWriter, r *http.Request) {
	s.renderEnrol(w, r, http.StatusOK, "")
}

func (s *Server) renderEnrol(w http.ResponseWriter, r *http.Request, status int, problem string) {
	l := s.cur()
	v := enrolView{Configured: l.enrolling(), Error: problem, Status: doneMessages[r.URL.Query().Get("done")]}
	if v.Configured {
		v.VoiceURL = s.enrol.voiceURL
		if _, sess, ok := s.enrol.session(r, s.now()); ok {
			if ident, known := s.registry.Get(sess.identityID); known {
				s.signedInView(&v, l.policy(), ident.ID, sess)
				v.Name = ident.DisplayName
				if ident.VoiceEnrolledAt != nil {
					v.VoiceDone = day(*ident.VoiceEnrolledAt)
				}
				v.Current = 3
				if v.VoiceURL != "" && v.VoiceDone == "" {
					v.Current = 2
				}
			}
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; "+
		"connect-src 'self'"+origin(v.VoiceURL)+"; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	if err := enrolTemplate.Execute(w, v); err != nil {
		log.Printf("enrol: render: %v", err)
	}
}

func (s *Server) signedInView(v *enrolView, policy decision.Policy, identityID string, sess session) {
	v.SignedIn = true
	v.CSRF = sess.csrf
	v.Sentence = readAloud
	v.NoRoles = len(s.rbac.Roles(identityID)) == 0
	now := s.now()
	for _, o := range s.offers(policy, identityID) {
		live := covered(o.permissions, s.ownConsent(identityID, o.agent, now))
		agent := agentView{ID: o.agent, Name: label(o.agent, true)}
		// Group by band, weakest first.
		for _, band := range policy.Bands {
			g := groupView{Need: describeBand(band)}
			for _, p := range o.permissions {
				if policy.Permissions[p] == band.Name {
					g.Permissions = append(g.Permissions, permView{Name: p, Label: permLabel(policy, p, true), Checked: slices.Contains(live, p)})
				}
			}
			if len(g.Permissions) > 0 {
				agent.Groups = append(agent.Groups, g)
			}
		}
		v.Agents = append(v.Agents, agent)
	}
	for _, d := range s.liveDelegations(identityID, "", now) {
		what := make([]string, len(d.Permissions))
		for i, p := range d.Permissions {
			what[i] = permLabel(policy, p, false)
			if p == "*" {
				what[i] = "anything you're allowed to do"
			}
		}
		g := grantView{ID: d.ID, Agent: label(d.Agent, true), What: strings.Join(what, ", "), Since: day(d.GrantedAt)}
		if d.GrantedBy != identityID {
			g.Managed = true
			g.Limits = s.limits(d)
		}
		v.Granted = append(v.Granted, g)
	}
}

// limits describes, in plain words, the restrictions on an administrator's
// delegation.
func (s *Server) limits(d decision.Delegation) string {
	var parts []string
	if len(d.Zones) > 0 {
		zones := make([]string, len(d.Zones))
		for i, z := range d.Zones {
			zones[i] = label(z, false)
		}
		parts = append(parts, "only in: "+strings.Join(zones, ", "))
	}
	if d.Ceiling != "" {
		parts = append(parts, "only for things that need at most the "+label(d.Ceiling, false)+" level of certainty")
	}
	if d.ExpiresAt != nil {
		parts = append(parts, "until "+day(*d.ExpiresAt))
	}
	return strings.Join(parts, "; ")
}

// describeBand says, in plain words, how sure attest must be that it is you.
func describeBand(b assurance.Band) string {
	need := "It must be able to tell it's you"
	if b.MinConfidence > 0 {
		need = "It must be at least " + strconv.FormatFloat(b.MinConfidence*100, 'g', 4, 64) + "% sure it's you"
	}
	if b.MinModalities > 1 {
		need += ", recognising you in " + count(b.MinModalities) + " separate ways, such as your face and your voice"
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

// origin is the scheme and host of a URL, for the page's connect-src.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Host == "" {
		return ""
	}
	return fmt.Sprintf(" %s://%s", u.Scheme, u.Host)
}
