package sequence

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

// The awkward timezones. Half-hour and 45-minute offsets, southern
// hemisphere DST, and zones that changed their rules recently are exactly
// where date arithmetic breaks.
var testZones = []string{
	"UTC",
	"America/New_York",
	"America/Los_Angeles",
	"Europe/London",
	"Europe/Berlin",
	"Asia/Kolkata",       // +05:30
	"Asia/Kathmandu",     // +05:45
	"Australia/Sydney",   // southern-hemisphere DST
	"Australia/Adelaide", // +09:30 with DST
	"Pacific/Chatham",    // +12:45
	"America/Sao_Paulo",  // abolished DST in 2019
	"Asia/Tehran",        // abolished DST in 2022
}

func zones(t *testing.T) []*time.Location {
	t.Helper()
	var out []*time.Location
	for _, name := range testZones {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skipf("timezone database unavailable: %v", err)
		}
		out = append(out, loc)
	}
	return out
}

// --- the property the whole scheduler exists to guarantee -------------------

func TestSendsAlwaysLandInBusinessHours(t *testing.T) {
	// §15.3's property test. Worth noting that the design document's own
	// reference implementation FAILS this: it adds jitter as a final step
	// after the window check, so a slot found at 16:58 plus up to 30
	// minutes of jitter lands at 17:20. It only breaks near the window
	// edge, which is why it survives casual testing.
	locs := zones(t)
	cal := NewStaticCalendar()
	s := NewScheduler(cal, 1)
	w := model.DefaultWindow()

	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20_000; i++ {
		loc := locs[rng.Intn(len(locs))]
		// Span several years so DST transitions in both hemispheres are hit.
		base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		at := base.Add(time.Duration(rng.Int63n(int64(3 * 365 * 24 * time.Hour))))

		next, ok := s.NextSendTime(at, loc, "US", w)
		if !ok {
			t.Fatalf("no send time found from %v in %v", at, loc)
		}

		local := next.In(loc)
		if !s.IsBusinessDay("US", local) {
			t.Fatalf("scheduled on a non-business day: %v (%v)", local, local.Weekday())
		}
		if local.Hour() < w.StartHour {
			t.Fatalf("scheduled before the window: %v", local)
		}
		if local.Hour() >= w.EndHour {
			t.Fatalf("scheduled at or after the window end: %v (window ends at %d:00)", local, w.EndHour)
		}
		if next.Before(at) {
			t.Fatalf("scheduled in the past: %v before %v", next, at)
		}
	}
}

func TestJitterCannotPushPastTheWindowEnd(t *testing.T) {
	// The specific failure the ordering fixes, forced rather than sampled:
	// start one minute before close with half an hour of jitter available.
	loc, _ := time.LoadLocation("America/New_York")
	s := NewScheduler(NewStaticCalendar(), 42)
	w := model.Window{StartHour: 9, EndHour: 17, JitterMax: 30 * time.Minute}

	// A Tuesday.
	at := time.Date(2026, 3, 10, 16, 59, 0, 0, loc)
	for i := 0; i < 2000; i++ {
		next, ok := s.NextSendTime(at, loc, "US", w)
		if !ok {
			t.Fatal("no send time")
		}
		local := next.In(loc)
		if local.Hour() >= 17 {
			t.Fatalf("jitter pushed the send to %v, past the 17:00 window end", local)
		}
	}
}

func TestJitterActuallyVaries(t *testing.T) {
	// Jitter is not decoration: sends at exactly 09:00:00.000 across
	// hundreds of recipients is a machine signature.
	loc, _ := time.LoadLocation("UTC")
	s := NewScheduler(NewStaticCalendar(), 3)
	w := model.DefaultWindow()
	at := time.Date(2026, 3, 10, 6, 0, 0, 0, loc) // before the window

	seen := make(map[time.Time]bool)
	for i := 0; i < 200; i++ {
		next, _ := s.NextSendTime(at, loc, "US", w)
		seen[next] = true
	}
	if len(seen) < 100 {
		t.Fatalf("only %d distinct send times from 200 draws; jitter is not working", len(seen))
	}
}

