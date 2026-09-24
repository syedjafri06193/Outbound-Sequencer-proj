package deliverability

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

func pm(domain string, sent, inboxed, complaints, bounces int) Metrics {
	return Metrics{
		DomainID: domain, Sent: sent, Inboxed: inboxed,
		Complaints: complaints, Bounces: bounces, Source: SourcePostmaster,
	}
}

// --- the arithmetic that justifies everything -------------------------------

func TestComplaintsAllowedAtRealVolumes(t *testing.T) {
	// The table operators do not believe until they see the integer. At a
	// thousand inboxed emails a day, ONE complaint is the target and FOUR
	// is over the hard ceiling.
	cases := []struct {
		inboxed          int
		atTarget, atCeil int
	}{
		{500, 0, 1},
		{1000, 1, 3},
		{5000, 5, 15},
		{10000, 10, 30},
	}
	for _, c := range cases {
		gotTarget := ComplaintsAllowed(c.inboxed, GoogleTargetRate)
		gotCeil := ComplaintsAllowed(c.inboxed, GoogleCeilingRate)
		if gotTarget != c.atTarget || gotCeil != c.atCeil {
			t.Errorf("%d inboxed: got %d/%d allowed, want %d/%d",
				c.inboxed, gotTarget, gotCeil, c.atTarget, c.atCeil)
		}
	}
}

func TestOneComplaintAtFiveHundredIsAlreadyOverTarget(t *testing.T) {
	// The floor, not round, matters here. At 500 inboxed a single
	// complaint is 0.2% -- already double the target -- so the allowance
	// must report zero rather than rounding up to one.
	if got := ComplaintsAllowed(500, GoogleTargetRate); got != 0 {
		t.Fatalf("got %d, want 0: one complaint at 500 inboxed is 0.2%%", got)
	}
}

func TestTheDenominatorDeathSpiral(t *testing.T) {
	// Identical sending behaviour, identical complaint counts, and the
	// rate quadruples in three days because the denominator collapsed.
	//
	// This is why a breaker watching raw complaint counts misses the
	// spiral entirely: the counts never change.
	proj := ProjectSpiral(10_000, 9_000, 27, 0.33, 4)

	for _, p := range proj {
		t.Logf("day %d: sent %d, inboxed %d, complaints %d -> %.2f%%",
			p.Day, p.Sent, p.Inboxed, p.Complaints, p.Rate*100)
	}

	if len(proj) != 4 {
		t.Fatalf("got %d days", len(proj))
	}
	// Complaints never change.
	for _, p := range proj {
		if p.Complaints != 27 || p.Sent != 10_000 {
			t.Fatalf("behaviour changed: %+v", p)
		}
	}
	// The rate at least triples over the window.
	if proj[3].Rate < proj[0].Rate*3 {
		t.Fatalf("rate went %.4f -> %.4f; expected at least a 3x collapse", proj[0].Rate, proj[3].Rate)
	}
	if proj[0].Rate < GoogleCeilingRate-1e-9 {
		t.Fatalf("day 1 rate %.4f should already be at the 0.3%% ceiling", proj[0].Rate)
	}
}

func TestBreakerCatchesTheSpiralOnDayOne(t *testing.T) {
	// The point of halting at 0.08%: by the time Postmaster shows 0.3%,
	// the spiral is running. The breaker must fire on the first day of
	// the sequence above, not the fourth.
	b := DefaultBudget()
	proj := ProjectSpiral(10_000, 9_000, 27, 0.33, 4)

	d := b.Evaluate(pm("d1", proj[0].Sent, proj[0].Inboxed, proj[0].Complaints, 0))
	if d.Action != HaltDomain {
		t.Fatalf("day 1 action = %v (%s); expected a halt", d.Action, d.Reason)
	}
	t.Logf("day 1 halt: %s", d.Reason)
}

// --- the complaint-rate denominator -----------------------------------------

