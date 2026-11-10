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
	// Settings is absent in files written before settings moved here.
	Settings *Settings `json:"settings,omitempty"`
}

// checkDataDir fails fast when the data file's directory is missing or not
// writable, rather than at the first mutation after people have used it.
func checkDataDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("data directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("data directory %s is not a directory", dir)
	}
	// Permission bits do not tell the whole story (ACLs, read-only mounts):
	// try what save does.
	probe, err := os.CreateTemp(dir, ".attest-probe-*")
	if err != nil {
		return fmt.Errorf("data directory %s is not writable: %w", dir, err)
	}
	probe.Close()
	return os.Remove(probe.Name())
}

// readSnapshot reads the data file. A missing file is a fresh start (nil); an
// unreadable one is an error, never silently empty.
func readSnapshot(path string) (*snapshot, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("parse data %s: %w", path, err)
	}
	return &snap, nil
}

func (s *Server) restore(snap snapshot) {
	s.registry.Restore(snap.Identities)
	s.rbac.Restore(snap.Assignments)
	s.delegations.Restore(snap.Delegations, snap.NextDelegation)
}

// save writes the current state.
func (s *Server) save() error {
	// Serialized so an older snapshot can never land after a newer one, and
	// so it cannot interleave with a settings change (see apply).
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	return s.writeLocked(s.cur().settings, s.rbac.Assignments())
}

// writeLocked writes state with the given settings and role assignments
// atomically: a temp file in
// the same directory, synced, then renamed over the old one. Callers hold
// saveMu.
func (s *Server) writeLocked(settings Settings, assignments map[string][]string) error {
	if s.data == "" {
		return nil
	}
	delegations, next := s.delegations.Snapshot()
	raw, err := json.MarshalIndent(snapshot{
		Identities:     s.registry.List(),
		Assignments:    assignments,
		Delegations:    delegations,
		NextDelegation: next,
		Settings:       &settings,
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
