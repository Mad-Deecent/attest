package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/identity"
)

// snapshot is everything attest must remember across a restart. Evidence and
// beliefs are deliberately absent: they are live state and lapse on their own.
type snapshot struct {
	Identities     []identity.Identity   `json:"identities"`
	Assignments    map[string][]string   `json:"assignments"`
	Delegations    []decision.Delegation `json:"delegations"`
	NextDelegation int                   `json:"nextDelegation"`
}

// load restores state from the data file. A missing file is a fresh start; an
// unreadable one is an error, never silently empty.
func (s *Server) load() error {
	if s.data == "" {
		return nil
	}
	raw, err := os.ReadFile(s.data)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read data: %w", err)
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("parse data %s: %w", s.data, err)
	}
	s.registry.Restore(snap.Identities)
	s.rbac.Restore(snap.Assignments)
	s.delegations.Restore(snap.Delegations, snap.NextDelegation)
	return nil
}

// save writes the current state atomically: a temp file in the same
// directory, synced, then renamed over the old one.
func (s *Server) save() error {
	if s.data == "" {
		return nil
	}
	// Serialised so an older snapshot can never land after a newer one.
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	delegations, next := s.delegations.Snapshot()
	raw, err := json.MarshalIndent(snapshot{
		Identities:     s.registry.List(),
		Assignments:    s.rbac.Assignments(),
		Delegations:    delegations,
		NextDelegation: next,
	}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.data)
	tmp, err := os.CreateTemp(dir, ".attest-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.data); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// commit saves after a mutation. A change that was not durably recorded fails
// the request.
func (s *Server) commit(w http.ResponseWriter) bool {
	if err := s.save(); err != nil {
		log.Printf("save state: %v", err)
		fail(w, http.StatusInternalServerError, "could not save state")
		return false
	}
	return true
}
