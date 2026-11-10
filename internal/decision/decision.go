// Package decision answers the three-party question: may this agent perform
// this action on behalf of the person it believes it is serving, right now?
package decision

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/mad-deecent/attest/internal/assurance"
	"github.com/mad-deecent/attest/internal/perception"
)

// Policy is tenant vocabulary. attest evaluates it and never interprets it.
type Policy struct {
	// Bands, weakest first. Order is what a delegation ceiling compares.
	Bands []assurance.Band `json:"bands"`
	// Permissions maps a permission to the band it requires.
	Permissions map[string]string `json:"permissions"`
	// Floor permissions are granted for any subject, including unknown.
	Floor []string `json:"floor,omitempty"`
	// Agents maps an agent to the permissions it may request ("*" for any).
	Agents map[string][]string `json:"agents"`
	// Roles and Assignments feed the built-in RBAC authorizer.
	Roles       map[string][]string `json:"roles,omitempty"`
	Assignments map[string][]string `json:"assignments,omitempty"`
}

func (p Policy) band(name string) (assurance.Band, int) {
	for i, b := range p.Bands {
		if b.Name == name {
			return b, i
		}
	}
	return assurance.Band{}, -1
}

// Validate rejects a policy that references bands or roles it does not define.
func (p Policy) Validate() error {
	for permission, name := range p.Permissions {
		if _, i := p.band(name); i < 0 {
			return fmt.Errorf("permission %q requires undefined band %q", permission, name)
		}
	}
	for id, roles := range p.Assignments {
		for _, role := range roles {
			if _, ok := p.Roles[role]; !ok {
				return fmt.Errorf("identity %q is assigned undefined role %q", id, role)
			}
		}
	}
	return nil
}

// Authorizer is the port to whatever decides what an identity may do: the
// built-in RBAC, or an external FGA engine.
type Authorizer interface {
	Check(identityID, permission, resource string) (bool, error)
	Roles(identityID string) []string
}

// RBAC is the zero-dependency Authorizer: identity -> roles -> permissions.
// Policy assignments seed it; roles can then be set at runtime, since
// identities are created at runtime.
type RBAC struct {
	mu          sync.RWMutex
	roles       map[string][]string
	assignments map[string][]string
}

func NewRBAC(p Policy) *RBAC {
	r := &RBAC{roles: p.Roles, assignments: map[string][]string{}}
	for id, roles := range p.Assignments {
		r.assignments[id] = slices.Clone(roles)
	}
	return r
}

