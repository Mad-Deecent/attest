package server

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mad-deecent/attest/internal/decision"
)

// Until setup is complete, "/" leads to setup; afterwards it is the sign-in
// page, unless the browser is already signed in somewhere.
func TestFrontDoor(t *testing.T) {
	now := time.Now()
	srv, _ := unconfigured(t, &now)
	c := newConsole(t, srv.Handler())
	res, err := c.c.Get(c.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Request.URL.Path != "/console/" {
		t.Fatalf("before setup, / ended at %s", res.Request.URL)
	}
	if res, _ = c.c.Get(c.base + "/login"); res.Request.URL.Path != "/console/" {
		t.Fatalf("before setup, /login ended at %s", res.Request.URL)
	}
	res.Body.Close()

	e := newEnrollEnv(t, "")
	jar, _ := cookiejar.New(nil)
	b := &browser{t: t, c: &http.Client{Jar: jar}, base: e.ts.URL}
	if code := b.get("/"); code != http.StatusOK {
		t.Fatalf("GET /: %d", code)
	}
	issuer, _ := url.Parse(e.is.srv.URL)
	for _, want := range []string{
		"Identity assurance for agents that listen.", "IDENTITY ASSURANCE · MIT LICENSE",
		`href="/login">Continue with your account`, "via " + issuer.Host, "Not enrolled yet?",
		`href="/enroll/login">Enroll</a>`, `href="https://github.com/Mad-Deecent/attest">GitHub</a>`, ">Documentation</a>",
	} {
		if !strings.Contains(b.body, want) {
			t.Errorf("sign-in page lacks %q", want)
		}
	}

	// Signed in to enrollment, "/" goes there instead.
	ana := e.signIn(ana)
	ana.get("/")
	if !strings.Contains(ana.body, "You’re signed in") {
		t.Fatalf("/ while signed in to enrollment: %s", ana.body)
	}
	// Signed in to the console, "/" goes to the console.
	adm := e.consoleSignIn(admin, "/console/")
	adm.navigate("/")
	if adm.at.Path != "/console/" {
		t.Fatalf("/ while signed in to the console ended at %s", adm.at)
	}
	// Signing out lands on the front door, which says so.
	ana.post("/enroll/logout", url.Values{"csrf": {ana.csrf()}})
	if !strings.Contains(ana.body, "You&#39;re signed out.") || !strings.Contains(ana.body, "Continue with your account") {
		t.Fatalf("after sign-out: %s", ana.body)
	}
}

// frontSignIn walks the front door's /login through the provider as p.
func (e *enrollEnv) frontSignIn(p person) *adminBrowser {
	e.t.Helper()
	e.is.signIn(p)
	b := e.newAdminBrowser()
	b.navigate("/login")
	return b
}

// One sign-in, routed by role: administrators land in the console, everyone
// else in their enrollment. Enroll always means enrollment.
func TestOneSignInRoutesByRole(t *testing.T) {
	e := newEnrollEnv(t, "")

	adm := e.frontSignIn(admin)
	if adm.at.Path != "/console/" || adm.cookie(consoleCookie) == "" {
		t.Fatalf("an administrator from the front door ended at %s", adm.at)
	}
	var info struct {
		SignedIn bool   `json:"signedIn"`
		CSRF     string `json:"csrf"`
	}
	adm.api("GET", "/v1/console/session", nil, http.StatusOK, &info)
	if !info.SignedIn {
		t.Fatal("an administrator from the front door has no console session")
	}
	if ids := e.identities(); len(ids) != 0 {
		t.Fatalf("an administrator's sign-in enrolled someone: %+v", ids)
	}

	person := e.frontSignIn(ana)
	if person.at.Path != "/enroll" || person.cookie(consoleCookie) != "" {
		t.Fatalf("a person from the front door ended at %s", person.at)
	}
	person.api("GET", "/v1/console/session", nil, http.StatusOK, &info)
	if info.SignedIn {
		t.Fatal("a person who is not an administrator holds a console session")
	}
	person.api("GET", "/v1/identities", nil, http.StatusUnauthorized, nil)
	if ids := e.identities(); len(ids) != 1 || ids[0].DisplayName != "Ana Lima" {
		t.Fatalf("a person from the front door was not enrolled: %+v", ids)
	}

	// Enroll leads to enrollment, even for an administrator.
	e.is.signIn(admin)
	enrolling := e.newAdminBrowser()
	enrolling.navigate("/enroll/login")
	if enrolling.at.Path != "/enroll" || enrolling.cookie(consoleCookie) != "" {
		t.Fatalf("an administrator who chose Enroll ended at %s", enrolling.at)
	}
	if ids := e.identities(); len(ids) != 2 {
		t.Fatalf("an administrator who chose Enroll was not enrolled: %+v", ids)
	}

	// The console's own sign-in still refuses people without the role.
	if n := e.consoleSignIn(ana, "/console/policy"); n.at.Path != "/console/no-access" {
		t.Fatalf("console sign-in without the role ended at %s", n.at)
	}
}

// Someone already signed in is not sent round the provider again, and a
// fresh session id is minted whenever the provider does sign someone in.
func TestSignedInPeopleAreNotAskedAgain(t *testing.T) {
	e := newEnrollEnv(t, "")
	b := e.signIn(ana)
	first := e.identities()[0].ID
	// Were the provider asked, it would now sign in someone else.
	e.is.signIn(person{sub: "kc-ben", name: "Ben"})
	for _, path := range []string{"/enroll/login", "/login"} {
		b.get(path)
		if !strings.Contains(b.body, "Ana Lima") {
			t.Fatalf("%s while signed in: %s", path, b.body)
		}
	}
	if ids := e.identities(); len(ids) != 1 || ids[0].ID != first {
		t.Fatalf("the provider was asked again: %+v", ids)
	}

	// A planted session cookie is not the one that becomes signed in.
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(e.ts.URL + "/")
	jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: "planted", Path: "/"}})
	fresh := &browser{t: t, c: &http.Client{Jar: jar}, base: e.ts.URL}
	e.is.signIn(ana)
	fresh.get("/enroll/login")
	for _, c := range jar.Cookies(u) {
		if c.Name == sessionCookie && c.Value == "planted" {
			t.Fatal("sign-in kept the planted session id")
		}
	}
	if !strings.Contains(fresh.body, "Ana Lima") {
		t.Fatalf("not signed in: %s", fresh.body)
	}
}

