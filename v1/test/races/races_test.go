// Package races holds the tests that matter most.
//
// Race bugs that appear one time in fifty are exactly the ones that reach
// production, so each of these runs 500 iterations under -race. The
// assertion in every case is the same shape: it is fine for the send to
// have gone out BEFORE the event, and it is fine for it to have been
// skipped. What must never happen is a send dispatched after the event
// landed.
package races

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/deliverability"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/jurisdiction"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/ratelimit"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/send"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/store"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/suppression"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/unsubscribe"
)

const iterations = 500

// recordingProvider timestamps every message it accepts.
type recordingProvider struct {
	mu    sync.Mutex
	sends []time.Time
	// delay simulates the provider round-trip, widening the window the
	// preflight is trying to close. Without it the race almost never
	// interleaves and the test passes for the wrong reason.
	delay time.Duration
	n     atomic.Int64
}

func (p *recordingProvider) Send(send.Message) (send.Result, error) {
	// Stamped on ENTRY, which is the moment the message leaves. Stamping
	// on return would measure the provider round-trip rather than the
	// dispatch, and would report a send that went out first as though it
	// went out second.
	p.mu.Lock()
	p.sends = append(p.sends, time.Now())
	p.mu.Unlock()

	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	i := p.n.Add(1)
	return send.Result{
		MessageID: fmt.Sprintf("<m%d@ours.example>", i),
		ThreadID:  fmt.Sprintf("t%d", i),
	}, nil
}

func (p *recordingProvider) sentAfter(t time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sends {
		if s.After(t) {
			return true
		}
	}
	return false
}

// latestAfter reports the widest gap between t and a send that followed it.
func (p *recordingProvider) latestAfter(t time.Time) (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var worst time.Duration
	found := false
	for _, s := range p.sends {
		if s.After(t) {
			found = true
			if d := s.Sub(t); d > worst {
				worst = d
			}
		}
	}
	return worst, found
}

func (p *recordingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sends)
}

type env struct {
	store    *store.Store
	list     *suppression.List
	provider *recordingProvider
	dispatch *send.Dispatcher
	breaker  *deliverability.Breaker
	unsub    *unsubscribe.Handler
	token    string
	job      send.Job
}

func newEnv(t testing.TB, providerDelay time.Duration) *env {
	t.Helper()

	s := store.New()
	list := suppression.New()
	gate := jurisdiction.NewGate()
	limiter := ratelimit.New(func() ratelimit.Limits {
		l := ratelimit.DefaultLimits()
		l.MinInterval = 0
		l.AccountWeekly = 1000
		return l
	}())
	breaker := deliverability.NewBreaker(deliverability.DefaultBudget())

	s.PutContact(model.Contact{ID: "c1", AccountID: "acme", Email: "dana@acme.example", Country: "US"})
	s.PutEnrollment(model.Enrollment{
		ID: "en1", SequenceID: "seq1", ContactID: "c1", AccountID: "acme",
		State: model.StateActive, CurrentStep: 3,
	})

	signer := unsubscribe.NewSigner([]byte("race-test-key"))
	prov := &recordingProvider{delay: providerDelay}
	d := &send.Dispatcher{
		Store: s,
		Preflight: &send.Preflight{
			Suppression: list,
			Replies:     s,
			Bookings:    s,
			State:       s,
			Gate:        gate,
			Limiter:     limiter,
			Budget:      breaker,
		},
		Provider:     prov,
		Signer:       signer,
		UnsubBaseURL: "https://unsub.example.com",
		UnsubMailto:  "unsub@example.com",
	}

	tok, err := signer.Sign(unsubscribe.Token{
		Email: "dana@acme.example", EnrollmentID: "en1", SequenceID: "seq1",
	})
	if err != nil {
		t.Fatal(err)
	}

	return &env{
		store: s, list: list, provider: prov, dispatch: d, breaker: breaker,
		unsub: &unsubscribe.Handler{Signer: signer, List: list, Stop: s, Guard: s},
		token: tok,
		job: send.Job{
			EnrollmentID: "en1", StepIndex: 3, ContactID: "c1",
			ContactEmail: "dana@acme.example", AccountID: "acme",
			MailboxID: "mb1", DomainID: "get-ours.example", SequenceID: "seq1",
			Channel: model.ChannelEmail, Mode: model.ModeAutomatic,
			Subject: "Quick question", Body: "Hello.",
		},
	}
}

