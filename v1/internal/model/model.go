// Package model holds the types shared across the engine.
//
// Kept small and dependency-free: everything imports it, so anything that
// grows here grows everywhere.
package model

import (
	"fmt"
	"strings"
	"time"
)

// Channel is how a step reaches the prospect.
type Channel string

const (
	ChannelEmail    Channel = "email"
	ChannelCall     Channel = "call"
	ChannelLinkedIn Channel = "linkedin"
	ChannelSMS      Channel = "sms"
)

// StepMode distinguishes what the engine does from what a human does.
//
// The distinction is load-bearing rather than cosmetic: LinkedIn steps are
// structurally restricted to ManualTask, because automating them violates
// LinkedIn's User Agreement and the asset at risk is the rep's personal
// account. Making it a type-level constraint means nobody can configure
// their way into a ban.
type StepMode string

const (
	ModeAutomatic  StepMode = "automatic"
	ModeManualTask StepMode = "manual_task"
)

// Step is one touch in a sequence.
type Step struct {
	Index      int
	Channel    Channel
	Mode       StepMode
	DelayAfter time.Duration
	TemplateID string
}

// Sequence is an ordered set of steps plus its settings.
type Sequence struct {
	ID       string
	Name     string
	Steps    []Step
	Settings SequenceSettings
}

// SequenceSettings are the per-sequence knobs.
type SequenceSettings struct {
	// DomainID is the sending domain. Never the primary domain; the
	// validator refuses it (deliverability.Domains).
	DomainID string
	// Window is the local business-hours window sends are allowed in.
	Window Window
	// PauseWholeAccountOnBooking defaults true. Continuing to cold-email
	// three colleagues after one books a meeting is the most visible
	// failure a sequencer can produce.
	PauseWholeAccountOnBooking bool
}

// EnrollmentState is where a contact sits in a sequence.
//
// The distinction between paused, stopped and finished is user-visible and
// therefore has to be precise:
//
//	paused   - stop, remember the position, resumable
//	stopped  - terminal, requires re-enrollment
//	finished - completed all steps normally
//
// A meeting booking is a *pause* with a cooldown expiry, not a stop: if the
// meeting falls through you want to resume from step 3 rather than
// starting over.
type EnrollmentState string

const (
	StateActive     EnrollmentState = "active"
	StatePaused     EnrollmentState = "paused"
	StateReplied    EnrollmentState = "replied"
	StateBooked     EnrollmentState = "booked"
	StateFinished   EnrollmentState = "finished"
	StateBounced    EnrollmentState = "bounced"
	StateSuppressed EnrollmentState = "suppressed"
	StateStopped    EnrollmentState = "stopped"
)

// Terminal reports whether a state can still produce sends.
func (s EnrollmentState) Terminal() bool {
	switch s {
	case StateFinished, StateBounced, StateSuppressed, StateStopped, StateReplied:
		return true
	default:
		return false
	}
}

// Enrollment is one contact's progress through one sequence.
type Enrollment struct {
	ID          string
	SequenceID  string
	ContactID   string
	AccountID   string
	MailboxID   string
	State       EnrollmentState
	CurrentStep int
	NextSendAt  time.Time
	PausedUntil time.Time
	PauseReason PauseReason
	Timezone    string
}

// PauseReason records why and on what evidence, because a pause a human
// cannot explain is a pause a human will simply clear.
type PauseReason struct {
	Kind      string
	Evidence  string
	ExpiresAt time.Time
}

// Window is a local business-hours sending window.
type Window struct {
	StartHour int
	EndHour   int
	// JitterMax randomises the send time within the window. Sends landing
	// at exactly 09:00:00.000 across hundreds of recipients is a machine
	// signature, so this is not decoration.
	JitterMax time.Duration
}

// DefaultWindow is 9am-5pm local with up to 30 minutes of jitter.
func DefaultWindow() Window {
	return Window{StartHour: 9, EndHour: 17, JitterMax: 30 * time.Minute}
}

// Valid reports whether the window is usable.
func (w Window) Valid() error {
	if w.StartHour < 0 || w.StartHour > 23 {
		return fmt.Errorf("window: start hour %d out of range", w.StartHour)
	}
	if w.EndHour < 1 || w.EndHour > 24 {
		return fmt.Errorf("window: end hour %d out of range", w.EndHour)
	}
	if w.EndHour <= w.StartHour {
		return fmt.Errorf("window: end hour %d is not after start hour %d", w.EndHour, w.StartHour)
	}
	if w.JitterMax < 0 {
		return fmt.Errorf("window: negative jitter")
	}
	// Jitter must fit inside the window or a send can be pushed past the
	// end of business hours -- see scheduler.NextSendTime.
	if w.JitterMax >= time.Duration(w.EndHour-w.StartHour)*time.Hour {
		return fmt.Errorf("window: jitter %v does not fit in a %d-hour window", w.JitterMax, w.EndHour-w.StartHour)
	}
	return nil
}

