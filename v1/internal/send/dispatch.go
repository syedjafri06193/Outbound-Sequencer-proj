package send

import (
	"fmt"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/store"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/unsubscribe"
)

// Provider is a mailbox API -- Gmail or Microsoft Graph.
type Provider interface {
	Send(msg Message) (Result, error)
}

// Message is what goes to the provider.
type Message struct {
	MailboxID string
	To        string
	Subject   string
	Body      string
	// Headers carries the RFC 8058 unsubscribe pair and the threading
	// headers.
	Headers map[string]string
}

// Result is what came back.
type Result struct {
	MessageID string
	ThreadID  string
}

// Logger is the minimum the dispatcher needs.
type Logger interface {
	Info(msg string, kv ...any)
	Error(msg string, kv ...any)
}

type nopLogger struct{}

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// Dispatcher executes sends.
type Dispatcher struct {
	Store     *store.Store
	Preflight *Preflight
	Provider  Provider
	Signer    *unsubscribe.Signer
	// UnsubBaseURL and UnsubMailto build the RFC 8058 headers.
	UnsubBaseURL string
	UnsubMailto  string
	Log          Logger
	Now          func() time.Time
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dispatcher) log() Logger {
	if d.Log != nil {
		return d.Log
	}
	return nopLogger{}
}

// Outcome reports what happened to one job.
type Outcome string

const (
	Sent      Outcome = "sent"
	Skipped   Outcome = "skipped"
	Duplicate Outcome = "duplicate"
	Failed    Outcome = "failed"
)

// Execute runs one send, in the order §14.2 sets out.
//
//  1. Claim the idempotency key FIRST. Fail toward not sending.
//  2. Re-check everything: the world changed since scheduling.
//  3. Budget at the domain level.
//  4. Send.
//  5. Persist the Message-ID for threading.
//
// A skipped send returns nil, not an error. A skip is the system working,
// and treating it as an error pollutes alerting with correct behaviour --
// which then trains everyone to ignore the alerts that matter.
func (d *Dispatcher) Execute(job Job) (Outcome, error) {
	now := d.now()

	// A manual task never reaches the provider.
	if err := ManualTaskGate(job); err != nil {
		d.Store.RecordSkipped(job.EnrollmentID, job.StepIndex, "manual_task", now)
		return Skipped, nil
	}

	// 0. Take the enrolment's row lock, and hold it through the provider
	// call.
	//
	// The preflight alone narrows the §7.4 race to the gap between the
	// check and the provider accepting the message. That gap is small and
	// it is not zero: measured over 500 iterations, a reply landing
	// inside it produced around 80 emails sent to someone who had already
	// replied. Holding the lock across the whole operation removes the
	// gap rather than shrinking it, because the reply, booking and
	// unsubscribe paths take the same lock before they write.
	// Contact first, then enrolment, always in that order. The contact
	// lock is what a global unsubscribe takes, and it knows nothing about
	// enrolments.
	releaseContact := d.Store.LockContact(job.ContactEmail)
	defer releaseContact()
	releaseEnrollment := d.Store.LockEnrollment(job.EnrollmentID)
	defer releaseEnrollment()

	// 1. Claim before dispatch.
	//
	// A crash after this line means a missed send: recoverable, visible
	// in StaleClaims, and fixable by a human. A crash before it means a
	// double send, which is not recoverable and which the prospect
	// notices.
	key := store.SendKey(job.EnrollmentID, job.StepIndex)
	claimed, err := d.Store.ClaimSendKey(key, now)
	if err != nil {
		return Failed, err
	}
	if !claimed {
		d.log().Info("send already dispatched, skipping", "key", key)
		return Duplicate, nil
	}

	// 2 and 3. Everything re-checked, including the budget, immediately
	// before dispatch.
	if v := d.Preflight.Check(job, now); !v.Allowed {
		d.Store.RecordSkipped(job.EnrollmentID, job.StepIndex, v.Gate, now)
		d.log().Info("send skipped", "enrollment", job.EnrollmentID, "gate", v.Gate, "reason", v.Reason)
		// The key stays claimed. Releasing it would let a retry loop
		// re-attempt a send that the gates already refused, and the
		// gates are not going to change their minds within the step's
		// lifetime.
		return Skipped, nil
	}

	// 4. Send.
	msg, err := d.buildMessage(job)
	if err != nil {
		return Failed, err
	}
	res, err := d.Provider.Send(msg)
	if err != nil {
		d.log().Error("provider send failed", "enrollment", job.EnrollmentID, "err", err)
		// The key is NOT released. Whether the provider accepted the
		// message before the error is unknowable from here, and a
		// retry that turns one uncertain send into two certain ones is
		// the worse failure.
		return Failed, fmt.Errorf("send: provider rejected step %d of %s: %w",
			job.StepIndex, job.EnrollmentID, err)
	}

	// 5. Persist for threading.
	d.Store.RecordSent(job.EnrollmentID, job.StepIndex, res.MessageID, res.ThreadID, now)
	if err := d.Store.CompleteSendKey(key, res.MessageID); err != nil {
		// Recorded, logged, not returned: the message is out and
		// reporting a failure would invite a retry.
		d.log().Error("could not complete send key", "key", key, "err", err)
	}
	return Sent, nil
}

func (d *Dispatcher) buildMessage(job Job) (Message, error) {
	headers := map[string]string{}

	// Threading on Message-ID and References, never on subject.
	if job.InReplyTo != "" {
		headers["In-Reply-To"] = job.InReplyTo
	}
	if len(job.References) > 0 {
		refs := ""
		for i, r := range job.References {
			if i > 0 {
				refs += " "
			}
			refs += r
		}
		headers["References"] = refs
	}

	body := job.Body
	if d.Signer != nil && d.UnsubBaseURL != "" {
		tok, err := d.Signer.Sign(unsubscribe.Token{
			Email:        job.ContactEmail,
			EnrollmentID: job.EnrollmentID,
			SequenceID:   job.SequenceID,
		})
		if err != nil {
			return Message{}, err
		}
		for k, v := range unsubscribe.Headers(d.UnsubBaseURL, d.UnsubMailto, tok) {
			headers[k] = v
		}
		// And in the body, visibly. Hiding the unsubscribe to reduce
		// opt-outs converts unsubscribes into spam complaints, which
		// trades a free action for a catastrophic one.
		body += "\n\n---\nIf you would rather not hear from me, unsubscribe here: " +
			unsubscribe.BodyLink(d.UnsubBaseURL, tok) + "\n"
	}

	return Message{
		MailboxID: job.MailboxID,
		To:        job.ContactEmail,
		Subject:   job.Subject,
		Body:      body,
		Headers:   headers,
	}, nil
}

// RequireUnsubscribe refuses to build a cold email without an opt-out.
//
// A validation error rather than a warning, for the same reason the
// primary-domain check is: a bulk sender without RFC 8058 headers is
// rejected by Google outright, and a commercial email without a working
// opt-out is a CAN-SPAM violation at roughly $53,000 per message.
func (d *Dispatcher) RequireUnsubscribe() error {
	if d.Signer == nil || d.UnsubBaseURL == "" {
		return fmt.Errorf("send: no unsubscribe signer or base URL configured; every cold email needs a working one-click opt-out")
	}
	return nil
}