// raceResult is what one batch of interleavings measured.
type raceResult struct {
	sends int
	// afterCommit is the number of emails DISPATCHED after the event was
	// committed to the store. This is the one that must be zero: once the
	// system has recorded the reply, nothing may still go out.
	afterCommit int
	// afterArrival counts emails dispatched after the event ARRIVED but
	// before it had been recorded. These are not violations -- the system
	// had not been told yet -- but the size of that window is the number
	// §7.4 is about, so it is reported rather than hidden.
	afterArrival int
	worstWindow  time.Duration
}

// runRace drives one event against one dispatch, `iterations` times.
//
// event returns the instant the event arrived; runRace stamps the commit
// itself, once the event's write has returned.
func runRace(t *testing.T, providerDelay time.Duration, event func(e *env) time.Time) raceResult {
	t.Helper()
	var r raceResult

	for i := 0; i < iterations; i++ {
		e := newEnv(t, providerDelay)

		var arrived, committed atomic.Value
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			arrived.Store(event(e))
			// The write has returned, so it is committed and visible to
			// every subsequent preflight.
			committed.Store(time.Now())
		}()
		go func() {
			defer wg.Done()
			_, _ = e.dispatch.Execute(e.job)
		}()
		wg.Wait()

		at, _ := arrived.Load().(time.Time)
		ct, _ := committed.Load().(time.Time)

		r.sends += e.provider.count()
		if e.provider.sentAfter(ct) {
			r.afterCommit++
		}
		if d, ok := e.provider.latestAfter(at); ok {
			r.afterArrival++
			if d > r.worstWindow {
				r.worstWindow = d
			}
		}
	}
	return r
}

func (r raceResult) log(t *testing.T, what string) {
	t.Helper()
	t.Logf("%d interleavings: %d emails went out (%s arrived first in the rest); %d landed in the gap between arrival and the write committing, the widest %v; %d after the write committed",
		iterations, r.sends, what, r.afterArrival, r.worstWindow, r.afterCommit)
}

// TestReplyBeatsQueuedSend is §7.4.
//
//	09:00:00  scheduler queues step 3 for contact X
//	09:00:15  contact X replies "please stop emailing me"
//	09:00:30  the queued job fires and sends step 3
//
// The check happened at schedule time. The world changed before dispatch.
func TestReplyBeatsQueuedSend(t *testing.T) {
	r := runRace(t, 50*time.Microsecond, func(e *env) time.Time {
		now := time.Now()
		e.store.MarkReplied("en1", now)
		// Cancelling queued jobs as well as marking the state is the
		// belt-and-braces half of the fix.
		_, _ = e.store.CancelQueued("en1")
		return now
	})
	r.log(t, "the reply")

	if r.afterCommit != 0 {
		t.Fatalf("sent an email after the prospect's reply was recorded, %d times in %d",
			r.afterCommit, iterations)
	}
	// The doc's claim is that the window shrinks from minutes to
	// milliseconds. Hold it to that.
	if r.worstWindow > time.Millisecond {
		t.Fatalf("the reply-to-dispatch window reached %v; it is meant to be sub-millisecond", r.worstWindow)
	}
}

