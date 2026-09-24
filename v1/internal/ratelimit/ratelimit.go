// Package ratelimit enforces the three caps and the inter-send interval.
//
//	Mailbox          warmup cap, or 30-50/day   behavioural plausibility
//	Domain           below 5,000/day            bulk classification is permanent
//	Account          N touches per company/week the "five reps emailed me" problem
//
// All three are enforced, and the tightest one wins. Enforcing only the
// mailbox cap is the common shortcut and it fails in both directions: ten
// warmed mailboxes on one domain sail past 5,000/day into permanent bulk
// classification, and five reps each within their own cap still land five
// emails on one company in a week.
package ratelimit

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Level names which cap refused a send.
type Level string

const (
	LevelNone     Level = ""
	LevelMailbox  Level = "mailbox"
	LevelDomain   Level = "domain"
	LevelAccount  Level = "account"
	LevelInterval Level = "interval"
)

// Reason is the operator-facing explanation.
type Reason struct {
	Level Level
	// RetryAfter is when the send could next be attempted. Zero means
	// not before the next window.
	RetryAfter time.Duration
	Message    string
}

// Limits are the configured caps.
type Limits struct {
	// MailboxDaily is the warmup cap or the cold ceiling, whichever is
	// lower. 30-50 for cold; above that a mailbox stops looking like a
	// person.
	MailboxDaily int
	// DomainDaily is held below the 5,000/day Gmail bulk threshold by
	// default, because crossing it once is permanent.
	DomainDaily int
	// AccountWeekly is touches per recipient company per week across
	// every rep.
	AccountWeekly int
	// MinInterval is the floor between two sends from one mailbox.
	// Forty emails in ninety seconds is not human.
	MinInterval time.Duration
}

// DefaultLimits are the conservative defaults.
func DefaultLimits() Limits {
	return Limits{
		MailboxDaily: 40,
		// 4,500, not 5,000. The threshold is a cliff with no way back,
		// and a counter that races a send in flight should not be the
		// thing standing between a domain and permanent bulk
		// classification.
		DomainDaily:   4500,
		AccountWeekly: 3,
		MinInterval:   90 * time.Second,
	}
}

// BulkThreshold is the Gmail bulk-sender line. Crossing it once, ever,
// subjects the domain to the stricter rules permanently.
const BulkThreshold = 5000

// Validate refuses a configuration that cannot be safe.
func (l Limits) Validate() error {
	if l.MailboxDaily <= 0 {
		return fmt.Errorf("ratelimit: mailbox daily cap must be positive")
	}
	if l.MailboxDaily > 100 {
		return fmt.Errorf("ratelimit: mailbox daily cap of %d is not behaviourally plausible for cold outbound; add mailboxes instead", l.MailboxDaily)
	}
	if l.DomainDaily >= BulkThreshold {
		return fmt.Errorf("ratelimit: domain daily cap of %d reaches the %d/day bulk-sender threshold; crossing it once classifies the domain permanently",
			l.DomainDaily, BulkThreshold)
	}
	if l.AccountWeekly <= 0 {
		return fmt.Errorf("ratelimit: account weekly cap must be positive")
	}
	if l.MinInterval < 0 {
		return fmt.Errorf("ratelimit: negative minimum interval")
	}
	return nil
}

type counter struct {
	day   string
	count int
}

type mailboxState struct {
	daily    counter
	lastSend time.Time
}

// Limiter enforces the caps.
//
// Safe for concurrent use by every sending worker. The check and the
// increment are one operation under one lock: splitting them lets two
// workers both read 39 against a cap of 40 and both send.
type Limiter struct {
	mu       sync.Mutex
	limits   Limits
	mailbox  map[string]*mailboxState
	domain   map[string]*counter
	accounts map[string][]time.Time
	// perMailbox overrides the global mailbox cap, for warmup.
	perMailbox map[string]int
}

func New(l Limits) *Limiter {
	return &Limiter{
		limits:     l,
		mailbox:    make(map[string]*mailboxState),
		domain:     make(map[string]*counter),
		accounts:   make(map[string][]time.Time),
		perMailbox: make(map[string]int),
	}
}