// Contact is a prospect.
type Contact struct {
	ID        string
	AccountID string
	Email     string
	Phone     string
	Timezone  string
	// Country drives jurisdiction gating. The recipient's location
	// determines which law applies, which is why it is a first-class field
	// and not a settings page.
	Country string
}

// Account is a company. Touch limits and meeting pauses apply here, not to
// individual contacts.
type Account struct {
	ID     string
	Name   string
	Domain string
}

// Provider is a mailbox provider.
type Provider string

const (
	ProviderGmail     Provider = "gmail"
	ProviderMicrosoft Provider = "microsoft"
)

// MailboxHealth is a mailbox's current standing.
type MailboxHealth string

const (
	HealthGood     MailboxHealth = "good"
	HealthWarning  MailboxHealth = "warning"
	HealthDegraded MailboxHealth = "degraded"
	HealthHalted   MailboxHealth = "halted"
)

// Mailbox is one sending identity.
//
// Sends go through the user's own mailbox rather than an ESP, for two
// reasons: cold outbound from a shared ESP IP pool is a reputation
// disaster for everyone on the pool, and most transactional ESP
// acceptable-use policies prohibit cold outreach outright.
type Mailbox struct {
	ID              string
	UserID          string
	Provider        Provider
	Address         string
	Domain          string
	DailyCap        int
	WarmupStartedAt *time.Time
	WarmupDay       int
	HealthState     MailboxHealth
	// MinInterval is the floor between two sends from this mailbox.
	// Forty emails in ninety seconds from one mailbox is not human.
	MinInterval time.Duration
}

// NormalizeEmail lowercases and trims an address for suppression matching.
//
// Deliberately conservative: it does NOT strip Gmail's dots or
// plus-addressing. That over-normalisation is tempting and wrong, because
// the equivalence rules differ per provider -- dots are significant at most
// hosts and insignificant at Gmail -- so applying Gmail's rules globally
// would collapse two genuinely different people into one suppression
// entry, and silently stop mail to someone who never opted out.
//
// The narrower risk it accepts is the reverse: a Gmail user who unsubscribes
// as foo.bar@gmail.com could still be reached at foobar@gmail.com. That is
// handled explicitly by GmailVariants, applied only to known Gmail domains.
func NormalizeEmail(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}

var gmailDomains = map[string]bool{
	"gmail.com":      true,
	"googlemail.com": true,
}

// GmailVariants returns the addresses that reach the same Gmail inbox.
//
// Gmail ignores dots in the local part and everything after a '+'. A
// suppression stored for one variant must apply to all of them, or an
// unsubscribe is trivially circumvented by a differently-formatted import
// of the same list.
//
// Returns just the normalised address for non-Gmail domains, where these
// rules do not hold.
func GmailVariants(addr string) []string {
	norm := NormalizeEmail(addr)
	at := strings.LastIndex(norm, "@")
	if at < 0 {
		return []string{norm}
	}
	local, domain := norm[:at], norm[at+1:]
	if !gmailDomains[domain] {
		return []string{norm}
	}

	canonical := local
	if plus := strings.Index(canonical, "+"); plus >= 0 {
		canonical = canonical[:plus]
	}
	canonical = strings.ReplaceAll(canonical, ".", "")
	if canonical == "" {
		// An address that is nothing but dots and a tag is malformed;
		// returning an empty local part would make it match everything.
		return []string{norm}
	}

	out := []string{norm}
	for _, d := range []string{"gmail.com", "googlemail.com"} {
		v := canonical + "@" + d
		if v != norm {
			out = append(out, v)
		}
	}
	return out
}

// CanonicalEmail returns the single address a suppression should be keyed
// on, collapsing Gmail variants.
//
// Computed directly rather than read off GmailVariants by position.
// Positional inference is subtly wrong for an address that is ALREADY
// canonical: GmailVariants("firstlast@gmail.com") skips the identical
// gmail.com entry, so the second element is the googlemail.com form, while
// GmailVariants("first.last@gmail.com") puts "firstlast@gmail.com" there.
// Keying on element two therefore files those two addresses -- the same
// inbox -- under different keys, and an unsubscribe from one does not stop
// mail to the other.
func CanonicalEmail(addr string) string {
	norm := NormalizeEmail(addr)
	at := strings.LastIndex(norm, "@")
	if at < 0 {
		return norm
	}
	local, domain := norm[:at], norm[at+1:]
	if !gmailDomains[domain] {
		return norm
	}
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	local = strings.ReplaceAll(local, ".", "")
	if local == "" {
		// Nothing but dots and a tag: malformed, and an empty local part
		// would collapse every such address onto one key.
		return norm
	}
	// gmail.com is the canonical domain: googlemail.com is an alias for
	// the same mailbox, so both must produce the same key.
	return local + "@gmail.com"
}
