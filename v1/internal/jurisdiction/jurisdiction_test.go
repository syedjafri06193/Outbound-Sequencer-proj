package jurisdiction

import (
	"errors"
	"testing"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

func now() time.Time { return time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC) }

func gate() *Gate {
	g := NewGate()
	g.SetClock(now)
	return g
}

func contact(country string) model.Contact {
	return model.Contact{ID: "c1", Email: "a@b.example", Country: country}
}

// TestBlockedByDefault is the design response: not a warning banner, a gate.
func TestBlockedByDefault(t *testing.T) {
	g := gate()
	for _, country := range []string{"CA", "DE", "FR", "AU", "GB", "NL", "XX"} {
		d := g.CheckEmail(contact(country))
		if d.Allowed {
			t.Errorf("%s: allowed with no lawful basis recorded", country)
		}
		if d.Reason == "" {
			t.Errorf("%s: refused without saying why", country)
		}
	}
}

// TestCanadaIsTheOneThatCatchesPeople: a US team treating Canada as part of
// the domestic market.
func TestCanadaIsTheOneThatCatchesPeople(t *testing.T) {
	g := gate()
	us := g.CheckEmail(contact("US"))
	ca := g.CheckEmail(contact("CA"))

	if !us.Allowed {
		t.Fatalf("US blocked: %s", us.Reason)
	}
	if ca.Allowed {
		t.Fatal("Canada allowed by default; CASL is effectively prohibitive without consent")
	}
	if !errors.Is(ca.Err, ErrNoBasis) {
		t.Fatalf("CA error %v", ca.Err)
	}
	t.Logf("same list, same send: US %v, CA %v -- %s", us.Allowed, ca.Allowed, ca.Reason)
}

func TestUnknownCountryIsBlocked(t *testing.T) {
	d := gate().CheckEmail(model.Contact{ID: "c1", Email: "someone@example.ca"})
	if d.Allowed {
		t.Fatal("a contact with no recorded country was allowed; jurisdiction cannot be inferred from an email domain")
	}
}

func TestUnknownRegimeIsBlocked(t *testing.T) {
	d := gate().CheckEmail(contact("ZW"))
	if d.Allowed {
		t.Fatal("a country absent from the regime table was allowed")
	}
	if !errors.Is(d.Err, ErrBlocked) {
		t.Fatalf("err %v", d.Err)
	}
}

func TestEnableRequiresANamedPersonAndAnAssertion(t *testing.T) {
	g := gate()
	cases := []LawfulBasis{
		{Country: "CA", Channel: model.ChannelEmail, Basis: "express consent", Assertion: "opt-in form"},
		{Country: "CA", Channel: model.ChannelEmail, Basis: "express consent", AssertedBy: "ops@x"},
		{Country: "CA", Channel: model.ChannelEmail, Assertion: "opt-in form", AssertedBy: "ops@x"},
	}
	for i, b := range cases {
		if err := g.Enable(b); !errors.Is(err, ErrIncompleteBasis) {
			t.Errorf("case %d accepted an incomplete basis: %v", i, err)
		}
	}
}