func TestComplaintRateRefusesToGuessTheDenominator(t *testing.T) {
	// The denominator is inboxed mail, which the sender cannot observe. A
	// rate computed against *sent* mail is too low by exactly the factor
	// the death spiral is made of, so returning one would be worse than
	// returning nothing.
	local := Metrics{Sent: 10_000, Inboxed: 9_000, Complaints: 27, Source: SourceLocal}
	if _, ok := local.ComplaintRate(); ok {
		t.Fatal("computed a complaint rate from locally-counted metrics")
	}

	fromGoogle := local
	fromGoogle.Source = SourcePostmaster
	rate, ok := fromGoogle.ComplaintRate()
	if !ok {
		t.Fatal("refused a rate from Postmaster data")
	}
	if rate < 0.0029 || rate > 0.0031 {
		t.Fatalf("rate = %.4f, want 0.003", rate)
	}
}

func TestBlindBreakerSaysSoRatherThanReportingFine(t *testing.T) {
	// A missing denominator means the breaker cannot see. Reporting
	// Continue with no explanation would look identical to a healthy
	// domain.
	d := DefaultBudget().Evaluate(Metrics{DomainID: "d", Sent: 5000, Complaints: 50, Source: SourceLocal})
	if d.Action != Continue {
		t.Fatalf("action = %v", d.Action)
	}
	if !strings.Contains(d.Reason, "Postmaster") {
		t.Fatalf("reason does not explain the blindness: %q", d.Reason)
	}
}

func TestBounceRateUsesSentNotInboxed(t *testing.T) {
	// A bounce means the message never reached an inbox, so inboxed is the
	// wrong denominator -- and the sender does observe sends.
	m := Metrics{Sent: 1000, Inboxed: 500, Bounces: 20, Source: SourceLocal}
	rate, ok := m.BounceRate()
	if !ok || rate != 0.02 {
		t.Fatalf("rate = %v ok = %v, want 0.02", rate, ok)
	}
}

// --- the breaker ------------------------------------------------------------

func TestHaltsWellBelowTheCeiling(t *testing.T) {
	b := DefaultBudget()
	if b.HaltThreshold >= GoogleTargetRate {
		t.Fatalf("halt threshold %.4f is not below Google's %.4f target", b.HaltThreshold, GoogleTargetRate)
	}
	if b.HaltThreshold >= GoogleCeilingRate/3 {
		t.Fatalf("halt threshold %.4f is not comfortably below the %.4f ceiling", b.HaltThreshold, GoogleCeilingRate)
	}

	// 0.08% on 10,000 inboxed is 8 complaints.
	d := b.Evaluate(pm("d", 10_000, 10_000, 8, 0))
	if d.Action != HaltDomain {
		t.Fatalf("8 complaints of 10,000 gave %v", d.Action)
	}
	if !d.RequiresHumanAck {
		t.Fatal("a halt did not require acknowledgment")
	}
}

func TestThrottlesBeforeHalting(t *testing.T) {
	b := DefaultBudget()
	d := b.Evaluate(pm("d", 10_000, 10_000, 6, 0)) // 0.06%
	if d.Action != ThrottleDomain {
		t.Fatalf("action = %v (%s)", d.Action, d.Reason)
	}
	if d.Factor != b.ThrottleFactor {
		t.Fatalf("factor = %v", d.Factor)
	}
}

func TestContinuesWhenHealthy(t *testing.T) {
	d := DefaultBudget().Evaluate(pm("d", 10_000, 10_000, 2, 0)) // 0.02%
	if d.Action != Continue {
		t.Fatalf("action = %v (%s)", d.Action, d.Reason)
	}
}

