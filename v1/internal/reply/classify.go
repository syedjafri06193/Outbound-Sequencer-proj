package reply

import (
	"regexp"
	"strings"
	"time"
)

// ReplyClass is what an inbound message actually is.
//
// Not every reply is a reply. The distinction is the whole point of this
// file: an out-of-office is an absence, a bounce notice is a machine, and
// treating either as engagement stops a sequence for someone who never read
// the email.
type ReplyClass string

const (
	ReplyPositive     ReplyClass = "positive"
	ReplyNegative     ReplyClass = "negative"
	ReplyHostile      ReplyClass = "hostile"
	ReplyReferral     ReplyClass = "referral"
	ReplyNotNow       ReplyClass = "not_now"
	ReplyOutOfOffice  ReplyClass = "out_of_office"
	ReplyBounceNotice ReplyClass = "bounce"
	ReplyLeftCompany  ReplyClass = "left_company"
	// ReplyUnknown is a human reply we could not place. It is not a
	// failure mode: a human replied, so the sequence stops and a rep
	// reads it. Guessing between positive and negative adds nothing that
	// a human reading one paragraph does not do better.
	ReplyUnknown ReplyClass = "unknown"
)

// Disposition is what the engine does about a classified reply.
type Disposition struct {
	Class ReplyClass
	// CountsAsReply distinguishes engagement from noise. An
	// out-of-office and a bounce notice are inbound mail that is not a
	// reply, and counting them corrupts the one metric (reply rate) that
	// still measures anything after open tracking died.
	CountsAsReply bool
	// StopSequence is terminal: the enrolment will not resume without
	// re-enrolment.
	StopSequence bool
	// PauseUntil is set instead of StopSequence when the contact is
	// merely absent. Zero means no pause.
	PauseUntil time.Time
	// SuppressGlobally removes the address from every sequence run by
	// every rep, forever.
	SuppressGlobally bool
	// SuppressReason is the suppression list reason to record. Empty
	// when SuppressGlobally is false.
	SuppressReason string
	// NotifyRep is whether a human should see this now.
	NotifyRep bool
	// FlagForReview marks it for a human to look at beyond the owning
	// rep -- hostile replies are complaint precursors and the person who
	// sent the email is not the right person to decide what happened.
	FlagForReview bool
	// FlagAccountForNewContact is set when the person has left and the
	// account is still worth pursuing through somebody else.
	FlagAccountForNewContact bool
	// ReEnrollAt is when to start again, for "circle back in Q3".
	ReEnrollAt time.Time
	// Rationale is operator-facing: why the engine did this.
	Rationale string
}

// Classifier turns inbound mail into a disposition.
type Classifier struct {
	// OOODelay overrides DefaultOOODelay when a return date cannot be
	// parsed.
	OOODelay time.Duration
}

func NewClassifier() *Classifier { return &Classifier{OOODelay: DefaultOOODelay} }

