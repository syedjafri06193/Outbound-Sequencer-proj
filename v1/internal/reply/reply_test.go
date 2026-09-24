package reply

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeIndex struct {
	byMsg    map[string]SentMessage
	byThread map[string]SentMessage
}

func (f fakeIndex) BySentMessageID(id string) (SentMessage, bool) {
	s, ok := f.byMsg[id]
	return s, ok
}
func (f fakeIndex) BySentThreadID(id string) (SentMessage, bool) {
	s, ok := f.byThread[id]
	return s, ok
}

func idx(sent ...SentMessage) fakeIndex {
	f := fakeIndex{byMsg: map[string]SentMessage{}, byThread: map[string]SentMessage{}}
	for _, s := range sent {
		f.byMsg[normalizeMessageID(s.MessageID)] = s
		if s.ThreadID != "" {
			f.byThread[s.ThreadID] = s
		}
	}
	return f
}

func recv() time.Time { return time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC) }

// --- threading --------------------------------------------------------------

func TestMatchPrefersInReplyToOverReferences(t *testing.T) {
	// A thread that was forwarded: References carries an ancestor from a
	// different enrolment, In-Reply-To names the real parent.
	i := idx(
		SentMessage{EnrollmentID: "old", MessageID: "<step1@ours>"},
		SentMessage{EnrollmentID: "current", MessageID: "<step3@ours>"},
	)
	msg := InboundMessage{
		InReplyTo:  []string{"<step3@ours>"},
		References: []string{"<step1@ours>", "<something@else>"},
	}
	got, ok := Match(i, msg)
	if !ok {
		t.Fatal("no match")
	}
	if got.EnrollmentID != "current" {
		t.Fatalf("matched %q, want the In-Reply-To parent %q", got.EnrollmentID, "current")
	}
}

func TestMatchWalksReferencesNewestFirst(t *testing.T) {
	i := idx(
		SentMessage{EnrollmentID: "older", MessageID: "<a@ours>"},
		SentMessage{EnrollmentID: "newer", MessageID: "<c@ours>"},
	)
	msg := InboundMessage{References: []string{"<a@ours>", "<b@theirs>", "<c@ours>"}}
	got, _ := Match(i, msg)
	if got.EnrollmentID != "newer" {
		t.Fatalf("matched %q, want the nearest ancestor %q", got.EnrollmentID, "newer")
	}
}

func TestMatchToleratesMissingAngleBrackets(t *testing.T) {
	i := idx(SentMessage{EnrollmentID: "e1", MessageID: "<Step1@Ours.example>"})
	for _, ref := range []string{"step1@ours.example", "<step1@ours.example>", " <Step1@Ours.example> "} {
		if _, ok := Match(i, InboundMessage{InReplyTo: []string{ref}}); !ok {
			t.Errorf("reference %q did not match", ref)
		}
	}
}

func TestMatchFallsBackToThreadID(t *testing.T) {
	i := idx(SentMessage{EnrollmentID: "e1", MessageID: "<x@ours>", ThreadID: "thr-9"})
	got, ok := Match(i, InboundMessage{ThreadID: "thr-9"})
	if !ok || got.EnrollmentID != "e1" {
		t.Fatalf("thread fallback failed: %v %v", got, ok)
	}
}

func TestMatchNeverUsesSubject(t *testing.T) {
	// Two enrolments whose emails share a subject. Header-free inbound
	// mail must NOT match: a false positive stops the wrong sequence.
	i := idx(SentMessage{EnrollmentID: "e1", MessageID: "<a@ours>"})
	msg := InboundMessage{Subject: "Re: Quick question", Body: "sure"}
	if _, ok := Match(i, msg); ok {
		t.Fatal("matched on subject alone")
	}
}

