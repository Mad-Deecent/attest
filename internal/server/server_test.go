package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/perception"
)

type harness struct {
	t   *testing.T
	h   http.Handler
	now time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, "")
}

// newHarnessWith keeps state in data; empty keeps it in memory.
func newHarnessWith(t *testing.T, data string) *harness {
	t.Helper()
	hn := &harness{t: t, now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	cfg := Config{
		Data: data,
		Tokens: map[string]Principal{
			"admin": {Kind: KindAdmin, ID: "admin"},
			"face":  {Kind: KindProducer, ID: "cam"},
			"voice": {Kind: KindProducer, ID: "mic"},
			"agent": {Kind: KindAgent, ID: "concierge"},
			"other": {Kind: KindAgent, ID: "stranger-bot"},
		},
		Fusion: perception.DefaultConfig(),
		Policy: decision.Policy{
			Permissions: map[string]string{"music.control": "presumed", "door.unlock": "corroborated"},
			Floor:       []string{"emergency.call"},
			Agents:      map[string][]string{"concierge": {"*"}},
			Roles: map[string][]string{
				"resident": {"music.control", "door.unlock"},
				"guest":    {"music.control"},
			},
		},
	}
	srv, err := New(cfg, func() time.Time { return hn.now })
	if err != nil {
		t.Fatal(err)
	}
	hn.h = srv.Handler()
	for _, id := range []string{"ana", "ben", "cy", "dee"} {
		hn.call("admin", "PUT", "/v1/identities/"+id, map[string]string{"displayName": id}, http.StatusOK, nil)
	}
	hn.call("admin", "PUT", "/v1/identities/ana/roles", map[string][]string{"roles": {"resident"}}, http.StatusOK, nil)
	hn.call("admin", "PUT", "/v1/identities/ben/roles", map[string][]string{"roles": {"guest"}}, http.StatusOK, nil)
	return hn
}

func (hn *harness) call(token, method, path string, body any, want int, into any) {
	hn.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			hn.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	hn.h.ServeHTTP(rec, req)
	if rec.Code != want {
		hn.t.Fatalf("%s %s: status %d, want %d: %s", method, path, rec.Code, want, rec.Body)
	}
	if into != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			hn.t.Fatal(err)
		}
	}
}

func (hn *harness) see(token, subject, modality, identity string, llr float64) {
	hn.t.Helper()
	hn.call(token, "POST", "/v1/evidence", perception.Evidence{
		Subject:     perception.SubjectRef{Key: subject, Zone: "lobby"},
		Modality:    modality,
		ObservedAt:  hn.now,
		ExpiresAt:   hn.now.Add(10 * time.Minute),
		Candidates:  []perception.Candidate{{IdentityID: identity, LLR: llr}},
		Calibration: "v1",
	}, http.StatusAccepted, nil)
}

func (hn *harness) delegate(d decision.Delegation) decision.Delegation {
	hn.t.Helper()
	d.GrantedBy = "self"
	var out decision.Delegation
	hn.call("admin", "POST", "/v1/delegations", d, http.StatusCreated, &out)
	return out
}

func (hn *harness) decide(subject, permission string) decision.Decision {
	hn.t.Helper()
	var d decision.Decision
	hn.call("agent", "POST", "/v1/decisions",
		decision.Request{Subject: subject, Permission: permission}, http.StatusOK, &d)
	return d
}

func denied(t *testing.T, d decision.Decision, reason string) {
	t.Helper()
	if d.Granted || !slices.ContainsFunc(d.Reasons, func(r string) bool { return strings.Contains(r, reason) }) {
		t.Fatalf("want denial mentioning %q, got %+v", reason, d)
	}
}

func TestDecisionLifecycle(t *testing.T) {
	hn := newHarness(t)

	denied(t, hn.decide("track-1", "music.control"), "no live evidence")
	if d := hn.decide("track-1", "emergency.call"); !d.Granted {
		t.Fatalf("floor permission must hold for an unknown subject: %+v", d)
	}

	hn.see("face", "track-1", "face", "ana", 6)
	denied(t, hn.decide("track-1", "music.control"), "no live delegation")

	hn.delegate(decision.Delegation{IdentityID: "ana", Agent: "concierge", Permissions: []string{"*"}})
	d := hn.decide("track-1", "music.control")
	if !d.Granted || d.IdentityID == nil || *d.IdentityID != "ana" {
		t.Fatalf("want grant for ana, got %+v", d)
	}
	// presumed allows 15 minutes; the evidence itself lapses after 10.
	if want := hn.now.Add(10 * time.Minute); d.ValidUntil == nil || !d.ValidUntil.Equal(want) {
		t.Fatalf("validUntil = %v, want %v", d.ValidUntil, want)
	}

	// One modality cannot clear a band that needs two.
	denied(t, hn.decide("track-1", "door.unlock"), "modalities 1 < min 2")

	hn.see("voice", "utt-1", "voice", "ana", 5)
	denied(t, hn.decide("track-1", "door.unlock"), "modalities 1 < min 2")
	hn.call("face", "POST", "/v1/associations", perception.Association{
		A: "track-1", B: "utt-1", Confidence: 0.97, ObservedAt: hn.now, ExpiresAt: hn.now.Add(time.Minute),
	}, http.StatusAccepted, nil)
	d = hn.decide("track-1", "door.unlock")
	if !d.Granted {
		t.Fatalf("want corroborated grant, got %+v", d)
	}
	if want := hn.now.Add(2 * time.Minute); !d.ValidUntil.Equal(want) {
		t.Fatalf("validUntil = %v, want %v", d.ValidUntil, want)
	}

	// Freshness is the session: with no new evidence the grant lapses.
	hn.now = hn.now.Add(3 * time.Minute)
	denied(t, hn.decide("track-1", "door.unlock"), "age 3m0s > max 2m0s")
}