func TestEnableOpensTheRegionAndRecordsWho(t *testing.T) {
	g := gate()
	err := g.Enable(LawfulBasis{
		Country:    "CA",
		Channel:    model.ChannelEmail,
		Basis:      "express consent",
		Assertion:  "every Canadian contact came from a double opt-in form; records in the CRM",
		AssertedBy: "jane@ours.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	d := g.CheckEmail(contact("CA"))
	if !d.Allowed {
		t.Fatalf("still blocked: %s", d.Reason)
	}
	// The record has to name the person, because that is the point of
	// asking for it.
	if !contains(d.Reason, "jane@ours.example") {
		t.Fatalf("the decision does not name who asserted it: %s", d.Reason)
	}
}

func TestBasisIsPerChannel(t *testing.T) {
	g := gate()
	_ = g.Enable(LawfulBasis{
		Country: "CA", Channel: model.ChannelEmail,
		Basis: "express consent", Assertion: "opt-in", AssertedBy: "jane",
	})
	// Enabling email in Canada says nothing about SMS.
	d := g.CheckSMS(contact("CA"), nil)
	if d.Allowed {
		t.Fatal("an email lawful basis opened SMS")
	}
}

func TestBasisExpiresAtItsReviewDate(t *testing.T) {
	g := gate()
	_ = g.Enable(LawfulBasis{
		Country: "DE", Channel: model.ChannelEmail,
		Basis: "prior consent", Assertion: "webinar sign-up", AssertedBy: "jane",
		AssertedAt: now().AddDate(-2, 0, 0),
		ReviewBy:   now().AddDate(-1, 0, 0),
	})
	d := g.CheckEmail(contact("DE"))
	if d.Allowed {
		t.Fatal("a lawful basis a year past its review date still allowed sending")
	}
	if !errors.Is(d.Err, ErrBasisExpired) {
		t.Fatalf("err %v", d.Err)
	}
}

func TestReviewDateDefaultsToAYear(t *testing.T) {
	g := gate()
	_ = g.Enable(LawfulBasis{
		Country: "GB", Channel: model.ChannelEmail,
		Basis: "corporate subscriber (PECR)", Assertion: "companies and LLPs only", AssertedBy: "jane",
	})
	got := g.EnabledRegions()
	if len(got) != 1 {
		t.Fatalf("%d regions", len(got))
	}
	if !got[0].ReviewBy.Equal(now().AddDate(1, 0, 0)) {
		t.Fatalf("review date %s", got[0].ReviewBy)
	}
}

func TestDisableClosesTheRegion(t *testing.T) {
	g := gate()
	_ = g.Enable(LawfulBasis{
		Country: "CA", Channel: model.ChannelEmail,
		Basis: "express consent", Assertion: "opt-in", AssertedBy: "jane",
	})
	g.Disable("CA", model.ChannelEmail)
	if g.CheckEmail(contact("CA")).Allowed {
		t.Fatal("still open after Disable")
	}
}

func TestUKRequiresABasisBecauseSoleTradersAreIndividuals(t *testing.T) {
	// PECR permits corporate subscribers. A list that does not
	// distinguish sole traders from companies is not covered by that, so
	// the gate asks someone to say which it is.
	if gate().CheckEmail(contact("GB")).Allowed {
		t.Fatal("UK allowed with no assertion that the list is corporate subscribers")
	}
}

func TestSMSRequiresAWrittenConsentArtifact(t *testing.T) {
	g := gate()
	c := contact("US")

	if d := g.CheckSMS(c, nil); d.Allowed {
		t.Fatal("cold SMS with no consent artifact was allowed")
	}
	// An incomplete artifact is not an artifact.
	incomplete := &SMSConsent{Source: "webform"}
	if d := g.CheckSMS(c, incomplete); d.Allowed {
		t.Fatal("an artifact with no consent language was accepted")
	}
	good := &SMSConsent{
		Source:     "pricing page form",
		Language:   "I agree to receive text messages from Example Ltd about my enquiry.",
		CapturedAt: now().AddDate(0, -1, 0),
		IPAddress:  "203.0.113.7",
	}
	if d := g.CheckSMS(c, good); !d.Allowed {
		t.Fatalf("a complete consent artifact was refused: %s", d.Reason)
	}
}

// TestNoOperatorAssertionCanOpenColdSMS: the consent the TCPA requires
// belongs to the recipient, and an operator cannot assert it for them.
func TestNoOperatorAssertionCanOpenColdSMS(t *testing.T) {
	g := gate()
	_ = g.Enable(LawfulBasis{
		Country: "US", Channel: model.ChannelSMS,
		Basis: "legitimate interest", Assertion: "they published the number", AssertedBy: "jane",
	})
	if d := g.CheckSMS(contact("US"), nil); d.Allowed {
		t.Fatal("an operator assertion opened cold SMS; at $500-$1,500 per message that is the most expensive bug in the product")
	}
}

func TestLinkedInIsNotGatedHereButIsManualByConstruction(t *testing.T) {
	d := gate().CheckChannel(contact("US"), model.ChannelLinkedIn, nil)
	if !d.Allowed {
		t.Fatal("LinkedIn refused by the jurisdiction gate; the constraint lives in the step mode, not here")
	}
	if !contains(d.Reason, "manual") {
		t.Fatalf("the reason does not point at where the constraint is: %s", d.Reason)
	}
}

func TestDueForReview(t *testing.T) {
	g := gate()
	_ = g.Enable(LawfulBasis{
		Country: "FR", Channel: model.ChannelEmail,
		Basis: "legitimate interest", Assertion: "role-relevant", AssertedBy: "jane",
		AssertedAt: now(), ReviewBy: now().AddDate(0, 0, 20),
	})
	_ = g.Enable(LawfulBasis{
		Country: "GB", Channel: model.ChannelEmail,
		Basis: "corporate subscriber", Assertion: "companies only", AssertedBy: "jane",
		AssertedAt: now(), ReviewBy: now().AddDate(1, 0, 0),
	})
	due := g.DueForReview(30 * 24 * time.Hour)
	if len(due) != 1 || due[0].Country != "FR" {
		t.Fatalf("%+v", due)
	}
}

// TestEveryEURegimeIsAtBestContested guards against someone adding a member
// state as Permitted.
func TestEveryEURegimeIsAtBestContested(t *testing.T) {
	for _, c := range append([]string{"DE", "FR"}, euGDPR...) {
		r := Regimes[c]
		if r.ColdEmailPermitted == Permitted {
			t.Errorf("%s is marked permitted; no EU member state is settled law for cold B2B email", c)
		}
		if !r.RequiresLawfulBasis {
			t.Errorf("%s does not require a lawful basis", c)
		}
	}
}

func TestEveryRegimeRecordsItsPenalty(t *testing.T) {
	// The gate says what is being risked. A refusal that does not is a
	// refusal someone overrides without thinking.
	for c, r := range Regimes {
		if r.MaxPenalty == "" {
			t.Errorf("%s records no penalty", c)
		}
		if r.Notes == "" {
			t.Errorf("%s records no notes", c)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
