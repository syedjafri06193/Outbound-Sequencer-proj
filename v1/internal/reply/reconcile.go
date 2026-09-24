package reply

import (
	"fmt"
	"sync"
	"time"
)

// Reconciliation of webhooks against polling.
//
// Gmail push (Pub/Sub watch) and Microsoft Graph subscriptions both deliver
// reply events quickly and both drop messages. Running only webhooks means
// a reply that never arrived looks identical to no reply at all -- and the
// sequence keeps sending.
//
// So: webhooks for latency, polling for completeness, and a measured
// discrepancy rate so a broken webhook path is discovered here rather than
// by a rep reading an angry thread.

// Source is how a message reached us.
type Source string

const (
	SourceWebhook Source = "webhook"
	SourcePoll    Source = "poll"
)

// Observation is one sighting of an inbound message.
type Observation struct {
	MessageID  string
	MailboxID  string
	Source     Source
	ObservedAt time.Time
	// ReceivedAt is the provider's timestamp on the message itself.
	ReceivedAt time.Time
}

type sighting struct {
	firstWebhook time.Time
	firstPoll    time.Time
	receivedAt   time.Time
	mailboxID    string
}

// Reconciler tracks which path saw what.
type Reconciler struct {
	mu   sync.Mutex
	seen map[string]*sighting
	// GracePeriod is how long a webhook has to arrive before a
	// poll-only sighting counts as a miss. Without it, every message the
	// poller happens to see first is counted as a webhook failure, and
	// the metric becomes noise.
	GracePeriod time.Duration
	// Retention bounds the map. A reconciler that remembers every
	// message id forever is a memory leak with a long fuse.
	Retention time.Duration
}

func NewReconciler() *Reconciler {
	return &Reconciler{
		seen:        make(map[string]*sighting),
		GracePeriod: 5 * time.Minute,
		Retention:   48 * time.Hour,
	}
}

// Observe records a sighting and reports whether this is the first time the
// message has been seen by any path.
//
// The return value is what the caller acts on: process the message once,
// on whichever path saw it first. Processing it twice would classify the
// same reply twice and, worse, could re-trigger a suppression write for an
// address already suppressed.
func (r *Reconciler) Observe(o Observation) (firstSighting bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	s := r.seen[o.MessageID]
	if s == nil {
		s = &sighting{receivedAt: o.ReceivedAt, mailboxID: o.MailboxID}
		r.seen[o.MessageID] = s
		firstSighting = true
	}

	switch o.Source {
	case SourceWebhook:
		if s.firstWebhook.IsZero() {
			s.firstWebhook = o.ObservedAt
		}
	case SourcePoll:
		if s.firstPoll.IsZero() {
			s.firstPoll = o.ObservedAt
		}
	}
	return firstSighting
}

// Discrepancy is the health of the webhook path.
type Discrepancy struct {
	Total int
	// WebhookSeen is how many the webhook path delivered at all.
	WebhookSeen int
	// WebhookMissed is how many the poller found and the webhook never
	// delivered, past the grace period.
	WebhookMissed int
	// PollOnlyWithinGrace is not a miss: the webhook may still arrive.
	PollOnlyWithinGrace int
	// MissRate is WebhookMissed / Total.
	MissRate float64
	// MedianWebhookLead is how much sooner the webhook path saw a
	// message than the poller, for messages both paths saw.
	MedianWebhookLead time.Duration
	Healthy           bool
	Message           string
}

// WebhookMissRateAlert is the level at which the webhook path is considered
// broken rather than lossy.
const WebhookMissRateAlert = 0.02

// Report summarises the discrepancy as of now.
func (r *Reconciler) Report(now time.Time) Discrepancy {
	r.mu.Lock()
	defer r.mu.Unlock()

	var d Discrepancy
	var leads []time.Duration

	for _, s := range r.seen {
		d.Total++
		if !s.firstWebhook.IsZero() {
			d.WebhookSeen++
			if !s.firstPoll.IsZero() {
				leads = append(leads, s.firstPoll.Sub(s.firstWebhook))
			}
			continue
		}
		// Poll-only.
		if !s.firstPoll.IsZero() && now.Sub(s.firstPoll) >= r.GracePeriod {
			d.WebhookMissed++
		} else {
			d.PollOnlyWithinGrace++
		}
	}

	if d.Total > 0 {
		d.MissRate = float64(d.WebhookMissed) / float64(d.Total)
	}
	d.MedianWebhookLead = median(leads)

	d.Healthy = d.MissRate < WebhookMissRateAlert
	if d.Healthy {
		d.Message = fmt.Sprintf("webhook path healthy: %d of %d messages delivered, %.2f%% missed",
			d.WebhookSeen, d.Total, d.MissRate*100)
	} else {
		d.Message = fmt.Sprintf(
			"webhook path degraded: %d of %d messages (%.2f%%) were found only by polling. Replies are being detected late, which means sends are going out after the prospect replied.",
			d.WebhookMissed, d.Total, d.MissRate*100)
	}
	return d
}

// Prune drops sightings older than Retention.
func (r *Reconciler) Prune(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := now.Add(-r.Retention)
	n := 0
	for id, s := range r.seen {
		last := s.firstPoll
		if s.firstWebhook.After(last) {
			last = s.firstWebhook
		}
		if last.Before(cutoff) {
			delete(r.seen, id)
			n++
		}
	}
	return n
}

// Size is the number of tracked messages.
func (r *Reconciler) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func median(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	// Insertion sort: these slices are small and it avoids a dependency
	// on sort just for this.
	for i := 1; i < len(s); i++ {
		v := s[i]
		j := i - 1
		for j >= 0 && s[j] > v {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = v
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// PollCursor tracks where the poller got to per mailbox.
//
// Advanced only past messages that were actually processed. A cursor moved
// on receipt rather than on completion loses every message in flight when
// the worker restarts, and those are exactly the replies that must not be
// lost.
type PollCursor struct {
	mu sync.Mutex
	at map[string]time.Time
	// Overlap re-reads a little before the cursor on each poll, because
	// provider timestamps are not strictly monotonic across a mailbox
	// and a message can be assigned a time just before one already read.
	Overlap time.Duration
}

func NewPollCursor() *PollCursor {
	return &PollCursor{at: make(map[string]time.Time), Overlap: 2 * time.Minute}
}

// Since returns the timestamp to poll from for a mailbox.
func (c *PollCursor) Since(mailboxID string, fallback time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.at[mailboxID]
	if !ok {
		return fallback
	}
	return t.Add(-c.Overlap)
}

// Advance moves the cursor, never backwards.
func (c *PollCursor) Advance(mailboxID string, to time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.at[mailboxID]; ok && !to.After(cur) {
		return
	}
	c.at[mailboxID] = to
}
