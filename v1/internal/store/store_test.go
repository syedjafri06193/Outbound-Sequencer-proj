package store

import (
	"sync"
	"testing"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

func now() time.Time { return time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC) }

func TestSendKeyIsStable(t *testing.T) {
	if SendKey("en1", 3) != "send:en1:3" {
		t.Fatalf("%s", SendKey("en1", 3))
	}
	if SendKey("en1", 3) == SendKey("en1", 4) {
		t.Fatal("two steps share a key")
	}
	if SendKey("en1", 3) == SendKey("en2", 3) {
		t.Fatal("two enrolments share a key")
	}
}

func TestClaimSendKeyIsOnce(t *testing.T) {
	s := New()
	ok, err := s.ClaimSendKey("k", now())
	if err != nil || !ok {
		t.Fatal("first claim failed")
	}
	ok, _ = s.ClaimSendKey("k", now())
	if ok {
		t.Fatal("second claim succeeded")
	}
}

func TestConcurrentClaimsYieldExactlyOneWinner(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := s.ClaimSendKey("k", now()); ok {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d workers claimed the same key", winners)
	}
}

// TestStaleClaimsSurfaceCrashSurvivors: a claim that never completed is a
// send nobody knows the fate of, so it goes to a human rather than a retry.
func TestStaleClaimsSurfaceCrashSurvivors(t *testing.T) {
	s := New()
	_, _ = s.ClaimSendKey("send:en1:1", now().Add(-3*time.Hour))
	_, _ = s.ClaimSendKey("send:en2:1", now().Add(-3*time.Hour))
	_ = s.CompleteSendKey("send:en2:1", "<m@x>")
	_, _ = s.ClaimSendKey("send:en3:1", now())

	stale := s.StaleClaims(time.Hour, now())
	if len(stale) != 1 || stale[0].Key != "send:en1:1" {
		t.Fatalf("%+v", stale)
	}
}

func TestCompleteUnclaimedKeyIsAnError(t *testing.T) {
	if err := New().CompleteSendKey("nope", "<m@x>"); err == nil {
		t.Fatal("completed a key that was never claimed")
	}
}

func TestPauseKeepsThePosition(t *testing.T) {
	s := New()
	s.PutEnrollment(model.Enrollment{ID: "en1", State: model.StateActive, CurrentStep: 3})

	if err := s.Pause("en1", model.PauseReason{Kind: "ooo", ExpiresAt: now().AddDate(0, 0, 7)}); err != nil {
		t.Fatal(err)
	}
	en, _ := s.Enrollment("en1")
	if en.State != model.StatePaused || en.CurrentStep != 3 {
		t.Fatalf("%+v", en)
	}

	if err := s.Resume("en1", now().AddDate(0, 0, 8)); err != nil {
		t.Fatal(err)
	}
	en, _ = s.Enrollment("en1")
	if en.State != model.StateActive || en.CurrentStep != 3 {
		t.Fatalf("resume lost the position: %+v", en)
	}
	if !en.PausedUntil.IsZero() || en.PauseReason.Kind != "" {
		t.Fatalf("stale pause state after resume: %+v", en)
	}
}

func TestTerminalEnrolmentsCannotBePausedBackIntoLife(t *testing.T) {
	s := New()
	s.PutEnrollment(model.Enrollment{ID: "en1", State: model.StateSuppressed})
	if err := s.Pause("en1", model.PauseReason{Kind: "meeting_booked"}); err != nil {
		t.Fatal(err)
	}
	en, _ := s.Enrollment("en1")
	if en.State != model.StateSuppressed {
		t.Fatalf("a suppressed enrolment became %s; a resume would then reanimate someone who unsubscribed", en.State)
	}
}

func TestResumeRefusesNonPaused(t *testing.T) {
	s := New()
	s.PutEnrollment(model.Enrollment{ID: "en1", State: model.StateActive})
	if err := s.Resume("en1", now()); err == nil {
		t.Fatal("resumed an active enrolment")
	}
}

func TestEnrollmentsAreReturnedByCopy(t *testing.T) {
	s := New()
	s.PutEnrollment(model.Enrollment{ID: "en1", State: model.StateActive, CurrentStep: 1})
	en, _ := s.Enrollment("en1")
	en.CurrentStep = 99
	again, _ := s.Enrollment("en1")
	if again.CurrentStep != 1 {
		t.Fatal("a caller mutated stored state through a returned value")
	}
}