func (b *browser) ticket(path string) string {
	b.t.Helper()
	b.get("/enroll")
	if code := b.post(path, nil, "X-CSRF-Token", b.csrf()); code != http.StatusOK {
		b.t.Fatalf("%s: %d %s", path, code, b.body)
	}
	var issued struct{ Ticket string }
	if err := json.Unmarshal([]byte(b.body), &issued); err != nil || issued.Ticket == "" {
		b.t.Fatalf("%s: %q %v", path, b.body, err)
	}
	return issued.Ticket
}

const (
	enrollRedeem = "/v1/enrollment-tickets/redeem"
	deleteRedeem = "/v1/voice-deletion-tickets/redeem"
)

// Deleting a voice: the page gets a ticket good only for deletion, the voice
// service redeems it, and attest forgets the enrollment date before it
// answers.
func TestDeleteMyVoice(t *testing.T) {
	e := newEnrollEnv(t, "")
	b := e.signIn(ana)
	id := e.identities()[0].ID
	e.api.call("voice", "POST", enrollRedeem, map[string]string{"ticket": b.ticket("/enroll/ticket")}, http.StatusOK, nil)
	if e.identities()[0].VoiceEnrolledAt == nil {
		t.Fatal("no voice on record after enrolling")
	}
	b.get("/enroll")
	if !strings.Contains(b.body, "Delete my voice") || !strings.Contains(b.body, "Saved "+shortDay(time.Now(), time.Now())) {
		t.Fatalf("manage screen does not offer deletion: %s", b.body)
	}

	ticket := b.ticket("/enroll/voice-deletion-ticket")
	// Only a producer may redeem it, and only for deletion.
	e.api.call("agent", "POST", deleteRedeem, map[string]string{"ticket": ticket}, http.StatusForbidden, nil)
	e.api.call("voice", "POST", enrollRedeem, map[string]string{"ticket": ticket}, http.StatusNotFound, nil)
	var out struct {
		IdentityID string `json:"identityId"`
		HadVoice   bool   `json:"hadVoice"`
	}
	e.api.call("voice", "POST", deleteRedeem, map[string]string{"ticket": ticket}, http.StatusOK, &out)
	if out.IdentityID != id || !out.HadVoice {
		t.Fatalf("redeemed: %+v", out)
	}
	if e.identities()[0].VoiceEnrolledAt != nil {
		t.Fatal("the enrollment date is still set after deletion")
	}
	// A ticket is spent once.
	e.api.call("voice", "POST", deleteRedeem, map[string]string{"ticket": ticket}, http.StatusNotFound, nil)

	b.get("/enroll?step=manage&done=voicedeleted")
	if !strings.Contains(b.body, "Your voice is deleted.") || !strings.Contains(b.body, "Not recorded") || strings.Contains(b.body, "Delete my voice") {
		t.Fatalf("manage screen after deletion: %s", b.body)
	}

	// With no voice on record a deletion can still be asked for, so one the
	// voice service did not finish can be retried.
	e.api.call("voice", "POST", deleteRedeem, map[string]string{"ticket": b.ticket("/enroll/voice-deletion-ticket")}, http.StatusOK, &out)
	if out.HadVoice {
		t.Fatalf("second deletion: %+v", out)
	}
}

