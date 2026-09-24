// Package deliverability enforces the physical limits that cap outbound
// volume.
//
// The framing that matters: a sequencer optimising for send volume is
// building a machine that destroys its user's business. The 0.3% complaint
// ceiling exists because the ecosystem decided to price spam, and it prices
// it at roughly one complaint per three hundred. There is no targeting
// strategy that survives that threshold at high volume with generic
// messaging.
//
// So the guardrails are not a feature of this product. They are the
// product.
package deliverability

import (
	"fmt"
	"sync"
	"time"
)

// The published thresholds, recalculated daily by the providers rather than
// averaged over a window.
const (
	// GoogleTargetRate is Google's stated target: below 0.1%.
	GoogleTargetRate = 0.001
	// GoogleCeilingRate is where enforcement begins: 0.3%.
	GoogleCeilingRate = 0.003
	// BounceWarnRate and BounceCriticalRate bracket list quality.
	BounceWarnRate     = 0.01
	BounceCriticalRate = 0.02
	// BulkSenderDailyThreshold is the Gmail volume that permanently
	// attaches bulk-sender classification.
	BulkSenderDailyThreshold = 5000
)

// Action is what the breaker decided.
type Action string

const (
	Continue       Action = "continue"
	ThrottleDomain Action = "throttle"
	HaltDomain     Action = "halt"
)

// Metrics is one day's numbers for one sending domain.
//
// Inboxed is the denominator, and it is not something the sender can
// observe: you do not know what was inboxed versus filtered. It comes from
// Postmaster Tools. Computing a complaint rate against *sent* mail instead
// produces a number that looks fine right up until the domain is blocked.
type Metrics struct {
	DomainID   string
	Day        time.Time
	Sent       int
	Inboxed    int
	Complaints int
	Bounces    int
	// Source records where the numbers came from, because a rate computed
	// locally and a rate from Postmaster Tools are different quantities
	// and must not be silently interchanged.
	Source MetricsSource
}

type MetricsSource string

const (
	SourcePostmaster MetricsSource = "postmaster_tools"
	// SourceLocal is sender-side counting. Usable for bounces, which the
	// sender does observe, and NOT usable for the complaint denominator.
	SourceLocal MetricsSource = "local"
)

// ComplaintRate is complaints over inboxed mail.
//
// Returns ok=false when the denominator is unobservable, rather than
// returning a plausible-looking wrong number. A caller that silently
// substitutes Sent for Inboxed gets a rate that is too low by exactly the
// factor the death spiral is made of.
func (m Metrics) ComplaintRate() (rate float64, ok bool) {
	if m.Source != SourcePostmaster {
		return 0, false
	}
	if m.Inboxed <= 0 {
		return 0, false
	}
	return float64(m.Complaints) / float64(m.Inboxed), true
}

// BounceRate is bounces over sent mail.
//
// Sent, not inboxed: a bounce means the message never reached an inbox, so
// inboxed is the wrong denominator and the sender does observe sends.
func (m Metrics) BounceRate() (float64, bool) {
	if m.Sent <= 0 {
		return 0, false
	}
	return float64(m.Bounces) / float64(m.Sent), true
}

// Decision is the breaker's output.
type Decision struct {
	Action Action
	Reason string
	// Factor is the send-rate multiplier for ThrottleDomain.
	Factor float64
	// RequiresHumanAck is set on a halt. An automatic resume restarts the
	// same behaviour and the spiral continues, so a human has to look at
	// what happened.
	RequiresHumanAck bool
	// Rate and Threshold are carried so the operator sees the arithmetic
	// rather than a bare verdict.
	Rate      float64
	Threshold float64
}

// Budget is the circuit breaker's configuration.
type Budget struct {
	// HaltThreshold is deliberately far below the 0.3% ceiling. By the
	// time Postmaster Tools shows 0.3%, the denominator spiral is already
	// running: the ceiling is where *enforcement* begins, not where risk
	// begins.
	HaltThreshold float64
	WarnThreshold float64
	// MinVolume stops the breaker acting on tiny samples, where a single
	// complaint is a large percentage of nothing.
	MinVolume int
	// BounceHalt and BounceWarn apply the same machinery to bounces. High
	// bounces usually mean list-quality problems that produce complaints
	// next, so it is a leading indicator rather than a separate concern.
	BounceHalt float64
	BounceWarn float64
	// ThrottleFactor is how hard a warn-level rate slows sending.
	ThrottleFactor float64
}

// DefaultBudget halts at 0.08%, well below Google's 0.1% target and far
// below the 0.3% ceiling.
func DefaultBudget() Budget {
	return Budget{
		HaltThreshold:  0.0008,
		WarnThreshold:  0.0005,
		MinVolume:      500,
		BounceHalt:     BounceCriticalRate,
		BounceWarn:     BounceWarnRate,
		ThrottleFactor: 0.5,
	}
}