// SetMailboxCap applies a warmup cap to one mailbox.
//
// Zero means the mailbox may not send at all, which is what an unstarted
// or halted warmup returns. A zero cap is honoured rather than treated as
// "unset": the alternative silently promotes a halted mailbox to the
// global default.
func (l *Limiter) SetMailboxCap(mailboxID string, cap int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cap < 0 {
		cap = 0
	}
	l.perMailbox[mailboxID] = cap
}

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (l *Limiter) mailboxCap(mailboxID string) int {
	if c, ok := l.perMailbox[mailboxID]; ok {
		if c < l.limits.MailboxDaily {
			return c
		}
		return l.limits.MailboxDaily
	}
	return l.limits.MailboxDaily
}

// Allow checks every cap and, if all pass, records the send.
//
// One call, one lock, check-and-commit together. Returning a "you may
// send" that the caller then has to report back is the shape that lets two
// workers both pass.
func (l *Limiter) Allow(mailboxID, domainID, accountID string, now time.Time) (bool, Reason) {
	l.mu.Lock()
	defer l.mu.Unlock()

	today := dayKey(now)

	mb := l.mailbox[mailboxID]
	if mb == nil {
		mb = &mailboxState{}
		l.mailbox[mailboxID] = mb
	}
	if mb.daily.day != today {
		mb.daily = counter{day: today}
	}

	// Interval first: it is the cheapest to check and the most likely to
	// trip in a burst.
	if l.limits.MinInterval > 0 && !mb.lastSend.IsZero() {
		if since := now.Sub(mb.lastSend); since < l.limits.MinInterval {
			wait := l.limits.MinInterval - since
			return false, Reason{
				Level:      LevelInterval,
				RetryAfter: wait,
				Message: fmt.Sprintf("mailbox %s sent %v ago; the minimum interval is %v. Forty emails in ninety seconds from one mailbox is not human.",
					mailboxID, since.Truncate(time.Second), l.limits.MinInterval),
			}
		}
	}

	if cap := l.mailboxCap(mailboxID); mb.daily.count >= cap {
		return false, Reason{
			Level:   LevelMailbox,
			Message: fmt.Sprintf("mailbox %s has sent %d today against a cap of %d", mailboxID, mb.daily.count, cap),
		}
	}

	dom := l.domain[domainID]
	if dom == nil {
		dom = &counter{}
		l.domain[domainID] = dom
	}
	if dom.day != today {
		*dom = counter{day: today}
	}
	if dom.count >= l.limits.DomainDaily {
		return false, Reason{
			Level: LevelDomain,
			Message: fmt.Sprintf("domain %s has sent %d today against a cap of %d, held below the %d/day bulk-sender threshold because crossing it is permanent",
				domainID, dom.count, l.limits.DomainDaily, BulkThreshold),
		}
	}

	// Account-level, across every rep and sequence. This is the cap that
	// catches the failure a prospect actually notices.
	if accountID != "" {
		week := now.AddDate(0, 0, -7)
		touches := l.accounts[accountID]
		live := touches[:0:0]
		for _, t := range touches {
			if t.After(week) {
				live = append(live, t)
			}
		}
		l.accounts[accountID] = live
		if len(live) >= l.limits.AccountWeekly {
			oldest := live[0]
			for _, t := range live {
				if t.Before(oldest) {
					oldest = t
				}
			}
			return false, Reason{
				Level:      LevelAccount,
				RetryAfter: oldest.AddDate(0, 0, 7).Sub(now),
				Message: fmt.Sprintf("account %s has had %d touches in the last seven days across all reps, against a limit of %d",
					accountID, len(live), l.limits.AccountWeekly),
			}
		}
	}

	// Commit.
	mb.daily.count++
	mb.lastSend = now
	dom.count++
	if accountID != "" {
		l.accounts[accountID] = append(l.accounts[accountID], now)
	}
	return true, Reason{}
}

// Remaining reports headroom without consuming any.
type Remaining struct {
	Mailbox       int
	Domain        int
	AccountWeek   int
	NextAllowedAt time.Time
}