// A ticket is good for its own purpose only, and presenting it for another
// does not spend it. A new ticket of either kind voids the person's older
// ones, so a recording on its way cannot store a voice after a deletion.
func TestTicketsAreBoundToTheirPurpose(t *testing.T) {
	e := newEnrollEnv(t, "")
	b := e.signIn(ana)
	enroll := b.ticket("/enroll/ticket")
	e.api.call("voice", "POST", deleteRedeem, map[string]string{"ticket": enroll}, http.StatusNotFound, nil)
	e.api.call("voice", "POST", enrollRedeem, map[string]string{"ticket": enroll}, http.StatusOK, nil)

	del := b.ticket("/enroll/voice-deletion-ticket")
	e.api.call("voice", "POST", enrollRedeem, map[string]string{"ticket": del}, http.StatusNotFound, nil)
	if e.identities()[0].VoiceEnrolledAt == nil {
		t.Fatal("a deletion ticket presented for enrollment changed the voice on record")
	}
	e.api.call("voice", "POST", deleteRedeem, map[string]string{"ticket": del}, http.StatusOK, nil)

	pending := b.ticket("/enroll/ticket")
	del = b.ticket("/enroll/voice-deletion-ticket")
	e.api.call("voice", "POST", enrollRedeem, map[string]string{"ticket": pending}, http.StatusNotFound, nil)
	e.api.call("voice", "POST", deleteRedeem, map[string]string{"ticket": del}, http.StatusOK, nil)
	if e.identities()[0].VoiceEnrolledAt != nil {
		t.Fatal("a voice is on record after deletion")
	}

	// Signed out, the page gets a JSON 401, not a redirect.
	jar, _ := cookiejar.New(nil)
	stranger := &browser{t: t, c: &http.Client{Jar: jar}, base: e.ts.URL}
	if code := stranger.post("/enroll/voice-deletion-ticket", nil); code != http.StatusUnauthorized || !strings.Contains(stranger.body, `"error"`) {
		t.Fatalf("deletion ticket while signed out: %d %s", code, stranger.body)
	}
	if code := b.post("/enroll/voice-deletion-ticket", nil); code != http.StatusForbidden {
		t.Fatalf("deletion ticket without the CSRF token: %d", code)
	}
}

func TestOnlyTheVoiceProducerDeletes(t *testing.T) {
	e := newEnrollEnv(t, "")
	e.cfg.Enroll.VoiceProducer = "mic"
	e.restart()
	b := e.signIn(ana)
	del := map[string]string{"ticket": b.ticket("/enroll/voice-deletion-ticket")}
	e.api.call("face", "POST", deleteRedeem, del, http.StatusForbidden, nil)
	e.api.call("voice", "POST", deleteRedeem, del, http.StatusOK, nil)
}

// If attest cannot save that the voice is gone, it puts the date back and
// tells the producer to delete nothing: the two never disagree that way.
func TestADeletionThatCannotBeSavedDeletesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e := newEnrollEnv(t, filepath.Join(dir, "attest.json"))
	b := e.signIn(ana)
	e.api.call("voice", "POST", enrollRedeem, map[string]string{"ticket": b.ticket("/enroll/ticket")}, http.StatusOK, nil)
	del := b.ticket("/enroll/voice-deletion-ticket")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	e.api.call("voice", "POST", deleteRedeem, map[string]string{"ticket": del}, http.StatusInternalServerError, nil)
	if e.identities()[0].VoiceEnrolledAt == nil {
		t.Fatal("the date was cleared though the deletion could not be saved")
	}
}