func TestZeroJitterIsDeterministic(t *testing.T) {
	// Needed for reproducible tests elsewhere, and rand.Int63n(0) panics,
	// so the zero case has to be handled rather than assumed away.
	loc := time.UTC
	s := NewScheduler(NewStaticCalendar(), 1)
	w := model.Window{StartHour: 9, EndHour: 17}
	at := time.Date(2026, 3, 10, 6, 0, 0, 0, loc)

	first, _ := s.NextSendTime(at, loc, "US", w)
	for i := 0; i < 50; i++ {
		got, _ := s.NextSendTime(at, loc, "US", w)
		if !got.Equal(first) {
			t.Fatalf("zero jitter produced %v then %v", first, got)
		}
	}
	if first.Hour() != 9 || first.Minute() != 0 {
		t.Fatalf("got %v, want exactly 09:00", first)
	}
}

// --- business days ----------------------------------------------------------

func TestSkipsWeekends(t *testing.T) {
	loc := time.UTC
	s := NewScheduler(NewStaticCalendar(), 1)
	w := model.Window{StartHour: 9, EndHour: 17}

	// Saturday.
	sat := time.Date(2026, 3, 14, 10, 0, 0, 0, loc)
	next, _ := s.NextSendTime(sat, loc, "US", w)
	if next.Weekday() != time.Monday {
		t.Fatalf("from Saturday got %v (%v)", next, next.Weekday())
	}
}

func TestSkipsCountrySpecificHolidays(t *testing.T) {
	// Emailing a German prospect on a German public holiday is a small but
	// real signal that nobody is paying attention -- and the calendar is
	// per-country, so the same date is a working day in the US.
	loc, _ := time.LoadLocation("Europe/Berlin")
	cal := NewStaticCalendar()
	unity := time.Date(2026, 10, 5, 0, 0, 0, 0, loc) // a Monday, for the test
	cal.Add("DE", unity)

	s := NewScheduler(cal, 1)
	w := model.Window{StartHour: 9, EndHour: 17}
	at := time.Date(2026, 10, 5, 8, 0, 0, 0, loc)

	de, _ := s.NextSendTime(at, loc, "DE", w)
	if de.Day() == 5 {
		t.Fatalf("scheduled on a German holiday: %v", de)
	}

	us, _ := s.NextSendTime(at, loc, "US", w)
	if us.Day() != 5 {
		t.Fatalf("a German holiday blocked a US recipient: %v", us)
	}
}

func TestGivesUpRatherThanSpinningForever(t *testing.T) {
	// A calendar that marks every day a holiday -- a misconfiguration, or
	// a country nobody implemented -- must not hang the worker.
	cal := everyDayIsAHoliday{}
	s := NewScheduler(cal, 1)
	if _, ok := s.NextSendTime(time.Now(), time.UTC, "XX", model.DefaultWindow()); ok {
		t.Fatal("found a send time in a calendar with no business days")
	}
}

type everyDayIsAHoliday struct{}

func (everyDayIsAHoliday) IsHoliday(string, time.Time) bool { return true }

// --- DST and offset arithmetic ----------------------------------------------

func TestSpringForwardDoesNotProduceAnInvalidTime(t *testing.T) {
	// 02:00 does not exist on the US transition date. A window starting
	// then must resolve to a real moment rather than a phantom one.
	loc, _ := time.LoadLocation("America/New_York")
	s := NewScheduler(NewStaticCalendar(), 1)
	w := model.Window{StartHour: 2, EndHour: 10}

	// 8 March 2026 is the US spring-forward date.
	at := time.Date(2026, 3, 8, 0, 30, 0, 0, loc)
	next, ok := s.NextSendTime(at, loc, "US", w)
	if !ok {
		t.Fatal("no send time across the spring-forward boundary")
	}
	// It must be a real instant that round-trips.
	if next.IsZero() || next.In(loc).Format(time.RFC3339) == "" {
		t.Fatalf("got a degenerate time: %v", next)
	}
	t.Logf("spring-forward scheduling resolved to %v", next.In(loc))
}