// Classify decides what an inbound message is and what to do about it.
//
// Order matters and is not arbitrary:
//
//  1. Bounce notices, because they come from a mailer daemon and their
//     body text is full of words that look like a human refusal.
//  2. Auto-replies, from HEADERS, before any body text is read. This is
//     the step that separates good sequencers from bad ones: "out of
//     office" in German, Japanese or Finnish is not something a phrase
//     list catches, but Auto-Submitted: auto-replied is the same in every
//     language.
//  3. Hostile, because an angry reply must never be softened into
//     "negative" by a later, gentler-looking match.
//  4. Everything else.
func (c *Classifier) Classify(msg InboundMessage) Disposition {
	if c.OOODelay <= 0 {
		c.OOODelay = DefaultOOODelay
	}

	if b, ok := classifyBounce(msg); ok {
		return b
	}

	// Headers first, and deliberately so. A body-text pass here would
	// mean a Finnish out-of-office with a perfectly correct
	// Auto-Submitted header gets read as an unknown human reply, the
	// sequence stops, and a prospect who was on holiday is never
	// contacted again.
	if IsAutoReply(msg) {
		return c.outOfOffice(msg, "detected from headers, before any body text was read")
	}

	// Bulk and list mail that landed in the thread is not engagement at
	// all. Neither a reply nor a reason to stop.
	if IsBulkOrList(msg) {
		return Disposition{
			Class:         ReplyUnknown,
			CountsAsReply: false,
			Rationale:     "bulk or list mail in the thread; not a reply and not a stop",
		}
	}

	body := normalizeBody(msg.Body)

	if matchAny(body, hostilePatterns) {
		return Disposition{
			Class:            ReplyHostile,
			CountsAsReply:    true,
			StopSequence:     true,
			SuppressGlobally: true,
			SuppressReason:   "hostile_reply",
			NotifyRep:        true,
			FlagForReview:    true,
			Rationale:        "hostile reply: the strongest complaint precursor there is. Suppressed everywhere, not just this sequence.",
		}
	}

	if matchAny(body, leftCompanyPatterns) {
		return Disposition{
			Class:                    ReplyLeftCompany,
			CountsAsReply:            false,
			StopSequence:             true,
			SuppressGlobally:         true,
			SuppressReason:           "manual",
			NotifyRep:                true,
			FlagAccountForNewContact: true,
			Rationale:                "contact has left; suppress the person and find someone else at the account",
		}
	}

	// "Not now" before "negative": "not interested right now, try me in
	// Q3" contains a refusal and a date, and the date is the useful part.
	if when, ok := parseNotNow(msg, body); ok {
		return Disposition{
			Class:         ReplyNotNow,
			CountsAsReply: true,
			StopSequence:  true,
			NotifyRep:     true,
			ReEnrollAt:    when,
			Rationale:     "deferred, not refused: stop now and re-enrol at the date they named",
		}
	}

	if matchAny(body, referralPatterns) {
		return Disposition{
			Class:         ReplyReferral,
			CountsAsReply: true,
			StopSequence:  true,
			NotifyRep:     true,
			Rationale:     "referred elsewhere; a human reads the name and decides",
		}
	}

	if matchAny(body, negativePatterns) {
		return Disposition{
			Class:         ReplyNegative,
			CountsAsReply: true,
			StopSequence:  true,
			NotifyRep:     true,
			Rationale:     "declined; stop the sequence",
		}
	}

	if matchAny(body, positivePatterns) {
		return Disposition{
			Class:         ReplyPositive,
			CountsAsReply: true,
			StopSequence:  true,
			NotifyRep:     true,
			Rationale:     "interested; stop the sequence and hand to the rep immediately",
		}
	}

	// A human replied and we could not place it. Stop anyway. The cost of
	// stopping a sequence for a human who wrote back is zero -- the rep
	// picks it up -- and the cost of continuing is a prospect who is
	// being talked over by a machine.
	return Disposition{
		Class:         ReplyUnknown,
		CountsAsReply: true,
		StopSequence:  true,
		NotifyRep:     true,
		Rationale:     "a human replied and the text did not match a known shape; stopped and handed to the rep",
	}
}

// outOfOffice builds the OOO disposition.
//
// A pause, never a stop, and explicitly not counted as a reply. This is the
// case the whole classifier exists for: the prospect did not read the
// email, did not decline, and is back on a known date.
func (c *Classifier) outOfOffice(msg InboundMessage, how string) Disposition {
	resume := ResumeAfter(msg)
	rationale := "out of office (" + how + "); paused until " + resume.Format("2006-01-02")
	if _, ok := ParseReturnDate(msg.Body, msg.ReceivedAt); !ok {
		rationale += ", no return date parsed so the conservative default was used"
	}
	return Disposition{
		Class: ReplyOutOfOffice,
		// The single most important false value in this file.
		CountsAsReply: false,
		StopSequence:  false,
		PauseUntil:    resume,
		NotifyRep:     false,
		Rationale:     rationale,
	}
}

// --- bounce handling --------------------------------------------------------

// BounceKind separates the two cases that need opposite handling.
type BounceKind string