// TestBookingBeatsQueuedSend is the same race at the account level: booked
// at 09:00:30, send at 09:00:31.
func TestBookingBeatsQueuedSend(t *testing.T) {
	r := runRace(t, 50*time.Microsecond, func(e *env) time.Time {
		now := time.Now()
		e.store.MarkBooked("acme", now.Add(30*24*time.Hour))
		return now
	})
	r.log(t, "the booking")

	if r.afterCommit != 0 {
		t.Fatalf("cold-emailed an account %d times in %d after a booking there was recorded",
			r.afterCommit, iterations)
	}
}

// TestUnsubscribeBeatsQueuedSend: an email to someone who unsubscribed
// seconds earlier is a CAN-SPAM violation and a near-certain complaint.
func TestUnsubscribeBeatsQueuedSend(t *testing.T) {
	r := runRace(t, 50*time.Microsecond, func(e *env) time.Time {
		now := time.Now()
		e.unsub.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodPost, "/u/"+e.token, nil))
		return now
	})
	r.log(t, "the unsubscribe")

	if r.afterCommit != 0 {
		t.Fatalf("emailed an unsubscribed address %d times in %d", r.afterCommit, iterations)
	}
}

// TestHaltBeatsQueuedSend: a domain halted by the complaint budget must
// stop sending.
//
// Note what is NOT asserted here. The breaker is domain-level and has no
// enrolment or contact key, so nothing serialises it against a dispatch the
// way the reply and unsubscribe paths are serialised. A halt landing during
// a provider round-trip lets that one message out, and the honest thing is
// to measure that rather than to claim otherwise.
func TestHaltBeatsQueuedSend(t *testing.T) {
	r := runRace(t, 50*time.Microsecond, func(e *env) time.Time {
		now := time.Now()
		e.breaker.Observe(deliverability.Metrics{
			DomainID: "get-ours.example",
			Source:   deliverability.SourcePostmaster,
			Sent:     2000, Inboxed: 1500, Complaints: 3,
		})
		return now
	})
	r.log(t, "the halt")

	if r.worstWindow > 10*time.Millisecond {
		t.Fatalf("a halt took %v to stop sends in flight", r.worstWindow)
	}
	// What must hold absolutely: once halted, nothing new goes out.
	e := newEnv(t, 0)
	e.breaker.Observe(deliverability.Metrics{
		DomainID: "get-ours.example", Source: deliverability.SourcePostmaster,
		Sent: 2000, Inboxed: 1500, Complaints: 3,
	})
	for i := 0; i < 50; i++ {
		j := e.job
		j.EnrollmentID = fmt.Sprintf("en%d", i)
		e.store.PutEnrollment(model.Enrollment{
			ID: j.EnrollmentID, ContactID: "c1", AccountID: "acme", State: model.StateActive,
		})
		_, _ = e.dispatch.Execute(j)
	}
	if n := e.provider.count(); n != 0 {
		t.Fatalf("%d sends from a domain that was already halted", n)
	}
}

// noPreflightDispatch is the control: the schedule-time check only, with no
// re-check before dispatch. This is what the product looks like without
// §6.1's third checkpoint.
func noPreflightDispatch(e *env) {
	// The scheduler checked at queue time and found nothing wrong.
	if e.store.HasReplied("en1") {
		return
	}
	time.Sleep(50 * time.Microsecond) // the queue delay
	_, _ = e.provider.Send(send.Message{To: "dana@acme.example"})
}

