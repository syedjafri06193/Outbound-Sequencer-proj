// Package meeting handles the account-level pause on a booked meeting.
//
// If someone at Acme books a meeting, continuing to cold-email three other
// people at Acme is the single most visible failure a sequencer can
// produce. The prospect mentions it on the call, and it reads as an
// organisation that does not talk to itself.
//
// So the pause is account-level by default, across every rep and every
// sequence -- with a per-sequence override, rather than the other way
// round.
package meeting

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

// DetectionSource is where a booking came from.
//
// Reliability differs enough to change the handling: a scheduling-link
// webhook is acted on, a reply that merely sounds like a booking is a
// prompt to the rep.
type DetectionSource string

const (
	SourceSchedulingLink DetectionSource = "scheduling_link"
	SourceCalendarEvent  DetectionSource = "calendar_event"
	SourceCRM            DetectionSource = "crm"
	SourceInferredReply  DetectionSource = "inferred_reply"
)

// Reliable reports whether this source may pause automatically.
//
// A reply classified as positive with a proposed time is not a booking. A
// sequencer that pauses an entire account on the strength of "how about
// Thursday?" will pause accounts that never book anything.
func (s DetectionSource) Reliable() bool {
	switch s {
	case SourceSchedulingLink, SourceCalendarEvent, SourceCRM:
		return true
	default:
		return false
	}
}

// Booked is a detected meeting.
type Booked struct {
	AttendeeEmail string
	AccountID     string
	StartsAt      time.Time
	Source        DetectionSource
	// SourceRef is the webhook id, calendar event id or CRM object id.
	// Recorded as the pause evidence, because a pause a human cannot
	// explain is a pause a human will simply clear.
	SourceRef  string
	DetectedAt time.Time
}

// Policy holds the timings.
type Policy struct {
	// CooldownAfterMeeting is how long after the meeting starts the pause
	// lasts.
	CooldownAfterMeeting time.Duration
	// CooldownAfterCancellation is the wait before resuming when a
	// meeting is cancelled. Days, not minutes: an instant resume after a
	// cancellation reads badly.
	CooldownAfterCancellation time.Duration
	// AccountLevel is the default. A per-sequence override can turn it
	// off; the default is on.
	AccountLevel bool
}

func DefaultPolicy() Policy {
	return Policy{
		CooldownAfterMeeting:      30 * 24 * time.Hour,
		CooldownAfterCancellation: 7 * 24 * time.Hour,
		AccountLevel:              true,
	}
}

// Store is what the handler needs from persistence.
type Store interface {
	ActiveEnrollmentsByAccount(accountID string) ([]model.Enrollment, error)
	ActiveEnrollmentsByContact(contactID string) ([]model.Enrollment, error)
	ContactByEmail(email string) (model.Contact, bool)
	Pause(enrollmentID string, reason model.PauseReason) error
	Resume(enrollmentID string, at time.Time) error
	// CancelQueued removes already-scheduled jobs for an enrolment.
	//
	// Marking the state is not enough on its own. A job queued at
	// 09:00:00 for 09:00:31 has already read the state; cancelling it is
	// what closes the window the send-time preflight cannot.
	CancelQueued(enrollmentID string) (int, error)
}

// Notifier tells a human.
type Notifier interface {
	Notify(accountID string, message string)
}

// Handler applies bookings.
type Handler struct {
	store  Store
	policy Policy
	notify Notifier

	mu sync.Mutex
	// overrides holds sequences that opted out of account-level pausing.
	overrides map[string]bool
}

func NewHandler(s Store, p Policy, n Notifier) *Handler {
	return &Handler{store: s, policy: p, notify: n, overrides: make(map[string]bool)}
}

// SetContactLevelOnly opts one sequence out of the account-level default.
func (h *Handler) SetContactLevelOnly(sequenceID string, only bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.overrides[sequenceID] = only
}

func (h *Handler) contactLevelOnly(sequenceID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.overrides[sequenceID]
}

// Outcome reports what a booking did.
type Outcome struct {
	Paused        []string
	CancelledJobs int
	// NeedsConfirmation is set when the source was not reliable enough to
	// act on. The rep is prompted instead.
	NeedsConfirmation bool
	PausedUntil       time.Time
	Message           string
}