func TestIgnoresTinySamples(t *testing.T) {
	// One complaint out of ten is 10%, and acting on it would halt every
	// new domain on its first day.
	d := DefaultBudget().Evaluate(pm("d", 10, 10, 1, 0))
	if d.Action != Continue {
		t.Fatalf("action = %v on a 10-message sample", d.Action)
	}
	if !strings.Contains(d.Reason, "insufficient volume") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestBouncesHaltIndependentlyOfComplaints(t *testing.T) {
	// High bounces usually mean list-quality problems that produce
	// complaints next, so this is a leading indicator -- and it is
	// actionable without waiting for Postmaster data.
	b := DefaultBudget()
	d := b.Evaluate(Metrics{DomainID: "d", Sent: 5000, Bounces: 150, Source: SourceLocal}) // 3%
	if d.Action != HaltDomain {
		t.Fatalf("3%% bounce rate gave %v (%s)", d.Action, d.Reason)
	}
	if !strings.Contains(d.Reason, "bounce") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestBounceWarnThrottles(t *testing.T) {
	d := DefaultBudget().Evaluate(Metrics{DomainID: "d", Sent: 5000, Bounces: 75, Source: SourceLocal}) // 1.5%
	if d.Action != ThrottleDomain {
		t.Fatalf("action = %v (%s)", d.Action, d.Reason)
	}
}

func TestDecisionCarriesTheArithmetic(t *testing.T) {
	// An operator seeing a bare "halted" verdict will clear it. One that
	// shows the numbers gets read.
	d := DefaultBudget().Evaluate(pm("d", 10_000, 10_000, 12, 0))
	if !strings.Contains(d.Reason, "12 complaints of 10000 inboxed") {
		t.Fatalf("reason = %q", d.Reason)
	}
	if d.Rate == 0 || d.Threshold == 0 {
		t.Fatalf("decision omitted the numbers: %+v", d)
	}
}

// --- halt state and acknowledgment ------------------------------------------

func TestHaltRequiresHumanAcknowledgment(t *testing.T) {
	// An automatic resume means the same behaviour resumes and the spiral
	// continues.
	br := NewBreaker(DefaultBudget())
	br.Observe(pm("d1", 10_000, 10_000, 20, 0))

	if _, halted := br.Halted("d1"); !halted {
		t.Fatal("domain not halted")
	}
	if f := br.ThrottleFactor("d1"); f != 0 {
		t.Fatalf("halted domain has throttle factor %v", f)
	}

	// A subsequent healthy day must NOT clear it.
	br.Observe(pm("d1", 10_000, 10_000, 0, 0))
	if _, halted := br.Halted("d1"); !halted {
		t.Fatal("a healthy day auto-resumed a halted domain")
	}
}

func TestAcknowledgmentRequiresAnOperatorAndANote(t *testing.T) {
	// The point of the human ack is that somebody looked. A one-click
	// resume with no explanation is an automatic resume with extra steps.
	br := NewBreaker(DefaultBudget())
	br.Observe(pm("d1", 10_000, 10_000, 20, 0))

	if err := br.Acknowledge("d1", "", "checked"); !errors.Is(err, ErrAckRequiresNote) {
		t.Fatalf("empty operator accepted: %v", err)
	}
	if err := br.Acknowledge("d1", "alex", ""); !errors.Is(err, ErrAckRequiresNote) {
		t.Fatalf("empty note accepted: %v", err)
	}
	if _, halted := br.Halted("d1"); !halted {
		t.Fatal("a rejected acknowledgment cleared the halt anyway")
	}

	if err := br.Acknowledge("d1", "alex", "bad list from source X, removed and re-verified"); err != nil {
		t.Fatal(err)
	}
	if _, halted := br.Halted("d1"); halted {
		t.Fatal("still halted after a valid acknowledgment")
	}
}

func TestResumeAfterAckIsThrottledNotFullSpeed(t *testing.T) {
	// Acknowledging a halt does not prove the cause is fixed.
	br := NewBreaker(DefaultBudget())
	br.Observe(pm("d1", 10_000, 10_000, 20, 0))
	br.Acknowledge("d1", "alex", "investigated")

	f := br.ThrottleFactor("d1")
	if f <= 0 || f >= 1 {
		t.Fatalf("throttle factor after ack = %v; expected a partial rate", f)
	}
}

func TestAcknowledgingAHealthyDomainIsAnError(t *testing.T) {
	br := NewBreaker(DefaultBudget())
	if err := br.Acknowledge("nope", "alex", "note"); !errors.Is(err, ErrNotHalted) {
		t.Fatalf("got %v", err)
	}
}

func TestFirstHaltReasonIsKept(t *testing.T) {
	// Later evaluations must not overwrite the original cause; clearing
	// requires an ack either way, and the first reason is the useful one.
	br := NewBreaker(DefaultBudget())
	br.Observe(pm("d1", 10_000, 10_000, 30, 0))
	first, _ := br.Halted("d1")

	br.Observe(pm("d1", 10_000, 10_000, 500, 0))
	second, _ := br.Halted("d1")

	if first.Reason != second.Reason {
		t.Fatalf("halt reason was overwritten:\n  %q\n  %q", first.Reason, second.Reason)
	}
}

func TestHaltIsPerDomainNotPerSequence(t *testing.T) {
	// Reputation is a domain-level property. A bad sequence on a shared
	// domain damages every sequence on it.
	br := NewBreaker(DefaultBudget())
	br.Observe(pm("burning", 10_000, 10_000, 30, 0))

	if _, halted := br.Halted("burning"); !halted {
		t.Fatal("domain not halted")
	}
	if _, halted := br.Halted("healthy"); halted {
		t.Fatal("halting one domain halted another")
	}
	if f := br.ThrottleFactor("healthy"); f != 1 {
		t.Fatalf("unrelated domain throttled to %v", f)
	}
}

func TestBreakerIsConcurrencySafe(t *testing.T) {
	// Run under -race. The send path consults it on every dispatch while
	// the Postmaster sync updates it from another goroutine.
	br := NewBreaker(DefaultBudget())
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			br.Observe(pm("d1", 10_000, 10_000, i%40, 0))
		}
	}()
	for i := 0; i < 5000; i++ {
		br.ThrottleFactor("d1")
		br.Halted("d1")
		br.HaltedDomains()
	}
	<-done
}