const (
	BounceNone BounceKind = ""
	BounceHard BounceKind = "hard"
	BounceSoft BounceKind = "soft"
)

// SoftBounceLimit is how many soft bounces to tolerate before treating the
// address as dead.
//
// Five: a mailbox that has been full for five consecutive attempts is not
// going to be emptied for a cold email, and continuing to hit it is
// volume spent on a wall.
const SoftBounceLimit = 5

// ClassifyBounceStatus maps an RFC 3463 enhanced status code to a kind.
//
// The status code is used in preference to the body, and in preference to
// the SMTP reply class, because it is the one field that says which of the
// two cases this is without ambiguity. 5.x.x is permanent, 4.x.x is
// transient -- with one documented exception below.
func ClassifyBounceStatus(status string) BounceKind {
	status = strings.TrimSpace(status)
	switch {
	case strings.HasPrefix(status, "5."):
		// 5.2.2 is "mailbox full", which is permanent by class and
		// transient in fact. Treating it as a hard bounce permanently
		// suppresses someone whose inbox was full for a week.
		if strings.HasPrefix(status, "5.2.2") {
			return BounceSoft
		}
		return BounceHard
	case strings.HasPrefix(status, "4."):
		return BounceSoft
	default:
		return BounceNone
	}
}

var (
	hardBounceBodyPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\buser unknown\b`),
		regexp.MustCompile(`(?i)\bno such user\b`),
		regexp.MustCompile(`(?i)\bdoes ?n[o']?t exist\b`),
		regexp.MustCompile(`(?i)\brecipient (?:address )?rejected\b`),
		regexp.MustCompile(`(?i)\baddress not found\b`),
		regexp.MustCompile(`(?i)\bunrouteable address\b`),
		regexp.MustCompile(`(?i)\bdomain not found\b`),
	}
	softBounceBodyPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bmailbox (?:is )?full\b`),
		regexp.MustCompile(`(?i)\bover quota\b`),
		regexp.MustCompile(`(?i)\bquota exceeded\b`),
		regexp.MustCompile(`(?i)\btemporar(?:y|ily) (?:failure|deferred|rejected)\b`),
		regexp.MustCompile(`(?i)\btry again later\b`),
		regexp.MustCompile(`(?i)\bgreylist`),
	}
)

// classifyBounce recognises a delivery status notification.
func classifyBounce(msg InboundMessage) (Disposition, bool) {
	kind := BounceNone

	// The structured route first: an RFC 3464 report carries the status
	// as a header field and needs no text matching at all.
	if s := msg.Header("Status"); s != "" {
		kind = ClassifyBounceStatus(s)
	}
	if kind == BounceNone && !looksLikeDSN(msg) {
		return Disposition{}, false
	}

	if kind == BounceNone {
		body := normalizeBody(msg.Body)
		switch {
		case matchAny(body, hardBounceBodyPatterns):
			kind = BounceHard
		case matchAny(body, softBounceBodyPatterns):
			kind = BounceSoft
		default:
			// A DSN we cannot classify is treated as soft. Getting this
			// wrong towards hard permanently suppresses a real address
			// on the strength of an unparsed machine message.
			kind = BounceSoft
		}
	}

	if kind == BounceHard {
		return Disposition{
			Class:            ReplyBounceNotice,
			CountsAsReply:    false,
			StopSequence:     true,
			SuppressGlobally: true,
			SuppressReason:   "hard_bounce",
			NotifyRep:        false,
			Rationale:        "hard bounce: the address does not exist. Suppressed permanently -- continuing to hit it is what builds a bad sender reputation.",
		}, true
	}

	return Disposition{
		Class:         ReplyBounceNotice,
		CountsAsReply: false,
		StopSequence:  false,
		Rationale:     "soft bounce: transient. Retry with backoff and suppress only after repeated failures.",
	}, true
}

// looksLikeDSN recognises a delivery status notification by its envelope.
func looksLikeDSN(msg InboundMessage) bool {
	ct := strings.ToLower(msg.Header("Content-Type"))
	if strings.Contains(ct, "report-type=delivery-status") ||
		strings.Contains(ct, "message/delivery-status") {
		return true
	}
	if msg.Header("X-Failed-Recipients") != "" || msg.Header("Diagnostic-Code") != "" {
		return true
	}
	// MAILER-DAEMON and postmaster are the conventional senders, and a
	// null Return-Path plus one of those is about as clear as machine
	// mail gets.
	from := strings.ToLower(msg.From)
	if strings.Contains(from, "mailer-daemon") || strings.Contains(from, "postmaster@") {
		return true
	}
	return false
}

// BounceOutcome is the running decision for an address across attempts.
type BounceOutcome struct {
	Suppress bool
	RetryAt  time.Time
	Reason   string
}

// OnBounce folds one bounce into the history for an address.
//
// consecutiveSoft is the count INCLUDING this bounce.
func OnBounce(kind BounceKind, consecutiveSoft int, now time.Time) BounceOutcome {
	switch kind {
	case BounceHard:
		return BounceOutcome{Suppress: true, Reason: "hard bounce: address does not exist"}
	case BounceSoft:
		if consecutiveSoft >= SoftBounceLimit {
			return BounceOutcome{
				Suppress: true,
				Reason:   "soft bounced " + itoa(consecutiveSoft) + " times in a row; treated as undeliverable",
			}
		}
		// Exponential backoff, capped. Retrying a deferred address every
		// hour is a good way to be greylisted into permanence.
		backoff := time.Duration(1<<uint(consecutiveSoft)) * time.Hour
		if backoff > 72*time.Hour {
			backoff = 72 * time.Hour
		}
		return BounceOutcome{RetryAt: now.Add(backoff), Reason: "soft bounce; retrying with backoff"}
	default:
		return BounceOutcome{}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// --- body patterns ----------------------------------------------------------
//
// Body matching is the LAST resort, used only after the header checks have
// had their say, and only to route a message a human already wrote. It is
// English-only and known to be so; a non-English reply falls through to
// ReplyUnknown, which stops the sequence and notifies the rep. That is the
// correct failure: a human sees it.
//
// The dangerous version of this is body-matching for out-of-office, which
// this file never does.

var (
	hostilePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:stop|quit|cease) (?:emailing|contacting|messaging|mailing) me\b`),
		regexp.MustCompile(`(?i)\bunsubscribe\b`),
		regexp.MustCompile(`(?i)\btake me off\b`),
		regexp.MustCompile(`(?i)\bremove me from\b`),
		regexp.MustCompile(`(?i)\b(?:do not|don'?t) (?:ever )?(?:contact|email|message) me\b`),
		regexp.MustCompile(`(?i)\bthis is spam\b`),
		regexp.MustCompile(`(?i)\breport(?:ing)? (?:you|this) (?:as|for) spam\b`),
		regexp.MustCompile(`(?i)\bmark(?:ing)? (?:this|you) as spam\b`),
		regexp.MustCompile(`(?i)\b(?:gdpr|can-?spam|casl)\b.{0,40}\b(?:violat|complain|report|breach)`),
		regexp.MustCompile(`(?i)\bleave me alone\b`),
		regexp.MustCompile(`(?i)\bhow did you get my (?:email|details|address)\b`),
	}

	leftCompanyPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bno longer (?:with|at|works? (?:at|for)|employed)\b`),
		regexp.MustCompile(`(?i)\bhas left (?:the )?(?:company|the business|the organi[sz]ation|us)\b`),
		regexp.MustCompile(`(?i)\bI(?:'ve| have) (?:since )?left\b`),
		regexp.MustCompile(`(?i)\bno longer part of\b`),
		regexp.MustCompile(`(?i)\bthis (?:person|employee) (?:is )?no longer\b`),
	}

	referralPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:speak|talk|reach out) to\b`),
		regexp.MustCompile(`(?i)\b(?:copying|cc'?ing|looping) in\b`),
		regexp.MustCompile(`(?i)\bnot the right person\b`),
		regexp.MustCompile(`(?i)\bwrong person\b`),
		regexp.MustCompile(`(?i)\bmy colleague\b`),
		regexp.MustCompile(`(?i)\bhandles? (?:this|that) (?:for us|area)\b`),
		regexp.MustCompile(`(?i)\bforward(?:ing|ed)? (?:this )?(?:on )?to\b`),
	}

	negativePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bnot interested\b`),
		regexp.MustCompile(`(?i)\bno thank(?:s| you)\b`),
		regexp.MustCompile(`(?i)\bwe(?:'re| are) (?:all )?(?:set|good|sorted)\b`),
		regexp.MustCompile(`(?i)\balready (?:have|use|using|working with)\b`),
		regexp.MustCompile(`(?i)\bno (?:budget|need|plans)\b`),
		regexp.MustCompile(`(?i)\bnot (?:a|the right) (?:fit|priority)\b`),
		regexp.MustCompile(`(?i)\bwe(?:'ll| will) pass\b`),
	}

	positivePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:sounds|looks) (?:good|interesting|great)\b`),
		regexp.MustCompile(`(?i)\b(?:i'?m|we'?re|i am|we are) interested\b`),
		regexp.MustCompile(`(?i)\bhappy to (?:chat|talk|meet|connect)\b`),
		regexp.MustCompile(`(?i)\b(?:book|set ?up|schedule) (?:a |some )?(?:time|call|meeting|chat)\b`),
		regexp.MustCompile(`(?i)\bsend (?:me )?(?:over )?(?:more|some) (?:info|details|information)\b`),
		regexp.MustCompile(`(?i)\btell me more\b`),
		regexp.MustCompile(`(?i)\bwhat(?:'s| is) your (?:pricing|availability)\b`),
		regexp.MustCompile(`(?i)\b(?:does|would) (?:next week|tomorrow|thursday|friday|monday|tuesday|wednesday) work\b`),
	}
)

