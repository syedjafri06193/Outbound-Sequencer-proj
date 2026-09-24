package sequence

import (
	"fmt"
	"sort"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

// Backpressure.
//
// When the queue cannot drain within the sending window there are two
// options and only one is correct:
//
//	compress - send faster to fit. Exceeds rate limits, looks robotic,
//	           damages reputation.
//	spill    - defer to tomorrow. Sequence timing slips.
//
// Always spill. Sequence timing is a preference; reputation is the
// business.
//
// And surface it loudly. "247 sends deferred to tomorrow -- you are
// enrolling faster than your mailbox capacity can deliver" is the product
// doing its job: telling the operator that the capacity arithmetic does not
// work for their plan.

// PendingSend is one queued step awaiting dispatch.
type PendingSend struct {
	EnrollmentID string
	MailboxID    string
	DomainID     string
	ScheduledAt  time.Time
	Priority     int
}

// Capacity is what a mailbox and its domain can absorb today.
type Capacity struct {
	MailboxID string
	DomainID  string
	// MailboxRemaining is today's warmup or daily cap minus what has
	// already gone out.
	MailboxRemaining int
	// DomainRemaining is the domain's daily cap minus today's volume.
	DomainRemaining int
	// ThrottleFactor comes from the circuit breaker; 0 means halted.
	ThrottleFactor float64
}

// Effective is what this mailbox may actually send now.
func (c Capacity) Effective() int {
	n := c.MailboxRemaining
	if c.DomainRemaining < n {
		n = c.DomainRemaining
	}
	if c.ThrottleFactor <= 0 {
		return 0
	}
	if c.ThrottleFactor < 1 {
		n = int(float64(n) * c.ThrottleFactor)
	}
	if n < 0 {
		return 0
	}
	return n
}

// Plan is the outcome of applying capacity to a queue.
type Plan struct {
	Send     []PendingSend
	Deferred []PendingSend
	// Notices are operator-facing messages. Deferral is not an error and
	// not a silent event: it is the arithmetic becoming visible.
	Notices []string
}

// Apply assigns as much of the queue as capacity allows and defers the rest.
//
// Never compresses: the returned Send slice is always within capacity, and
// everything beyond it lands in Deferred with a notice explaining why.
func Apply(queue []PendingSend, caps map[string]Capacity) Plan {
	// Oldest first, then by priority. Deterministic ordering matters:
	// without it, which sends get deferred varies run to run, and a
	// prospect can sit at the back of the queue indefinitely through pure
	// map-iteration luck.
	sorted := append([]PendingSend(nil), queue...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].ScheduledAt.Equal(sorted[j].ScheduledAt) {
			return sorted[i].ScheduledAt.Before(sorted[j].ScheduledAt)
		}
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority > sorted[j].Priority
		}
		return sorted[i].EnrollmentID < sorted[j].EnrollmentID
	})

	remaining := make(map[string]int, len(caps))
	for id, c := range caps {
		remaining[id] = c.Effective()
	}

	var p Plan
	deferredByMailbox := make(map[string]int)
	haltedByMailbox := make(map[string]bool)

	for _, s := range sorted {
		c, known := caps[s.MailboxID]
		if !known {
			// An unknown mailbox is deferred rather than sent. Sending
			// against capacity nobody could verify is exactly the
			// behaviour the limits exist to prevent.
			p.Deferred = append(p.Deferred, s)
			deferredByMailbox[s.MailboxID]++
			continue
		}
		if remaining[s.MailboxID] <= 0 {
			p.Deferred = append(p.Deferred, s)
			deferredByMailbox[s.MailboxID]++
			if c.ThrottleFactor <= 0 {
				haltedByMailbox[s.MailboxID] = true
			}
			continue
		}
		remaining[s.MailboxID]--
		p.Send = append(p.Send, s)
	}

	for mb, n := range deferredByMailbox {
		if haltedByMailbox[mb] {
			p.Notices = append(p.Notices, fmt.Sprintf(
				"%d sends held for mailbox %s: its domain is halted by the complaint budget", n, mb))
			continue
		}
		p.Notices = append(p.Notices, fmt.Sprintf(
			"%d sends deferred to tomorrow for mailbox %s - you are enrolling faster than your mailbox capacity can deliver",
			n, mb))
	}
	sort.Strings(p.Notices)
	return p
}

// SpillEstimate projects how long a backlog takes to clear.
//
// The number that tells an operator their plan does not work. A backlog
// that never clears is the interesting case, and reporting "infinite" is
// more useful than a large finite number.
type SpillEstimate struct {
	Backlog        int
	DailyCapacity  int
	DailyInflow    int
	DaysToClear    int
	NeverClears    bool
	Recommendation string
}

func EstimateSpill(backlog, dailyCapacity, dailyInflow int) SpillEstimate {
	e := SpillEstimate{Backlog: backlog, DailyCapacity: dailyCapacity, DailyInflow: dailyInflow}

	drain := dailyCapacity - dailyInflow
	if drain <= 0 {
		e.NeverClears = true
		needed := dailyInflow - dailyCapacity
		e.Recommendation = fmt.Sprintf(
			"backlog never clears: %d sends arrive daily against %d capacity. Add mailboxes for %d more sends a day, or enrol fewer prospects.",
			dailyInflow, dailyCapacity, needed)
		return e
	}

	e.DaysToClear = backlog / drain
	if backlog%drain != 0 {
		e.DaysToClear++
	}
	e.Recommendation = fmt.Sprintf("backlog of %d clears in %d days at %d/day net drain",
		backlog, e.DaysToClear, drain)
	return e
}

// EnrollmentAdmission decides whether a new enrolment should be accepted.
//
// Refusing at enrolment is kinder than accepting and deferring forever: an
// operator who enrols 5,000 prospects into a sequence with one mailbox
// should be told at the point of the decision.
type EnrollmentAdmission struct {
	Allowed bool
	Reason  string
}

func AdmitEnrollment(pendingBacklog, dailyCapacity, maxBacklogDays int) EnrollmentAdmission {
	if dailyCapacity <= 0 {
		return EnrollmentAdmission{false, "no sending capacity: no warmed mailbox is available"}
	}
	maxBacklog := dailyCapacity * maxBacklogDays
	if pendingBacklog >= maxBacklog {
		return EnrollmentAdmission{false, fmt.Sprintf(
			"backlog of %d is already %d days of capacity (limit %d days); add mailboxes or wait for the queue to drain",
			pendingBacklog, pendingBacklog/dailyCapacity, maxBacklogDays)}
	}
	return EnrollmentAdmission{Allowed: true}
}

// Compile-time reminder that a Window must be valid before scheduling.
var _ = model.Window{}