// --- warmup -----------------------------------------------------------------

func mailbox(day int, started bool) *model.Mailbox {
	m := &model.Mailbox{HealthState: model.HealthGood, WarmupDay: day}
	if started {
		t := time.Now().AddDate(0, 0, -day)
		m.WarmupStartedAt = &t
	}
	return m
}

func TestUnstartedMailboxCannotSendColdAtAll(t *testing.T) {
	// Zero, not "a small number". This is what stops a freshly authorised
	// mailbox from being used the moment it connects.
	if got := DefaultWarmup().Cap(mailbox(0, false), 50); got != 0 {
		t.Fatalf("cap = %d for an unstarted mailbox", got)
	}
}

func TestWarmupRampIsMonotonic(t *testing.T) {
	// A ramp that dips would let a mailbox send more yesterday than today,
	// which reads as erratic to the provider heuristics it exists to
	// satisfy.
	s := DefaultWarmup()
	prev := -1
	for day := 0; day <= 40; day++ {
		got := s.Cap(mailbox(day, true), 50)
		if got < prev {
			t.Fatalf("day %d cap %d is below day %d's %d", day, got, day-1, prev)
		}
		prev = got
	}
}

func TestWarmupReachesTargetAndStops(t *testing.T) {
	s := DefaultWarmup()
	if got := s.Cap(mailbox(5, true), 50); got != 5 {
		t.Fatalf("day 5 cap = %d, want 5", got)
	}
	if got := s.Cap(mailbox(7, true), 50); got != 10 {
		t.Fatalf("day 7 cap = %d, want 10", got)
	}
	if got := s.Cap(mailbox(28, true), 50); got != 50 {
		t.Fatalf("day 28 cap = %d, want the target 50", got)
	}
	if got := s.Cap(mailbox(400, true), 50); got != 50 {
		t.Fatalf("day 400 cap = %d, want the target 50", got)
	}
}

func TestWarmupNeverExceedsTarget(t *testing.T) {
	s := DefaultWarmup()
	for day := 0; day <= 60; day++ {
		if got := s.Cap(mailbox(day, true), 8); got > 8 {
			t.Fatalf("day %d cap %d exceeds the target 8", day, got)
		}
	}
}

func TestDegradedMailboxSendsLess(t *testing.T) {
	// A mailbox that degraded is not a mailbox that should keep sending at
	// full volume while you investigate.
	s := DefaultWarmup()
	good := s.Cap(mailbox(30, true), 40)

	warn := mailbox(30, true)
	warn.HealthState = model.HealthWarning
	deg := mailbox(30, true)
	deg.HealthState = model.HealthDegraded

	if s.Cap(warn, 40) >= good {
		t.Fatal("a warning mailbox did not reduce its cap")
	}
	if s.Cap(deg, 40) >= s.Cap(warn, 40) {
		t.Fatal("a degraded mailbox did not reduce below warning")
	}
}

func TestHaltedMailboxSendsNothing(t *testing.T) {
	m := mailbox(30, true)
	m.HealthState = model.HealthHalted
	if got := DefaultWarmup().Cap(m, 40); got != 0 {
		t.Fatalf("cap = %d for a halted mailbox", got)
	}
}

