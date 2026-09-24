// Package reply detects and classifies inbound mail.
package reply

import (
	"regexp"
	"strings"
	"time"
)

// InboundMessage is a received email.
type InboundMessage struct {
	MessageID  string
	InReplyTo  []string
	References []string
	ThreadID   string
	From       string
	Subject    string
	Body       string
	Headers    map[string]string
	ReceivedAt time.Time
}

// Header does a case-insensitive lookup.
//
// Header names are case-insensitive per RFC 5322, and providers disagree
// about capitalisation -- Gmail returns "Auto-Submitted", Graph sometimes
// returns "auto-submitted". A case-sensitive map lookup silently misses
// every auto-reply from one of them.
func (m InboundMessage) Header(name string) string {
	if m.Headers == nil {
		return ""
	}
	if v, ok := m.Headers[name]; ok {
		return v
	}
	lower := strings.ToLower(name)
	for k, v := range m.Headers {
		if strings.ToLower(k) == lower {
			return v
		}
	}
	return ""
}

// SentMessage records a message we sent, for threading.
type SentMessage struct {
	EnrollmentID string
	StepIndex    int
	// MessageID is the RFC 5322 Message-ID we generated.
	MessageID string
	// ThreadID is the provider's thread id, which survives some client
	// mangling that breaks header chains.
	ThreadID string
	SentAt   time.Time
}

// SentIndex looks up sends by message and thread id.
type SentIndex interface {
	BySentMessageID(messageID string) (SentMessage, bool)
	BySentThreadID(threadID string) (SentMessage, bool)
}

// Match finds the enrolment an inbound message replies to.
//
// Threaded on Message-ID and References, never on subject. Subject
// matching breaks on Re: prefixes, localised prefixes (AW:, SV:, Antw:,
// RE:, Res:), forwards, and subject edits -- and it produces false
// positives across unrelated threads that happen to share a subject, which
// is worse than missing the match.
func Match(idx SentIndex, msg InboundMessage) (SentMessage, bool) {
	// In-Reply-To first: it names the immediate parent, which is the most
	// specific answer. References is the whole ancestry and can include
	// messages from a different enrolment if a thread was forwarded and
	// replied to.
	for _, ref := range msg.InReplyTo {
		if sent, ok := idx.BySentMessageID(normalizeMessageID(ref)); ok {
			return sent, true
		}
	}
	// References walked newest-first, since the last entry is the nearest
	// ancestor.
	for i := len(msg.References) - 1; i >= 0; i-- {
		if sent, ok := idx.BySentMessageID(normalizeMessageID(msg.References[i])); ok {
			return sent, true
		}
	}
	if msg.ThreadID != "" {
		if sent, ok := idx.BySentThreadID(msg.ThreadID); ok {
			return sent, true
		}
	}
	return SentMessage{}, false
}

// normalizeMessageID strips the angle brackets and surrounding space.
//
// Some clients include them in References and some do not, so storing one
// form and comparing against the other silently breaks threading for those
// clients.
func normalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.ToLower(id)
}