// Evaluate decides what to do with one day's metrics.
func (b Budget) Evaluate(m Metrics) Decision {
	// Bounces first. The sender observes them directly, so they are
	// actionable even when Postmaster data has not arrived -- and a
	// bouncing domain should stop before anyone argues about complaint
	// denominators.
	if br, ok := m.BounceRate(); ok && m.Sent >= b.MinVolume {
		switch {
		case br >= b.BounceHalt:
			return Decision{
				Action:           HaltDomain,
				Reason:           fmt.Sprintf("bounce rate %.2f%% at or above halt threshold %.2f%% (%d bounces of %d sent)", br*100, b.BounceHalt*100, m.Bounces, m.Sent),
				RequiresHumanAck: true,
				Rate:             br,
				Threshold:        b.BounceHalt,
			}
		case br >= b.BounceWarn:
			return Decision{
				Action:    ThrottleDomain,
				Factor:    b.ThrottleFactor,
				Reason:    fmt.Sprintf("bounce rate %.2f%% at or above warn threshold %.2f%%", br*100, b.BounceWarn*100),
				Rate:      br,
				Threshold: b.BounceWarn,
			}
		}
	}

	rate, ok := m.ComplaintRate()
	if !ok {
		// No usable complaint rate. Deliberately NOT treated as "fine":
		// a missing denominator means the breaker is blind, and a blind
		// breaker that reports Continue is worse than one that says so.
		return Decision{
			Action: Continue,
			Reason: "no complaint rate available (denominator is inboxed mail, which requires Postmaster Tools data)",
		}
	}

	if m.Inboxed < b.MinVolume {
		return Decision{
			Action: Continue,
			Reason: fmt.Sprintf("insufficient volume: %d inboxed, minimum %d", m.Inboxed, b.MinVolume),
			Rate:   rate,
		}
	}

	switch {
	case rate >= b.HaltThreshold:
		return Decision{
			Action: HaltDomain,
			Reason: fmt.Sprintf("complaint rate %.3f%% at or above halt threshold %.3f%% (%d complaints of %d inboxed)",
				rate*100, b.HaltThreshold*100, m.Complaints, m.Inboxed),
			RequiresHumanAck: true,
			Rate:             rate,
			Threshold:        b.HaltThreshold,
		}
	case rate >= b.WarnThreshold:
		return Decision{
			Action:    ThrottleDomain,
			Factor:    b.ThrottleFactor,
			Reason:    fmt.Sprintf("complaint rate %.3f%% at or above warn threshold %.3f%%", rate*100, b.WarnThreshold*100),
			Rate:      rate,
			Threshold: b.WarnThreshold,
		}
	}

	return Decision{Action: Continue, Rate: rate}
}

// ComplaintsAllowed is the arithmetic the product must surface.
//
// At a thousand inboxed emails a day, one complaint is the target ceiling
// and four is over the hard ceiling. Operators consistently do not believe
// this until they see the integer.
func ComplaintsAllowed(inboxed int, rate float64) int {
	if inboxed <= 0 {
		return 0
	}
	// Floor, not round: at 500 inboxed and a 0.1% target, one complaint is
	// 0.2% -- already over. Rounding up would report an allowance that
	// breaches the very threshold it claims to respect.
	return int(float64(inboxed) * rate)
}

// SpiralProjection models the denominator death spiral.
//
// The rate is complaints over *inboxed* mail. As filtering increases the
// denominator shrinks, so identical sending behaviour and identical
// complaint counts produce a worse rate, which triggers more filtering.
//
// This exists as code because a breaker that watches raw complaint counts
// misses the spiral entirely: the counts never change.
type SpiralProjection struct {
	Day        int
	Sent       int
	Inboxed    int
	Complaints int
	Rate       float64
}

// ProjectSpiral simulates the collapse from a starting point.
//
// filterGrowth is the fraction of the previous day's inboxed volume that
// gets filtered instead on the next day.
func ProjectSpiral(sent, inboxed, complaints int, filterGrowth float64, days int) []SpiralProjection {
	out := make([]SpiralProjection, 0, days)
	for d := 1; d <= days; d++ {
		rate := 0.0
		if inboxed > 0 {
			rate = float64(complaints) / float64(inboxed)
		}
		out = append(out, SpiralProjection{
			Day: d, Sent: sent, Inboxed: inboxed, Complaints: complaints, Rate: rate,
		})
		inboxed = int(float64(inboxed) * (1 - filterGrowth))
		if inboxed < 0 {
			inboxed = 0
		}
	}
	return out
}

// HaltState records a halted domain and whether a human has cleared it.
type HaltState struct {
	DomainID string
	HaltedAt time.Time
	Reason   string
	Rate     float64
	Acked    bool
	AckedBy  string
	AckedAt  time.Time
	AckNote  string
}

// Breaker holds halt state across domains.
//
// Safe for concurrent use: the send path consults it on every dispatch
// while the Postmaster sync updates it from another goroutine.
type Breaker struct {
	mu     sync.RWMutex
	budget Budget
	halted map[string]*HaltState
	// throttle is the current send-rate multiplier per domain.
	throttle map[string]float64
	now      func() time.Time
}