// SetRoles replaces an identity's roles.
func (r *RBAC) SetRoles(identityID string, roles []string) error {
	for _, role := range roles {
		if _, ok := r.roles[role]; !ok {
			return fmt.Errorf("undefined role %q", role)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(roles) == 0 {
		delete(r.assignments, identityID)
	} else {
		r.assignments[identityID] = slices.Clone(roles)
	}
	return nil
}

func (r *RBAC) Check(identityID, permission, _ string) (bool, error) {
	for _, role := range r.Roles(identityID) {
		if slices.Contains(r.roles[role], permission) {
			return true, nil
		}
	}
	return false, nil
}

func (r *RBAC) Roles(identityID string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.assignments[identityID])
}

// Delegation is the standing consent that lets an agent act for an identity.
type Delegation struct {
	ID          string   `json:"id"`
	IdentityID  string   `json:"identityId"`
	Agent       string   `json:"agent"`
	Permissions []string `json:"permissions"`
	// Ceiling is the strongest band this delegation can satisfy. Empty means
	// no ceiling.
	Ceiling string `json:"ceiling,omitempty"`
	// Zones restricts where the delegation applies. Empty means anywhere.
	Zones     []string   `json:"zones,omitempty"`
	GrantedBy string     `json:"grantedBy"`
	GrantedAt time.Time  `json:"grantedAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

type Delegations struct {
	mu   sync.RWMutex
	next int
	all  map[string]Delegation
}

func NewDelegations() *Delegations { return &Delegations{all: map[string]Delegation{}} }

func (s *Delegations) Add(d Delegation, now time.Time) (Delegation, error) {
	if d.IdentityID == "" || d.Agent == "" || len(d.Permissions) == 0 || d.GrantedBy == "" {
		return Delegation{}, errors.New("identityId, agent, permissions and grantedBy are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	d.ID = "del_" + strconv.Itoa(s.next)
	d.GrantedAt = now
	d.RevokedAt = nil
	s.all[d.ID] = d
	return d, nil
}

func (s *Delegations) Revoke(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.all[id]
	if !ok {
		return false
	}
	d.RevokedAt = &now
	s.all[id] = d
	return true
}

func (s *Delegations) RevokeIdentity(identityID string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, d := range s.all {
		if d.IdentityID == identityID && d.RevokedAt == nil {
			d.RevokedAt = &now
			s.all[id] = d
		}
	}
}

func (s *Delegations) List() []Delegation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Delegation, 0, len(s.all))
	for _, d := range s.all {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Delegation) int { return a.GrantedAt.Compare(b.GrantedAt) })
	return out
}

// live returns the delegations covering this request, ignoring ceilings.
func (s *Delegations) live(identityID, agent, permission, zone string, now time.Time) []Delegation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Delegation
	for _, d := range s.all {
		if d.IdentityID != identityID || d.Agent != agent || d.RevokedAt != nil {
			continue
		}
		if d.ExpiresAt != nil && !d.ExpiresAt.After(now) {
			continue
		}
		if !slices.Contains(d.Permissions, permission) && !slices.Contains(d.Permissions, "*") {
			continue
		}
		if len(d.Zones) > 0 && !slices.Contains(d.Zones, zone) {
			continue
		}
		out = append(out, d)
	}
	return out
}

type Request struct {
	// Agent comes from the caller's credential, never from the request body.
	Agent      string `json:"-"`
	Subject    string `json:"subject"`
	Permission string `json:"permission"`
	Resource   string `json:"resource,omitempty"`
}

type Decision struct {
	Granted    bool    `json:"granted"`
	Agent      string  `json:"agent"`
	Permission string  `json:"permission"`
	Resource   string  `json:"resource,omitempty"`
	IdentityID *string `json:"identityId"`
	Band       string  `json:"band,omitempty"`
	// Reasons is one line per predicate checked.
	Reasons []string `json:"reasons"`
	// ValidUntil is the earliest instant a satisfied predicate lapses. A
	// caller that caches a grant must not hold it past this.
	ValidUntil *time.Time `json:"validUntil,omitempty"`
}

type Evaluator struct {
	Policy      Policy
	Fusion      *perception.Fusion
	Authz       Authorizer
	Delegations *Delegations
	Bands       assurance.Policy
}

func (e *Evaluator) Decide(req Request, now time.Time) Decision {
	d := Decision{Agent: req.Agent, Permission: req.Permission, Resource: req.Resource}
	deny := func(format string, args ...any) Decision {
		d.Reasons = append(d.Reasons, fmt.Sprintf(format, args...))
		return d
	}

	allowed := e.Policy.Agents[req.Agent]
	if !slices.Contains(allowed, req.Permission) && !slices.Contains(allowed, "*") {
		return deny("agent may not request this permission")
	}
	if slices.Contains(e.Policy.Floor, req.Permission) {
		d.Granted = true
		d.Reasons = append(d.Reasons, "floor permission: granted for any subject")
		return d
	}
	bandName, ok := e.Policy.Permissions[req.Permission]
	if !ok {
		return deny("no policy for permission")
	}
	d.Band = bandName
	band, rank := e.Policy.band(bandName)
	if rank < 0 {
		return deny("undefined band %s", bandName)
	}

	belief := e.Fusion.Current(req.Subject, now)
	if belief == nil {
		return deny("subject is unknown: no live evidence")
	}
	if belief.Leading == nil {
		return deny("subject is unknown: belief is %s", belief.State)
	}
	identityID := *belief.Leading
	d.IdentityID = &identityID

	delegations := e.Delegations.live(identityID, req.Agent, req.Permission, belief.Subject.Zone, now)
	if len(delegations) == 0 {
		return deny("no live delegation from identity to agent")
	}
	// Among delegations whose ceiling admits the band, the longest-lived one
	// sets the horizon. A delegation with no expiry leaves it open.
	var horizon *time.Time
	covered, open := false, false
	for _, del := range delegations {
		if del.Ceiling != "" {
			if _, ceiling := e.Policy.band(del.Ceiling); ceiling < rank {
				continue
			}
		}
		covered = true
		switch {
		case del.ExpiresAt == nil:
			open = true
		case horizon == nil || del.ExpiresAt.After(*horizon):
			horizon = del.ExpiresAt
		}
	}
	if open {
		horizon = nil
	}
	if !covered {
		return deny("delegation ceiling is below band %s", bandName)
	}
	d.Reasons = append(d.Reasons, "delegation: live")

	permitted, err := e.Authz.Check(identityID, req.Permission, req.Resource)
	if err != nil {
		return deny("authorization engine unavailable")
	}
	if !permitted {
		return deny("identity does not hold the permission")
	}
	d.Reasons = append(d.Reasons, "authorization: identity holds the permission")

	res := e.Bands.Holds(band, *belief, e.presence(belief, now), now)
	d.Reasons = append(d.Reasons, res.Reasons...)
	if !res.OK {
		return d
	}
	d.Granted = true
	until := res.ValidUntil
	if horizon != nil && (until.IsZero() || horizon.Before(until)) {
		until = *horizon
	}
	if !until.IsZero() {
		d.ValidUntil = &until
	}
	return d
}

// presence reports whether a role is held by some other resolved subject in
// the same zone.
func (e *Evaluator) presence(subject *perception.Belief, now time.Time) func(string) bool {
	return func(role string) bool {
		for _, other := range e.Fusion.Beliefs(now) {
			if other.Leading == nil || *other.Leading == *subject.Leading {
				continue
			}
			if other.Subject.Zone != subject.Subject.Zone {
				continue
			}
			if slices.Contains(e.Authz.Roles(*other.Leading), role) {
				return true
			}
		}
		return false
	}
}