func TestFallBackDayHasTwentyFiveHours(t *testing.T) {
	// A 25-hour day breaks any "add 24 hours" arithmetic. The scheduler
	// builds the next day from date parts instead.
	loc, _ := time.LoadLocation("America/New_York")
	s := NewScheduler(NewStaticCalendar(), 1)
	w := model.Window{StartHour: 9, EndHour: 17}

	// 1 November 2026 is the US fall-back date (a Sunday), so start
	// Saturday evening and expect Monday.
	at := time.Date(2026, 10, 31, 20, 0, 0, 0, loc)
	next, ok := s.NextSendTime(at, loc, "US", w)
	if !ok {
		t.Fatal("no send time")
	}
	local := next.In(loc)
	if local.Weekday() != time.Monday {
		t.Fatalf("got %v (%v), want Monday", local, local.Weekday())
	}
	if local.Hour() < 9 || local.Hour() >= 17 {
		t.Fatalf("outside the window: %v", local)
	}
}

func TestHalfAndQuarterHourOffsets(t *testing.T) {
	// Kathmandu is +05:45 and Chatham is +12:45. A scheduler that assumes
	// whole-hour offsets lands outside the window in both.
	for _, name := range []string{"Asia/Kathmandu", "Pacific/Chatham", "Asia/Kolkata"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skipf("no tzdata: %v", err)
		}
		s := NewScheduler(NewStaticCalendar(), 1)
		w := model.DefaultWindow()

		at := time.Date(2026, 6, 15, 3, 0, 0, 0, time.UTC)
		next, ok := s.NextSendTime(at, loc, "XX", w)
		if !ok {
			t.Fatalf("%s: no send time", name)
		}
		local := next.In(loc)
		if local.Hour() < w.StartHour || local.Hour() >= w.EndHour {
			t.Fatalf("%s: scheduled at %v, outside the window", name, local)
		}
	}
}

func TestAlreadyInsideTheWindowSchedulesNow(t *testing.T) {
	loc := time.UTC
	s := NewScheduler(NewStaticCalendar(), 1)
	w := model.Window{StartHour: 9, EndHour: 17}

	at := time.Date(2026, 3, 10, 11, 0, 0, 0, loc) // Tuesday 11am
	next, _ := s.NextSendTime(at, loc, "US", w)
	if next.Day() != 10 || next.Hour() != 11 {
		t.Fatalf("got %v, want the same day at 11:00", next)
	}
}

func TestNilTimezoneFallsBackToUTC(t *testing.T) {
	// A contact with no timezone is a data problem, not a reason to panic
	// in the scheduler.
	s := NewScheduler(NewStaticCalendar(), 1)
	if _, ok := s.NextSendTime(time.Now(), nil, "US", model.DefaultWindow()); !ok {
		t.Fatal("no send time with a nil location")
	}
}

// --- window validation ------------------------------------------------------

func TestWindowRejectsJitterLargerThanItself(t *testing.T) {
	// Caught at configuration time rather than producing out-of-window
	// sends at runtime.
	w := model.Window{StartHour: 9, EndHour: 10, JitterMax: 2 * time.Hour}
	if err := w.Valid(); err == nil {
		t.Fatal("accepted two hours of jitter in a one-hour window")
	}
}

func TestWindowRejectsInvertedHours(t *testing.T) {
	if err := (model.Window{StartHour: 17, EndHour: 9}).Valid(); err == nil {
		t.Fatal("accepted an inverted window")
	}
}

func TestDefaultWindowIsValid(t *testing.T) {
	if err := model.DefaultWindow().Valid(); err != nil {
		t.Fatal(err)
	}
}

