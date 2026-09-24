package send

import (
	"strings"
	"testing"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/deliverability"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/jurisdiction"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/ratelimit"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/store"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/suppression"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/unsubscribe"
)

func now() time.Time { return time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC) }

type capturing struct {
	msgs []Message
	err  error
}

func (c *capturing) Send(m Message) (Result, error) {
	if c.err != nil {
		return Result{}, c.err
	}
	c.msgs = append(c.msgs, m)
	return Result{MessageID: "<sent@ours.example>", ThreadID: "thr-1"}, nil
}

type harness struct {
	store    *store.Store
	list     *suppression.List
	gate     *jurisdiction.Gate
	limiter  *ratelimit.Limiter
	breaker  *deliverability.Breaker
	provider *capturing
	pre      *Preflight
	d        *Dispatcher
	job      Job
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := store.New()
	list := suppression.New()
	gate := jurisdiction.NewGate()
	lim := ratelimit.DefaultLimits()
	lim.MinInterval = 0
	limiter := ratelimit.New(lim)
	breaker := deliverability.NewBreaker(deliverability.DefaultBudget())

	s.PutContact(model.Contact{ID: "c1", AccountID: "acme", Email: "dana@acme.example", Country: "US"})
	s.PutEnrollment(model.Enrollment{
		ID: "en1", SequenceID: "seq1", ContactID: "c1", AccountID: "acme",
		State: model.StateActive, CurrentStep: 3,
	})

	prov := &capturing{}
	pre := &Preflight{
		Suppression: list, Replies: s, Bookings: s, State: s,
		Gate: gate, Limiter: limiter, Budget: breaker,
	}
	d := &Dispatcher{
		Store: s, Preflight: pre, Provider: prov,
		Signer:       unsubscribe.NewSigner([]byte("k")),
		UnsubBaseURL: "https://unsub.example.com",
		UnsubMailto:  "unsub@example.com",
		Now:          now,
	}
	return &harness{
		store: s, list: list, gate: gate, limiter: limiter, breaker: breaker,
		provider: prov, pre: pre, d: d,
		job: Job{
			EnrollmentID: "en1", StepIndex: 3, ContactID: "c1",
			ContactEmail: "dana@acme.example", AccountID: "acme",
			MailboxID: "mb1", DomainID: "get-ours.example", SequenceID: "seq1",
			Channel: model.ChannelEmail, Mode: model.ModeAutomatic,
			Subject: "Quick question", Body: "Hello.",
			InReplyTo:  "<step2@ours.example>",
			References: []string{"<step1@ours.example>", "<step2@ours.example>"},
		},
	}
}

func TestHappyPath(t *testing.T) {
	h := newHarness(t)
	out, err := h.d.Execute(h.job)
	if err != nil {
		t.Fatal(err)
	}
	if out != Sent {
		t.Fatalf("outcome %s", out)
	}
	if len(h.provider.msgs) != 1 {
		t.Fatalf("%d messages", len(h.provider.msgs))
	}
	if n := h.store.SentCount("en1", 3); n != 1 {
		t.Fatalf("%d send records", n)
	}
}