func TestMatchDoesNotMutateInput(t *testing.T) {
	// The document's own implementation does
	// append(msg.References, msg.InReplyTo...), which can write into the
	// caller's backing array when it has spare capacity.
	i := idx(SentMessage{EnrollmentID: "e1", MessageID: "<a@ours>"})
	refs := make([]string, 2, 8)
	refs[0], refs[1] = "<a@ours>", "<b@theirs>"
	msg := InboundMessage{References: refs, InReplyTo: []string{"<z@ours>"}}
	Match(i, msg)
	if refs[0] != "<a@ours>" || refs[1] != "<b@theirs>" || len(refs) != 2 {
		t.Fatalf("Match mutated the caller's References slice: %v", refs)
	}
}

func TestParseReferences(t *testing.T) {
	got := ParseReferences("<a@x>  <b@y>\r\n <c@z>")
	want := []string{"a@x", "b@y", "c@z"}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// --- auto-reply detection ---------------------------------------------------

func TestHeaderLookupIsCaseInsensitive(t *testing.T) {
	// Gmail returns "Auto-Submitted", Graph sometimes "auto-submitted".
	m := InboundMessage{Headers: map[string]string{"auto-submitted": "auto-replied"}}
	if m.Header("Auto-Submitted") != "auto-replied" {
		t.Fatal("case-insensitive header lookup failed")
	}
}

func TestIsAutoReplyFromHeaders(t *testing.T) {
	cases := map[string]map[string]string{
		"RFC 3834 auto-replied": {"Auto-Submitted": "auto-replied"},
		"auto-generated":        {"Auto-Submitted": "auto-generated"},
		"X-Autoreply":           {"X-Autoreply": "yes"},
		"X-Autorespond":         {"X-Autorespond": "vacation"},
		"Precedence auto_reply": {"Precedence": "auto_reply"},
		"Exchange suppress":     {"X-Auto-Response-Suppress": "All"},
		"null return path":      {"Return-Path": "<>"},
		"Exchange loop marker":  {"X-MS-Exchange-Inbox-Rules-Loop": "user@corp.example"},
	}
	for name, h := range cases {
		if !IsAutoReply(InboundMessage{Headers: h}) {
			t.Errorf("%s: not detected as an auto-reply", name)
		}
	}
}

func TestAutoSubmittedNoIsNotAnAutoReply(t *testing.T) {
	// RFC 3834: "no" explicitly means this is NOT an automatic response.
	// Checking only for the header's presence turns every well-behaved
	// human client that sets it into a permanent out-of-office.
	m := InboundMessage{Headers: map[string]string{"Auto-Submitted": "no"}}
	if IsAutoReply(m) {
		t.Fatal("Auto-Submitted: no was treated as an auto-reply")
	}
}

// TestOutOfOfficeDetectedWithoutReadingTheBody is the test for the claim the
// document makes: header-based detection is far more reliable than body text
// matching, which fails across languages.
func TestOutOfOfficeDetectedWithoutReadingTheBody(t *testing.T) {
	bodies := map[string]string{
		"German":   "Ich bin bis zum 16. März nicht im Büro und habe keinen Zugriff auf meine E-Mails.",
		"Japanese": "現在休暇中のため、メールの確認ができません。",
		"Finnish":  "Olen lomalla enkä lue sähköpostia.",
		"Polish":   "Przebywam na urlopie i nie mam dostępu do poczty.",
		"Arabic":   "أنا خارج المكتب حالياً.",
	}
	c := NewClassifier()
	for lang, body := range bodies {
		msg := InboundMessage{
			Headers:    map[string]string{"Auto-Submitted": "auto-replied"},
			Body:       body,
			ReceivedAt: recv(),
		}
		d := c.Classify(msg)
		if d.Class != ReplyOutOfOffice {
			t.Errorf("%s OOO classified as %s; header detection is what makes this work in any language", lang, d.Class)
		}
		if d.CountsAsReply {
			t.Errorf("%s: OOO counted as a reply", lang)
		}
		if d.StopSequence {
			t.Errorf("%s: OOO stopped the sequence instead of pausing it", lang)
		}
		if d.PauseUntil.IsZero() {
			t.Errorf("%s: OOO produced no resume time", lang)
		}
	}
}

// TestBodyMatchingWouldMissEveryOneOfThem measures the alternative rather
// than asserting it is worse.
func TestBodyMatchingWouldMissEveryOneOfThem(t *testing.T) {
	// The phrase list a body-matching implementation would plausibly use.
	phrases := []string{"out of office", "on vacation", "annual leave", "away from my desk", "ooo"}
	bodies := []string{
		"Ich bin bis zum 16. März nicht im Büro.",
		"現在休暇中のため、メールの確認ができません。",
		"Olen lomalla enkä lue sähköpostia.",
		"Przebywam na urlopie.",
		"Je suis absent du bureau jusqu'au 16 mars.",
	}
	missed := 0
	for _, b := range bodies {
		hit := false
		for _, p := range phrases {
			if strings.Contains(strings.ToLower(b), p) {
				hit = true
				break
			}
		}
		if !hit {
			missed++
		}
	}
	t.Logf("body-text matching missed %d of %d non-English out-of-office replies; header detection caught all %d",
		missed, len(bodies), len(bodies))
	if missed != len(bodies) {
		t.Fatalf("expected body matching to miss all %d, missed %d", len(bodies), missed)
	}
}

// --- return date parsing ----------------------------------------------------

func TestParseReturnDate(t *testing.T) {
	from := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		body string
		want string
	}{
		{"I am back on 2026-03-16 and will reply then.", "2026-03-16"},
		{"I'm out until 16 March.", "2026-03-16"},
		{"Away, returning on March 16th.", "2026-03-16"},
		{"On leave until March 16", "2026-03-16"},
		{"Back on 16th March", "2026-03-16"},
	}
	for _, tc := range cases {
		got, ok := ParseReturnDate(tc.body, from)
		if !ok {
			t.Errorf("%q: no date parsed", tc.body)
			continue
		}
		if got.Format("2006-01-02") != tc.want {
			t.Errorf("%q: got %s want %s", tc.body, got.Format("2006-01-02"), tc.want)
		}
	}
}

