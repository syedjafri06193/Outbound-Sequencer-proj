// Package store is an in-memory implementation of everything the engine
// persists.
//
// In memory because the point of this repository is the behaviour, not the
// storage engine. Every method that a real database would have to make
// atomic is atomic here under one mutex, and the tests that matter -- the
// race tests -- exercise exactly those methods. Swapping this for Postgres
// is a question of preserving the atomicity boundaries drawn here, and
// those boundaries are the design.
package store

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

// Store holds engine state.
type Store struct {
	mu sync.Mutex

	// enrollmentLocks serialise a dispatch against the events that would
	// cancel it.
	//
	// The send-time preflight narrows the §7.4 race from minutes to
	// milliseconds, and the residual milliseconds are real: the preflight
	// reads "no reply", the reply lands, and the provider then accepts a
	// message the prospect has already answered. Measured over 500
	// iterations with a 50us provider round-trip, that residual fires
	// around 80 times.
	//
	// This closes it. A dispatch holds the enrolment's lock across the
	// claim, the preflight and the provider call; a reply, a booking or
	// an unsubscribe takes the same lock before it writes. The two then
	// cannot interleave: either the event lands first and the preflight
	// sees it, or the send completes first and the event was genuinely
	// later.
	//
	// In a real deployment this is the enrolment row's SELECT ... FOR
	// UPDATE, held across the provider call. The cost is one row locked
	// for the length of an API round-trip, which at 40 sends per mailbox
	// per day is not a contention problem.
	// Two namespaces, always taken in this order: contact, then
	// enrolment. A global suppression is keyed on the address and knows
	// nothing about enrolments, while a reply or a booking is keyed on
	// the enrolment. A dispatch takes one of each, and the fixed order is
	// what keeps that from deadlocking against an account-level pause
	// holding several enrolment locks.
	lockMu          sync.Mutex
	contactLocks    map[string]*sync.Mutex
	enrollmentLocks map[string]*sync.Mutex

	contacts    map[string]model.Contact
	byEmail     map[string]string
	enrollments map[string]*model.Enrollment
	sequences   map[string]model.Sequence
	mailboxes   map[string]*model.Mailbox

	// sendKeys is the idempotency ledger. Once a key is here, that step
	// has been claimed and will never be claimed again.
	sendKeys map[string]SendClaim

	sentByMessageID  map[string]SentRecord
	sentByThreadID   map[string]SentRecord
	sentByEnrollment map[string][]SentRecord

	replied  map[string]time.Time
	bookings map[string]time.Time

	queued  map[string]map[string]bool
	skipped []SkipRecord
}

// SendClaim records a claimed send key.
type SendClaim struct {
	Key       string
	ClaimedAt time.Time
	// Completed is set once the provider accepted the message. A claimed
	// but uncompleted key is a send that crashed mid-flight: it must not
	// be retried automatically, because nobody knows whether the
	// provider accepted it.
	Completed bool
	MessageID string
}

// SentRecord is one delivered message, kept for threading.
type SentRecord struct {
	EnrollmentID string
	StepIndex    int
	MessageID    string
	ThreadID     string
	SentAt       time.Time
}

// SkipRecord is a send that the preflight refused.
//
// Kept, and kept visible: a skipped send is the system working, and the
// count of skips by reason is the most honest health metric the product
// has.
type SkipRecord struct {
	EnrollmentID string
	StepIndex    int
	Reason       string
	At           time.Time
}

func New() *Store {
	return &Store{
		contacts:         make(map[string]model.Contact),
		byEmail:          make(map[string]string),
		enrollments:      make(map[string]*model.Enrollment),
		sequences:        make(map[string]model.Sequence),
		mailboxes:        make(map[string]*model.Mailbox),
		sendKeys:         make(map[string]SendClaim),
		sentByMessageID:  make(map[string]SentRecord),
		sentByThreadID:   make(map[string]SentRecord),
		sentByEnrollment: make(map[string][]SentRecord),
		replied:          make(map[string]time.Time),
		bookings:         make(map[string]time.Time),
		queued:           make(map[string]map[string]bool),
		enrollmentLocks:  make(map[string]*sync.Mutex),
		contactLocks:     make(map[string]*sync.Mutex),
	}
}

