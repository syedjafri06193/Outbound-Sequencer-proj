// Package governance enforces account-level touch limits.
//
// Without it, four reps working the same target account each enrol two
// contacts, and the prospect receives eight cold emails in a week from one
// company. That is a complaint, and it is entirely self-inflicted.
//
// This is one of the highest-value features in the product and almost
// nobody builds it, because it makes individual reps' numbers look worse
// while making the company's outcomes better.
package governance

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Policy is the per-account limit set.
type Policy struct {
	// MaxTouchesPerWeek counts across ALL reps and sequences, which is
	// the entire point. A per-rep limit does not solve the problem it
	// appears to solve.
	MaxTouchesPerWeek int
	// MaxContactsInFlight caps simultaneous enrolments at one account.
	MaxContactsInFlight  int
	CooldownAfterReply   time.Duration
	CooldownAfterBooking time.Duration
}

func DefaultPolicy() Policy {
	return Policy{
		MaxTouchesPerWeek:    3,
		MaxContactsInFlight:  2,
		CooldownAfterReply:   14 * 24 * time.Hour,
		CooldownAfterBooking: 30 * 24 * time.Hour,
	}
}

// Touch is one outbound contact with an account.
type Touch struct {
	AccountID string
	ContactID string
	RepID     string
	Channel   string
	At        time.Time
}

// Cooldown suppresses an account until a moment.
type Cooldown struct {
	AccountID string
	Until     time.Time
	Reason    string
}

// Ledger tracks touches and cooldowns per account.
type Ledger struct {
	mu        sync.RWMutex
	policy    Policy
	touches   map[string][]Touch
	inFlight  map[string]map[string]bool // account -> contact set
	cooldowns map[string]Cooldown
	now       func() time.Time
}

func NewLedger(p Policy) *Ledger {
	return &Ledger{
		policy:    p,
		touches:   make(map[string][]Touch),
		inFlight:  make(map[string]map[string]bool),
		cooldowns: make(map[string]Cooldown),
		now:       time.Now,
	}
}

func (l *Ledger) SetClock(f func() time.Time) { l.now = f }

// Decision is the outcome of a governance check.
type Decision struct {
	Allowed bool
	Reason  string
}

// CheckSend is called at send time, after enrolment.
//
// Re-checked rather than trusted from enrolment: three other reps may have
// enrolled contacts at this account since, and the whole failure mode is
// the aggregate.
func (l *Ledger) CheckSend(accountID string) Decision {
	now := l.now()

	l.mu.RLock()
	defer l.mu.RUnlock()

	if cd, ok := l.cooldowns[accountID]; ok && now.Before(cd.Until) {
		return Decision{false, fmt.Sprintf("account in cooldown until %s: %s",
			cd.Until.Format(time.RFC3339), cd.Reason)}
	}

	weekAgo := now.Add(-7 * 24 * time.Hour)
	n := 0
	for _, t := range l.touches[accountID] {
		if t.At.After(weekAgo) {
			n++
		}
	}
	if n >= l.policy.MaxTouchesPerWeek {
		return Decision{false, fmt.Sprintf(
			"account has had %d touches in the last week across all reps (limit %d)",
			n, l.policy.MaxTouchesPerWeek)}
	}
	return Decision{Allowed: true}
}

// CheckEnroll is called before adding a contact to a sequence.
func (l *Ledger) CheckEnroll(accountID, contactID string) Decision {
	if d := l.CheckSend(accountID); !d.Allowed {
		return d
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	set := l.inFlight[accountID]
	if set[contactID] {
		// Already enrolled. Not an error worth failing loudly on, but it
		// must not count twice toward the in-flight limit.
		return Decision{Allowed: true}
	}
	if len(set) >= l.policy.MaxContactsInFlight {
		return Decision{false, fmt.Sprintf(
			"account already has %d contacts enrolled across all reps (limit %d)",
			len(set), l.policy.MaxContactsInFlight)}
	}
	return Decision{Allowed: true}
}

// RecordTouch logs an outbound contact.
func (l *Ledger) RecordTouch(t Touch) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.touches[t.AccountID] = append(l.touches[t.AccountID], t)
}

// Enroll marks a contact as in flight at an account.
func (l *Ledger) Enroll(accountID, contactID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[accountID] == nil {
		l.inFlight[accountID] = make(map[string]bool)
	}
	l.inFlight[accountID][contactID] = true
}

// Unenroll clears a contact from the in-flight set.
func (l *Ledger) Unenroll(accountID, contactID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.inFlight[accountID], contactID)
	if len(l.inFlight[accountID]) == 0 {
		delete(l.inFlight, accountID)
	}
}

// StartCooldown suppresses an account for a period.
func (l *Ledger) StartCooldown(accountID string, d time.Duration, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	until := l.now().Add(d)
	// Extend rather than replace: a booking cooldown must not be shortened
	// by a later reply cooldown that happens to be briefer.
	if existing, ok := l.cooldowns[accountID]; ok && existing.Until.After(until) {
		return
	}
	l.cooldowns[accountID] = Cooldown{AccountID: accountID, Until: until, Reason: reason}
}

// ClearCooldown removes a cooldown, for an operator override.
func (l *Ledger) ClearCooldown(accountID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.cooldowns, accountID)
}

// TouchesInWeek is the current count, for the UI.
func (l *Ledger) TouchesInWeek(accountID string) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	weekAgo := l.now().Add(-7 * 24 * time.Hour)
	n := 0
	for _, t := range l.touches[accountID] {
		if t.At.After(weekAgo) {
			n++
		}
	}
	return n
}

// InFlight is the current enrolment count at an account.
func (l *Ledger) InFlight(accountID string) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.inFlight[accountID])
}

// Prune drops touches older than the window, so the ledger does not grow
// without bound in a long-running process.
func (l *Ledger) Prune(olderThan time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := l.now().Add(-olderThan)
	removed := 0
	for acct, ts := range l.touches {
		kept := ts[:0]
		for _, t := range ts {
			if t.At.After(cutoff) {
				kept = append(kept, t)
			} else {
				removed++
			}
		}
		if len(kept) == 0 {
			delete(l.touches, acct)
			continue
		}
		l.touches[acct] = kept
	}
	return removed
}

// BusiestAccounts reports where the touch budget is being spent.
func (l *Ledger) BusiestAccounts(n int) []AccountTouches {
	l.mu.RLock()
	defer l.mu.RUnlock()

	weekAgo := l.now().Add(-7 * 24 * time.Hour)
	out := make([]AccountTouches, 0, len(l.touches))
	for acct, ts := range l.touches {
		c := 0
		reps := make(map[string]bool)
		for _, t := range ts {
			if t.At.After(weekAgo) {
				c++
				reps[t.RepID] = true
			}
		}
		if c > 0 {
			out = append(out, AccountTouches{AccountID: acct, Touches: c, Reps: len(reps)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Touches != out[j].Touches {
			return out[i].Touches > out[j].Touches
		}
		return out[i].AccountID < out[j].AccountID
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

type AccountTouches struct {
	AccountID string
	Touches   int
	// Reps is how many different people touched this account. More than
	// one is the signal that the coordination problem is real here.
	Reps int
}