// notNowPatterns capture the deferral and, where possible, the date.
var notNowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:circle back|check back|reach out|get in touch|follow up|touch base|try me|ping me)\b[^.!?\n]{0,40}?\b(?:in|after|around|next|during)\s+([^.!?\n]{1,30})`),
	regexp.MustCompile(`(?i)\b(?:not (?:right )?now|bad timing|wrong time|revisit)\b[^.!?\n]{0,60}?\b(?:in|after|around|next|during)\s+([^.!?\n]{1,30})`),
	regexp.MustCompile(`(?i)\bnot (?:until|before)\s+([^.!?\n]{1,30})`),
}

var bareNotNowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bnot (?:right )?now\b`),
	regexp.MustCompile(`(?i)\bbad timing\b`),
	regexp.MustCompile(`(?i)\bcircle back\b`),
	regexp.MustCompile(`(?i)\bcheck back\b`),
	regexp.MustCompile(`(?i)\brevisit (?:this )?(?:later|next)\b`),
}

// DefaultNotNowDelay is used when someone defers without naming a date.
//
// Ninety days. Long enough that "not now" was respected, short enough that
// the prospect is not lost.
const DefaultNotNowDelay = 90 * 24 * time.Hour

func parseNotNow(msg InboundMessage, body string) (time.Time, bool) {
	for _, re := range notNowPatterns {
		m := re.FindStringSubmatch(body)
		if len(m) < 2 {
			continue
		}
		if when, ok := parseFuturePeriod(strings.TrimSpace(m[1]), msg.ReceivedAt); ok {
			return when, true
		}
		// Matched the shape but not the date: still a deferral.
		return msg.ReceivedAt.Add(DefaultNotNowDelay), true
	}
	if matchAny(body, bareNotNowPatterns) {
		return msg.ReceivedAt.Add(DefaultNotNowDelay), true
	}
	return time.Time{}, false
}