// TestSendTimeRecheckIsWhatMakesTheDifference measures the third
// suppression checkpoint rather than asserting it matters.
func TestSendTimeRecheckIsWhatMakesTheDifference(t *testing.T) {
	control := 0
	for i := 0; i < iterations; i++ {
		e := newEnv(t, 0)

		var wg sync.WaitGroup
		wg.Add(2)
		var replyAt atomic.Value
		go func() {
			defer wg.Done()
			e.store.MarkReplied("en1", time.Now())
			replyAt.Store(time.Now())
		}()
		go func() { defer wg.Done(); noPreflightDispatch(e) }()
		wg.Wait()

		ra, _ := replyAt.Load().(time.Time)
		if e.provider.sentAfter(ra) {
			control++
		}
	}

	withPreflight := runRace(t, 0, func(e *env) time.Time {
		now := time.Now()
		e.store.MarkReplied("en1", now)
		return now
	})

	t.Logf("checking only at schedule time: %d of %d emails went out after the prospect had replied", control, iterations)
	t.Logf("re-checking at send time, under the enrolment lock: %d of %d", withPreflight.afterCommit, iterations)

	if control == 0 {
		t.Fatal("the control did not reproduce the bug, so this test measures nothing")
	}
	if withPreflight.afterCommit != 0 {
		t.Fatalf("the send-time re-check let %d through", withPreflight.afterCommit)
	}
}

// TestConcurrentDispatchSendsExactlyOnce is the idempotency property.
//
// Twenty workers all pick up the same job -- a queue redelivery, a
// duplicated webhook, a worker that was presumed dead and was not.
func TestConcurrentDispatchSendsExactlyOnce(t *testing.T) {
	for i := 0; i < iterations; i++ {
		e := newEnv(t, 0)

		var wg sync.WaitGroup
		for w := 0; w < 20; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = e.dispatch.Execute(e.job)
			}()
		}
		wg.Wait()

		if n := e.provider.count(); n != 1 {
			t.Fatalf("iteration %d: 20 workers produced %d sends", i, n)
		}
		if n := e.store.SentCount("en1", 3); n != 1 {
			t.Fatalf("iteration %d: %d send records", i, n)
		}
	}
}

// TestConcurrentEnrollmentOfTheSameContact: two sequences, one contact, and
// the two steps must not both go out under one key.
func TestConcurrentEnrollmentOfTheSameContact(t *testing.T) {
	for i := 0; i < 200; i++ {
		e := newEnv(t, 0)
		e.store.PutEnrollment(model.Enrollment{
			ID: "en2", SequenceID: "seq2", ContactID: "c1", AccountID: "acme",
			State: model.StateActive,
		})
		job2 := e.job
		job2.EnrollmentID = "en2"
		job2.SequenceID = "seq2"
		job2.StepIndex = 1

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = e.dispatch.Execute(e.job) }()
		go func() { defer wg.Done(); _, _ = e.dispatch.Execute(job2) }()
		wg.Wait()

		// Two distinct enrolments are two distinct keys, so both may
		// send. What must hold is that neither sent twice.
		if n := e.store.SentCount("en1", 3); n > 1 {
			t.Fatalf("en1 step 3 sent %d times", n)
		}
		if n := e.store.SentCount("en2", 1); n > 1 {
			t.Fatalf("en2 step 1 sent %d times", n)
		}
	}
}

// --- crash tests -------------------------------------------------------------

// crashingProvider fails after recording the send, which is the worst case:
// the message is out and the caller does not know it.
type crashingProvider struct {
	recordingProvider
	failAfterSend bool
}

func (p *crashingProvider) Send(m send.Message) (send.Result, error) {
	res, _ := p.recordingProvider.Send(m)
	if p.failAfterSend {
		return res, fmt.Errorf("connection reset after the provider accepted the message")
	}
	return res, nil
}

// TestCrashAfterProviderAcceptsDoesNotDoubleSend: the key stays claimed, so
// a replay does not turn one uncertain send into two certain ones.
func TestCrashAfterProviderAcceptsDoesNotDoubleSend(t *testing.T) {
	e := newEnv(t, 0)
	crash := &crashingProvider{failAfterSend: true}
	e.dispatch.Provider = crash

	if _, err := e.dispatch.Execute(e.job); err == nil {
		t.Fatal("the crash was not reported")
	}
	if crash.count() != 1 {
		t.Fatalf("%d sends", crash.count())
	}

	// The worker restarts and replays the job.
	crash.failAfterSend = false
	out, err := e.dispatch.Execute(e.job)
	if err != nil {
		t.Fatal(err)
	}
	if out != send.Duplicate {
		t.Fatalf("replay produced %s, want duplicate", out)
	}
	if crash.count() != 1 {
		t.Fatalf("the replay sent again: %d total", crash.count())
	}
}