func (l *Limiter) Remaining(mailboxID, domainID, accountID string, now time.Time) Remaining {
	l.mu.Lock()
	defer l.mu.Unlock()

	today := dayKey(now)
	r := Remaining{}

	cap := l.mailboxCap(mailboxID)
	if mb := l.mailbox[mailboxID]; mb != nil {
		used := 0
		if mb.daily.day == today {
			used = mb.daily.count
		}
		r.Mailbox = cap - used
		if !mb.lastSend.IsZero() {
			r.NextAllowedAt = mb.lastSend.Add(l.limits.MinInterval)
		}
	} else {
		r.Mailbox = cap
	}
	if r.Mailbox < 0 {
		r.Mailbox = 0
	}

	r.Domain = l.limits.DomainDaily
	if d := l.domain[domainID]; d != nil && d.day == today {
		r.Domain -= d.count
	}
	if r.Domain < 0 {
		r.Domain = 0
	}

	r.AccountWeek = l.limits.AccountWeekly
	if accountID != "" {
		week := now.AddDate(0, 0, -7)
		n := 0
		for _, t := range l.accounts[accountID] {
			if t.After(week) {
				n++
			}
		}
		r.AccountWeek -= n
		if r.AccountWeek < 0 {
			r.AccountWeek = 0
		}
	}
	return r
}

// DomainVolumeToday is what the bulk-classification tracker reads.
func (l *Limiter) DomainVolumeToday(domainID string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.domain[domainID]
	if d == nil || d.day != dayKey(now) {
		return 0
	}
	return d.count
}

// Prune drops account touch history older than a week.
func (l *Limiter) Prune(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	week := now.AddDate(0, 0, -7)
	dropped := 0
	for id, touches := range l.accounts {
		live := touches[:0:0]
		for _, t := range touches {
			if t.After(week) {
				live = append(live, t)
			}
		}
		dropped += len(touches) - len(live)
		if len(live) == 0 {
			delete(l.accounts, id)
			continue
		}
		l.accounts[id] = live
	}
	return dropped
}

// Spacing spreads a day's allowance across a sending window.
//
// Used to pick send times that are not a burst at 09:00. The engine still
// jitters on top of this; the point here is the average gap, not the exact
// moment.
func Spacing(allowance int, window time.Duration, min time.Duration) time.Duration {
	if allowance <= 1 {
		return window
	}
	gap := window / time.Duration(allowance)
	if gap < min {
		return min
	}
	return gap
}

// CapacityShortfall reports whether the plan fits the caps at all.
type CapacityShortfall struct {
	Required        int
	Available       int
	MailboxesNeeded int
	Message         string
}

// PlanFits works out whether a day's send plan can be delivered.
func PlanFits(dailySends int, mailboxes []string, l Limits, perMailboxCap map[string]int) CapacityShortfall {
	available := 0
	for _, mb := range mailboxes {
		cap := l.MailboxDaily
		if c, ok := perMailboxCap[mb]; ok && c < cap {
			cap = c
		}
		available += cap
	}
	s := CapacityShortfall{Required: dailySends, Available: available}
	if available >= dailySends {
		s.Message = fmt.Sprintf("%d sends a day fits in %d mailboxes (%d/day available)",
			dailySends, len(mailboxes), available)
		return s
	}
	short := dailySends - available
	s.MailboxesNeeded = short / l.MailboxDaily
	if short%l.MailboxDaily != 0 {
		s.MailboxesNeeded++
	}
	s.Message = fmt.Sprintf(
		"%d sends a day against %d/day of capacity: add %d more mailboxes, or enrol fewer prospects. Raising the per-mailbox cap is not an option.",
		dailySends, available, s.MailboxesNeeded)
	return s
}

// BusiestDomains reports today's volume, for the bulk-threshold watch.
func (l *Limiter) BusiestDomains(now time.Time, n int) []DomainVolume {
	l.mu.Lock()
	defer l.mu.Unlock()

	today := dayKey(now)
	var out []DomainVolume
	for id, c := range l.domain {
		if c.day != today {
			continue
		}
		out = append(out, DomainVolume{
			DomainID: id,
			Volume:   c.count,
			// A domain at 90% of the bulk threshold is worth saying
			// something about before it crosses, not after.
			NearBulkThreshold: float64(c.count) >= 0.9*float64(BulkThreshold),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Volume != out[j].Volume {
			return out[i].Volume > out[j].Volume
		}
		return out[i].DomainID < out[j].DomainID
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// DomainVolume is one domain's day.
type DomainVolume struct {
	DomainID          string
	Volume            int
	NearBulkThreshold bool
}