var (
	quarterRe  = regexp.MustCompile(`(?i)\bq([1-4])\b(?:\s+(\d{4}))?`)
	monthsRe   = regexp.MustCompile(`(?i)\b(\d{1,2})\s+months?\b`)
	weeksRe    = regexp.MustCompile(`(?i)\b(\d{1,2})\s+weeks?\b`)
	nextYearRe = regexp.MustCompile(`(?i)\bnext year\b`)
	monthNames = map[string]time.Month{
		"january": time.January, "jan": time.January,
		"february": time.February, "feb": time.February,
		"march": time.March, "mar": time.March,
		"april": time.April, "apr": time.April,
		"may":  time.May,
		"june": time.June, "jun": time.June,
		"july": time.July, "jul": time.July,
		"august": time.August, "aug": time.August,
		"september": time.September, "sep": time.September, "sept": time.September,
		"october": time.October, "oct": time.October,
		"november": time.November, "nov": time.November,
		"december": time.December, "dec": time.December,
	}
)

// parseFuturePeriod turns "Q3", "three months", "next year", "March" into a
// date.
//
// Always resolves forward. "Q1" said in November means next year's Q1, and
// re-enrolling someone eleven months in the past means re-enrolling them
// tomorrow, which is the opposite of what they asked for.
func parseFuturePeriod(s string, from time.Time) (time.Time, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	loc := from.Location()

	if m := quarterRe.FindStringSubmatch(s); m != nil {
		q := int(m[1][0] - '0')
		month := time.Month((q-1)*3 + 1)
		year := from.Year()
		if len(m) > 2 && m[2] != "" {
			y := 0
			for _, c := range m[2] {
				y = y*10 + int(c-'0')
			}
			year = y
		}
		t := time.Date(year, month, 1, 9, 0, 0, 0, loc)
		for !t.After(from) {
			t = t.AddDate(1, 0, 0)
		}
		return t, true
	}

	if m := monthsRe.FindStringSubmatch(s); m != nil {
		n := atoiSmall(m[1])
		if n > 0 && n <= 24 {
			return from.AddDate(0, n, 0), true
		}
	}
	if m := weeksRe.FindStringSubmatch(s); m != nil {
		n := atoiSmall(m[1])
		if n > 0 && n <= 104 {
			return from.AddDate(0, 0, 7*n), true
		}
	}
	if nextYearRe.MatchString(s) {
		return time.Date(from.Year()+1, time.January, 15, 9, 0, 0, 0, loc), true
	}

	for _, word := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z')
	}) {
		if month, ok := monthNames[word]; ok {
			t := time.Date(from.Year(), month, 1, 9, 0, 0, 0, loc)
			if !t.After(from) {
				t = t.AddDate(1, 0, 0)
			}
			return t, true
		}
	}

	return time.Time{}, false
}

