// Package identity is the stable core: who a person is, never how they are
// recognised. It holds no biometric material and depends on nothing.
package identity

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Identity is a person attest may perceive. ID is the only durable join;
// DisplayName is presentation.
type Identity struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"displayName"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
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