func TestHealthEventRollsTheRampBack(t *testing.T) {
	s := DefaultWarmup()
	m := mailbox(20, true)
	before := s.Cap(m, 50)

	s.ResetOnHealthEvent(m, EventBounceSpike, time.Now())

	if m.WarmupDay != 13 {
		t.Fatalf("warmup day = %d, want a one-week rollback to 13", m.WarmupDay)
	}
	if got := s.Cap(m, 50); got >= before {
		t.Fatalf("cap did not fall after a health event: %d -> %d", before, got)
	}
}

func TestHealthEventDoesNotRestartFromZero(t *testing.T) {
	// A full restart punishes one transient bounce with four weeks of
	// reduced capacity, which operators respond to by disabling the
	// feature entirely.
	s := DefaultWarmup()
	m := mailbox(25, true)
	s.ResetOnHealthEvent(m, EventComplaint, time.Now())
	if m.WarmupDay == 0 {
		t.Fatal("a single complaint reset warmup to day zero")
	}
}

func TestProviderRejectHaltsRatherThanThrottles(t *testing.T) {
	// The provider is actively refusing mail. That is not a volume
	// problem and reducing the cap does not address it.
	s := DefaultWarmup()
	m := mailbox(30, true)
	s.ResetOnHealthEvent(m, EventProviderReject, time.Now())

	if m.HealthState != model.HealthHalted {
		t.Fatalf("health = %v after a provider reject", m.HealthState)
	}
	if got := s.Cap(m, 50); got != 0 {
		t.Fatalf("cap = %d after a provider reject", got)
	}
}

func TestRepeatedComplaintsEscalate(t *testing.T) {
	s := DefaultWarmup()
	m := mailbox(30, true)

	s.ResetOnHealthEvent(m, EventComplaint, time.Now())
	if m.HealthState != model.HealthWarning {
		t.Fatalf("first complaint -> %v", m.HealthState)
	}
	s.ResetOnHealthEvent(m, EventComplaint, time.Now())
	if m.HealthState != model.HealthDegraded {
		t.Fatalf("second complaint -> %v", m.HealthState)
	}
}

func TestDayForCountsWholeDays(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	if got := DayFor(start, start.Add(23*time.Hour)); got != 0 {
		t.Fatalf("23 hours in = day %d", got)
	}
	if got := DayFor(start, start.Add(25*time.Hour)); got != 1 {
		t.Fatalf("25 hours in = day %d", got)
	}
	if got := DayFor(start, start.Add(-time.Hour)); got != 0 {
		t.Fatalf("before the start = day %d", got)
	}
}

// --- capacity planning ------------------------------------------------------

func TestCapacityPlanningTellsOperatorsTheTruthEarly(t *testing.T) {
	// The design document's worked example: 2,000 prospects, four steps,
	// 21 business days, 40 sends per mailbox per day. They need ten
	// mailboxes, not one -- and they should learn this at sequence-design
	// time, not by watching their domain burn.
	p := PlanCapacity(2000, 4, 21, 40, 4999)

	if p.TotalSends != 8000 {
		t.Fatalf("total sends = %d, want 8000", p.TotalSends)
	}
	if p.MailboxesNeeded != 10 {
		t.Fatalf("mailboxes = %d, want 10", p.MailboxesNeeded)
	}
	if !p.Feasible {
		t.Fatalf("plan marked infeasible: %s", p.Note)
	}
	t.Logf("%s", p.Note)
}

func TestCapacityPlanRefusesUnsafePerMailboxVolume(t *testing.T) {
	// The escape hatch an operator reaches for -- "just send more per
	// mailbox" -- is the one that does not work.
	p := PlanCapacity(2000, 4, 21, 400, 4999)
	if p.Feasible {
		t.Fatal("400 sends per mailbox per day was accepted")
	}
	if !strings.Contains(p.Note, "does not look like a human") {
		t.Fatalf("note = %q", p.Note)
	}
}

func TestCapacityPlanCountsDomains(t *testing.T) {
	p := PlanCapacity(200_000, 1, 10, 50, 4999)
	if p.DomainsNeeded < 5 {
		t.Fatalf("20,000 sends/day needs more than %d domains below the bulk threshold", p.DomainsNeeded)
	}
}