func TestParseReturnDateRefusesRatherThanGuesses(t *testing.T) {
	from := recv()
	for _, body := range []string{
		"I am out of the office until further notice.",
		"Away for a while.",
		"",
		"Back on the moon",
		"Returning on 2026-02-01", // in the past
		"until 2099-01-01",        // absurdly far ahead
	} {
		if d, ok := ParseReturnDate(body, from); ok {
			t.Errorf("%q: guessed %s; a confident wrong parse resumes while the prospect is still away", body, d)
		}
	}
}

func TestYearlessDateRollsForward(t *testing.T) {
	// An OOO in November naming "March 12" means next March.
	from := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	got, ok := ParseReturnDate("back on 12 March", from)
	if !ok {
		t.Fatal("no parse")
	}
	if got.Year() != 2027 || got.Month() != time.March {
		t.Fatalf("got %s, want March 2027", got.Format("2006-01-02"))
	}
}

func TestResumeAfterUsesConservativeDefault(t *testing.T) {
	msg := InboundMessage{Body: "Out of the office.", ReceivedAt: recv()}
	got := ResumeAfter(msg)
	want := recv().Add(DefaultOOODelay)
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestResumeIsTheDayAfterTheStatedReturn(t *testing.T) {
	msg := InboundMessage{
		Body:       "Back on 2026-03-16.",
		ReceivedAt: recv(),
		Headers:    map[string]string{"Auto-Submitted": "auto-replied"},
	}
	got := ResumeAfter(msg)
	if got.Format("2006-01-02") != "2026-03-17" {
		t.Fatalf("resumed %s; an inbox on the first day back is not where a cold email wants to be",
			got.Format("2006-01-02"))
	}
}

// --- classification ---------------------------------------------------------

func TestHostileSuppressesGlobally(t *testing.T) {
	c := NewClassifier()
	for _, body := range []string{
		"Stop emailing me.",
		"Take me off your list.",
		"unsubscribe",
		"Do not contact me again.",
		"This is spam and I'm reporting you for spam.",
		"How did you get my email?",
		"remove me from this list immediately",
	} {
		d := c.Classify(InboundMessage{Body: body, ReceivedAt: recv()})
		if d.Class != ReplyHostile {
			t.Errorf("%q classified as %s", body, d.Class)
			continue
		}
		if !d.SuppressGlobally {
			t.Errorf("%q: not suppressed globally", body)
		}
		if !d.StopSequence || !d.NotifyRep || !d.FlagForReview {
			t.Errorf("%q: %+v", body, d)
		}
	}
}

// TestHostileIsNeverSoftenedToNegative guards the ordering.
func TestHostileIsNeverSoftenedToNegative(t *testing.T) {
	// Contains both a hostile demand and a polite refusal.
	body := "Not interested, and please stop emailing me."
	d := NewClassifier().Classify(InboundMessage{Body: body, ReceivedAt: recv()})
	if d.Class != ReplyHostile {
		t.Fatalf("got %s; a hostile reply must not be downgraded by a gentler later match", d.Class)
	}
	if !d.SuppressGlobally {
		t.Fatal("not suppressed globally")
	}
}

func TestNotNowSchedulesReEnrolment(t *testing.T) {
	c := NewClassifier()
	from := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		body      string
		wantAfter time.Time
		wantMonth time.Month
	}{
		{"Bad timing - circle back in Q3.", from, time.July},
		{"Not now, try me in 6 months.", from, time.September},
		{"Reach out in September please.", from, time.September},
		{"Check back in 3 weeks.", from, time.March},
	}
	for _, tc := range cases {
		d := c.Classify(InboundMessage{Body: tc.body, ReceivedAt: from})
		if d.Class != ReplyNotNow {
			t.Errorf("%q classified as %s", tc.body, d.Class)
			continue
		}
		if d.ReEnrollAt.IsZero() || !d.ReEnrollAt.After(tc.wantAfter) {
			t.Errorf("%q: re-enrol at %s is not in the future", tc.body, d.ReEnrollAt)
			continue
		}
		if d.ReEnrollAt.Month() != tc.wantMonth {
			t.Errorf("%q: re-enrol in %s, want %s", tc.body, d.ReEnrollAt.Month(), tc.wantMonth)
		}
		if !d.StopSequence {
			t.Errorf("%q: not stopped", tc.body)
		}
	}
}

