package meeting

import (
	"testing"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/store"
)

func now() time.Time { return time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC) }

type recorder struct{ msgs []string }

func (r *recorder) Notify(_ string, m string) { r.msgs = append(r.msgs, m) }

// acme sets up four contacts at one account, enrolled by three different
// reps in two different sequences.
func acme(t *testing.T) (*store.Store, *recorder, *Handler) {
	t.Helper()
	s := store.New()

	for i, email := range []string{"dana@acme.example", "raj@acme.example", "kim@acme.example", "lee@acme.example"} {
		id := "c" + string(rune('1'+i))
		s.PutContact(model.Contact{ID: id, AccountID: "acme", Email: email, Country: "US"})
		seq := "seq-a"
		if i%2 == 1 {
			seq = "seq-b"
		}
		s.PutEnrollment(model.Enrollment{
			ID: "en" + string(rune('1'+i)), SequenceID: seq, ContactID: id,
			AccountID: "acme", State: model.StateActive, CurrentStep: 2,
		})
		s.EnqueueJob("en"+string(rune('1'+i)), "job-"+id)
	}

	rec := &recorder{}
	return s, rec, NewHandler(s, DefaultPolicy(), rec)
}

// TestBookingPausesTheWholeAccount is §8.2: continuing to cold-email three
// other people at Acme is the single most visible failure a sequencer can
// produce.
func TestBookingPausesTheWholeAccount(t *testing.T) {
	s, rec, h := acme(t)

	out, err := h.OnBooked(Booked{
		AttendeeEmail: "dana@acme.example",
		StartsAt:      now().AddDate(0, 0, 5),
		Source:        SourceSchedulingLink,
		SourceRef:     "cal-evt-991",
		DetectedAt:    now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(out.Paused) != 4 {
		t.Fatalf("paused %d of 4 enrolments; the other three people at Acme would still have been cold-emailed", len(out.Paused))
	}
	if out.CancelledJobs != 4 {
		t.Fatalf("cancelled %d queued jobs; marking the state is not enough on its own", out.CancelledJobs)
	}
	for _, id := range out.Paused {
		en, _ := s.Enrollment(id)
		if en.State != model.StatePaused {
			t.Errorf("%s is %s", id, en.State)
		}
		if en.PauseReason.Kind != "meeting_booked" {
			t.Errorf("%s pause reason %q", id, en.PauseReason.Kind)
		}
		// The evidence has to be there, or a human clears the pause
		// without knowing what it was for.
		if en.PauseReason.Evidence != "scheduling_link:cal-evt-991" {
			t.Errorf("%s evidence %q", id, en.PauseReason.Evidence)
		}
	}
	if len(rec.msgs) != 1 {
		t.Fatalf("%d notifications", len(rec.msgs))
	}
}

// TestPauseIsNotAStop is §8.4: the position has to survive, so a resume
// picks up at step 3 rather than starting over.
func TestPauseIsNotAStop(t *testing.T) {
	s, _, h := acme(t)
	_, _ = h.OnBooked(Booked{
		AttendeeEmail: "dana@acme.example", StartsAt: now().AddDate(0, 0, 5),
		Source: SourceSchedulingLink, SourceRef: "x",
	})

	en, _ := s.Enrollment("en1")
	if en.State.Terminal() {
		t.Fatal("a booking produced a terminal state; re-enrolment would restart from step 1")
	}
	if en.CurrentStep != 2 {
		t.Fatalf("step position lost: %d", en.CurrentStep)
	}
	if err := PauseIsNotAStop(en); err != nil {
		t.Fatal(err)
	}

	if err := s.Resume("en1", now().AddDate(0, 0, 40)); err != nil {
		t.Fatal(err)
	}
	en, _ = s.Enrollment("en1")
	if en.State != model.StateActive || en.CurrentStep != 2 {
		t.Fatalf("resume did not pick up where it left off: %+v", en)
	}
}

// TestDistantMeetingPausesNow: waiting until the meeting starts means three
// more cold emails go out in the meantime.
func TestDistantMeetingPausesNow(t *testing.T) {
	s, _, h := acme(t)
	out, err := h.OnBooked(Booked{
		AttendeeEmail: "dana@acme.example",
		StartsAt:      now().AddDate(0, 3, 0),
		Source:        SourceCalendarEvent, SourceRef: "evt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Paused) != 4 {
		t.Fatalf("paused %d", len(out.Paused))
	}
	en, _ := s.Enrollment("en2")
	if en.State != model.StatePaused {
		t.Fatal("a meeting three months out did not pause anything now")
	}
}

// TestInferredReplyPromptsRatherThanPauses: "how about Thursday?" is not a
// booking.
func TestInferredReplyPromptsRatherThanPauses(t *testing.T) {
	s, rec, h := acme(t)
	out, err := h.OnBooked(Booked{
		AttendeeEmail: "dana@acme.example", StartsAt: now().AddDate(0, 0, 3),
		Source: SourceInferredReply, SourceRef: "reply-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsConfirmation {
		t.Fatal("a reply-inferred booking paused the account automatically")
	}
	if len(out.Paused) != 0 {
		t.Fatalf("paused %d", len(out.Paused))
	}
	en, _ := s.Enrollment("en2")
	if en.State != model.StateActive {
		t.Fatal("state changed on an unconfirmed booking")
	}
	if len(rec.msgs) != 1 {
		t.Fatal("the rep was not prompted")
	}
}

// TestBookedWithADifferentRepStillPauses: the account has engaged, and that
// is what matters.
func TestBookedWithADifferentRepStillPauses(t *testing.T) {
	_, _, h := acme(t)
	// dana is in seq-a; the pause must reach seq-b's enrolments too.
	out, _ := h.OnBooked(Booked{
		AttendeeEmail: "dana@acme.example", StartsAt: now().AddDate(0, 0, 2),
		Source: SourceCRM, SourceRef: "crm-1",
	})
	if len(out.Paused) != 4 {
		t.Fatalf("paused %d; a booking with one rep must pause the other reps' sequences", len(out.Paused))
	}
}

// TestPerSequenceOverrideStillPausesTheAttendee: account-level is the
// default with a per-sequence override, and the override never extends to
// the person you are about to meet.
func TestPerSequenceOverrideStillPausesTheAttendee(t *testing.T) {
	s, _, h := acme(t)
	h.SetContactLevelOnly("seq-b", true)

	out, _ := h.OnBooked(Booked{
		AttendeeEmail: "raj@acme.example", // raj is in seq-b
		StartsAt:      now().AddDate(0, 0, 2),
		Source:        SourceSchedulingLink, SourceRef: "x",
	})

	// seq-a enrolments (c1, c3) still pause; seq-b's other contact (c4)
	// does not; raj himself does.
	paused := map[string]bool{}
	for _, id := range out.Paused {
		paused[id] = true
	}
	if !paused["en2"] {
		t.Fatal("the attendee's own enrolment was not paused")
	}
	if paused["en4"] {
		t.Fatal("the override did not apply to the other seq-b contact")
	}
	if !paused["en1"] || !paused["en3"] {
		t.Fatal("the override leaked into seq-a")
	}
	en, _ := s.Enrollment("en4")
	if en.State != model.StateActive {
		t.Fatalf("en4 is %s", en.State)
	}
}

func TestContactLevelPolicy(t *testing.T) {
	s := store.New()
	s.PutContact(model.Contact{ID: "c1", AccountID: "acme", Email: "dana@acme.example"})
	s.PutContact(model.Contact{ID: "c2", AccountID: "acme", Email: "raj@acme.example"})
	s.PutEnrollment(model.Enrollment{ID: "en1", ContactID: "c1", AccountID: "acme", State: model.StateActive})
	s.PutEnrollment(model.Enrollment{ID: "en2", ContactID: "c2", AccountID: "acme", State: model.StateActive})

	p := DefaultPolicy()
	p.AccountLevel = false
	h := NewHandler(s, p, nil)

	out, _ := h.OnBooked(Booked{
		AttendeeEmail: "dana@acme.example", StartsAt: now(),
		Source: SourceSchedulingLink, SourceRef: "x",
	})
	if len(out.Paused) != 1 {
		t.Fatalf("paused %d with account-level off", len(out.Paused))
	}
}

func TestUnknownAttendeeIsAnError(t *testing.T) {
	_, _, h := acme(t)
	if _, err := h.OnBooked(Booked{
		AttendeeEmail: "stranger@elsewhere.example",
		Source:        SourceSchedulingLink, SourceRef: "x",
	}); err == nil {
		t.Fatal("a booking for an unknown attendee was applied silently")
	}
}

// TestCancelledAndNoShowAreDifferent is §8.3.
func TestCancelledAndNoShowAreDifferent(t *testing.T) {
	_, _, h := acme(t)

	c := h.OnNotHeld(Cancelled, now())
	if !c.ResumeColdOutreach {
		t.Fatal("a cancellation did not resume cold outreach")
	}
	if !c.ResumeAt.After(now().AddDate(0, 0, 6)) {
		t.Fatalf("resumed %s after cancellation; an instant resume reads badly", c.ResumeAt.Sub(now()))
	}

	n := h.OnNotHeld(NoShow, now())
	if n.ResumeColdOutreach {
		t.Fatal("a no-show resumed cold outreach; the account engaged and then missed, which is not the same as never engaging")
	}
	if !n.FollowUpSequence {
		t.Fatal("a no-show produced no follow-up")
	}
}

func TestTerminalEnrolmentsAreNotPausedBackIntoLife(t *testing.T) {
	s := store.New()
	s.PutContact(model.Contact{ID: "c1", AccountID: "acme", Email: "dana@acme.example"})
	s.PutEnrollment(model.Enrollment{ID: "en1", ContactID: "c1", AccountID: "acme", State: model.StateSuppressed})
	h := NewHandler(s, DefaultPolicy(), nil)

	out, _ := h.OnBooked(Booked{
		AttendeeEmail: "dana@acme.example", StartsAt: now(),
		Source: SourceSchedulingLink, SourceRef: "x",
	})
	if len(out.Paused) != 0 {
		t.Fatal("a suppressed enrolment was paused; a resume button would then reanimate someone who unsubscribed")
	}
	en, _ := s.Enrollment("en1")
	if en.State != model.StateSuppressed {
		t.Fatalf("state changed to %s", en.State)
	}
}

func TestReliableSources(t *testing.T) {
	for _, s := range []DetectionSource{SourceSchedulingLink, SourceCalendarEvent, SourceCRM} {
		if !s.Reliable() {
			t.Errorf("%s is not reliable", s)
		}
	}
	if SourceInferredReply.Reliable() {
		t.Fatal("a reply-inferred booking is treated as reliable")
	}
}