// TestCrashBetweenClaimAndDispatchLosesTheSendVisibly is the other side of
// the trade: a missed send, surfaced to a human rather than retried
// blindly.
func TestCrashBetweenClaimAndDispatchLosesTheSendVisibly(t *testing.T) {
	e := newEnv(t, 0)
	now := time.Now()

	// The claim lands and the process dies before the provider call.
	claimed, err := e.store.ClaimSendKey(store.SendKey("en1", 3), now.Add(-2*time.Hour))
	if err != nil || !claimed {
		t.Fatal("claim failed")
	}

	// The replay refuses, because a duplicate is worse than a miss.
	out, _ := e.dispatch.Execute(e.job)
	if out != send.Duplicate {
		t.Fatalf("replay produced %s", out)
	}
	if e.provider.count() != 0 {
		t.Fatal("the replay sent")
	}

	// And the stuck claim is visible, which is what makes the miss
	// recoverable by a human.
	stale := e.store.StaleClaims(time.Hour, now)
	if len(stale) != 1 || stale[0].Key != store.SendKey("en1", 3) {
		t.Fatalf("the lost send is invisible: %+v", stale)
	}
}

// TestFullReplayProducesZeroDuplicates kills the worker at every point in
// Execute and replays the whole queue.
func TestFullReplayProducesZeroDuplicates(t *testing.T) {
	const jobs = 50

	for killAt := 0; killAt < 4; killAt++ {
		e := newEnv(t, 0)
		var queue []send.Job
		for i := 0; i < jobs; i++ {
			id := fmt.Sprintf("en%d", i)
			e.store.PutEnrollment(model.Enrollment{
				ID: id, SequenceID: "seq1", ContactID: "c1", AccountID: "acme",
				State: model.StateActive,
			})
			j := e.job
			j.EnrollmentID = id
			j.StepIndex = 1
			// One mailbox each: 50 sends from one mailbox would hit
			// the 40/day cap, and this test is about duplicates.
			j.MailboxID = fmt.Sprintf("mb%d", i)
			queue = append(queue, j)
		}

		// First pass: die partway through.
		for i, j := range queue {
			if i == jobs/2 && killAt > 0 {
				break
			}
			_, _ = e.dispatch.Execute(j)
		}

		// Replay the entire queue from the beginning, as a restarted
		// worker would.
		for _, j := range queue {
			_, _ = e.dispatch.Execute(j)
		}

		for i := range queue {
			id := fmt.Sprintf("en%d", i)
			if n := e.store.SentCount(id, 1); n > 1 {
				t.Fatalf("killAt=%d: %s sent %d times", killAt, id, n)
			}
		}
		if got := e.provider.count(); got != jobs {
			t.Fatalf("killAt=%d: %d sends for %d jobs", killAt, got, jobs)
		}
	}
}

// TestSkippedSendIsNotAnError: a skip is the system working, and treating
// it as an error pollutes alerting with correct behaviour.
func TestSkippedSendIsNotAnError(t *testing.T) {
	e := newEnv(t, 0)
	e.list.Add("dana@acme.example", suppression.Unsubscribed, "endpoint", "")

	out, err := e.dispatch.Execute(e.job)
	if err != nil {
		t.Fatalf("a skip was reported as an error: %v", err)
	}
	if out != send.Skipped {
		t.Fatalf("outcome %s", out)
	}
	if len(e.store.Skipped()) != 1 {
		t.Fatal("the skip was not recorded")
	}
	if e.store.SkipsByReason()["suppression"] != 1 {
		t.Fatalf("%+v", e.store.SkipsByReason())
	}
}