func TestBareNotNowGetsTheDefaultDelay(t *testing.T) {
	d := NewClassifier().Classify(InboundMessage{Body: "Bad timing right now.", ReceivedAt: recv()})
	if d.Class != ReplyNotNow {
		t.Fatalf("got %s", d.Class)
	}
	want := recv().Add(DefaultNotNowDelay)
	if !d.ReEnrollAt.Equal(want) {
		t.Fatalf("got %s want %s", d.ReEnrollAt, want)
	}
}

func TestQuarterInThePastRollsToNextYear(t *testing.T) {
	// "Q1" said in November is next year's Q1. Resolving backwards would
	// re-enrol them the following morning.
	from := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	d := NewClassifier().Classify(InboundMessage{Body: "Circle back in Q1.", ReceivedAt: from})
	if d.Class != ReplyNotNow {
		t.Fatalf("got %s", d.Class)
	}
	if d.ReEnrollAt.Year() != 2027 || d.ReEnrollAt.Month() != time.January {
		t.Fatalf("re-enrol at %s, want January 2027", d.ReEnrollAt.Format("2006-01-02"))
	}
}

func TestLeftCompanyFlagsTheAccount(t *testing.T) {
	d := NewClassifier().Classify(InboundMessage{
		Body:       "Jane is no longer with the company. Please direct enquiries elsewhere.",
		ReceivedAt: recv(),
	})
	if d.Class != ReplyLeftCompany {
		t.Fatalf("got %s", d.Class)
	}
	if !d.SuppressGlobally || !d.FlagAccountForNewContact {
		t.Fatalf("%+v: suppress the person, keep the account", d)
	}
	if d.CountsAsReply {
		t.Fatal("counted as a reply; nobody engaged")
	}
}