func NewBreaker(b Budget) *Breaker {
	return &Breaker{
		budget:   b,
		halted:   make(map[string]*HaltState),
		throttle: make(map[string]float64),
		now:      time.Now,
	}
}

// SetClock is for tests.
func (br *Breaker) SetClock(f func() time.Time) { br.now = f }

// Observe applies a day's metrics and updates state.
func (br *Breaker) Observe(m Metrics) Decision {
	d := br.budget.Evaluate(m)

	br.mu.Lock()
	defer br.mu.Unlock()

	switch d.Action {
	case HaltDomain:
		// Do not overwrite an existing halt. The first reason is the
		// useful one; replacing it each evaluation loses the original
		// cause and resets nothing, because clearing requires an ack
		// either way.
		if _, already := br.halted[m.DomainID]; !already {
			br.halted[m.DomainID] = &HaltState{
				DomainID: m.DomainID,
				HaltedAt: br.now(),
				Reason:   d.Reason,
				Rate:     d.Rate,
			}
		}
		br.throttle[m.DomainID] = 0

	case ThrottleDomain:
		if _, halted := br.halted[m.DomainID]; !halted {
			br.throttle[m.DomainID] = d.Factor
		}

	case Continue:
		if _, halted := br.halted[m.DomainID]; !halted {
			delete(br.throttle, m.DomainID)
		}
	}

	return d
}

// Halted reports whether a domain is currently stopped.
func (br *Breaker) Halted(domainID string) (*HaltState, bool) {
	br.mu.RLock()
	defer br.mu.RUnlock()
	h, ok := br.halted[domainID]
	if !ok {
		return nil, false
	}
	cp := *h
	return &cp, true
}

// ThrottleFactor is the current send-rate multiplier, 1.0 when unthrottled
// and 0 when halted.
func (br *Breaker) ThrottleFactor(domainID string) float64 {
	br.mu.RLock()
	defer br.mu.RUnlock()
	if _, halted := br.halted[domainID]; halted {
		return 0
	}
	if f, ok := br.throttle[domainID]; ok {
		return f
	}
	return 1
}

// ErrNotHalted is returned when acknowledging a domain that is not halted.
var ErrNotHalted = fmt.Errorf("deliverability: domain is not halted")

// ErrAckRequiresNote is returned for an empty acknowledgment.
var ErrAckRequiresNote = fmt.Errorf("deliverability: acknowledging a halt requires an operator and a note")

// Acknowledge clears a halt.
//
// Requires a named operator and a note, because the point of the human ack
// is that somebody looked at what happened. A one-click "resume" button
// with no explanation is an automatic resume with extra steps, and the
// same behaviour resumes and the spiral continues.
func (br *Breaker) Acknowledge(domainID, operator, note string) error {
	if operator == "" || note == "" {
		return ErrAckRequiresNote
	}

	br.mu.Lock()
	defer br.mu.Unlock()

	h, ok := br.halted[domainID]
	if !ok {
		return ErrNotHalted
	}
	h.Acked = true
	h.AckedBy = operator
	h.AckedAt = br.now()
	h.AckNote = note

	delete(br.halted, domainID)
	// Resume throttled rather than at full speed. Whatever caused the
	// halt has not been proven fixed by the act of acknowledging it.
	br.throttle[domainID] = br.budget.ThrottleFactor
	return nil
}

// HaltedDomains lists every currently halted domain.
func (br *Breaker) HaltedDomains() []HaltState {
	br.mu.RLock()
	defer br.mu.RUnlock()
	out := make([]HaltState, 0, len(br.halted))
	for _, h := range br.halted {
		out = append(out, *h)
	}
	return out
}

// BreakerStatus is the one-call answer the send path needs.
//
// Combining the halt flag and the throttle factor into one read matters:
// two separate calls can straddle a halt, and a send that reads
// "not halted" and then "throttle 1.0" from either side of the write goes
// out from a domain that is stopped.
type BreakerStatus struct {
	DomainID string
	Halted   bool
	// Acked is true once a human has looked at the halt. A halt stays
	// halted until it is acknowledged; acknowledging resumes at a
	// throttle, not at full speed.
	Acked          bool
	ThrottleFactor float64
	Reason         string
}

// Status returns the domain's current standing in one atomic read.
func (br *Breaker) Status(domainID string) BreakerStatus {
	br.mu.RLock()
	defer br.mu.RUnlock()

	s := BreakerStatus{DomainID: domainID, ThrottleFactor: 1}
	if h, ok := br.halted[domainID]; ok {
		s.Halted = true
		s.Acked = h.Acked
		s.Reason = h.Reason
		s.ThrottleFactor = 0
		return s
	}
	if f, ok := br.throttle[domainID]; ok {
		s.ThrottleFactor = f
		if f < 1 {
			s.Reason = "throttled by the complaint budget"
		}
	}
	return s
}