func (s *Store) lockIn(m map[string]*sync.Mutex, key string) func() {
	s.lockMu.Lock()
	mu, ok := m[key]
	if !ok {
		mu = &sync.Mutex{}
		m[key] = mu
	}
	s.lockMu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// LockContact takes the row lock for an address, keyed canonically so that
// Gmail variants share one lock.
//
// Held by anything writing a suppression and by every dispatch to that
// address, so a one-click unsubscribe cannot land between a preflight and
// the provider call.
func (s *Store) LockContact(email string) func() {
	return s.lockIn(s.contactLocks, model.CanonicalEmail(email))
}

// LockEnrollment takes the enrolment's row lock and returns its release.
//
// Callers must hold it for the whole of whatever they are doing: a
// dispatch holds it from the idempotency claim through to the recorded
// send, and an event handler holds it across its write. Taking it for only
// the read reintroduces exactly the gap it exists to remove.
func (s *Store) LockEnrollment(enrollmentID string) func() {
	return s.lockIn(s.enrollmentLocks, enrollmentID)
}

// LockEnrollments takes several row locks in a stable order.
//
// Sorted, because an account-level pause locks every enrolment at the
// account while individual dispatches hold single locks. Without a
// consistent order, two account-level events touching overlapping sets
// deadlock against each other.
func (s *Store) LockEnrollments(ids []string) func() {
	ordered := append([]string(nil), ids...)
	sort.Strings(ordered)

	releases := make([]func(), 0, len(ordered))
	var prev string
	for i, id := range ordered {
		if i > 0 && id == prev {
			continue // a duplicate would self-deadlock
		}
		releases = append(releases, s.LockEnrollment(id))
		prev = id
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
}

// --- idempotency ------------------------------------------------------------

// SendKey is the idempotency key for one step of one enrolment.
func SendKey(enrollmentID string, stepIndex int) string {
	return fmt.Sprintf("send:%s:%d", enrollmentID, stepIndex)
}

// ClaimSendKey claims the key, returning false if it was already taken.
//
// Called BEFORE dispatch, and that ordering is the whole point. A crash
// after claiming means a missed send, which a human can see and re-queue.
// A crash before claiming means a double send, which cannot be undone and
// which the prospect definitely notices.
//
// When you must choose, fail toward not sending.
func (s *Store) ClaimSendKey(key string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.sendKeys[key]; taken {
		return false, nil
	}
	s.sendKeys[key] = SendClaim{Key: key, ClaimedAt: now}
	return true, nil
}

// CompleteSendKey marks a claimed key as actually delivered.
func (s *Store) CompleteSendKey(key, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.sendKeys[key]
	if !ok {
		return fmt.Errorf("store: completing unclaimed key %s", key)
	}
	c.Completed = true
	c.MessageID = messageID
	s.sendKeys[key] = c
	return nil
}

// StaleClaims lists keys claimed but never completed.
//
// These are the crash survivors. They are surfaced to a human rather than
// retried, because the one thing nobody knows about a claim that died
// mid-dispatch is whether the provider accepted the message.
func (s *Store) StaleClaims(olderThan time.Duration, now time.Time) []SendClaim {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := now.Add(-olderThan)
	var out []SendClaim
	for _, c := range s.sendKeys {
		if !c.Completed && c.ClaimedAt.Before(cutoff) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// --- contacts and enrolments ------------------------------------------------

func (s *Store) PutContact(c model.Contact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contacts[c.ID] = c
	s.byEmail[model.CanonicalEmail(c.Email)] = c.ID
}

func (s *Store) Contact(id string) (model.Contact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.contacts[id]
	return c, ok
}

func (s *Store) ContactByEmail(email string) (model.Contact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byEmail[model.CanonicalEmail(email)]
	if !ok {
		return model.Contact{}, false
	}
	c, ok := s.contacts[id]
	return c, ok
}

func (s *Store) PutSequence(seq model.Sequence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequences[seq.ID] = seq
}

func (s *Store) Sequence(id string) (model.Sequence, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, ok := s.sequences[id]
	return seq, ok
}

func (s *Store) PutMailbox(m model.Mailbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := m
	s.mailboxes[m.ID] = &cp
}

func (s *Store) Mailbox(id string) (model.Mailbox, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.mailboxes[id]
	if !ok {
		return model.Mailbox{}, false
	}
	return *m, true
}

func (s *Store) PutEnrollment(e model.Enrollment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := e
	s.enrollments[e.ID] = &cp
}

func (s *Store) Enrollment(id string) (model.Enrollment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.enrollments[id]
	if !ok {
		return model.Enrollment{}, false
	}
	return *e, true
}

func (s *Store) ActiveEnrollmentsByAccount(accountID string) ([]model.Enrollment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []model.Enrollment
	for _, e := range s.enrollments {
		if e.AccountID == accountID && !e.State.Terminal() {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) ActiveEnrollmentsByContact(contactID string) ([]model.Enrollment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []model.Enrollment
	for _, e := range s.enrollments {
		if e.ContactID == contactID && !e.State.Terminal() {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Pause stops an enrolment while remembering its position.
func (s *Store) Pause(enrollmentID string, reason model.PauseReason) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.enrollments[enrollmentID]
	if !ok {
		return fmt.Errorf("store: no enrolment %s", enrollmentID)
	}
	// A terminal enrolment is not paused back into life. Pausing a
	// stopped enrolment would make a "resume" button reanimate someone
	// who unsubscribed.
	if e.State.Terminal() {
		return nil
	}
	e.State = model.StatePaused
	e.PauseReason = reason
	e.PausedUntil = reason.ExpiresAt
	return nil
}

func (s *Store) Resume(enrollmentID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.enrollments[enrollmentID]
	if !ok {
		return fmt.Errorf("store: no enrolment %s", enrollmentID)
	}
	if e.State != model.StatePaused {
		return fmt.Errorf("store: enrolment %s is %s, not paused", enrollmentID, e.State)
	}
	e.State = model.StateActive
	e.NextSendAt = at
	e.PausedUntil = time.Time{}
	e.PauseReason = model.PauseReason{}
	return nil
}

// SetState moves an enrolment to a terminal or active state.
func (s *Store) SetState(enrollmentID string, st model.EnrollmentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.enrollments[enrollmentID]
	if !ok {
		return fmt.Errorf("store: no enrolment %s", enrollmentID)
	}
	e.State = st
	return nil
}

// StopForUnsubscribe satisfies unsubscribe.EnrollmentStopper.
func (s *Store) StopForUnsubscribe(enrollmentID string) error {
	return s.SetState(enrollmentID, model.StateSuppressed)
}

// --- replies and bookings ---------------------------------------------------

// MarkReplied records a reply and is the write side of the race in §7.4.
//
// Takes the enrolment's row lock, so it cannot land in the middle of a
// dispatch that has already passed its preflight.
func (s *Store) MarkReplied(enrollmentID string, at time.Time) {
	defer s.LockEnrollment(enrollmentID)()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.replied[enrollmentID]; !ok {
		s.replied[enrollmentID] = at
	}
	if e, ok := s.enrollments[enrollmentID]; ok && !e.State.Terminal() {
		e.State = model.StateReplied
	}
}

func (s *Store) HasReplied(enrollmentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.replied[enrollmentID]
	return ok
}

// MarkBooked records an account-level booking.
//
// Takes every active enrolment's row lock at the account first, in sorted
// order, so the booking cannot land between a preflight and a provider
// call for any contact at that company. That is the whole point: the
// visible failure is the OTHER three people at Acme getting cold-emailed
// after one of their colleagues booked.
func (s *Store) MarkBooked(accountID string, until time.Time) {
	ens, _ := s.ActiveEnrollmentsByAccount(accountID)
	ids := make([]string, 0, len(ens))
	for _, e := range ens {
		ids = append(ids, e.ID)
	}
	defer s.LockEnrollments(ids)()

	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.bookings[accountID]; !ok || until.After(cur) {
		s.bookings[accountID] = until
	}
}

func (s *Store) AccountHasBooking(accountID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.bookings[accountID]
	return ok && now.Before(until)
}

// --- queued jobs ------------------------------------------------------------

// EnqueueJob records a scheduled job so it can be cancelled.
func (s *Store) EnqueueJob(enrollmentID, jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.queued[enrollmentID]
	if m == nil {
		m = make(map[string]bool)
		s.queued[enrollmentID] = m
	}
	m[jobID] = true
}

// CancelQueued removes queued jobs for an enrolment.
//
// Marking the enrolment state is not enough on its own: a job that already
// read the state and is waiting on a timer will still fire. Cancelling is
// what closes the window that the send-time preflight narrows but cannot
// eliminate.
func (s *Store) CancelQueued(enrollmentID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.queued[enrollmentID])
	delete(s.queued, enrollmentID)
	return n, nil
}

// JobIsLive reports whether a job survived cancellation.
func (s *Store) JobIsLive(enrollmentID, jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queued[enrollmentID][jobID]
}

// --- sends ------------------------------------------------------------------

func (s *Store) RecordSent(enrollmentID string, stepIndex int, messageID, threadID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := SentRecord{
		EnrollmentID: enrollmentID,
		StepIndex:    stepIndex,
		MessageID:    normalizeID(messageID),
		ThreadID:     threadID,
		SentAt:       at,
	}
	s.sentByMessageID[r.MessageID] = r
	if threadID != "" {
		s.sentByThreadID[threadID] = r
	}
	s.sentByEnrollment[enrollmentID] = append(s.sentByEnrollment[enrollmentID], r)
}

func (s *Store) SentByEnrollment(enrollmentID string) []SentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SentRecord(nil), s.sentByEnrollment[enrollmentID]...)
}

// SentCount is what the duplicate-send tests assert on.
func (s *Store) SentCount(enrollmentID string, stepIndex int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.sentByEnrollment[enrollmentID] {
		if r.StepIndex == stepIndex {
			n++
		}
	}
	return n
}

func (s *Store) RecordSkipped(enrollmentID string, stepIndex int, reason string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skipped = append(s.skipped, SkipRecord{
		EnrollmentID: enrollmentID, StepIndex: stepIndex, Reason: reason, At: at,
	})
}

func (s *Store) Skipped() []SkipRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SkipRecord(nil), s.skipped...)
}

// SkipsByReason is the health view: a spike in one reason is the signal.
func (s *Store) SkipsByReason() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int)
	for _, r := range s.skipped {
		out[r.Reason]++
	}
	return out
}

// --- reply.SentIndex --------------------------------------------------------

// BySentMessageID satisfies reply.SentIndex.
func (s *Store) BySentMessageID(messageID string) (SentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.sentByMessageID[normalizeID(messageID)]
	return r, ok
}

func (s *Store) BySentThreadID(threadID string) (SentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.sentByThreadID[threadID]
	return r, ok
}

func normalizeID(id string) string {
	id = trimSpace(id)
	if len(id) >= 2 && id[0] == '<' && id[len(id)-1] == '>' {
		id = id[1 : len(id)-1]
	}
	return lower(id)
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