func TestCapacityPlanHandlesEmptyInput(t *testing.T) {
	p := PlanCapacity(0, 0, 0, 0, 0)
	if p.Feasible || p.TotalSends != 0 {
		t.Fatalf("got %+v", p)
	}
}

// --- domains ----------------------------------------------------------------

func goodDomain(name, tracker string) *SendingDomain {
	return &SendingDomain{
		Name:           name,
		TrackingDomain: tracker,
		HasWebsite:     true,
		DailyCap:       2000,
		Auth: AuthStatus{
			SPF: true, DKIM: true, DMARC: true, DKIMKeyBits: 2048,
			DMARCPolicy: "none", ForwardDNS: true, ReverseDNS: true,
		},
	}
}

func TestRefusesThePrimaryDomain(t *testing.T) {
	// A burned primary domain stops invoices, contracts and password
	// resets. Existential, not a marketing setback -- so this is a
	// validation error, not a warning.
	r := NewRegistry("yourcompany.com")
	err := r.Register(goodDomain("yourcompany.com", "track.get-yourcompany.com"))
	if !errors.Is(err, ErrPrimaryDomain) {
		t.Fatalf("got %v", err)
	}
}

func TestRefusesASubdomainOfThePrimary(t *testing.T) {
	// Subdomain reputation is not fully isolated from the parent, and
	// isolation is the entire point. An operator reaching for
	// mail.yourcompany.com has understood the goal and picked the one
	// shortcut that defeats it.
	r := NewRegistry("yourcompany.com")
	err := r.Register(goodDomain("mail.yourcompany.com", "track.other.com"))
	if !errors.Is(err, ErrSubdomainOfPrimary) {
		t.Fatalf("got %v", err)
	}
}

func TestAcceptsASeparateRegistrableDomain(t *testing.T) {
	r := NewRegistry("yourcompany.com")
	if err := r.Register(goodDomain("get-yourcompany.com", "link.get-yourcompany.com")); err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateForSending("get-yourcompany.com"); err != nil {
		t.Fatal(err)
	}
}

func TestDomainNameIsCaseAndDotInsensitive(t *testing.T) {
	// A trailing dot is a valid FQDN and uppercase is legal in DNS; both
	// would otherwise slip past the primary-domain check.
	r := NewRegistry("YourCompany.com")
	if err := r.Register(goodDomain("YOURCOMPANY.COM.", "t.example.com")); !errors.Is(err, ErrPrimaryDomain) {
		t.Fatalf("got %v", err)
	}
}

func TestRefusesASharedTrackingDomain(t *testing.T) {
	// A shared tracker means inheriting every other sender's reputation,
	// and a shared tracker on a blocklist takes you with it.
	r := NewRegistry("yourcompany.com")
	if err := r.Register(goodDomain("a-outbound.com", "shared-tracker.com")); err != nil {
		t.Fatal(err)
	}
	err := r.Register(goodDomain("b-outbound.com", "shared-tracker.com"))
	if !errors.Is(err, ErrSharedTracker) {
		t.Fatalf("got %v", err)
	}
}

func TestRefusesADomainWithNoAuthentication(t *testing.T) {
	r := NewRegistry("yourcompany.com")
	d := goodDomain("a.com", "t.a.com")
	d.Auth.SPF, d.Auth.DKIM = false, false
	if err := r.Register(d); !errors.Is(err, ErrNoAuth) {
		t.Fatalf("got %v", err)
	}
}

func TestRefusesAShortDKIMKey(t *testing.T) {
	// Yahoo publishes a 1024-bit minimum.
	r := NewRegistry("yourcompany.com")
	d := goodDomain("a.com", "t.a.com")
	d.Auth.DKIMKeyBits = 512
	err := r.Register(d)
	if !errors.Is(err, ErrNoAuth) || !strings.Contains(err.Error(), "1024") {
		t.Fatalf("got %v", err)
	}
}

func TestRefusesMissingReverseDNS(t *testing.T) {
	// A missing PTR is a reject, not a spam placement.
	r := NewRegistry("yourcompany.com")
	d := goodDomain("a.com", "t.a.com")
	d.Auth.ReverseDNS = false
	if err := r.Register(d); !errors.Is(err, ErrNoAuth) {
		t.Fatalf("got %v", err)
	}
}