func TestReferralAndNegativeAndPositive(t *testing.T) {
	c := NewClassifier()
	cases := []struct {
		body string
		want ReplyClass
	}{
		{"I'm not the right person, speak to Dan who handles this for us.", ReplyReferral},
		{"Not interested, thanks.", ReplyNegative},
		{"We already use a vendor for this.", ReplyNegative},
		{"Sounds interesting, happy to chat next week.", ReplyPositive},
		{"Tell me more.", ReplyPositive},
		{"What's your pricing?", ReplyPositive},
	}
	for _, tc := range cases {
		d := c.Classify(InboundMessage{Body: tc.body, ReceivedAt: recv()})
		if d.Class != tc.want {
			t.Errorf("%q: got %s want %s", tc.body, d.Class, tc.want)
		}
		if !d.StopSequence || !d.NotifyRep {
			t.Errorf("%q: %+v", tc.body, d)
		}
	}
}

func TestUnknownHumanReplyStillStops(t *testing.T) {
	d := NewClassifier().Classify(InboundMessage{
		Body:       "Merci pour votre message, je vais y réfléchir.",
		ReceivedAt: recv(),
	})
	if d.Class != ReplyUnknown {
		t.Fatalf("got %s", d.Class)
	}
	if !d.StopSequence || !d.NotifyRep {
		t.Fatalf("%+v: a human wrote back; the machine stops talking", d)
	}
}

// TestQuotedOriginalIsNotClassified is the bug that makes every reply look
// positive: the template's own words are quoted back underneath.
func TestQuotedOriginalIsNotClassified(t *testing.T) {
	body := "Not interested.\n\nOn Mon, 2 Mar 2026, Rep <rep@ours.example> wrote:\n" +
		"> Happy to chat next week? Tell me more about your stack and\n" +
		"> I'll send over some info. Does Thursday work?\n"
	d := NewClassifier().Classify(InboundMessage{Body: body, ReceivedAt: recv()})
	if d.Class != ReplyNegative {
		t.Fatalf("got %s; the quoted original was classified instead of the reply", d.Class)
	}
}

func TestQuotedOriginalStrippedAtDividers(t *testing.T) {
	for _, divider := range []string{
		"-----Original Message-----",
		"________________________________",
		"-------- Forwarded message --------",
		"From: rep@ours.example",
	} {
		body := "No thanks.\n\n" + divider + "\nHappy to chat? Tell me more.\n"
		d := NewClassifier().Classify(InboundMessage{Body: body, ReceivedAt: recv()})
		if d.Class != ReplyNegative {
			t.Errorf("divider %q: got %s", divider, d.Class)
		}
	}
}

func TestBulkMailIsNeitherReplyNorStop(t *testing.T) {
	d := NewClassifier().Classify(InboundMessage{
		Headers:    map[string]string{"Precedence": "bulk", "List-Id": "<news.example>"},
		Body:       "Our March newsletter! Tell me more about...",
		ReceivedAt: recv(),
	})
	if d.CountsAsReply || d.StopSequence {
		t.Fatalf("%+v: a newsletter in the thread is not engagement", d)
	}
}

// --- bounces ----------------------------------------------------------------

