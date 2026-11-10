// Package identity is the stable core: who a person is, never how they are
// recognised. It holds no biometric material and depends on nothing.
package identity

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Identity is a person attest may perceive. ID is the only durable join;
// DisplayName is presentation.
type Identity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	// Provider links the identity to an account at an identity provider.
	Provider *Provider `json:"provider,omitempty"`
	// VoiceEnrolledAt is when a voice producer last enrolled this identity.
	// The recording itself never reaches attest.
	VoiceEnrolledAt *time.Time `json:"voiceEnrolledAt,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

// Provider names an account at an identity provider. It is a link, never a key.
type Provider struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// NewID returns a random (version 4) UUID.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Registry is the in-memory identity store.
type Registry struct {
	mu  sync.RWMutex
	ids map[string]Identity
}

func NewRegistry() *Registry { return &Registry{ids: map[string]Identity{}} }

// Put creates or renames an identity.
func (r *Registry) Put(id, displayName string, now time.Time) (Identity, error) {
	if id == "" {
		return Identity{}, errors.New("identity id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ident, ok := r.ids[id]
	if !ok {
		ident = Identity{ID: id, CreatedAt: now}
	}
	ident.DisplayName = displayName
	ident.UpdatedAt = now
	r.ids[id] = ident
	return ident, nil
}

// Link returns the identity linked to a provider account, creating one with a
// fresh id on first sight. The display name follows the provider.
func (r *Registry) Link(p Provider, displayName string, now time.Time) (Identity, error) {
	if p.Issuer == "" || p.Subject == "" {
		return Identity{}, errors.New("provider issuer and subject are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Linear: a site enrols people, not a population.
	for id, ident := range r.ids {
		if ident.Provider != nil && *ident.Provider == p {
			ident.DisplayName = displayName
			ident.UpdatedAt = now
			r.ids[id] = ident
			return ident, nil
		}
	}
	ident := Identity{ID: NewID(), DisplayName: displayName, Provider: &p, CreatedAt: now, UpdatedAt: now}
	r.ids[ident.ID] = ident
	return ident, nil
}

// MarkVoiceEnrolled records that a voice producer enrolled the identity.
func (r *Registry) MarkVoiceEnrolled(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ident, ok := r.ids[id]
	if !ok {
		return false
	}
	ident.VoiceEnrolledAt = &now
	ident.UpdatedAt = now
	r.ids[id] = ident
	return true
}

func (r *Registry) Get(id string) (Identity, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ident, ok := r.ids[id]
	return ident, ok
}

// Restore replaces the registry's contents, e.g. from a saved snapshot.
func (r *Registry) Restore(ids []Identity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = make(map[string]Identity, len(ids))
	for _, ident := range ids {
		r.ids[ident.ID] = ident
	}
}

func (r *Registry) Delete(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.ids[id]
	delete(r.ids, id)
	return ok
}

func (r *Registry) Has(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.ids[id]
	return ok
}

func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.ids)
}

func (r *Registry) List() []Identity {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Identity, 0, len(r.ids))
	for _, ident := range r.ids {
		out = append(out, ident)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