func TestRefusesADomainWithNoWebsite(t *testing.T) {
	// A domain with no resolving site is itself a spam signal.
	r := NewRegistry("yourcompany.com")
	d := goodDomain("a.com", "t.a.com")
	d.HasWebsite = false
	if err := r.Register(d); !errors.Is(err, ErrNoWebsite) {
		t.Fatalf("got %v", err)
	}
}

func TestRefusesADailyCapAtTheBulkThreshold(t *testing.T) {
	// Crossing 5,000/day permanently changes the domain's obligations, so
	// it has to be a deliberate configuration change rather than an
	// emergent consequence of enrolling too many prospects.
	r := NewRegistry("yourcompany.com")
	d := goodDomain("a.com", "t.a.com")
	d.DailyCap = BulkSenderDailyThreshold
	err := r.Register(d)
	if err == nil || !strings.Contains(err.Error(), "permanently") {
		t.Fatalf("got %v", err)
	}
}

func TestBulkClassificationIsPermanent(t *testing.T) {
	// Once a domain crosses 5,000 messages in a day to Gmail, the
	// classification attaches and does not expire when volume decreases.
	r := NewRegistry("yourcompany.com")
	r.Register(goodDomain("a.com", "t.a.com"))

	if newly := r.RecordDailyVolume("a.com", 4999); newly {
		t.Fatal("classified below the threshold")
	}
	if newly := r.RecordDailyVolume("a.com", 5000); !newly {
		t.Fatal("did not classify at the threshold")
	}
	// Volume drops right back down.
	r.RecordDailyVolume("a.com", 10)
	d, _ := r.Get("a.com")
	if !d.BulkClassified {
		t.Fatal("classification expired when volume decreased")
	}
}

func TestBulkRequirementsAreListedOnlyOnceClassified(t *testing.T) {
	d := goodDomain("a.com", "t.a.com")
	if got := d.BulkRequirements(); got != nil {
		t.Fatalf("unclassified domain listed requirements: %v", got)
	}

	d.BulkClassified = true
	d.Auth.DMARCPolicy = ""
	d.Auth.DKIM = false
	missing := d.BulkRequirements()
	if len(missing) < 2 {
		t.Fatalf("missing = %v", missing)
	}
}

func TestUnregisteredDomainIsRefused(t *testing.T) {
	r := NewRegistry("yourcompany.com")
	if err := r.ValidateForSending("random.com"); !errors.Is(err, ErrUnknownDomain) {
		t.Fatalf("got %v", err)
	}
}

func TestTrackingDomainCannotBeThePrimary(t *testing.T) {
	// A tracking subdomain of the primary puts the primary's reputation
	// back in the blast radius through the side door.
	r := NewRegistry("yourcompany.com")
	err := r.Register(goodDomain("get-yourcompany.com", "track.yourcompany.com"))
	if err == nil || !strings.Contains(err.Error(), "tracking domain") {
		t.Fatalf("got %v", err)
	}
}

// --- postmaster ingestion ----------------------------------------------------

func TestParseCSVUsesTheHeaderNotThePosition(t *testing.T) {
	// Columns in a different order from the usual export.
	in := "Domain Reputation,Date,Spam Rate,Inboxed,DKIM Success Rate\n" +
		"High,2026-03-01,0.0004,1200,0.99\n" +
		"Medium,2026-03-02,0.12%,900,98%\n"

	rows, err := ParseCSV(strings.NewReader(in), "get-ours.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	if rows[0].SpamRate != 0.0004 || rows[0].InboxedEstimate != 1200 {
		t.Fatalf("%+v", rows[0])
	}
	// Percent-suffixed values are the other export format.
	if rows[1].SpamRate != 0.0012 {
		t.Fatalf("percent parsing: %v", rows[1].SpamRate)
	}
	if rows[1].DKIMSuccess != 0.98 {
		t.Fatalf("dkim: %v", rows[1].DKIMSuccess)
	}
	if rows[1].DomainRep != RepMedium {
		t.Fatalf("rep %q", rows[1].DomainRep)
	}
}