func TestClassifyBounceStatus(t *testing.T) {
	cases := map[string]BounceKind{
		"5.1.1": BounceHard,
		"5.1.2": BounceHard,
		"5.7.1": BounceHard,
		"4.2.2": BounceSoft,
		"4.4.1": BounceSoft,
		// 5.2.2 is "mailbox full": permanent by class, transient in
		// fact. Suppressing on it loses a real prospect whose inbox was
		// full for a week.
		"5.2.2": BounceSoft,
		"":      BounceNone,
		"hello": BounceNone,
	}
	for status, want := range cases {
		if got := ClassifyBounceStatus(status); got != want {
			t.Errorf("status %q: got %s want %s", status, got, want)
		}
	}
}

func TestHardBounceSuppressesPermanently(t *testing.T) {
	d := NewClassifier().Classify(InboundMessage{
		From:       "MAILER-DAEMON@mx.example",
		Headers:    map[string]string{"Status": "5.1.1", "Content-Type": "multipart/report; report-type=delivery-status"},
		Body:       "550 5.1.1 user unknown",
		ReceivedAt: recv(),
	})
	if d.Class != ReplyBounceNotice {
		t.Fatalf("got %s", d.Class)
	}
	if !d.SuppressGlobally || d.SuppressReason != "hard_bounce" {
		t.Fatalf("%+v", d)
	}
	if d.CountsAsReply {
		t.Fatal("a bounce counted as a reply")
	}
}

func TestSoftBounceDoesNotSuppress(t *testing.T) {
	d := NewClassifier().Classify(InboundMessage{
		From:       "postmaster@mx.example",
		Headers:    map[string]string{"Status": "4.2.2"},
		Body:       "452 4.2.2 mailbox full",
		ReceivedAt: recv(),
	})
	if d.SuppressGlobally || d.StopSequence {
		t.Fatalf("%+v: a soft bounce is transient", d)
	}
}

func TestUnparseableDSNIsTreatedAsSoft(t *testing.T) {
	d := NewClassifier().Classify(InboundMessage{
		From:       "MAILER-DAEMON@mx.example",
		Body:       "Delivery failed for reasons this parser does not know about.",
		ReceivedAt: recv(),
	})
	if d.Class != ReplyBounceNotice {
		t.Fatalf("got %s", d.Class)
	}
	if d.SuppressGlobally {
		t.Fatal("suppressed a real address on the strength of an unparsed machine message")
	}
}

// TestBounceBodyTextIsNotReadAsAHumanRefusal is why bounces are classified
// first: a DSN body quotes the original and is full of refusal-shaped text.
func TestBounceBodyTextIsNotReadAsAHumanRefusal(t *testing.T) {
	d := NewClassifier().Classify(InboundMessage{
		From:    "MAILER-DAEMON@mx.example",
		Headers: map[string]string{"Status": "5.1.1"},
		Body: "The following message could not be delivered.\n" +
			"Remove me from the recipient list. 550 user unknown.\n",
		ReceivedAt: recv(),
	})
	if d.Class != ReplyBounceNotice {
		t.Fatalf("got %s; a mailer daemon does not file complaints", d.Class)
	}
}

func TestOnBounceBackoffAndLimit(t *testing.T) {
	now := recv()
	if o := OnBounce(BounceHard, 0, now); !o.Suppress {
		t.Fatal("hard bounce did not suppress")
	}
	prev := time.Duration(0)
	for i := 1; i < SoftBounceLimit; i++ {
		o := OnBounce(BounceSoft, i, now)
		if o.Suppress {
			t.Fatalf("soft bounce %d suppressed early", i)
		}
		d := o.RetryAt.Sub(now)
		if d <= prev {
			t.Fatalf("backoff did not grow: %v then %v", prev, d)
		}
		if d > 72*time.Hour {
			t.Fatalf("backoff %v exceeds the cap", d)
		}
		prev = d
	}
	if o := OnBounce(BounceSoft, SoftBounceLimit, now); !o.Suppress {
		t.Fatalf("soft bounce %d did not suppress", SoftBounceLimit)
	}
}

