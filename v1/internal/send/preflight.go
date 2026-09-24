// Package send is the dispatch path.
//
// Everything else in this repository exists so that this package can refuse
// to send. The preflight is a list of checks, each of which is there
// because of a specific way outbound sequencing goes wrong, and the order
// is load-bearing.
package send

import (
	"fmt"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/deliverability"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/jurisdiction"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/ratelimit"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/suppression"
)

// Job is one queued send.
type Job struct {
	EnrollmentID string
	StepIndex    int
	ContactID    string
	ContactEmail string
	AccountID    string
	MailboxID    string
	DomainID     string
	SequenceID   string
	Channel      model.Channel
	Mode         model.StepMode
	ScheduledAt  time.Time
	Subject      string
	Body         string
	// InReplyTo threads this step onto the previous one.
	InReplyTo  string
	References []string
}

// Verdict is the preflight's answer.
type Verdict struct {
	Allowed bool
	// Gate names which check refused, for the skip metrics.
	Gate   string
	Reason string
}

func deny(gate, format string, args ...any) Verdict {
	return Verdict{Gate: gate, Reason: fmt.Sprintf(format, args...)}
}

// ReplyState is the reply side of the race.
type ReplyState interface {
	HasReplied(enrollmentID string) bool
}

// BookingState is the meeting side.
type BookingState interface {
	AccountHasBooking(accountID string, now time.Time) bool
}

// EnrollmentState reads the enrolment.
type EnrollmentState interface {
	Enrollment(id string) (model.Enrollment, bool)
	Contact(id string) (model.Contact, bool)
}

// Budget is the domain-level circuit breaker.
type Budget interface {
	Status(domainID string) deliverability.BreakerStatus
}

// Preflight holds the dependencies of the send-time gate.
type Preflight struct {
	Suppression *suppression.List
	Replies     ReplyState
	Bookings    BookingState
	State       EnrollmentState
	Gate        *jurisdiction.Gate
	Limiter     *ratelimit.Limiter
	Budget      Budget
}

// Check runs every gate, in order, immediately before dispatch.
//
// The order is not cosmetic:
//
//  1. Suppression, because an email to someone who unsubscribed is a
//     CAN-SPAM violation and a near-certain complaint. This is the third
//     of the three suppression checkpoints and the one people skip:
//     everything can change between enrolment and send. A prospect
//     unsubscribes from one sequence on Tuesday; a different sequence has
//     a step queued for Wednesday.
//  2. Enrolment state, which catches a stop that arrived after queueing.
//  3. Reply, the §7.4 race: the check happened at schedule time and the
//     world changed before dispatch.
//  4. Booking, the same race at the account level.
//  5. Jurisdiction, which can be revoked between enrolment and send.
//  6. Budget, before the rate limiter, because the limiter CONSUMES
//     allowance. Checking a halted domain after spending a mailbox's
//     daily slot on it throws away capacity on a send that will not
//     happen.
//  7. Rate limits, last, and the only check with a side effect.
//
// Returning a refusal is not an error. A skipped send is the system
// working.
func (p *Preflight) Check(job Job, now time.Time) Verdict {
	// 1. Suppression.
	if p.Suppression != nil {
		if s := p.Suppression.Check(job.ContactEmail); s != nil {
			return deny("suppression", "suppressed: %s (recorded %s from %s)",
				s.Reason, s.CreatedAt.Format("2006-01-02"), s.Source)
		}
	}

	// 2. Enrolment state.
	if p.State != nil {
		en, ok := p.State.Enrollment(job.EnrollmentID)
		if !ok {
			return deny("enrollment", "no enrolment %s", job.EnrollmentID)
		}
		if en.State.Terminal() {
			return deny("enrollment", "enrolment is %s", en.State)
		}
		if en.State == model.StatePaused {
			if en.PausedUntil.IsZero() || now.Before(en.PausedUntil) {
				return deny("enrollment", "paused (%s) until %s",
					en.PauseReason.Kind, en.PausedUntil.Format("2006-01-02"))
			}
		}
	}

	// 3. The reply race.
	if p.Replies != nil && p.Replies.HasReplied(job.EnrollmentID) {
		return deny("reply", "the prospect replied after this send was queued")
	}

	// 4. The booking race, at the account level.
	if p.Bookings != nil && p.Bookings.AccountHasBooking(job.AccountID, now) {
		return deny("booking", "someone at account %s booked a meeting", job.AccountID)
	}

	// 5. Jurisdiction.
	if p.Gate != nil && p.State != nil {
		contact, ok := p.State.Contact(job.ContactID)
		if !ok {
			return deny("jurisdiction", "no contact %s; jurisdiction cannot be determined", job.ContactID)
		}
		if d := p.Gate.CheckChannel(contact, job.Channel, nil); !d.Allowed {
			return deny("jurisdiction", "%s", d.Reason)
		}
	}

	// 6. Budget, before the limiter consumes anything.
	if p.Budget != nil {
		st := p.Budget.Status(job.DomainID)
		if st.Halted {
			return deny("budget", "domain %s is halted: %s", job.DomainID, st.Reason)
		}
	}

	// 7. Rate limits. Consuming.
	if p.Limiter != nil {
		ok, reason := p.Limiter.Allow(job.MailboxID, job.DomainID, job.AccountID, now)
		if !ok {
			return deny("ratelimit", "%s: %s", reason.Level, reason.Message)
		}
	}

	return Verdict{Allowed: true}
}

// ManualTaskGate refuses to dispatch a step that a human must perform.
//
// A structural check rather than a policy one. LinkedIn steps are
// ModeManualTask in the type system precisely so that nobody can configure
// their way into automating them -- the rep's own account is the asset at
// risk, and for a salesperson that account is a significant part of their
// professional identity. This function is the runtime half of that
// guarantee.
func ManualTaskGate(job Job) error {
	if job.Channel == model.ChannelLinkedIn && job.Mode != model.ModeManualTask {
		return fmt.Errorf("send: LinkedIn step on enrolment %s is marked %s; automating LinkedIn violates the User Agreement and risks the rep's own account, so these steps are manual tasks by construction",
			job.EnrollmentID, job.Mode)
	}
	if job.Mode == model.ModeManualTask {
		return fmt.Errorf("send: step %d of enrolment %s is a manual task and is not dispatched by the engine",
			job.StepIndex, job.EnrollmentID)
	}
	return nil
}
