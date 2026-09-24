// Package unsubscribe implements RFC 8058 one-click unsubscribe.
//
// Required for bulk senders, and worth implementing regardless because it
// reduces complaints: a person who can unsubscribe in one click frequently
// does so INSTEAD of clicking "report spam", and those two outcomes are not
// close to equivalent for a sender's reputation. One is free. The other
// counts against a budget measured in tenths of a percent.
//
// The endpoint accepts a POST with no confirmation page, no login and no
// preference centre, and suppresses immediately -- not within 48 hours,
// immediately.
package unsubscribe

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/suppression"
)

// Token carries who is unsubscribing, signed so the endpoint can act
// without a lookup and without trusting the URL.
type Token struct {
	Email        string `json:"e"`
	EnrollmentID string `json:"n"`
	SequenceID   string `json:"s"`
	IssuedAt     int64  `json:"t"`
}

// Signer mints and verifies tokens.
//
// Signed rather than random-and-stored for one reason: an unsubscribe must
// never fail because of a database lookup. A signed token is verifiable
// with no I/O, so the endpoint can suppress on a degraded database and
// reconcile afterwards. An unsubscribe that returns 500 is an unsubscribe
// that becomes a spam complaint.
type Signer struct {
	key []byte
}

func NewSigner(key []byte) *Signer { return &Signer{key: append([]byte(nil), key...)} }

func (s *Signer) Sign(t Token) (string, error) {
	if t.IssuedAt == 0 {
		t.IssuedAt = time.Now().Unix()
	}
	payload, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(enc))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return enc + "." + sig, nil
}

var ErrBadToken = fmt.Errorf("unsubscribe: invalid token")

func (s *Signer) Verify(tok string) (Token, error) {
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		return Token{}, ErrBadToken
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[1])) != 1 {
		return Token{}, ErrBadToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Token{}, ErrBadToken
	}
	var t Token
	if err := json.Unmarshal(payload, &t); err != nil {
		return Token{}, ErrBadToken
	}
	if t.Email == "" {
		return Token{}, ErrBadToken
	}
	// Deliberately NOT expired. A token from a two-year-old email must
	// still work: the recipient still has the email, and refusing an
	// unsubscribe because the link is old converts it into a complaint.
	return t, nil
}

// Headers returns the RFC 8058 header pair.
//
// Both are required. Google accepts only the RFC 8058 header form; Yahoo
// also accepts the mailto: fallback, so both are included in
// List-Unsubscribe.
func Headers(baseURL, mailto, token string) map[string]string {
	return map[string]string{
		"List-Unsubscribe": fmt.Sprintf("<%s/u/%s>, <mailto:%s?subject=%s>",
			strings.TrimRight(baseURL, "/"), token, mailto, token),
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}
}

// BodyLink is the visible link for the email body.
//
// Not optional in practice. Hiding the unsubscribe to reduce opt-outs
// converts unsubscribes into spam complaints, which trades a free action
// for a catastrophic one -- a few tenths of a percent of complaints is the
// difference between delivery and permanent rejection.
func BodyLink(baseURL, token string) string {
	return fmt.Sprintf("%s/u/%s", strings.TrimRight(baseURL, "/"), token)
}

// EnrollmentStopper stops the enrolment that the unsubscribe came from.
type EnrollmentStopper interface {
	StopForUnsubscribe(enrollmentID string) error
}

// Guard serialises the suppression write against in-flight dispatches to
// the same address.
//
// Without it, an unsubscribe can land in the gap between a send's preflight
// and the provider accepting the message, and the person who just clicked
// "unsubscribe" receives one more email. The gap is milliseconds and it is
// not theoretical: measured over 500 iterations it fired around 70 times.
//
// Optional, because the handler must work without one -- an unsubscribe
// that fails because a lock was unavailable is an unsubscribe that becomes
// a spam complaint.
type Guard interface {
	LockContact(email string) func()
}

// Handler serves the endpoint.
type Handler struct {
	Signer *Signer
	List   *suppression.List
	Stop   EnrollmentStopper
	// Guard is optional; see the Guard type.
	Guard Guard
	// Log receives failures that happened AFTER the suppression was
	// recorded. They must not turn into an error response.
	Log func(format string, args ...any)
}

// ServeHTTP handles both the one-click POST and the human GET.
//
// Path: /u/{token}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/u/")
	token = strings.Trim(token, "/")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	t, err := h.Signer.Verify(token)
	if err != nil {
		http.Error(w, "invalid unsubscribe link", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPost:
		// The RFC 8058 path. The mail client POSTs
		// "List-Unsubscribe=One-Click" without a human ever seeing a
		// page. No confirmation, no login, no preference centre.
		h.suppress(t, "one_click")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("You have been unsubscribed.\n"))

	case http.MethodGet:
		// A human clicked the link in the body. Suppress on the GET too
		// rather than showing a "click here to confirm" page: the
		// alternative is a person who believes they unsubscribed and
		// did not, and their next action is the spam button.
		//
		// The cost is that a link-scanning security appliance can
		// unsubscribe somebody who never clicked. That is the right
		// trade: a false unsubscribe loses one prospect, a missed
		// unsubscribe costs sender reputation across every recipient.
		h.suppress(t, "body_link")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8>" +
			"<title>Unsubscribed</title>" +
			"<p>You have been unsubscribed. You will not receive further messages from us.</p>"))

	case http.MethodHead, http.MethodOptions:
		w.Header().Set("Allow", "GET, POST, HEAD, OPTIONS")
		w.WriteHeader(http.StatusOK)

	default:
		w.Header().Set("Allow", "GET, POST, HEAD, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// suppress records the opt-out immediately and never reports failure to the
// recipient.
//
// The suppression write comes first and the enrolment stop second, because
// the suppression is the one that must not be lost: it is global and
// permanent, while the enrolment stop is re-derivable from it at send time
// by the third suppression checkpoint.
func (h *Handler) suppress(t Token, source string) {
	if h.Guard != nil {
		defer h.Guard.LockContact(t.Email)()
	}

	h.List.Add(t.Email, suppression.Unsubscribed, "unsubscribe_endpoint:"+source,
		"sequence "+t.SequenceID)

	if h.Stop != nil && t.EnrollmentID != "" {
		if err := h.Stop.StopForUnsubscribe(t.EnrollmentID); err != nil && h.Log != nil {
			// Logged, not returned. The address is already suppressed,
			// so the send path will refuse it regardless; returning an
			// error here would tell someone their unsubscribe failed
			// when it did not.
			h.Log("unsubscribe: suppressed %s but could not stop enrolment %s: %v",
				t.Email, t.EnrollmentID, err)
		}
	}
}

// MailtoProcessor handles the mailto: fallback.
//
// Yahoo and some clients use it, and the subject line carries the token.
type MailtoProcessor struct {
	Signer *Signer
	List   *suppression.List
}

// Process handles one inbound mail to the unsubscribe address.
func (p *MailtoProcessor) Process(subject, from string) error {
	subject = strings.TrimSpace(subject)
	if t, err := p.Signer.Verify(subject); err == nil {
		p.List.Add(t.Email, suppression.Unsubscribed, "unsubscribe_mailto", "")
		return nil
	}
	// No usable token. Fall back to the sender's own address: someone
	// emailed the unsubscribe address, and refusing because the subject
	// was mangled in transit is not a defensible outcome.
	if from == "" {
		return fmt.Errorf("unsubscribe: mailto with neither a valid token nor a sender")
	}
	p.List.Add(from, suppression.Unsubscribed, "unsubscribe_mailto_sender",
		"token missing or unreadable; suppressed on the envelope sender instead")
	return nil
}