// --- reconciliation ---------------------------------------------------------

func TestReconcilerProcessesEachMessageOnce(t *testing.T) {
	r := NewReconciler()
	now := recv()
	first := r.Observe(Observation{MessageID: "m1", Source: SourceWebhook, ObservedAt: now})
	second := r.Observe(Observation{MessageID: "m1", Source: SourcePoll, ObservedAt: now.Add(time.Minute)})
	if !first {
		t.Fatal("first sighting not reported as first")
	}
	if second {
		t.Fatal("the poller's duplicate would have been processed twice")
	}
}

func TestReconcilerCountsWebhookMissesPastGraceOnly(t *testing.T) {
	r := NewReconciler()
	now := recv()

	for i := 0; i < 98; i++ {
		id := "ok" + itoa(i)
		r.Observe(Observation{MessageID: id, Source: SourceWebhook, ObservedAt: now})
		r.Observe(Observation{MessageID: id, Source: SourcePoll, ObservedAt: now.Add(2 * time.Minute)})
	}
	// One the webhook never delivered, long past grace.
	r.Observe(Observation{MessageID: "missed", Source: SourcePoll, ObservedAt: now})
	// One the poller just found; the webhook may still arrive.
	r.Observe(Observation{MessageID: "recent", Source: SourcePoll, ObservedAt: now.Add(9 * time.Minute)})

	rep := r.Report(now.Add(10 * time.Minute))
	if rep.Total != 100 {
		t.Fatalf("total %d", rep.Total)
	}
	if rep.WebhookMissed != 1 {
		t.Fatalf("missed %d, want 1", rep.WebhookMissed)
	}
	if rep.PollOnlyWithinGrace != 1 {
		t.Fatalf("within grace %d, want 1", rep.PollOnlyWithinGrace)
	}
	if rep.MedianWebhookLead != 2*time.Minute {
		t.Fatalf("median lead %v", rep.MedianWebhookLead)
	}
	if !rep.Healthy {
		t.Fatalf("1%% miss rate reported unhealthy: %s", rep.Message)
	}
}

func TestReconcilerAlertsOnABrokenWebhookPath(t *testing.T) {
	r := NewReconciler()
	now := recv()
	for i := 0; i < 90; i++ {
		r.Observe(Observation{MessageID: "w" + itoa(i), Source: SourceWebhook, ObservedAt: now})
	}
	for i := 0; i < 10; i++ {
		r.Observe(Observation{MessageID: "p" + itoa(i), Source: SourcePoll, ObservedAt: now})
	}
	rep := r.Report(now.Add(time.Hour))
	if rep.Healthy {
		t.Fatal("a 10% webhook miss rate was reported healthy")
	}
	if !strings.Contains(rep.Message, "degraded") {
		t.Fatalf("message does not say what is wrong: %s", rep.Message)
	}
}

func TestReconcilerPrunes(t *testing.T) {
	r := NewReconciler()
	now := recv()
	r.Observe(Observation{MessageID: "old", Source: SourceWebhook, ObservedAt: now.Add(-72 * time.Hour)})
	r.Observe(Observation{MessageID: "new", Source: SourceWebhook, ObservedAt: now})
	if n := r.Prune(now); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	if r.Size() != 1 {
		t.Fatalf("size %d", r.Size())
	}
}

func TestReconcilerIsConcurrencySafe(t *testing.T) {
	r := NewReconciler()
	now := recv()
	var wg sync.WaitGroup
	var mu sync.Mutex
	firsts := 0
	for i := 0; i < 50; i++ {
		for _, src := range []Source{SourceWebhook, SourcePoll} {
			wg.Add(1)
			go func(i int, src Source) {
				defer wg.Done()
				if r.Observe(Observation{MessageID: "m" + itoa(i), Source: src, ObservedAt: now}) {
					mu.Lock()
					firsts++
					mu.Unlock()
				}
			}(i, src)
		}
	}
	wg.Wait()
	if firsts != 50 {
		t.Fatalf("%d first sightings for 50 messages; each must be processed exactly once", firsts)
	}
	_ = r.Report(now)
}