// TestEveryGateRefuses walks each check in the preflight.
func TestEveryGateRefuses(t *testing.T) {
	cases := map[string]struct {
		setup func(h *harness)
		gate  string
	}{
		"suppression": {func(h *harness) {
			h.list.Add("dana@acme.example", suppression.Unsubscribed, "endpoint", "")
		}, "suppression"},
		// The reply ledger is checked on its own, behind the state.
		// This models the window where the reply has been recorded but
		// the enrolment state write has not landed yet -- two writes,
		// and only one of them is the gate that must hold.
		"reply": {func(h *harness) {
			h.store.MarkReplied("en1", now())
			_ = h.store.SetState("en1", model.StateActive)
		}, "reply"},
		"booking": {func(h *harness) {
			h.store.MarkBooked("acme", now().AddDate(0, 0, 30))
		}, "booking"},
		"enrolment stopped": {func(h *harness) {
			_ = h.store.SetState("en1", model.StateStopped)
		}, "enrollment"},
		"enrolment paused": {func(h *harness) {
			_ = h.store.Pause("en1", model.PauseReason{Kind: "ooo", ExpiresAt: now().AddDate(0, 0, 7)})
		}, "enrollment"},
		"jurisdiction": {func(h *harness) {
			h.store.PutContact(model.Contact{ID: "c1", AccountID: "acme", Email: "dana@acme.example", Country: "CA"})
		}, "jurisdiction"},
		"budget halted": {func(h *harness) {
			h.breaker.Observe(deliverability.Metrics{
				DomainID: "get-ours.example", Source: deliverability.SourcePostmaster,
				Sent: 2000, Inboxed: 1500, Complaints: 3,
			})
		}, "budget"},
		"rate limited": {func(h *harness) {
			// Empty account id, so this exhausts the mailbox's daily cap
			// without also tripping the account cap first.
			for i := 0; i < 40; i++ {
				h.limiter.Allow("mb1", "get-ours.example", "", now())
			}
		}, "ratelimit"},
	}

	for name, tc := range cases {
		h := newHarness(t)
		tc.setup(h)

		v := h.pre.Check(h.job, now())
		if v.Allowed {
			t.Errorf("%s: allowed", name)
			continue
		}
		if v.Gate != tc.gate {
			t.Errorf("%s: refused at gate %q, want %q", name, v.Gate, tc.gate)
		}
		if v.Reason == "" {
			t.Errorf("%s: refused without saying why", name)
		}

		out, err := h.d.Execute(h.job)
		if err != nil {
			t.Errorf("%s: skip reported as an error: %v", name, err)
		}
		if out != Skipped {
			t.Errorf("%s: outcome %s", name, out)
		}
		if len(h.provider.msgs) != 0 {
			t.Errorf("%s: sent anyway", name)
		}
	}
}

// TestBudgetIsCheckedBeforeTheLimiterConsumes: the limiter has a side
// effect, so checking a halted domain after it spends a mailbox slot throws
// away capacity on a send that will not happen.
func TestBudgetIsCheckedBeforeTheLimiterConsumes(t *testing.T) {
	h := newHarness(t)
	h.breaker.Observe(deliverability.Metrics{
		DomainID: "get-ours.example", Source: deliverability.SourcePostmaster,
		Sent: 2000, Inboxed: 1500, Complaints: 3,
	})
	before := h.limiter.Remaining("mb1", "get-ours.example", "acme", now())
	h.pre.Check(h.job, now())
	after := h.limiter.Remaining("mb1", "get-ours.example", "acme", now())

	if after.Mailbox != before.Mailbox {
		t.Fatalf("a halted domain consumed mailbox allowance: %d then %d", before.Mailbox, after.Mailbox)
	}
}

// TestPausePastItsExpiryDoesNotBlock: a pause is resumable by definition.
func TestPausePastItsExpiryDoesNotBlock(t *testing.T) {
	h := newHarness(t)
	_ = h.store.Pause("en1", model.PauseReason{Kind: "ooo", ExpiresAt: now().AddDate(0, 0, -1)})
	if v := h.pre.Check(h.job, now()); !v.Allowed {
		t.Fatalf("an expired pause still blocked: %s", v.Reason)
	}
}

// TestUnsubscribeHeadersAreOnEverySend.
func TestUnsubscribeHeadersAreOnEverySend(t *testing.T) {
	h := newHarness(t)
	if _, err := h.d.Execute(h.job); err != nil {
		t.Fatal(err)
	}
	m := h.provider.msgs[0]
	if m.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Fatalf("headers: %+v", m.Headers)
	}
	if !strings.Contains(m.Headers["List-Unsubscribe"], "https://unsub.example.com/u/") {
		t.Fatalf("List-Unsubscribe: %s", m.Headers["List-Unsubscribe"])
	}
	// And prominently in the body: hiding it converts unsubscribes into
	// spam complaints.
	if !strings.Contains(m.Body, "unsubscribe here: https://unsub.example.com/u/") {
		t.Fatalf("body has no visible unsubscribe link:\n%s", m.Body)
	}
}