func TestActiveEnrollmentsExcludeTerminal(t *testing.T) {
	s := New()
	s.PutEnrollment(model.Enrollment{ID: "a", AccountID: "acme", State: model.StateActive})
	s.PutEnrollment(model.Enrollment{ID: "b", AccountID: "acme", State: model.StateStopped})
	s.PutEnrollment(model.Enrollment{ID: "c", AccountID: "acme", State: model.StatePaused})

	got, _ := s.ActiveEnrollmentsByAccount("acme")
	if len(got) != 2 {
		t.Fatalf("%d, want 2 (paused counts, stopped does not)", len(got))
	}
}

func TestContactLookupUsesTheCanonicalAddress(t *testing.T) {
	s := New()
	s.PutContact(model.Contact{ID: "c1", Email: "First.Last+sales@gmail.com"})
	for _, variant := range []string{"firstlast@gmail.com", "first.last@googlemail.com", "FIRSTLAST@GMAIL.COM"} {
		if _, ok := s.ContactByEmail(variant); !ok {
			t.Errorf("%s did not resolve", variant)
		}
	}
}

func TestCancelQueuedRemovesJobs(t *testing.T) {
	s := New()
	s.EnqueueJob("en1", "j1")
	s.EnqueueJob("en1", "j2")
	s.EnqueueJob("en2", "j3")

	n, _ := s.CancelQueued("en1")
	if n != 2 {
		t.Fatalf("cancelled %d", n)
	}
	if s.JobIsLive("en1", "j1") {
		t.Fatal("a cancelled job is still live")
	}
	if !s.JobIsLive("en2", "j3") {
		t.Fatal("cancelled another enrolment's job")
	}
}

func TestMarkRepliedIsIdempotentAndSetsState(t *testing.T) {
	s := New()
	s.PutEnrollment(model.Enrollment{ID: "en1", State: model.StateActive})
	s.MarkReplied("en1", now())
	s.MarkReplied("en1", now().Add(time.Hour))

	if !s.HasReplied("en1") {
		t.Fatal("not recorded")
	}
	en, _ := s.Enrollment("en1")
	if en.State != model.StateReplied {
		t.Fatalf("state %s", en.State)
	}
}

func TestBookingExpires(t *testing.T) {
	s := New()
	s.MarkBooked("acme", now().AddDate(0, 0, 30))
	if !s.AccountHasBooking("acme", now()) {
		t.Fatal("not booked")
	}
	if s.AccountHasBooking("acme", now().AddDate(0, 0, 31)) {
		t.Fatal("booking did not expire")
	}
	// A longer cooldown extends; a shorter one does not truncate.
	s.MarkBooked("acme", now().AddDate(0, 0, 10))
	if !s.AccountHasBooking("acme", now().AddDate(0, 0, 20)) {
		t.Fatal("a shorter booking truncated a longer one")
	}
}

func TestSentIndexNormalisesMessageIDs(t *testing.T) {
	s := New()
	s.RecordSent("en1", 1, "<Step1@Ours.Example>", "thr-1", now())

	for _, id := range []string{"<step1@ours.example>", "step1@ours.example", " <Step1@Ours.Example> "} {
		if _, ok := s.BySentMessageID(id); !ok {
			t.Errorf("%q did not resolve", id)
		}
	}
	if _, ok := s.BySentThreadID("thr-1"); !ok {
		t.Fatal("thread id did not resolve")
	}
}

func TestSkipsByReason(t *testing.T) {
	s := New()
	s.RecordSkipped("en1", 1, "suppression", now())
	s.RecordSkipped("en2", 1, "suppression", now())
	s.RecordSkipped("en3", 1, "ratelimit", now())

	got := s.SkipsByReason()
	if got["suppression"] != 2 || got["ratelimit"] != 1 {
		t.Fatalf("%+v", got)
	}
}

// TestLockEnrollmentsIsDeadlockFree: an account-level pause takes several
// locks while individual dispatches hold single ones.
func TestLockEnrollmentsIsDeadlockFree(t *testing.T) {
	s := New()
	ids := []string{"c", "a", "b", "a"} // unsorted, with a duplicate

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			release := s.LockEnrollments(ids)
			release()
		}
	}()
	go func() {
		for i := 0; i < 200; i++ {
			r := s.LockEnrollment("b")
			r()
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlocked")
	}
}

func TestLockNamespacesAreIndependent(t *testing.T) {
	s := New()
	release := s.LockContact("a@example.com")
	defer release()

	done := make(chan struct{})
	go func() {
		r := s.LockEnrollment("a@example.com")
		r()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the contact and enrolment lock namespaces collided")
	}
}

func TestContactLockCollapsesGmailVariants(t *testing.T) {
	s := New()
	release := s.LockContact("first.last@gmail.com")

	blocked := make(chan struct{})
	go func() {
		r := s.LockContact("firstlast@googlemail.com")
		r()
		close(blocked)
	}()

	select {
	case <-blocked:
		t.Fatal("two addresses reaching one inbox took two different locks")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	<-blocked
}
