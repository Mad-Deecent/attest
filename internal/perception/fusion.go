// Package perception turns calibrated evidence into a Belief: the posterior
// over who a perceived subject is. It is pure in-memory state with no I/O.
//
// Modalities report log-likelihood ratios, not confidences. For a candidate
// identity, LLR = ln( P(observation | subject is this identity) /
// P(observation | subject is someone else) ). Positive supports the identity,
// negative counts against it, zero says nothing. LLRs from independent
// modalities add, which is what lets a voice mismatch lower a face match.
//
// The posterior is taken over every enrolled identity plus "not enrolled":
//
//	P(i | evidence) ∝ prior(i) · exp(Σ_modality LLR_i)
//	P(not enrolled | evidence) ∝ PriorUnknown
//
// Enrolled identities share (1 - PriorUnknown) uniformly.
package perception

import (
	"errors"
	"math"
	"sort"
	"sync"
	"time"
)

// SubjectRef names a perception lifetime (one track, one utterance), not a
// person. Zone is the context co-presence predicates consult.
type SubjectRef struct {
	Key  string `json:"key"`
	Zone string `json:"zone,omitempty"`
}

type Candidate struct {
	IdentityID string  `json:"identityId"`
	LLR        float64 `json:"llr"`
}

// Evidence is one modality's report about one subject. No candidates means
// the producer looked and matched nobody; it still supersedes earlier evidence
// in the same slot.
type Evidence struct {
	Subject    SubjectRef  `json:"subject"`
	Modality   string      `json:"modality"`
	Source     string      `json:"source"`
	ObservedAt time.Time   `json:"observedAt"`
	ExpiresAt  time.Time   `json:"expiresAt"`
	Candidates []Candidate `json:"candidates"`
	// Calibration is the version of the producer's score-to-LLR mapping.
	// Evidence without one cannot support anybody.
	Calibration string `json:"calibration"`
}

