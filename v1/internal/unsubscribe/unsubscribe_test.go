package unsubscribe

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/suppression"
)

func setup() (*Handler, *suppression.List, *Signer) {
	list := suppression.New()
	signer := NewSigner([]byte("test-key-not-a-real-secret"))
	return &Handler{Signer: signer, List: list}, list, signer
}

func token(t *testing.T, s *Signer, email string) string {
	t.Helper()
	tok, err := s.Sign(Token{Email: email, EnrollmentID: "en1", SequenceID: "seq1"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// TestOneClickPostSuppressesImmediately is the RFC 8058 path: no
// confirmation page, no login, no preference centre.
func TestOneClickPostSuppressesImmediately(t *testing.T) {
	h, list, s := setup()
	tok := token(t, s, "prospect@example.com")

	req := httptest.NewRequest(http.MethodPost, "/u/"+tok,
		strings.NewReader("List-Unsubscribe=One-Click"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if !list.Suppressed("prospect@example.com") {
		t.Fatal("not suppressed; the requirement is immediately, not within 48 hours")
	}
	e := list.Check("prospect@example.com")
	if e.Reason != suppression.Unsubscribed {
		t.Fatalf("reason %s", e.Reason)
	}
}

// TestGetAlsoSuppresses: a human clicking the body link must not be shown a
// confirmation page they might not complete.
func TestGetAlsoSuppresses(t *testing.T) {
	h, list, s := setup()
	tok := token(t, s, "human@example.com")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/u/"+tok, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if !list.Suppressed("human@example.com") {
		t.Fatal("a GET on the body link did not suppress; a person who believes they unsubscribed and did not reaches for the spam button next")
	}
	body := w.Body.String()
	for _, forbidden := range []string{"confirm", "sign in", "log in", "preference"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("the response asks the user to %q", forbidden)
		}
	}
}

func TestInvalidTokenIsRejected(t *testing.T) {
	h, list, _ := setup()
	for _, tok := range []string{"garbage", "a.b", "", "notatoken.signature"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/u/"+tok, nil))
		if w.Code == http.StatusOK {
			t.Errorf("token %q accepted", tok)
		}
	}
	if list.Count() != 0 {
		t.Fatal("something was suppressed on an invalid token")
	}
}

func TestTamperedTokenIsRejected(t *testing.T) {
	h, list, s := setup()
	tok := token(t, s, "prospect@example.com")
	// Swap the payload, keep the signature.
	other, _ := s.Sign(Token{Email: "victim@example.com"})
	forged := strings.SplitN(other, ".", 2)[0] + "." + strings.SplitN(tok, ".", 2)[1]

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/u/"+forged, nil))
	if w.Code == http.StatusOK {
		t.Fatal("a forged token was accepted")
	}
	if list.Suppressed("victim@example.com") {
		t.Fatal("a forged token suppressed a third party")
	}
}

// TestOldTokensStillWork: refusing an unsubscribe because the link is old
// converts it into a complaint.
func TestOldTokensStillWork(t *testing.T) {
	_, list, s := setup()
	h := &Handler{Signer: s, List: list}
	old, err := s.Sign(Token{
		Email:    "prospect@example.com",
		IssuedAt: time.Now().AddDate(-3, 0, 0).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/u/"+old, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("a three-year-old unsubscribe link was refused (status %d)", w.Code)
	}
	if !list.Suppressed("prospect@example.com") {
		t.Fatal("not suppressed")
	}
}

type failingStopper struct{}

func (failingStopper) StopForUnsubscribe(string) error {
	return errNope
}

var errNope = &stopError{}

type stopError struct{}

func (*stopError) Error() string { return "database unavailable" }

// TestSuppressionSucceedsEvenWhenTheEnrolmentStopFails: the recipient must
// never be told their unsubscribe failed when the suppression landed.
func TestSuppressionSucceedsEvenWhenTheEnrolmentStopFails(t *testing.T) {
	list := suppression.New()
	s := NewSigner([]byte("k"))
	logged := 0
	h := &Handler{
		Signer: s, List: list, Stop: failingStopper{},
		Log: func(string, ...any) { logged++ },
	}
	tok := token(t, s, "prospect@example.com")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/u/"+tok, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d; an unsubscribe that returns an error becomes a spam complaint", w.Code)
	}
	if !list.Suppressed("prospect@example.com") {
		t.Fatal("not suppressed")
	}
	if logged != 1 {
		t.Fatalf("the failure was not logged (%d)", logged)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _, s := setup()
	tok := token(t, s, "a@b.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/u/"+tok, nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", w.Code)
	}
}

// TestHeadersAreRFC8058Compliant checks the pair Google requires.
func TestHeadersAreRFC8058Compliant(t *testing.T) {
	h := Headers("https://unsub.example.com", "unsub@example.com", "TOK")

	post := h["List-Unsubscribe-Post"]
	if post != "List-Unsubscribe=One-Click" {
		t.Fatalf("List-Unsubscribe-Post is %q; Google accepts only this form", post)
	}

	lu := h["List-Unsubscribe"]
	if !strings.Contains(lu, "<https://unsub.example.com/u/TOK>") {
		t.Fatalf("no https URL in List-Unsubscribe: %s", lu)
	}
	// Yahoo also accepts the mailto: fallback, so both are present.
	if !strings.Contains(lu, "<mailto:unsub@example.com?subject=TOK>") {
		t.Fatalf("no mailto fallback in List-Unsubscribe: %s", lu)
	}
}

func TestHeadersTolerateATrailingSlashOnTheBaseURL(t *testing.T) {
	h := Headers("https://unsub.example.com/", "u@e.example", "TOK")
	if strings.Contains(h["List-Unsubscribe"], "//u/TOK") {
		t.Fatalf("doubled slash: %s", h["List-Unsubscribe"])
	}
}

func TestBodyLink(t *testing.T) {
	if got := BodyLink("https://unsub.example.com", "TOK"); got != "https://unsub.example.com/u/TOK" {
		t.Fatalf("got %s", got)
	}
}

// TestBodyLinkEndpointWorks joins the two: the visible link has to resolve
// to the same handler, or making it prominent achieves nothing.
func TestBodyLinkEndpointWorks(t *testing.T) {
	h, list, s := setup()
	tok := token(t, s, "prospect@example.com")
	link := BodyLink("https://unsub.example.com", tok)

	srv := httptest.NewServer(h)
	defer srv.Close()

	path := strings.TrimPrefix(link, "https://unsub.example.com")
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !list.Suppressed("prospect@example.com") {
		t.Fatal("the body link did not suppress")
	}
}

func TestMailtoFallback(t *testing.T) {
	list := suppression.New()
	s := NewSigner([]byte("k"))
	p := &MailtoProcessor{Signer: s, List: list}

	tok := token(t, s, "prospect@example.com")
	if err := p.Process(tok, "prospect@example.com"); err != nil {
		t.Fatal(err)
	}
	if !list.Suppressed("prospect@example.com") {
		t.Fatal("not suppressed from the mailto token")
	}
}

// TestMailtoFallsBackToTheSender: a mangled subject must not defeat an
// unsubscribe.
func TestMailtoFallsBackToTheSender(t *testing.T) {
	list := suppression.New()
	p := &MailtoProcessor{Signer: NewSigner([]byte("k")), List: list}

	if err := p.Process("Re: unsubscribe", "someone@example.com"); err != nil {
		t.Fatal(err)
	}
	if !list.Suppressed("someone@example.com") {
		t.Fatal("a mangled subject line defeated the unsubscribe")
	}
}

func TestMailtoWithNothingUsableErrors(t *testing.T) {
	p := &MailtoProcessor{Signer: NewSigner([]byte("k")), List: suppression.New()}
	if err := p.Process("", ""); err == nil {
		t.Fatal("accepted a mailto with neither token nor sender")
	}
}

// TestUnsubscribeIsGlobalNotPerSequence is §6.2: per-sequence opt-out is
// how a person unsubscribes three times and then files a complaint.
func TestUnsubscribeIsGlobalNotPerSequence(t *testing.T) {
	h, list, s := setup()
	tok, _ := s.Sign(Token{Email: "prospect@example.com", EnrollmentID: "en1", SequenceID: "seq1"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/u/"+tok, nil))

	// Another rep's sequence, same person.
	if list.Check("prospect@example.com") == nil {
		t.Fatal("the suppression did not apply outside the sequence it came from")
	}
	// And a Gmail dot variant reaches the same inbox.
	list2 := suppression.New()
	h2 := &Handler{Signer: s, List: list2}
	tok2, _ := s.Sign(Token{Email: "first.last@gmail.com"})
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/u/"+tok2, nil))
	if !list2.Suppressed("firstlast@gmail.com") {
		t.Fatal("a Gmail dot variant of an unsubscribed address is still reachable")
	}
}

// TestUnsubscribeCanNeverExpire is enforced by the suppression list, and
// this is the endpoint's half of that contract.
func TestUnsubscribeCanNeverExpire(t *testing.T) {
	h, list, s := setup()
	tok := token(t, s, "prospect@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/u/"+tok, nil))

	e := list.Check("prospect@example.com")
	if !e.Reason.Permanent() {
		t.Fatal("the unsubscribe was recorded with an expiring reason")
	}
	if !e.ExpiresAt.IsZero() {
		t.Fatalf("the unsubscribe expires at %s", e.ExpiresAt)
	}
}
