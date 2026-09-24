package deliverability

import (
	"fmt"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

// Warmup enforcement.
//
// A brand-new domain and mailbox sending fifty cold emails on day one is a
// textbook spam pattern. The ramp is enforced by the engine rather than
// suggested in the UI, because a suggestion is something an operator under
// quota pressure turns off.

// WarmupSchedule defines the ramp.
type WarmupSchedule struct {
	// Week1 is the flat cap for the first seven days.
	Week1 int
	// Week2Start and Week2PerDay define the second week's ramp.
	Week2Start  int
	Week2PerDay int
	// Week3Start and Week3PerDay cover days 14-27.
	Week3Start  int
	Week3PerDay int
	// Days is how long the ramp runs before the mailbox reaches target.
	Days int
}

// DefaultWarmup ramps from 5/day to target over four weeks.
//
// Week 2 starts at 10 rather than 5, which doubles the cap between day 6
// and day 7. That step is deliberate and is the largest in the schedule:
// the first week establishes that the mailbox exists and behaves, and the
// providers' early-reputation signal is dominated by whether anything
// bounces or gets complained about at all, not by the exact slope.
func DefaultWarmup() WarmupSchedule {
	return WarmupSchedule{
		Week1:       5,
		Week2Start:  10,
		Week2PerDay: 2,
		Week3Start:  24,
		Week3PerDay: 2,
		Days:        28,
	}
}

// Cap returns the maximum cold sends allowed today for a mailbox.
//
// Returns 0 for a mailbox with no warmup started. Not "the target", not
// "a small number" -- zero. A mailbox that has not begun warming cannot
// send cold mail at all, and making that a hard zero rather than a low
// number is what stops a freshly connected mailbox from being used the
// moment it is authorised.
func (s WarmupSchedule) Cap(m *model.Mailbox, target int) int {
	if m == nil || m.WarmupStartedAt == nil {
		return 0
	}
	// A halted mailbox sends nothing regardless of where it is in the ramp.
	if m.HealthState == model.HealthHalted {
		return 0
	}

	day := m.WarmupDay
	if day < 0 {
		day = 0
	}

	var cap int
	switch {
	case day < 7:
		cap = s.Week1
	case day < 14:
		cap = s.Week2Start + s.Week2PerDay*(day-7)
	case day < s.Days:
		cap = s.Week3Start + s.Week3PerDay*(day-14)
	default:
		cap = target
	}

	// A degraded mailbox is not a mailbox that should keep sending at full
	// volume while you investigate.
	switch m.HealthState {
	case model.HealthDegraded:
		cap = cap / 4
	case model.HealthWarning:
		cap = cap / 2
	}

	if cap > target {
		cap = target
	}
	if cap < 0 {
		cap = 0
	}
	return cap
}

// DayFor computes the warmup day from the start date.
func DayFor(startedAt time.Time, now time.Time) int {
	if now.Before(startedAt) {
		return 0
	}
	return int(now.Sub(startedAt).Hours() / 24)
}

// Advance updates a mailbox's warmup day from the clock.
func (s WarmupSchedule) Advance(m *model.Mailbox, now time.Time) {
	if m.WarmupStartedAt == nil {
		return
	}
	m.WarmupDay = DayFor(*m.WarmupStartedAt, now)
}

// HealthEvent is something that happened to a mailbox's reputation.
type HealthEvent string

const (
	EventBounceSpike    HealthEvent = "bounce_spike"
	EventComplaint      HealthEvent = "complaint"
	EventSpamFoldering  HealthEvent = "spam_foldering"
	EventAuthFailure    HealthEvent = "auth_failure"
	EventProviderReject HealthEvent = "provider_reject"
)

// ResetOnHealthEvent rolls a mailbox back down the ramp.
//
// Warmup resets on a health event rather than merely pausing, because the
// mailbox has demonstrated that its current volume is not sustainable. The
// rollback is to the start of the previous week rather than to day zero:
// a full restart punishes a single transient bounce with four weeks of
// reduced capacity, which operators respond to by disabling the feature.
func (s WarmupSchedule) ResetOnHealthEvent(m *model.Mailbox, ev HealthEvent, now time.Time) {
	switch ev {
	case EventProviderReject, EventAuthFailure:
		// The provider is actively refusing mail. Stop entirely; this is
		// not a volume problem.
		m.HealthState = model.HealthHalted
		return
	case EventBounceSpike, EventSpamFoldering:
		m.HealthState = model.HealthDegraded
	case EventComplaint:
		if m.HealthState == model.HealthGood {
			m.HealthState = model.HealthWarning
		} else {
			m.HealthState = model.HealthDegraded
		}
	}

	// Roll back one week, floored at the start of week 1 rather than at
	// day 0, so an established mailbox does not lose its whole history to
	// one bad day.
	rolled := m.WarmupDay - 7
	if rolled < 0 {
		rolled = 0
	}
	m.WarmupDay = rolled

	if m.WarmupStartedAt != nil {
		restarted := now.AddDate(0, 0, -rolled)
		m.WarmupStartedAt = &restarted
	}
}

// Recover moves a mailbox back up one health level.
func (s WarmupSchedule) Recover(m *model.Mailbox) {
	switch m.HealthState {
	case model.HealthDegraded:
		m.HealthState = model.HealthWarning
	case model.HealthWarning:
		m.HealthState = model.HealthGood
	}
}

// ColdVolumeGuidance is the per-mailbox arithmetic from the design doc.
//
// Google Workspace permits far higher daily sending, but a mailbox sending
// 500 cold emails a day does not look like a human, and provider
// heuristics are tuned on behavioural patterns as well as authentication.
const (
	ColdSafePerMailbox    = 50
	ColdWarningPerMailbox = 100
)

// CapacityPlan is the arithmetic an operator needs at sequence-design time
// rather than while watching their domain burn.
type CapacityPlan struct {
	Prospects        int
	StepsPerProspect int
	BusinessDays     int
	PerMailboxPerDay int

	TotalSends      int
	SendsPerDay     int
	MailboxesNeeded int
	DomainsNeeded   int
	// Feasible is false when the plan cannot be delivered at safe volumes.
	Feasible bool
	Note     string
}

// PlanCapacity answers "how many mailboxes do I need?" before enrolment
// rather than after.
//
// The design document's worked example: 2,000 prospects, a four-step
// sequence, 21 business days, 40 sends per mailbox per day. That is 8,000
// sends against a per-mailbox capacity of 840, so ten mailboxes -- not one.
func PlanCapacity(prospects, stepsPerProspect, businessDays, perMailboxPerDay, perDomainDailyCap int) CapacityPlan {
	p := CapacityPlan{
		Prospects:        prospects,
		StepsPerProspect: stepsPerProspect,
		BusinessDays:     businessDays,
		PerMailboxPerDay: perMailboxPerDay,
	}
	if prospects <= 0 || stepsPerProspect <= 0 || businessDays <= 0 || perMailboxPerDay <= 0 {
		p.Note = "incomplete plan"
		return p
	}

	p.TotalSends = prospects * stepsPerProspect
	p.SendsPerDay = ceilDiv(p.TotalSends, businessDays)
	p.MailboxesNeeded = ceilDiv(p.SendsPerDay, perMailboxPerDay)

	if perDomainDailyCap > 0 {
		p.DomainsNeeded = ceilDiv(p.SendsPerDay, perDomainDailyCap)
	}

	p.Feasible = true
	if perMailboxPerDay > ColdWarningPerMailbox {
		p.Feasible = false
		p.Note = fmt.Sprintf(
			"%d sends per mailbox per day is above the %d/day warning threshold; a mailbox at that volume does not look like a human",
			perMailboxPerDay, ColdWarningPerMailbox)
		return p
	}

	p.Note = fmt.Sprintf(
		"%d prospects x %d steps = %d sends over %d business days = %d/day, which needs %d mailboxes at %d/day",
		prospects, stepsPerProspect, p.TotalSends, businessDays, p.SendsPerDay,
		p.MailboxesNeeded, perMailboxPerDay)
	return p
}

func ceilDiv(a, b int) int {
	if b == 0 {
		return 0
	}
	if a%b == 0 {
		return a / b
	}
	return a/b + 1
}