func TestDecisionWithoutASubject(t *testing.T) {
	hn := newHarness(t)
	hn.see("face", "track-1", "face", "ana", 8)
	hn.delegate(decision.Delegation{IdentityID: "ana", Agent: "concierge", Permissions: []string{"*"}})

	for _, body := range []any{
		map[string]string{"permission": "music.control"},
		map[string]string{"subject": "", "permission": "music.control"},
	} {
		var d decision.Decision
		hn.call("agent", "POST", "/v1/decisions", body, http.StatusOK, &d)
		denied(t, d, "no subject: only floor permissions apply")
		if d.IdentityID != nil {
			t.Fatalf("a request without a subject names nobody: %+v", d)
		}
	}
	var d decision.Decision
	hn.call("agent", "POST", "/v1/decisions", map[string]string{"permission": "emergency.call"}, http.StatusOK, &d)
	if !d.Granted {
		t.Fatalf("floor permission must hold without a subject: %+v", d)
	}
	hn.call("agent", "POST", "/v1/decisions", map[string]string{"subject": "track-1"}, http.StatusBadRequest, nil)
}

func TestDelegationLimits(t *testing.T) {
	hn := newHarness(t)
	hn.see("face", "t-ana", "face", "ana", 8)
	hn.see("voice", "t-ana", "voice", "ana", 8)
	hn.see("face", "t-ben", "face", "ben", 8)

	capped := hn.delegate(decision.Delegation{
		IdentityID: "ana", Agent: "concierge", Permissions: []string{"*"}, Ceiling: "confident",
	})
	denied(t, hn.decide("t-ana", "door.unlock"), "delegation ceiling")
	if d := hn.decide("t-ana", "music.control"); !d.Granted {
		t.Fatalf("ceiling must not block a weaker band: %+v", d)
	}

	hn.call("admin", "DELETE", "/v1/delegations/"+capped.ID, nil, http.StatusNoContent, nil)
	denied(t, hn.decide("t-ana", "music.control"), "no live delegation")

	// A delegation does not widen what the identity itself may do.
	hn.delegate(decision.Delegation{IdentityID: "ben", Agent: "concierge", Permissions: []string{"*"}})
	denied(t, hn.decide("t-ben", "door.unlock"), "does not hold the permission")

	expiry := hn.now.Add(30 * time.Second)
	hn.delegate(decision.Delegation{
		IdentityID: "ana", Agent: "concierge", Permissions: []string{"door.unlock"}, ExpiresAt: &expiry,
	})
	if d := hn.decide("t-ana", "door.unlock"); !d.Granted || !d.ValidUntil.Equal(expiry) {
		t.Fatalf("validUntil must not outlive the delegation: %+v", d)
	}
}

func TestCredentialKindsAreDisjoint(t *testing.T) {
	hn := newHarness(t)
	req := decision.Request{Subject: "s", Permission: "music.control"}
	hn.call("face", "POST", "/v1/decisions", req, http.StatusForbidden, nil)
	hn.call("agent", "POST", "/v1/evidence", perception.Evidence{}, http.StatusForbidden, nil)
	hn.call("nope", "POST", "/v1/decisions", req, http.StatusUnauthorized, nil)

	var d decision.Decision
	hn.call("other", "POST", "/v1/decisions", req, http.StatusOK, &d)
	denied(t, d, "agent may not request")
}

func TestDeletingAnIdentityRevokesEverything(t *testing.T) {
	hn := newHarness(t)
	hn.see("face", "s", "face", "ana", 8)
	hn.delegate(decision.Delegation{IdentityID: "ana", Agent: "concierge", Permissions: []string{"*"}})
	if d := hn.decide("s", "music.control"); !d.Granted {
		t.Fatalf("precondition: %+v", d)
	}

	hn.call("admin", "DELETE", "/v1/identities/ana", nil, http.StatusNoContent, nil)
	denied(t, hn.decide("s", "music.control"), "subject is unknown")
}