// Association asserts that two subjects are the same perceived someone.
// Evidence never combines across subjects without one.
type Association struct {
	A          string    `json:"a"`
	B          string    `json:"b"`
	Confidence float64   `json:"confidence"`
	ObservedAt time.Time `json:"observedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type State string

const (
	Resolved   State = "resolved"
	Unknown    State = "unknown"
	Ambiguous  State = "ambiguous"
	Conflicted State = "conflicted"
)

type BeliefCandidate struct {
	IdentityID  string   `json:"identityId"`
	Probability float64  `json:"probability"`
	Modalities  []string `json:"modalities"`
}

// Belief is the current posterior about a subject. Leading is nil unless the
// state is resolved; Confidence is then the leader's posterior probability.
type Belief struct {
	Subject    SubjectRef        `json:"subject"`
	State      State             `json:"state"`
	Candidates []BeliefCandidate `json:"candidates"`
	Leading    *string           `json:"leading"`
	Confidence float64           `json:"confidence"`
	// Unknown is the posterior mass on "nobody we have evidence for".
	Unknown float64 `json:"unknown"`
	// Modalities are the independent modality groups supporting the leader.
	Modalities []string `json:"modalities"`
	// FreshestAt is the newest evidence supporting the leader; ExpiresAt is
	// when the first piece of that support lapses.
	FreshestAt time.Time `json:"freshestAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// Modality tunes how one modality participates in fusion.
type Modality struct {
	// MaxLLR bounds how far this modality alone can move the posterior.
	MaxLLR float64 `json:"maxLLR,omitempty"`
	// Group names what this modality is independent of. Modalities sharing a
	// group (say, face and appearance from the same camera) count once toward
	// a band's minModalities.
	Group string `json:"group,omitempty"`
}

type Config struct {
	// PriorUnknown is the prior probability that a subject is not enrolled.
	PriorUnknown float64 `json:"priorUnknown"`
	// AmbiguityMargin is how far the leader must clear the runner-up.
	AmbiguityMargin float64 `json:"ambiguityMargin"`
	// MinAssociation is the confidence an Association needs to join subjects.
	MinAssociation float64 `json:"minAssociation"`
	// MaxLLR is the per-modality bound when a modality does not set its own.
	MaxLLR     float64             `json:"maxLLR"`
	Modalities map[string]Modality `json:"modalities,omitempty"`
}

func DefaultConfig() Config {
	return Config{PriorUnknown: 0.5, AmbiguityMargin: 0.2, MinAssociation: 0.9, MaxLLR: 10}
}

func (c Config) maxLLR(modality string) float64 {
	if m := c.Modalities[modality]; m.MaxLLR > 0 {
		return m.MaxLLR
	}
	return c.MaxLLR
}

func (c Config) group(modality string) string {
	if m := c.Modalities[modality]; m.Group != "" {
		return m.Group
	}
	return modality
}

// Population is what fusion needs to know about the identity registry.
type Population interface {
	Has(id string) bool
	Count() int
}

type slot struct{ subject, modality, source string }

type Fusion struct {
	mu      sync.Mutex
	cfg     Config
	pop     Population
	records map[slot]Evidence
	assocs  map[[2]string]Association
	zones   map[string]string
}

func NewFusion(cfg Config, pop Population) *Fusion {
	return &Fusion{
		cfg:     cfg,
		pop:     pop,
		records: map[slot]Evidence{},
		assocs:  map[[2]string]Association{},
		zones:   map[string]string{},
	}
}

// Observe records evidence, superseding whatever the same source last said
// about the same subject in the same modality.
func (f *Fusion) Observe(e Evidence) error {
	if e.Subject.Key == "" || e.Modality == "" || e.Source == "" {
		return errors.New("subject.key, modality and source are required")
	}
	if !e.ExpiresAt.After(e.ObservedAt) {
		return errors.New("expiresAt must be after observedAt")
	}
	limit := f.cfg.maxLLR(e.Modality)
	kept := make([]Candidate, 0, len(e.Candidates))
	for _, c := range e.Candidates {
		if math.IsNaN(c.LLR) || math.IsInf(c.LLR, 0) {
			return errors.New("candidate llr must be finite")
		}
		// Uncalibrated scores are not evidence, and an unregistered label is
		// never an identity.
		if e.Calibration == "" || !f.pop.Has(c.IdentityID) {
			continue
		}
		c.LLR = math.Max(-limit, math.Min(limit, c.LLR))
		kept = append(kept, c)
	}
	e.Candidates = kept

	f.mu.Lock()
	defer f.mu.Unlock()
	s := slot{e.Subject.Key, e.Modality, e.Source}
	if prev, ok := f.records[s]; ok && prev.ObservedAt.After(e.ObservedAt) {
		return nil
	}
	f.records[s] = e
	if e.Subject.Zone != "" {
		f.zones[e.Subject.Key] = e.Subject.Zone
	}
	return nil
}

func (f *Fusion) Associate(a Association) error {
	if a.A == "" || a.B == "" || a.A == a.B {
		return errors.New("association needs two distinct subjects")
	}
	if !(a.Confidence >= 0 && a.Confidence <= 1) {
		return errors.New("association confidence must be within 0..1")
	}
	if !a.ExpiresAt.After(a.ObservedAt) {
		return errors.New("expiresAt must be after observedAt")
	}
	pair := [2]string{a.A, a.B}
	if pair[0] > pair[1] {
		pair[0], pair[1] = pair[1], pair[0]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if prev, ok := f.assocs[pair]; ok && prev.ObservedAt.After(a.ObservedAt) {
		return nil
	}
	f.assocs[pair] = a
	return nil
}

// RetireSubject drops everything known about a subject, so a reused key
// inherits nothing.
func (f *Fusion) RetireSubject(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s := range f.records {
		if s.subject == key {
			delete(f.records, s)
		}
	}
	for pair := range f.assocs {
		if pair[0] == key || pair[1] == key {
			delete(f.assocs, pair)
		}
	}
	delete(f.zones, key)
}

// RetireSource drops all evidence from one producer.
func (f *Fusion) RetireSource(source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s := range f.records {
		if s.source == source {
			delete(f.records, s)
		}
	}
}

// RetireIdentity removes a deleted identity from all live evidence.
func (f *Fusion) RetireIdentity(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s, r := range f.records {
		kept := r.Candidates[:0:0]
		for _, c := range r.Candidates {
			if c.IdentityID != id {
				kept = append(kept, c)
			}
		}
		r.Candidates = kept
		f.records[s] = r
	}
}

// Current returns the Belief for a subject, or nil when nothing live is known
// about it.
func (f *Fusion) Current(key string, now time.Time) *Belief {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prune(now)
	return f.derive(key, f.members(key, now), now)
}

// Beliefs returns one Belief per group of associated subjects.
func (f *Fusion) Beliefs(now time.Time) []Belief {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prune(now)
	seen := map[string]bool{}
	var out []Belief
	for s := range f.records {
		members := f.members(s.subject, now)
		if seen[members[0]] {
			continue
		}
		seen[members[0]] = true
		if b := f.derive(members[0], members, now); b != nil {
			out = append(out, *b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject.Key < out[j].Subject.Key })
	return out
}

func (f *Fusion) prune(now time.Time) {
	for s, r := range f.records {
		if !r.ExpiresAt.After(now) {
			delete(f.records, s)
		}
	}
	for pair, a := range f.assocs {
		if !a.ExpiresAt.After(now) {
			delete(f.assocs, pair)
		}
	}
}

// members returns the sorted set of subjects joined to key by live
// associations, key included.
func (f *Fusion) members(key string, now time.Time) []string {
	seen := map[string]bool{key: true}
	queue := []string{key}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		for pair, a := range f.assocs {
			if a.Confidence < f.cfg.MinAssociation {
				continue
			}
			var other string
			switch k {
			case pair[0]:
				other = pair[1]
			case pair[1]:
				other = pair[0]
			default:
				continue
			}
			if !seen[other] {
				seen[other] = true
				queue = append(queue, other)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type contribution struct {
	llr     float64
	at, exp time.Time
}

func (f *Fusion) derive(key string, members []string, now time.Time) *Belief {
	member := map[string]bool{}
	zone := f.zones[key]
	for _, m := range members {
		member[m] = true
		if zone == "" {
			zone = f.zones[m]
		}
	}

	// identity -> modality -> the strongest source's contribution. One
	// modality counts once however many sensors report it.
	per := map[string]map[string]contribution{}
	producerLeaders := map[string]bool{}
	live := false
	var newest time.Time
	for s, r := range f.records {
		if !member[s.subject] {
			continue
		}
		live = true
		if r.ObservedAt.After(newest) {
			newest = r.ObservedAt
		}
		best, bestLLR := "", 0.0
		for _, c := range r.Candidates {
			if per[c.IdentityID] == nil {
				per[c.IdentityID] = map[string]contribution{}
			}
			if prev, ok := per[c.IdentityID][r.Modality]; !ok || c.LLR > prev.llr {
				per[c.IdentityID][r.Modality] = contribution{c.LLR, r.ObservedAt, r.ExpiresAt}
			}
			if c.LLR > bestLLR {
				best, bestLLR = c.IdentityID, c.LLR
			}
		}
		if best != "" {
			producerLeaders[best] = true
		}
	}
	if !live {
		return nil
	}

	belief := &Belief{
		Subject:    SubjectRef{Key: key, Zone: zone},
		State:      Unknown,
		Candidates: []BeliefCandidate{},
		Unknown:    1,
		Modalities: []string{},
		FreshestAt: newest,
	}
	if len(per) == 0 {
		return belief
	}

	enrolled := max(f.pop.Count(), len(per))
	prior := (1 - f.cfg.PriorUnknown) / float64(enrolled)
	// Mass that stays off every candidate: not enrolled, plus enrolled
	// identities no evidence mentions.
	rest := f.cfg.PriorUnknown + float64(enrolled-len(per))*prior
	total := rest
	weights := map[string]float64{}
	for id, modalities := range per {
		sum := 0.0
		for _, c := range modalities {
			sum += c.llr
		}
		weights[id] = prior * math.Exp(sum)
		total += weights[id]
	}
	for id, w := range weights {
		groups := map[string]bool{}
		for modality, c := range per[id] {
			if c.llr > 0 {
				groups[f.cfg.group(modality)] = true
			}
		}
		belief.Candidates = append(belief.Candidates, BeliefCandidate{
			IdentityID: id, Probability: w / total, Modalities: sortedKeys(groups),
		})
	}
	sort.Slice(belief.Candidates, func(i, j int) bool {
		a, b := belief.Candidates[i], belief.Candidates[j]
		if a.Probability != b.Probability {
			return a.Probability > b.Probability
		}
		return a.IdentityID < b.IdentityID
	})
	belief.Unknown = rest / total

	leader := belief.Candidates[0]
	contested := len(belief.Candidates) > 1 &&
		leader.Probability-belief.Candidates[1].Probability < f.cfg.AmbiguityMargin
	switch {
	case contested && len(producerLeaders) > 1:
		belief.State = Conflicted
	case contested:
		belief.State = Ambiguous
	case leader.Probability > belief.Unknown:
		belief.State = Resolved
	}
	if belief.State != Resolved {
		return belief
	}

	// Freshness belongs to the evidence that supports the leader, so a fresh
	// mismatch cannot renew an old match.
	var freshest, expires time.Time
	for _, c := range per[leader.IdentityID] {
		if c.llr <= 0 {
			continue
		}
		if c.at.After(freshest) {
			freshest = c.at
		}
		if expires.IsZero() || c.exp.Before(expires) {
			expires = c.exp
		}
	}
	id := leader.IdentityID
	belief.Leading = &id
	belief.Confidence = leader.Probability
	belief.Modalities = leader.Modalities
	belief.FreshestAt = freshest
	belief.ExpiresAt = expires
	return belief
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