// --- step delays ------------------------------------------------------------

func TestStepDelayIsMeasuredFromThePreviousSend(t *testing.T) {
	prev := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	step := model.Step{DelayAfter: 72 * time.Hour}
	if got := StepDelay(prev, step); !got.Equal(prev.Add(72 * time.Hour)) {
		t.Fatalf("got %v", got)
	}
}

func TestScheduleStepSnapsIntoTheWindow(t *testing.T) {
	loc := time.UTC
	s := NewScheduler(NewStaticCalendar(), 1)
	w := model.Window{StartHour: 9, EndHour: 17}

	// Tuesday 09:00 plus 72 hours is Friday 09:00.
	prev := time.Date(2026, 3, 10, 9, 0, 0, 0, loc)
	next, ok := s.ScheduleStep(prev, model.Step{DelayAfter: 72 * time.Hour}, loc, "US", w)
	if !ok {
		t.Fatal("no send time")
	}
	if next.Weekday() != time.Friday {
		t.Fatalf("got %v (%v)", next, next.Weekday())
	}
}

func TestDelayLandingOnAWeekendSpillsToMonday(t *testing.T) {
	loc := time.UTC
	s := NewScheduler(NewStaticCalendar(), 1)
	w := model.Window{StartHour: 9, EndHour: 17}

	// Friday + 24h = Saturday.
	prev := time.Date(2026, 3, 13, 9, 0, 0, 0, loc)
	next, _ := s.ScheduleStep(prev, model.Step{DelayAfter: 24 * time.Hour}, loc, "US", w)
	if next.Weekday() != time.Monday {
		t.Fatalf("got %v (%v)", next, next.Weekday())
	}
}

// --- backpressure -----------------------------------------------------------

func pending(id, mailbox string, at time.Time) PendingSend {
	return PendingSend{EnrollmentID: id, MailboxID: mailbox, DomainID: "d1", ScheduledAt: at}
}

func TestSpillsRatherThanCompressing(t *testing.T) {
	// Sequence timing is a preference; reputation is the business.
	base := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	var q []PendingSend
	for i := 0; i < 100; i++ {
		q = append(q, pending(string(rune('a'+i%26))+string(rune('0'+i/26)), "mb1", base.Add(time.Duration(i)*time.Minute)))
	}

	p := Apply(q, map[string]Capacity{
		"mb1": {MailboxID: "mb1", MailboxRemaining: 30, DomainRemaining: 1000, ThrottleFactor: 1},
	})

	if len(p.Send) != 30 {
		t.Fatalf("scheduled %d sends against a capacity of 30", len(p.Send))
	}
	if len(p.Deferred) != 70 {
		t.Fatalf("deferred %d", len(p.Deferred))
	}
}

func TestDeferralIsSurfacedLoudly(t *testing.T) {
	// "You are enrolling faster than your mailbox capacity can deliver" is
	// the product doing its job.
	base := time.Now()
	q := []PendingSend{pending("a", "mb1", base), pending("b", "mb1", base)}
	p := Apply(q, map[string]Capacity{
		"mb1": {MailboxID: "mb1", MailboxRemaining: 1, DomainRemaining: 100, ThrottleFactor: 1},
	})

	if len(p.Notices) == 0 {
		t.Fatal("deferred silently")
	}
	if !strings.Contains(p.Notices[0], "enrolling faster") {
		t.Fatalf("notice = %q", p.Notices[0])
	}
}

func TestHaltedDomainGetsADistinctNotice(t *testing.T) {
	// "Deferred, add mailboxes" and "your domain is halted" call for
	// completely different responses.
	q := []PendingSend{pending("a", "mb1", time.Now())}
	p := Apply(q, map[string]Capacity{
		"mb1": {MailboxID: "mb1", MailboxRemaining: 50, DomainRemaining: 100, ThrottleFactor: 0},
	})

	if len(p.Send) != 0 {
		t.Fatal("sent from a halted domain")
	}
	if !strings.Contains(p.Notices[0], "halted") {
		t.Fatalf("notice = %q", p.Notices[0])
	}
}