func atoiSmall(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// quoteRe strips quoted history from a reply.
//
// Without it, the original cold email's own text is matched against these
// patterns, and a template containing "happy to chat" classifies every
// reply as positive regardless of what the human wrote.
var quoteRe = regexp.MustCompile(`(?m)^\s*>.*$`)

// replyDividers mark the start of the quoted original in the common
// clients.
var replyDividers = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^\s*-{2,}\s*original message\s*-{2,}\s*$`),
	regexp.MustCompile(`(?im)^\s*_{5,}\s*$`),
	regexp.MustCompile(`(?im)^\s*on .{5,80}\bwrote:\s*$`),
	regexp.MustCompile(`(?im)^\s*from:\s.+$`),
	regexp.MustCompile(`(?im)^\s*-{2,}\s*forwarded message\s*-{2,}\s*$`),
}

func normalizeBody(body string) string {
	// Cut at the first reply divider: everything below it is our own
	// email being quoted back.
	cut := len(body)
	for _, re := range replyDividers {
		if loc := re.FindStringIndex(body); loc != nil && loc[0] < cut {
			cut = loc[0]
		}
	}
	b := body[:cut]
	b = quoteRe.ReplaceAllString(b, "")
	return strings.TrimSpace(b)
}

func matchAny(s string, patterns []*regexp.Regexp) bool {
	for _, re := range patterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