// ParseReferences splits a References header into ids.
func ParseReferences(header string) []string {
	if header == "" {
		return nil
	}
	fields := strings.Fields(header)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if id := normalizeMessageID(f); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// --- auto-reply detection ---------------------------------------------------

// IsAutoReply detects automated responses from headers.
//
// Header-based rather than body-text based, and that is the whole point:
// body matching fails across languages, and "out of office" in German,
// Japanese or Finnish is not something a phrase list catches. Headers are
// standardised and the same everywhere.
//
// Treating an auto-reply as engagement stops the sequence for someone who
// never saw the email, which is the most common way a sequencer wastes a
// prospect.
func IsAutoReply(msg InboundMessage) bool {
	// RFC 3834. "no" explicitly means this is NOT an automatic response,
	// so the value has to be checked rather than merely the presence.
	if v := msg.Header("Auto-Submitted"); v != "" && !strings.EqualFold(strings.TrimSpace(v), "no") {
		return true
	}
	if msg.Header("X-Autoreply") != "" || msg.Header("X-Autorespond") != "" {
		return true
	}
	if p := strings.ToLower(msg.Header("Precedence")); strings.Contains(p, "auto_reply") {
		return true
	}
	if msg.Header("X-Auto-Response-Suppress") != "" {
		return true
	}
	// RFC 3834: automatic responses should carry a null Return-Path, which
	// is also what stops them looping.
	if strings.TrimSpace(msg.Header("Return-Path")) == "<>" {
		return true
	}
	// Microsoft's own marker, which Exchange sets on OOF replies.
	if v := msg.Header("X-MS-Exchange-Inbox-Rules-Loop"); v != "" {
		return true
	}
	return false
}

// IsBulkOrList reports mail that is not a human reply at all.
//
// A newsletter or list message that happens to land in the thread is not
// engagement, and counting it stops a sequence for nothing.
func IsBulkOrList(msg InboundMessage) bool {
	if p := strings.ToLower(msg.Header("Precedence")); p == "bulk" || p == "list" || p == "junk" {
		return true
	}
	if msg.Header("List-Id") != "" || msg.Header("List-Unsubscribe") != "" {
		return true
	}
	return false
}

// returnDatePatterns match the common phrasings.
//
// Deliberately narrow. A confident wrong parse resumes a sequence while
// someone is still away, which wastes the touch and reads as inattentive;
// a failed parse falls back to a conservative default, which costs only
// time.
var returnDatePatterns = []*regexp.Regexp{
	// "back on 12 March", "returning on March 12", "return on 2026-03-12"
	regexp.MustCompile(`(?i)\b(?:back|return(?:ing)?|returns)\s+(?:to the office\s+)?on\s+(\d{4}-\d{2}-\d{2})`),
	regexp.MustCompile(`(?i)\b(?:back|return(?:ing)?|returns)\s+(?:to the office\s+)?on\s+(\d{1,2}(?:st|nd|rd|th)?\s+\w+)`),
	regexp.MustCompile(`(?i)\b(?:back|return(?:ing)?|returns)\s+(?:to the office\s+)?on\s+(\w+\s+\d{1,2}(?:st|nd|rd|th)?)`),
	// "until 12 March", "until March 12"
	regexp.MustCompile(`(?i)\buntil\s+(\d{4}-\d{2}-\d{2})`),
	regexp.MustCompile(`(?i)\buntil\s+(\d{1,2}(?:st|nd|rd|th)?\s+\w+)`),
	regexp.MustCompile(`(?i)\buntil\s+(\w+\s+\d{1,2}(?:st|nd|rd|th)?)`),
}

var dateLayouts = []string{
	"2006-01-02",
	"2 January",
	"02 January",
	"2 Jan",
	"January 2",
	"Jan 2",
}

// DefaultOOODelay is used when no date can be parsed confidently.
//
// Seven days, and deliberately conservative: resuming while someone is
// still away wastes a touch, and the cost of waiting too long is only
// time.
const DefaultOOODelay = 7 * 24 * time.Hour

// ParseReturnDate extracts a return date from an out-of-office body.
//
// Returns ok=false rather than guessing. The caller then applies
// DefaultOOODelay.
func ParseReturnDate(body string, received time.Time) (time.Time, bool) {
	for _, re := range returnDatePatterns {
		m := re.FindStringSubmatch(body)
		if len(m) < 2 {
			continue
		}
		raw := strings.TrimSpace(m[1])
		if d, ok := parseDateFragment(raw, received); ok {
			return d, true
		}
	}
	return time.Time{}, false
}

func parseDateFragment(raw string, received time.Time) (time.Time, bool) {
	cleaned := stripOrdinals(raw)

	for _, layout := range dateLayouts {
		t, err := time.ParseInLocation(layout, cleaned, received.Location())
		if err != nil {
			continue
		}
		// Layouts without a year parse as year 0. Assume the next
		// occurrence: an OOO reply naming "March 12" in November means
		// next March, not one that already passed.
		if t.Year() == 0 {
			t = time.Date(received.Year(), t.Month(), t.Day(), 9, 0, 0, 0, received.Location())
			if t.Before(received) {
				t = t.AddDate(1, 0, 0)
			}
		}
		// A date in the past is a failed parse, not a return date.
		if t.Before(received) {
			return time.Time{}, false
		}
		// And one absurdly far ahead is a misparse -- "until further
		// notice" style text can produce a year that is not a year.
		if t.After(received.AddDate(1, 0, 0)) {
			return time.Time{}, false
		}
		return t, true
	}
	return time.Time{}, false
}

var ordinalRe = regexp.MustCompile(`(\d+)(?:st|nd|rd|th)\b`)

func stripOrdinals(s string) string {
	return ordinalRe.ReplaceAllString(s, "$1")
}

// ResumeAfter returns when a sequence paused for an out-of-office should
// resume.
func ResumeAfter(msg InboundMessage) time.Time {
	if d, ok := ParseReturnDate(msg.Body, msg.ReceivedAt); ok {
		// Resume the morning after the stated return date rather than on
		// it: an inbox on the first day back is not where a cold email
		// wants to be.
		return d.AddDate(0, 0, 1)
	}
	return msg.ReceivedAt.Add(DefaultOOODelay)
}
