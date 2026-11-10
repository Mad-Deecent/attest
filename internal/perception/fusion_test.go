package perception

import (
	"testing"
	"time"
)

type population map[string]bool

func (p population) Has(id string) bool { return p[id] }
func (p population) Count() int         { return len(p) }

var (
	t0       = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	enrolled = population{"ana": true, "ben": true, "cy": true, "dee": true}
)

func evidence(subject, modality, source string, at time.Time, candidates ...Candidate) Evidence {
	return Evidence{
		Subject:     SubjectRef{Key: subject, Zone: "lobby"},
		Modality:    modality,
		Source:      source,
		ObservedAt:  at,
		ExpiresAt:   at.Add(time.Minute),
		Candidates:  candidates,
		Calibration: "v1",
	}
}

func observe(t *testing.T, f *Fusion, e Evidence) {
	t.Helper()
	if err := f.Observe(e); err != nil {
		t.Fatal(err)
	}
}

func associate(t *testing.T, f *Fusion, a, b string, confidence float64) {
	t.Helper()
	err := f.Associate(Association{A: a, B: b, Confidence: confidence, ObservedAt: t0, ExpiresAt: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
}

func leading(b *Belief) string {
	if b == nil || b.Leading == nil {
		return ""
	}
	return *b.Leading
}

func TestSingleModalityResolves(t *testing.T) {
	f := NewFusion(DefaultConfig(), enrolled)
	observe(t, f, evidence("track-1", "face", "cam", t0, Candidate{"ana", 6}))

	b := f.Current("track-1", t0)
	if leading(b) != "ana" || b.State != Resolved {
		t.Fatalf("want ana resolved, got %+v", b)
	}
	// prior 0.125 · e^6 against 0.875 of remaining mass.
	if b.Confidence < 0.98 || b.Confidence > 0.99 {
		t.Fatalf("confidence = %v", b.Confidence)
	}
	if len(b.Modalities) != 1 {
		t.Fatalf("modalities = %v", b.Modalities)
	}
}

func TestNoEvidenceIsNil(t *testing.T) {
	f := NewFusion(DefaultConfig(), enrolled)
	if b := f.Current("nobody", t0); b != nil {
		t.Fatalf("want nil, got %+v", b)
	}
}

func TestModalitiesCombineOnlyWhenAssociated(t *testing.T) {
	f := NewFusion(DefaultConfig(), enrolled)
	observe(t, f, evidence("track-1", "face", "cam", t0, Candidate{"ana", 4}))
	observe(t, f, evidence("utt-1", "voice", "mic", t0, Candidate{"ana", 3}))

	alone := f.Current("track-1", t0)
	if len(alone.Modalities) != 1 {
		t.Fatalf("unassociated subjects must not corroborate: %v", alone.Modalities)
	}

	associate(t, f, "track-1", "utt-1", 0.5)
	if b := f.Current("track-1", t0); len(b.Modalities) != 1 {
		t.Fatalf("weak association must not join subjects: %v", b.Modalities)
	}

	associate(t, f, "utt-1", "track-1", 0.95)
	joined := f.Current("track-1", t0)
	if len(joined.Modalities) != 2 || joined.Confidence <= alone.Confidence {
		t.Fatalf("want two modalities and higher confidence, got %+v", joined)
	}
	if joined.Confidence < 0.99 {
		t.Fatalf("confidence = %v", joined.Confidence)
	}
	if len(f.Beliefs(t0)) != 1 {
		t.Fatalf("associated subjects are one belief, got %d", len(f.Beliefs(t0)))
	}
}

func TestDisagreementLowersConfidence(t *testing.T) {
	f := NewFusion(DefaultConfig(), enrolled)
	observe(t, f, evidence("s", "face", "cam", t0, Candidate{"ana", 6}))
	before := f.Current("s", t0).Confidence
	observe(t, f, evidence("s", "voice", "mic", t0, Candidate{"ana", -3}))

	after := f.Current("s", t0)
	if after.Confidence >= before {
		t.Fatalf("a mismatch must lower confidence: %v -> %v", before, after.Confidence)
	}
	if len(after.Modalities) != 1 {
		t.Fatalf("a mismatching modality is not support: %v", after.Modalities)
	}
}

func TestSameModalityCountsOnce(t *testing.T) {
	f := NewFusion(DefaultConfig(), enrolled)
	observe(t, f, evidence("s", "face", "cam-1", t0, Candidate{"ana", 4}))
	one := f.Current("s", t0).Confidence
	observe(t, f, evidence("s", "face", "cam-2", t0, Candidate{"ana", 4}))

	b := f.Current("s", t0)
	if b.Confidence != one || len(b.Modalities) != 1 {
		t.Fatalf("a second camera must not corroborate: %+v", b)
	}
}

func TestModalityGroupsCountOnce(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Modalities = map[string]Modality{"face": {Group: "camera"}, "appearance": {Group: "camera"}}
	f := NewFusion(cfg, enrolled)
	observe(t, f, evidence("s", "face", "cam", t0, Candidate{"ana", 4}))
	observe(t, f, evidence("s", "appearance", "cam", t0, Candidate{"ana", 3}))

	if b := f.Current("s", t0); len(b.Modalities) != 1 {
		t.Fatalf("modalities in one group count once: %v", b.Modalities)
	}
}

func TestLLRIsCapped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLLR = 2
	f := NewFusion(cfg, enrolled)
	observe(t, f, evidence("s", "face", "cam", t0, Candidate{"ana", 50}))

	// prior 0.125 · e^2 against 0.875.
	if b := f.Current("s", t0); b.Confidence > 0.52 {
		t.Fatalf("one overconfident sensor must not reach certainty: %v", b.Confidence)
	}
}

func TestCloseCandidatesAreNotResolved(t *testing.T) {
	f := NewFusion(DefaultConfig(), enrolled)
	observe(t, f, evidence("s", "face", "cam", t0, Candidate{"ana", 5}, Candidate{"ben", 4.8}))
	if b := f.Current("s", t0); b.State != Ambiguous || b.Leading != nil {
		t.Fatalf("want ambiguous, got %+v", b)
	}

	observe(t, f, evidence("s", "face", "cam", t0.Add(time.Second), Candidate{"ana", 5}))
	observe(t, f, evidence("s", "voice", "mic", t0.Add(time.Second), Candidate{"ben", 5}))
	if b := f.Current("s", t0.Add(time.Second)); b.State != Conflicted || b.Leading != nil {
		t.Fatalf("want conflicted, got %+v", b)
	}
}

// Candidates that are close to each other but all less likely than "not
// enrolled" are not a contest: the subject is simply unknown.
func TestUnlikelyCandidatesAreUnknownNotContested(t *testing.T) {
	f := NewFusion(DefaultConfig(), population{"ana": true, "cy": true})
	observe(t, f, evidence("s", "voice", "mic", t0, Candidate{"ana", -4.1}, Candidate{"cy", -3.9}))
	b := f.Current("s", t0)
	if b.State != Unknown || b.Leading != nil {
		t.Fatalf("want unknown, got %+v", b)
	}
	if b.Unknown < 0.95 || b.Candidates[0].Probability > 0.05 {
		t.Fatalf("posterior should sit on not enrolled: %+v", b)
	}

	// Producers that each lean, weakly, to a different person do not make it
	// a conflict while neither person outweighs "not enrolled".
	observe(t, f, evidence("s", "face", "cam", t0, Candidate{"cy", 0.3}))
	observe(t, f, evidence("s", "voice", "mic", t0, Candidate{"ana", 0.3}))
	if b := f.Current("s", t0); b.State != Unknown || b.Leading != nil {
		t.Fatalf("want unknown, got %+v", b)
	}
}

func TestUnknownSupersedesAndEvidenceExpires(t *testing.T) {
	f := NewFusion(DefaultConfig(), enrolled)
	observe(t, f, evidence("s", "face", "cam", t0, Candidate{"ana", 6}))
	observe(t, f, evidence("s", "face", "cam", t0.Add(time.Second)))
	if b := f.Current("s", t0.Add(time.Second)); b.State != Unknown || b.Leading != nil {
		t.Fatalf("a newer no-match must supersede: %+v", b)
	}

	observe(t, f, evidence("s", "face", "cam", t0.Add(2*time.Second), Candidate{"ana", 6}))
	if b := f.Current("s", t0.Add(2*time.Minute)); b != nil {
		t.Fatalf("expired evidence must disappear: %+v", b)
	}
}

func TestUnregisteredAndUncalibratedAreNotEvidence(t *testing.T) {
	f := NewFusion(DefaultConfig(), enrolled)
	observe(t, f, evidence("s", "face", "cam", t0, Candidate{"stranger-label", 9}))
	if b := f.Current("s", t0); b.Leading != nil {
		t.Fatalf("an unregistered label is not an identity: %+v", b)
	}

	e := evidence("s2", "face", "cam", t0, Candidate{"ana", 9})
	e.Calibration = ""
	observe(t, f, e)
	if b := f.Current("s2", t0); b.Leading != nil {
		t.Fatalf("uncalibrated evidence must not resolve: %+v", b)
	}
}

func TestFreshnessFollowsSupport(t *testing.T) {
	f := NewFusion(DefaultConfig(), enrolled)
	observe(t, f, evidence("s", "face", "cam", t0, Candidate{"ana", 8}))
	later := t0.Add(30 * time.Second)
	observe(t, f, evidence("s", "voice", "mic", later, Candidate{"ana", -1}))

	if b := f.Current("s", later); !b.FreshestAt.Equal(t0) {
		t.Fatalf("a fresh mismatch must not renew an old match: %v", b.FreshestAt)
	}
}

func TestRetirement(t *testing.T) {
	f := NewFusion(DefaultConfig(), enrolled)
	observe(t, f, evidence("s", "face", "cam", t0, Candidate{"ana", 6}))
	f.RetireIdentity("ana")
	if b := f.Current("s", t0); b.Leading != nil {
		t.Fatalf("a retired identity must not lead: %+v", b)
	}
	f.RetireSubject("s")
	if b := f.Current("s", t0); b != nil {
		t.Fatalf("a retired subject has no belief: %+v", b)
	}
}
