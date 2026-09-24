// Package suppression is the most important data structure in the system.
//
// An unsubscribe applies to every sequence from every rep, not just the one
// it came from. Per-sequence opt-out is how a person unsubscribes three
// times and then files a complaint.
//
// Suppression records outlive contact records, list imports and CRM
// deletions. If a contact is deleted and re-imported, the suppression must
// still apply -- which is why entries are keyed on the normalised address
// rather than on a contact id.
package suppression

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

// Reason is why an address is suppressed.
type Reason string

const (
	Unsubscribed        Reason = "unsubscribed"
	SpamComplaint       Reason = "spam_complaint"
	HardBounce          Reason = "hard_bounce"
	DoNotContactCRM     Reason = "crm_dnc"
	ExistingCustomer    Reason = "existing_customer"
	OpenOpportunity     Reason = "open_opportunity"
	Competitor          Reason = "competitor"
	RecentlyContacted   Reason = "recently_contacted"
	JurisdictionBlocked Reason = "jurisdiction_blocked"
	HostileReply        Reason = "hostile_reply"
	ManualSuppression   Reason = "manual"
)

// Permanent reports whether a reason can ever expire.
//
// The distinction matters: "recently contacted" is a cooldown and should
// lapse, while an unsubscribe or a spam complaint is forever. Getting this
// backwards in either direction is a serious bug -- an expiring
// unsubscribe is a CAN-SPAM violation, and a permanent cooldown quietly
// removes a prospect from the database.
func (r Reason) Permanent() bool {
	switch r {
	case Unsubscribed, SpamComplaint, HardBounce, HostileReply, DoNotContactCRM:
		return true
	default:
		return false
	}
}

// Entry is one suppression record.
type Entry struct {
	Email  string
	Reason Reason
	// Source records where it came from, so an operator can answer "why
	// can't I email this person?" without archaeology.
	Source    string
	CreatedAt time.Time
	// ExpiresAt is zero for permanent reasons.
	ExpiresAt time.Time
	Note      string
}

// Active reports whether the entry currently blocks sending.
func (e Entry) Active(now time.Time) bool {
	if e.Reason.Permanent() {
		return true
	}
	if e.ExpiresAt.IsZero() {
		return true
	}
	return now.Before(e.ExpiresAt)
}

// List is the suppression store.
//
// Safe for concurrent use: the send path checks it on every dispatch while
// the unsubscribe endpoint and reply watcher write to it.
type List struct {
	mu sync.RWMutex
	// byEmail is keyed on the canonical address, so Gmail dot and
	// plus-address variants collapse to one entry.
	byEmail map[string][]Entry
	now     func() time.Time
}

func New() *List {
	return &List{byEmail: make(map[string][]Entry), now: time.Now}
}

// SetClock is for tests.
func (l *List) SetClock(f func() time.Time) { l.now = f }

// Add records a suppression.
//
// Never removes or overwrites: an address can accumulate several reasons,
// and the history is the audit trail that answers a regulator's question.
func (l *List) Add(email string, reason Reason, source, note string) Entry {
	key := model.CanonicalEmail(email)
	e := Entry{
		Email:     model.NormalizeEmail(email),
		Reason:    reason,
		Source:    source,
		Note:      note,
		CreatedAt: l.now(),
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.byEmail[key] = append(l.byEmail[key], e)
	return e
}

// AddWithExpiry records a temporary suppression, such as a cooldown.
//
// Refuses to put an expiry on a permanent reason. An unsubscribe that
// lapses after ninety days is a CAN-SPAM violation with a timer on it.
func (l *List) AddWithExpiry(email string, reason Reason, source, note string, expires time.Time) (Entry, error) {
	if reason.Permanent() {
		return Entry{}, fmt.Errorf("suppression: %s is permanent and cannot be given an expiry", reason)
	}
	key := model.CanonicalEmail(email)
	e := Entry{
		Email:     model.NormalizeEmail(email),
		Reason:    reason,
		Source:    source,
		Note:      note,
		CreatedAt: l.now(),
		ExpiresAt: expires,
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.byEmail[key] = append(l.byEmail[key], e)
	return e, nil
}

// Check returns the active suppression blocking this address, if any.
//
// Permanent reasons are reported in preference to temporary ones, because
// "unsubscribed" and "contacted last week" call for entirely different
// operator responses.
func (l *List) Check(email string) *Entry {
	key := model.CanonicalEmail(email)

	l.mu.RLock()
	entries := l.byEmail[key]
	l.mu.RUnlock()
	if len(entries) == 0 {
		return nil
	}

	now := l.now()
	var temporary *Entry
	for i := range entries {
		if !entries[i].Active(now) {
			continue
		}
		if entries[i].Reason.Permanent() {
			e := entries[i]
			return &e
		}
		if temporary == nil {
			e := entries[i]
			temporary = &e
		}
	}
	return temporary
}

// Suppressed is the boolean form.
func (l *List) Suppressed(email string) bool { return l.Check(email) != nil }

// History returns every record for an address, newest first.
func (l *List) History(email string) []Entry {
	key := model.CanonicalEmail(email)
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := append([]Entry(nil), l.byEmail[key]...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Count is the number of distinct suppressed addresses.
func (l *List) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.byEmail)
}

// FilterImport partitions a list of addresses at import time.
//
// The first of the three checkpoints. Catching suppressed addresses here
// saves the operator from an import that looks larger than it is.
func (l *List) FilterImport(emails []string) (allowed []string, blocked map[string]Reason) {
	blocked = make(map[string]Reason)
	for _, e := range emails {
		if s := l.Check(e); s != nil {
			blocked[model.NormalizeEmail(e)] = s.Reason
			continue
		}
		allowed = append(allowed, e)
	}
	return allowed, blocked
}

// Stats summarises the list by reason.
type Stats struct {
	Total    int
	ByReason map[Reason]int
}

func (l *List) Stats() Stats {
	l.mu.RLock()
	defer l.mu.RUnlock()

	s := Stats{ByReason: make(map[Reason]int)}
	now := l.now()
	for _, entries := range l.byEmail {
		counted := false
		for _, e := range entries {
			if !e.Active(now) {
				continue
			}
			s.ByReason[e.Reason]++
			counted = true
		}
		if counted {
			s.Total++
		}
	}
	return s
}