func TestParseCSVRefusesToGuessAtAMissingColumn(t *testing.T) {
	// A positional parser would read some other column as the spam rate
	// and produce a plausible, wrong number.
	in := "Date,Inboxed\n2026-03-01,1200\n"
	if _, err := ParseCSV(strings.NewReader(in), "d"); err == nil {
		t.Fatal("accepted an export with no spam rate column")
	}
}

func TestMediumReputationCountsAsDegraded(t *testing.T) {
	// By the time Google says "low", mail is already going to spam.
	if !RepMedium.Degraded() {
		t.Fatal("medium reputation is not treated as degraded")
	}
	if RepHigh.Degraded() || RepUnknown.Degraded() {
		t.Fatal("high or unknown treated as degraded")
	}
}

func TestPostmasterRowsAreTaggedAsPostmasterData(t *testing.T) {
	m := PostmasterRow{DomainID: "d", SpamRate: 0.002, InboxedEstimate: 1000}.ToMetrics()
	if m.Source != SourcePostmaster {
		t.Fatalf("source %q", m.Source)
	}
	if m.Complaints != 2 {
		t.Fatalf("complaints %d", m.Complaints)
	}
	rate, ok := m.ComplaintRate()
	if !ok {
		t.Fatal("a Postmaster row did not yield a usable rate")
	}
	if rate != 0.002 {
		t.Fatalf("rate %v", rate)
	}
}

func TestAuthHealthIsTheLeadingIndicator(t *testing.T) {
	good := PostmasterRow{DKIMSuccess: 0.99, SPFSuccess: 0.99, DMARCSuccess: 0.99}
	if !good.AuthHealth().OK {
		t.Fatal("healthy auth flagged")
	}
	bad := PostmasterRow{DKIMSuccess: 0.70, SPFSuccess: 0.99, DMARCSuccess: 0.99}
	h := bad.AuthHealth()
	if h.OK {
		t.Fatal("70% DKIM pass rate was not flagged")
	}
	if !strings.Contains(h.Problem, "denominator") {
		t.Fatalf("the warning does not connect auth to the denominator: %s", h.Problem)
	}
}

// TestAnalyseTrendSeesTheSpiralThatCountsWouldMiss is §2.3 in the data a
// real deployment actually has.
func TestAnalyseTrendSeesTheSpiralThatCountsWouldMiss(t *testing.T) {
	day := func(d int, inboxed, complaints int) PostmasterRow {
		return PostmasterRow{
			Date:            time.Date(2026, 3, d, 0, 0, 0, 0, time.UTC),
			DomainID:        "get-ours.example",
			InboxedEstimate: inboxed,
			Complaints:      complaints,
			SpamRate:        float64(complaints) / float64(inboxed),
		}
	}
	// Identical behaviour: two complaints a day. Filtering grows.
	rows := []PostmasterRow{
		day(1, 1000, 2),
		day(2, 700, 2),
		day(3, 450, 2),
		day(4, 250, 2),
	}

	tr := AnalyseTrend(rows)
	if !tr.DenominatorCollapse {
		t.Fatalf("the spiral was not detected: %s", tr.Message)
	}
	t.Log(tr.Message)

	// The complaint COUNT never moved, which is the whole point.
	if rows[0].Complaints != rows[3].Complaints {
		t.Fatal("the fixture changed the complaint count")
	}
	first := rows[0].SpamRate
	last := rows[3].SpamRate
	if last/first < 3.5 {
		t.Fatalf("rate went from %.3f%% to %.3f%%, only %.1fx", first*100, last*100, last/first)
	}
	t.Logf("identical behaviour, %d complaints a day throughout: rate went from %.2f%% to %.2f%%, a %.1fx increase in four days",
		rows[0].Complaints, first*100, last*100, last/first)
}

func TestAnalyseTrendOnHealthyData(t *testing.T) {
	rows := []PostmasterRow{
		{Date: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), InboxedEstimate: 1000, SpamRate: 0.0004, Complaints: 0},
		{Date: time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC), InboxedEstimate: 1100, SpamRate: 0.0004, Complaints: 0},
	}
	if AnalyseTrend(rows).DenominatorCollapse {
		t.Fatal("healthy data flagged as a collapse")
	}
}