func TestRequireUnsubscribe(t *testing.T) {
	h := newHarness(t)
	if err := h.d.RequireUnsubscribe(); err != nil {
		t.Fatal(err)
	}
	h.d.Signer = nil
	if err := h.d.RequireUnsubscribe(); err == nil {
		t.Fatal("a dispatcher with no opt-out configured was accepted")
	}
}

func TestThreadingHeaders(t *testing.T) {
	h := newHarness(t)
	if _, err := h.d.Execute(h.job); err != nil {
		t.Fatal(err)
	}
	m := h.provider.msgs[0]
	if m.Headers["In-Reply-To"] != "<step2@ours.example>" {
		t.Fatalf("In-Reply-To %q", m.Headers["In-Reply-To"])
	}
	if m.Headers["References"] != "<step1@ours.example> <step2@ours.example>" {
		t.Fatalf("References %q", m.Headers["References"])
	}
	// Nothing in the headers threads on the subject.
	for k := range m.Headers {
		if strings.EqualFold(k, "Subject") {
			t.Fatal("threading leaned on the subject")
		}
	}
}

// TestManualTasksAreNeverDispatched, and LinkedIn most of all.
func TestManualTasksAreNeverDispatched(t *testing.T) {
	h := newHarness(t)
	j := h.job
	j.Channel = model.ChannelLinkedIn
	j.Mode = model.ModeManualTask

	if err := ManualTaskGate(j); err == nil {
		t.Fatal("a manual task passed the gate")
	}
	out, err := h.d.Execute(j)
	if err != nil || out != Skipped {
		t.Fatalf("%s %v", out, err)
	}
	if len(h.provider.msgs) != 0 {
		t.Fatal("a manual task reached the provider")
	}
}

// TestAutomaticLinkedInStepIsRefusedStructurally: the rep's own account is
// the asset at risk, so this is not a configuration anyone can reach.
func TestAutomaticLinkedInStepIsRefusedStructurally(t *testing.T) {
	j := Job{EnrollmentID: "en1", Channel: model.ChannelLinkedIn, Mode: model.ModeAutomatic}
	err := ManualTaskGate(j)
	if err == nil {
		t.Fatal("an automatic LinkedIn step was accepted")
	}
	if !strings.Contains(err.Error(), "User Agreement") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}

func TestProviderFailureDoesNotReleaseTheKey(t *testing.T) {
	h := newHarness(t)
	h.provider.err = errProvider

	if _, err := h.d.Execute(h.job); err == nil {
		t.Fatal("no error")
	}
	// A retry must not turn one uncertain send into two certain ones.
	h.provider.err = nil
	out, _ := h.d.Execute(h.job)
	if out != Duplicate {
		t.Fatalf("retry produced %s", out)
	}
	if len(h.provider.msgs) != 0 {
		t.Fatal("the retry sent")
	}
}

var errProvider = &provErr{}

type provErr struct{}

func (*provErr) Error() string { return "provider unavailable" }

func TestMissingEnrolmentIsRefused(t *testing.T) {
	h := newHarness(t)
	j := h.job
	j.EnrollmentID = "nope"
	v := h.pre.Check(j, now())
	if v.Allowed {
		t.Fatal("allowed a send for an enrolment that does not exist")
	}
}

func TestMissingContactBlocksJurisdiction(t *testing.T) {
	h := newHarness(t)
	j := h.job
	j.ContactID = "nope"
	v := h.pre.Check(j, now())
	if v.Allowed || v.Gate != "jurisdiction" {
		t.Fatalf("%+v: jurisdiction cannot be determined without the contact", v)
	}
}