// OnBooked pauses everyone at the account.
func (h *Handler) OnBooked(ev Booked) (Outcome, error) {
	contact, ok := h.store.ContactByEmail(ev.AttendeeEmail)
	if !ok {
		return Outcome{}, fmt.Errorf("meeting: no contact for %s", ev.AttendeeEmail)
	}
	accountID := contact.AccountID
	if ev.AccountID != "" {
		accountID = ev.AccountID
	}

	if !ev.Source.Reliable() {
		msg := fmt.Sprintf(
			"possible meeting with %s inferred from a reply; confirm before pausing the account",
			ev.AttendeeEmail)
		if h.notify != nil {
			h.notify.Notify(accountID, msg)
		}
		return Outcome{NeedsConfirmation: true, Message: msg}, nil
	}

	// Pause now even for a meeting three weeks out. Waiting until the
	// meeting starts means three more cold emails go out in the meantime,
	// which is the failure this exists to prevent.
	until := ev.StartsAt.Add(h.policy.CooldownAfterMeeting)

	var enrollments []model.Enrollment
	var err error
	if h.policy.AccountLevel {
		enrollments, err = h.store.ActiveEnrollmentsByAccount(accountID)
	} else {
		enrollments, err = h.store.ActiveEnrollmentsByContact(contact.ID)
	}
	if err != nil {
		return Outcome{}, err
	}

	out := Outcome{PausedUntil: until}
	for _, en := range enrollments {
		// A sequence that opted out still pauses the attendee's own
		// enrolment. Continuing to cold-email the person you are about
		// to meet is not a configurable preference.
		if h.contactLevelOnly(en.SequenceID) && en.ContactID != contact.ID {
			continue
		}
		reason := model.PauseReason{
			Kind:      "meeting_booked",
			Evidence:  string(ev.Source) + ":" + ev.SourceRef,
			ExpiresAt: until,
		}
		if err := h.store.Pause(en.ID, reason); err != nil {
			return out, err
		}
		out.Paused = append(out.Paused, en.ID)

		n, err := h.store.CancelQueued(en.ID)
		if err != nil {
			return out, err
		}
		out.CancelledJobs += n
	}
	sort.Strings(out.Paused)

	out.Message = fmt.Sprintf(
		"%s booked a meeting: paused %d enrolments across the account until %s, and cancelled %d queued sends",
		ev.AttendeeEmail, len(out.Paused), until.Format("2006-01-02"), out.CancelledJobs)
	if h.notify != nil {
		h.notify.Notify(accountID, out.Message)
	}
	return out, nil
}

// CancellationKind separates the two outcomes that look alike and are not.
type CancellationKind string

const (
	// Cancelled: the prospect called it off. Resume cold outreach after a
	// cooldown.
	Cancelled CancellationKind = "cancelled"
	// NoShow: they did not turn up. A specific short follow-up, not a
	// resume of cold outreach -- someone who booked and missed is not
	// the same as someone who never engaged, and treating them as cold
	// again wastes the engagement.
	NoShow CancellationKind = "no_show"
)

// AfterMeeting is what to do once the meeting did not happen.
type AfterMeeting struct {
	ResumeColdOutreach bool
	ResumeAt           time.Time
	FollowUpSequence   bool
	Rationale          string
}

func (h *Handler) OnNotHeld(kind CancellationKind, at time.Time) AfterMeeting {
	switch kind {
	case NoShow:
		return AfterMeeting{
			FollowUpSequence: true,
			Rationale: "no-show: the account engaged and then missed. A short follow-up, " +
				"not a resume of cold outreach -- restarting the cold sequence throws away the engagement.",
		}
	default:
		return AfterMeeting{
			ResumeColdOutreach: true,
			ResumeAt:           at.Add(h.policy.CooldownAfterCancellation),
			Rationale: fmt.Sprintf("cancelled: resume after a %s cooldown. An instant resume after a cancellation reads badly.",
				h.policy.CooldownAfterCancellation),
		}
	}
}

// PauseIsNotAStop documents, in code, the distinction §8.4 insists on.
//
// A meeting booking is a pause with a cooldown expiry. If the meeting falls
// through and the deal goes quiet, the rep wants to resume from step 3
// rather than start over -- which a stop makes impossible.
func PauseIsNotAStop(en model.Enrollment) error {
	if en.State == model.StateStopped {
		return fmt.Errorf("meeting: enrolment %s was stopped rather than paused; the step position is lost and it can only be re-enrolled from the beginning", en.ID)
	}
	return nil
}