// Revoke withdraws one action and leaves the others the person allowed.
func TestRevokeOneAction(t *testing.T) {
	e := newEnrollEnv(t, "")
	b := e.signIn(ana)
	id := e.identities()[0].ID
	b.consent(map[string][]string{"concierge": {"music.control", "calendar.read"}, "doorman": {"door.unlock"}})
	until := time.Now().Add(48 * time.Hour)
	e.api.call("admin", "POST", "/v1/delegations", decision.Delegation{
		IdentityID: id, Agent: "concierge", Permissions: []string{"door.unlock"}, Zones: []string{"hallway"},
		ExpiresAt: &until, GrantedBy: "admin",
	}, http.StatusCreated, nil)
	e.api.see("face", "track-1", "face", id, 8)
	e.api.see("voice", "track-1", "voice", id, 8)
	// The agent token is concierge's.
	decide := func(permission string) decision.Decision {
		var d decision.Decision
		e.api.call("agent", "POST", "/v1/decisions", decision.Request{Subject: "track-1", Permission: permission}, http.StatusOK, &d)
		return d
	}
	if d := decide("calendar.read"); !d.Granted {
		t.Fatalf("calendar before revoke: %+v", d)
	}

	revoke := func(agent, perm string) int {
		t.Helper()
		b.get("/enroll")
		return b.post("/enroll/revoke", url.Values{"csrf": {b.csrf()}, "agent": {agent}, "perm": {perm}})
	}
	if code := b.post("/enroll/revoke", url.Values{"agent": {"concierge"}, "perm": {"calendar.read"}}); code != http.StatusForbidden {
		t.Fatalf("revoke without the CSRF token: %d", code)
	}
	if code := revoke("concierge", "calendar.read"); code != http.StatusOK || !strings.Contains(b.body, "Removed.") {
		t.Fatalf("revoke: %d %s", code, b.body)
	}
	denied(t, decide("calendar.read"), "no live delegation")
	if d := decide("music.control"); !d.Granted {
		t.Fatalf("revoking one action took another with it: %+v", d)
	}
	own := e.srv.ownConsent(id, "concierge", time.Now())
	if len(own) != 1 || len(own[0].Permissions) != 1 || own[0].Permissions[0] != "music.control" {
		t.Fatalf("own consent after revoke: %+v", own)
	}
	if own := e.srv.ownConsent(id, "doorman", time.Now()); len(own) != 1 {
		t.Fatalf("another agent's consent was touched: %+v", own)
	}

	// What an administrator set up is shown locked and cannot be revoked here.
	if !strings.Contains(b.body, "Set up by an administrator: hallway only, until "+shortDay(until, time.Now())+". Ask them to change it.") {
		t.Fatalf("locked delegation not shown with its limits: %s", b.body)
	}
	if code := revoke("concierge", "door.unlock"); code != http.StatusForbidden {
		t.Fatalf("revoking an administrator's delegation: %d", code)
	}
	if code := revoke("concierge", "calendar.read"); code != http.StatusNotFound {
		t.Fatalf("revoking twice: %d", code)
	}
	// The last action goes, and with it the delegation.
	if code := revoke("concierge", "music.control"); code != http.StatusOK {
		t.Fatalf("revoke the last: %d", code)
	}
	if own := e.srv.ownConsent(id, "concierge", time.Now()); len(own) != 0 {
		t.Fatalf("own consent should be gone: %+v", own)
	}
}

// A newcomer saving choices finishes on the summary; someone changing them
// goes back to what they have, with what they allowed before still ticked.
func TestChoicesFlow(t *testing.T) {
	e := newEnrollEnv(t, "")
	b := e.signIn(ana)
	b.get("/enroll?step=choices")
	if !strings.Contains(b.body, "Save, allowing nothing") || !strings.Contains(b.body, `name="then" value="done"`) {
		t.Fatalf("newcomer's choices: %s", b.body)
	}
	for _, want := range []string{"Concierge may", "Doorman may", ">Likely</span>", ">Sure</span>", ">Confirmed</span>"} {
		if !strings.Contains(b.body, want) {
			t.Errorf("choices lack %q", want)
		}
	}
	form := url.Values{"csrf": {b.csrf()}, "then": {"done"}, "perm:concierge": {"calendar.read"}}
	if code := b.post("/enroll/consent", form); code != http.StatusOK || !strings.Contains(b.body, "You’re enrolled") ||
		!strings.Contains(b.body, "When it’s sure enough that it’s you, Concierge may:") {
		t.Fatalf("done: %d %s", code, b.body)
	}
	// Now returning: "/enroll" is the manage screen, and changing choices
	// starts from what is allowed.
	b.get("/enroll")
	if !strings.Contains(b.body, "Change what it may do") || strings.Contains(b.body, `aria-label="Steps"`) {
		t.Fatalf("manage: %s", b.body)
	}
	b.get("/enroll?step=choices&edit=1")
	if !strings.Contains(b.body, `value="calendar.read" checked`) || !strings.Contains(b.body, "Save 1 choice") || strings.Contains(b.body, `name="then"`) {
		t.Fatalf("returning choices: %s", b.body)
	}
}