func TestDomainCapBindsBeforeMailboxCap(t *testing.T) {
	q := make([]PendingSend, 50)
	base := time.Now()
	for i := range q {
		q[i] = pending(string(rune('a'+i%26)), "mb1", base)
	}
	p := Apply(q, map[string]Capacity{
		"mb1": {MailboxID: "mb1", MailboxRemaining: 40, DomainRemaining: 5, ThrottleFactor: 1},
	})
	if len(p.Send) != 5 {
		t.Fatalf("sent %d against a domain remainder of 5", len(p.Send))
	}
}

func TestThrottleReducesThroughput(t *testing.T) {
	q := make([]PendingSend, 100)
	base := time.Now()
	for i := range q {
		q[i] = PendingSend{EnrollmentID: string(rune(i)), MailboxID: "mb1", ScheduledAt: base}
	}
	full := Apply(q, map[string]Capacity{
		"mb1": {MailboxRemaining: 40, DomainRemaining: 1000, ThrottleFactor: 1},
	})
	half := Apply(q, map[string]Capacity{
		"mb1": {MailboxRemaining: 40, DomainRemaining: 1000, ThrottleFactor: 0.5},
	})
	if len(half.Send) != len(full.Send)/2 {
		t.Fatalf("throttled to %d from %d", len(half.Send), len(full.Send))
	}
}

func TestUnknownMailboxIsDeferredNotSent(t *testing.T) {
	// Sending against capacity nobody could verify is exactly what the
	// limits exist to prevent.
	p := Apply([]PendingSend{pending("a", "ghost", time.Now())}, map[string]Capacity{})
	if len(p.Send) != 0 {
		t.Fatal("sent from an unknown mailbox")
	}
	if len(p.Deferred) != 1 {
		t.Fatal("did not defer")
	}
}

func TestOrderingIsDeterministicAndOldestFirst(t *testing.T) {
	// Without it, which sends get deferred varies run to run, and a
	// prospect can sit at the back of the queue indefinitely through pure
	// map-iteration luck.
	base := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	q := []PendingSend{
		pending("c", "mb1", base.Add(2*time.Hour)),
		pending("a", "mb1", base),
		pending("b", "mb1", base.Add(time.Hour)),
	}
	caps := map[string]Capacity{"mb1": {MailboxRemaining: 2, DomainRemaining: 10, ThrottleFactor: 1}}

	first := Apply(q, caps)
	for i := 0; i < 50; i++ {
		got := Apply(q, caps)
		for j := range got.Send {
			if got.Send[j].EnrollmentID != first.Send[j].EnrollmentID {
				t.Fatal("ordering varied between runs")
			}
		}
	}
	if first.Send[0].EnrollmentID != "a" || first.Send[1].EnrollmentID != "b" {
		t.Fatalf("not oldest-first: %v", []string{first.Send[0].EnrollmentID, first.Send[1].EnrollmentID})
	}
}

func TestCapacityEffectiveNeverNegative(t *testing.T) {
	c := Capacity{MailboxRemaining: -5, DomainRemaining: 100, ThrottleFactor: 1}
	if got := c.Effective(); got != 0 {
		t.Fatalf("got %d", got)
	}
}

// --- spill estimation -------------------------------------------------------

func TestBacklogThatNeverClearsIsReportedAsSuch(t *testing.T) {
	// The interesting case. A large finite number invites waiting; "never
	// clears" invites fixing.
	e := EstimateSpill(5000, 40, 100)
	if !e.NeverClears {
		t.Fatalf("got %+v", e)
	}
	if !strings.Contains(e.Recommendation, "never clears") {
		t.Fatalf("recommendation = %q", e.Recommendation)
	}
	t.Logf("%s", e.Recommendation)
}