func TestPollCursorNeverGoesBackwards(t *testing.T) {
	c := NewPollCursor()
	now := recv()
	c.Advance("mb1", now)
	c.Advance("mb1", now.Add(-time.Hour))
	if got := c.Since("mb1", time.Time{}); !got.Equal(now.Add(-c.Overlap)) {
		t.Fatalf("cursor moved backwards: %s", got)
	}
}

func TestPollCursorOverlapsToCatchUnorderedTimestamps(t *testing.T) {
	c := NewPollCursor()
	now := recv()
	c.Advance("mb1", now)
	since := c.Since("mb1", time.Time{})
	if !since.Before(now) {
		t.Fatal("no overlap: a message timestamped just before the cursor would be missed forever")
	}
}

// --- what the document's own implementation does ----------------------------

// rawIndex stores Message-IDs exactly as generated, which is what "store
// the Message-ID of every send" means if taken at face value.
type rawIndex map[string]SentMessage

func newRawIndex(sent ...SentMessage) rawIndex {
	r := rawIndex{}
	for _, s := range sent {
		r[s.MessageID] = s
	}
	return r
}

// docMatch is §7.1 reproduced verbatim, adapted only to this package's
// types so it can be run.
func docMatch(idx rawIndex, msg InboundMessage) (SentMessage, bool) {
	for _, ref := range append(msg.References, msg.InReplyTo...) {
		if sent, ok := idx[ref]; ok {
			return sent, true
		}
	}
	if sent, ok := idx[msg.ThreadID]; ok {
		return sent, true
	}
	return SentMessage{}, false
}

// TestDocumentsOwnMatchHasThreeDefects measures them rather than asserting
// them.
func TestDocumentsOwnMatchHasThreeDefects(t *testing.T) {
	i := newRawIndex(
		SentMessage{EnrollmentID: "old-sequence", MessageID: "<step1@ours>"},
		SentMessage{EnrollmentID: "current", MessageID: "<step3@ours>"},
	)

	// 1. References are searched before In-Reply-To, and oldest-first, so
	//    a forwarded thread matches an ancestor from a different
	//    enrolment instead of the message actually being replied to.
	msg := InboundMessage{
		InReplyTo:  []string{"<step3@ours>"},
		References: []string{"<step1@ours>", "<step3@ours>"},
	}
	got, _ := docMatch(i, msg)
	if got.EnrollmentID != "old-sequence" {
		t.Fatalf("expected the document's version to pick the wrong enrolment, got %q", got.EnrollmentID)
	}
	t.Logf("ordering: the document's version matched %q; the reply was to %q", got.EnrollmentID, "current")

	// 2. append(msg.References, ...) writes into the caller's backing
	//    array whenever it has spare capacity.
	refs := make([]string, 1, 4)
	refs[0] = "<step1@ours>"
	msg2 := InboundMessage{References: refs, InReplyTo: []string{"<step3@ours>"}}
	docMatch(i, msg2)
	clobbered := refs[:2][1]
	if clobbered != "<step3@ours>" {
		t.Fatalf("expected the caller's slice to be clobbered, got %q", clobbered)
	}
	t.Logf("aliasing: the caller's References backing array was overwritten with %q", clobbered)

	// 3. Angle brackets are compared literally. A client that omits them
	//    in References -- which is common -- never threads at all.
	misses := 0
	for _, ref := range []string{"step1@ours", "step3@ours"} {
		if _, ok := docMatch(i, InboundMessage{References: []string{ref}}); !ok {
			misses++
		}
	}
	t.Logf("normalisation: %d of 2 bracket-free references failed to thread", misses)
	if misses != 2 {
		t.Fatalf("expected both to miss, got %d", misses)
	}
}
