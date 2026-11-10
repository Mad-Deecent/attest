// Package assurance classifies a Belief into bands. A Band is a named
// predicate over Belief and is the one concept attest adds to authorization.
package assurance

import (
	"fmt"
	"time"

	"github.com/mad-deecent/attest/internal/perception"
)

type Band struct {
	Name string `json:"name"`
	// MinConfidence is the posterior probability the leader must reach.
	MinConfidence float64 `json:"minConfidence,omitempty"`
	// MaxAgeSeconds bounds the age of the newest supporting evidence.
	MaxAgeSeconds int `json:"maxAgeSeconds,omitempty"`
	// MinModalities is the number of independent modality groups that must
	// support the leader.
	MinModalities int `json:"minModalities,omitempty"`
	// CoPresence lists roles that must also be believed present, held by
	// someone other than the subject, in the subject's zone.
	CoPresence []string `json:"coPresence,omitempty"`
}

// DefaultLadder is the band vocabulary attest ships with, weakest first.
// Thresholds are posterior probabilities, so "corroborated" reads as: at most
// a 1% chance this is somebody else, on two independent modalities, within
// the last two minutes.
func DefaultLadder() []Band {
	return []Band{
		{Name: "presumed", MinConfidence: 0.80, MaxAgeSeconds: 900},
		{Name: "confident", MinConfidence: 0.95, MaxAgeSeconds: 300},
		{Name: "corroborated", MinConfidence: 0.99, MaxAgeSeconds: 120, MinModalities: 2},
	}
}

// Policy evaluates a band against a belief. Static is the v0 implementation;
// bands that move with live signal conditions fit the same interface.
type Policy interface {
	Holds(b Band, belief perception.Belief, present func(role string) bool, now time.Time) Result
}

type Result struct {
	OK      bool
	Reasons []string
	// ValidUntil is when the result lapses if no new evidence arrives.
	ValidUntil time.Time
}

type Static struct{}

func (Static) Holds(b Band, belief perception.Belief, present func(role string) bool, now time.Time) Result {
	res := Result{OK: true, ValidUntil: belief.ExpiresAt}
	check := func(pass bool, format string, args ...any) {
		res.OK = res.OK && pass
		res.Reasons = append(res.Reasons, fmt.Sprintf(format, args...))
	}
	if b.MinConfidence > 0 {
		pass := belief.Confidence >= b.MinConfidence
		check(pass, "confidence %.3f %s min %.3f", belief.Confidence, cmp(pass, ">=", "<"), b.MinConfidence)
	}
	if b.MaxAgeSeconds > 0 {
		maxAge := time.Duration(b.MaxAgeSeconds) * time.Second
		age := now.Sub(belief.FreshestAt)
		pass := age <= maxAge
		check(pass, "age %s %s max %s", age.Round(time.Second), cmp(pass, "<=", ">"), maxAge)
		if lapse := belief.FreshestAt.Add(maxAge); res.ValidUntil.IsZero() || lapse.Before(res.ValidUntil) {
			res.ValidUntil = lapse
		}
	}
	if b.MinModalities > 0 {
		n := len(belief.Modalities)
		pass := n >= b.MinModalities
		check(pass, "modalities %d %s min %d", n, cmp(pass, ">=", "<"), b.MinModalities)
	}
	for _, role := range b.CoPresence {
		pass := present(role)
		check(pass, "co-presence %s: %s", role, cmp(pass, "present", "absent"))
	}
	return res
}

func cmp(pass bool, yes, no string) string {
	if pass {
		return yes
	}
	return no
}