func TestBacklogClearanceArithmetic(t *testing.T) {
	e := EstimateSpill(300, 100, 50) // net drain 50/day
	if e.NeverClears || e.DaysToClear != 6 {
		t.Fatalf("got %+v", e)
	}
}

func TestEqualInflowAndCapacityNeverClears(t *testing.T) {
	// The boundary: a backlog with zero net drain stays exactly where it
	// is forever.
	if e := EstimateSpill(100, 50, 50); !e.NeverClears {
		t.Fatalf("got %+v", e)
	}
}

func TestEnrollmentAdmissionRefusesAnImpossibleBacklog(t *testing.T) {
	// Refusing at enrolment is kinder than accepting and deferring
	// forever.
	a := AdmitEnrollment(5000, 40, 10)
	if a.Allowed {
		t.Fatal("admitted an enrolment into a 125-day backlog")
	}
	t.Logf("%s", a.Reason)
}

func TestEnrollmentAdmissionRefusesWithNoCapacity(t *testing.T) {
	a := AdmitEnrollment(0, 0, 10)
	if a.Allowed || !strings.Contains(a.Reason, "no warmed mailbox") {
		t.Fatalf("got %+v", a)
	}
}

func TestEnrollmentAdmissionAllowsAReasonableQueue(t *testing.T) {
	if a := AdmitEnrollment(100, 40, 10); !a.Allowed {
		t.Fatalf("refused: %s", a.Reason)
	}
}

// docOrderNextSendTime reproduces the design document's §5.2 reference
// implementation, which applies jitter as a final step after the window
// check.
//
// Present only so the claim in docs/notes-on-the-spec.md is demonstrated
// rather than asserted.
func docOrderNextSendTime(s *Scheduler, after time.Time, tz *time.Location, w model.Window) time.Time {
	t := after.In(tz)
	for i := 0; i < MaxSearchDays; i++ {
		if !s.IsBusinessDay("US", t) {
			t = startOfDay(t.AddDate(0, 0, 1), tz)
			continue
		}
		if t.Before(atHour(t, w.StartHour, tz)) {
			t = atHour(t, w.StartHour, tz)
		}
		if t.After(atHour(t, w.EndHour, tz)) {
			t = startOfDay(t.AddDate(0, 0, 1), tz)
			continue
		}
		// The document's final line: jitter added after every check.
		return t.Add(time.Duration(s.rng.Int63n(int64(w.JitterMax))))
	}
	return t
}

func TestDocumentsOwnImplementationViolatesItsOwnPropertyTest(t *testing.T) {
	// §15.3 asserts local.Hour() < window.EndHour. §5.2's reference
	// implementation adds jitter after the window check, so a slot near
	// the edge is pushed past it. Both cannot be right.
	loc, _ := time.LoadLocation("America/New_York")
	s := NewScheduler(NewStaticCalendar(), 99)
	w := model.Window{StartHour: 9, EndHour: 17, JitterMax: 30 * time.Minute}
	at := time.Date(2026, 3, 10, 16, 59, 0, 0, loc) // Tuesday, one minute to close

	violations := 0
	for i := 0; i < 1000; i++ {
		got := docOrderNextSendTime(s, at, loc, w).In(loc)
		if got.Hour() >= w.EndHour {
			violations++
		}
	}
	t.Logf("the document's ordering put %d of 1000 sends past the 17:00 window end", violations)
	if violations == 0 {
		t.Fatal("expected the documented ordering to violate the property; it did not, so this note is stale")
	}

	// And this implementation never does.
	for i := 0; i < 1000; i++ {
		got, ok := s.NextSendTime(at, loc, "US", w)
		if !ok {
			t.Fatal("no send time")
		}
		if got.In(loc).Hour() >= w.EndHour {
			t.Fatalf("our scheduler produced %v, past the window end", got.In(loc))
		}
	}
}
