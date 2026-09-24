// Package jurisdiction gates sending on the recipient's location.
//
// Jurisdiction is determined by the RECIPIENT, which makes routing by
// recipient location a core feature and not a settings page.
//
// The design response to the patchwork below is to block by default, with
// per-region enablement that requires the operator to assert a lawful basis
// and record it. Not a warning banner -- a gate. A warning banner produces
// a company that emailed Canada for eight months and has nothing to show a
// regulator; a gate produces a refusal and a dated record of who decided
// otherwise.
package jurisdiction

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

// Regime describes what the law in a country requires for cold commercial
// contact.
type Regime struct {
	Country string
	Name    string
	// ColdEmailPermitted is whether cold B2B email is lawful with no
	// prior relationship.
	ColdEmailPermitted Stance
	// RequiresLawfulBasis is whether enabling the region requires the
	// operator to record one.
	RequiresLawfulBasis bool
	Notes               string
	// MaxPenalty is recorded so the gate can say what is being risked.
	MaxPenalty string
}

// Stance is the three-valued answer, because two values would force the
// contested cases into one of the confident ones.
type Stance string

const (
	// Permitted with conditions -- CAN-SPAM's requirements still apply.
	Permitted Stance = "permitted"
	// Contested: arguable, varies by member state, and not something to
	// default on.
	Contested Stance = "contested"
	// Prohibited without consent.
	Prohibited Stance = "prohibited"
)

// Regimes is the table from §3.2, as data the gate consults.
//
// Not exhaustive and not legal advice; a country absent from this table is
// blocked, which is the correct default for a table that is known to be
// incomplete.
var Regimes = map[string]Regime{
	"US": {
		Country: "US", Name: "CAN-SPAM",
		ColdEmailPermitted:  Permitted,
		RequiresLawfulBasis: false,
		Notes: "Permitted with requirements: accurate From/Reply-To, non-deceptive subject, " +
			"valid physical postal address, functioning opt-out. CAN-SPAM allows 10 business " +
			"days to honour an opt-out; Google and Yahoo require 48 hours. Build to 48 hours " +
			"and process immediately.",
		MaxPenalty: "approximately $53,000 per email, each message a separate violation",
	},
	"GB": {
		Country: "GB", Name: "PECR",
		ColdEmailPermitted:  Permitted,
		RequiresLawfulBasis: true,
		Notes: "Permitted to CORPORATE subscribers: companies and LLPs. Sole traders and " +
			"partnerships are treated as individuals and require consent, so a list that " +
			"does not distinguish entity type is not covered by this.",
		MaxPenalty: "GBP 500,000 (ICO), plus UK GDPR exposure",
	},
	"DE": {
		Country: "DE", Name: "UWG + GDPR",
		ColdEmailPermitted:  Prohibited,
		RequiresLawfulBasis: true,
		Notes:               "Germany's UWG effectively requires prior consent for commercial email.",
		MaxPenalty:          "GDPR: up to EUR 20M or 4% of global turnover",
	},
	"FR": {
		Country: "FR", Name: "GDPR + CNIL guidance",
		ColdEmailPermitted:  Contested,
		RequiresLawfulBasis: true,
		Notes: "CNIL requires the message to relate to the recipient's professional function. " +
			"A generic pitch to a professional address does not qualify.",
		MaxPenalty: "up to EUR 20M or 4% of global turnover",
	},
	"CA": {
		Country: "CA", Name: "CASL",
		ColdEmailPermitted:  Prohibited,
		RequiresLawfulBasis: true,
		Notes: "Effectively prohibited without consent. Express or implied; implied needs an " +
			"existing business relationship or a conspicuously published business address " +
			"relevant to the role. THIS IS THE ONE THAT CATCHES PEOPLE: US teams treat " +
			"Canada as part of the domestic market and it is one of the strictest regimes " +
			"in the world for commercial email.",
		MaxPenalty: "CAD $10,000,000",
	},
	"AU": {
		Country: "AU", Name: "Spam Act 2003",
		ColdEmailPermitted:  Prohibited,
		RequiresLawfulBasis: true,
		Notes:               "Consent required. Inferred consent possible from a conspicuously published business address.",
		MaxPenalty:          "AUD 2.2M per day for repeat corporate offenders",
	},
}

// euGDPR are the remaining EU member states, which all sit under GDPR and
// ePrivacy and are treated as contested rather than permitted.
var euGDPR = []string{
	"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "GR", "HU", "IE",
	"IT", "LV", "LT", "LU", "MT", "NL", "PL", "PT", "RO", "SK", "SI", "ES", "SE",
}

func init() {
	for _, c := range euGDPR {
		Regimes[c] = Regime{
			Country: c, Name: "GDPR + ePrivacy",
			ColdEmailPermitted:  Contested,
			RequiresLawfulBasis: true,
			Notes: "Legitimate interest is sometimes argued for B2B but is contested and " +
				"varies by member state.",
			MaxPenalty: "up to EUR 20M or 4% of global turnover",
		}
	}
}

// LawfulBasis is the operator's recorded assertion for a region.
//
// A structure rather than a boolean, because "we decided this was fine" is
// not an answer to a regulator and a checkbox cannot be one either.
type LawfulBasis struct {
	Country string
	Channel model.Channel
	// Basis is what is being claimed -- "legitimate interest",
	// "corporate subscriber (PECR)", "express consent", and so on.
	Basis string
	// Assertion is the operator's own words for why it applies to this
	// list.
	Assertion string
	// AssertedBy is a person, not a system. Someone has to own it.
	AssertedBy string
	AssertedAt time.Time
	// ReviewBy forces the assertion to be revisited. A lawful basis
	// asserted in 2024 and never looked at again is a record of what
	// somebody used to believe.
	ReviewBy time.Time
}

var (
	ErrBlocked          = fmt.Errorf("jurisdiction: blocked by default")
	ErrNoBasis          = fmt.Errorf("jurisdiction: region not enabled and no lawful basis recorded")
	ErrBasisExpired     = fmt.Errorf("jurisdiction: the recorded lawful basis is past its review date")
	ErrIncompleteBasis  = fmt.Errorf("jurisdiction: a lawful basis needs a named person, a basis and an assertion")
	ErrNoWrittenConsent = fmt.Errorf("jurisdiction: SMS requires prior express written consent (TCPA)")
)

// Gate decides whether a contact may be contacted on a channel.
type Gate struct {
	mu    sync.RWMutex
	bases map[string]LawfulBasis
	now   func() time.Time
}

func NewGate() *Gate {
	return &Gate{bases: make(map[string]LawfulBasis), now: time.Now}
}

func (g *Gate) SetClock(f func() time.Time) { g.now = f }

func key(country string, ch model.Channel) string {
	return strings.ToUpper(strings.TrimSpace(country)) + "/" + string(ch)
}

// Enable records a lawful basis, which is the only way to open a region.
func (g *Gate) Enable(b LawfulBasis) error {
	if strings.TrimSpace(b.AssertedBy) == "" ||
		strings.TrimSpace(b.Basis) == "" ||
		strings.TrimSpace(b.Assertion) == "" {
		return ErrIncompleteBasis
	}
	if b.AssertedAt.IsZero() {
		b.AssertedAt = g.now()
	}
	if b.ReviewBy.IsZero() {
		b.ReviewBy = b.AssertedAt.AddDate(1, 0, 0)
	}
	b.Country = strings.ToUpper(strings.TrimSpace(b.Country))

	g.mu.Lock()
	defer g.mu.Unlock()
	g.bases[key(b.Country, b.Channel)] = b
	return nil
}

// Disable removes a recorded basis, closing the region again.
func (g *Gate) Disable(country string, ch model.Channel) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.bases, key(country, ch))
}

// Decision is the gate's answer.
type Decision struct {
	Allowed bool
	Country string
	Regime  string
	Reason  string
	Err     error
}

// SMSConsent is the artifact the TCPA requires.
//
// Prior express WRITTEN consent. Not implied consent, not a business
// relationship, not a published number. Cold SMS without it is $500 per
// message, trebled to $1,500 for willful violations, with no cap, under one
// of the most heavily class-actioned statutes in US consumer law.
type SMSConsent struct {
	// Source is where the consent was captured -- the form, the
	// conversation, the contract.
	Source string
	// Language is the exact text the person agreed to. Without it there
	// is no way to show what was consented to.
	Language   string
	CapturedAt time.Time
	// IPAddress and UserAgent where a web form captured it.
	IPAddress string
}

// Valid reports whether the artifact is complete enough to rely on.
func (c SMSConsent) Valid() bool {
	return strings.TrimSpace(c.Source) != "" &&
		strings.TrimSpace(c.Language) != "" &&
		!c.CapturedAt.IsZero()
}

// CheckEmail is the gate for email.
func (g *Gate) CheckEmail(contact model.Contact) Decision {
	country := strings.ToUpper(strings.TrimSpace(contact.Country))
	if country == "" {
		// An unknown recipient country is blocked. Guessing from the
		// email domain is how a .com-hosted Canadian gets emailed under
		// CASL.
		return Decision{
			Allowed: false, Country: "",
			Reason: "recipient country unknown; jurisdiction is determined by the recipient and cannot be inferred from an email domain",
			Err:    ErrBlocked,
		}
	}

	regime, known := Regimes[country]
	d := Decision{Country: country, Regime: regime.Name}

	g.mu.RLock()
	basis, hasBasis := g.bases[key(country, model.ChannelEmail)]
	g.mu.RUnlock()

	if !known {
		d.Regime = "unknown"
		if !hasBasis {
			d.Reason = fmt.Sprintf("no regime recorded for %s; blocked by default until someone records what the law there requires", country)
			d.Err = ErrBlocked
			return d
		}
	}

	if known && regime.ColdEmailPermitted == Permitted && !regime.RequiresLawfulBasis {
		d.Allowed = true
		d.Reason = regime.Name + ": permitted, subject to its requirements"
		return d
	}

	if !hasBasis {
		d.Reason = fmt.Sprintf("%s: %s. No lawful basis recorded for %s, so sending is blocked. Maximum penalty: %s.",
			regime.Name, stanceText(regime.ColdEmailPermitted), country, regime.MaxPenalty)
		d.Err = ErrNoBasis
		return d
	}

	if now := g.now(); !basis.ReviewBy.IsZero() && now.After(basis.ReviewBy) {
		d.Reason = fmt.Sprintf("%s: the lawful basis asserted by %s on %s was due for review on %s and has not been renewed",
			regime.Name, basis.AssertedBy, basis.AssertedAt.Format("2006-01-02"), basis.ReviewBy.Format("2006-01-02"))
		d.Err = ErrBasisExpired
		return d
	}

	d.Allowed = true
	d.Reason = fmt.Sprintf("%s: enabled on the basis of %q, asserted by %s on %s",
		regime.Name, basis.Basis, basis.AssertedBy, basis.AssertedAt.Format("2006-01-02"))
	return d
}

// CheckSMS gates SMS on a recorded consent artifact and nothing else.
//
// There is no jurisdiction where cold SMS is enabled by an operator
// assertion, because the consent the TCPA requires belongs to the
// recipient and an operator cannot assert it on their behalf.
func (g *Gate) CheckSMS(contact model.Contact, consent *SMSConsent) Decision {
	d := Decision{Country: strings.ToUpper(strings.TrimSpace(contact.Country))}
	if consent == nil || !consent.Valid() {
		d.Reason = "no prior express written consent artifact on file. Cold SMS is $500 per message, " +
			"trebled to $1,500 for willful violations, with no cap. This is not a gray area."
		d.Err = ErrNoWrittenConsent
		return d
	}
	d.Allowed = true
	d.Reason = fmt.Sprintf("written consent captured via %s on %s", consent.Source, consent.CapturedAt.Format("2006-01-02"))
	return d
}

// CheckChannel is the general entry point.
func (g *Gate) CheckChannel(contact model.Contact, ch model.Channel, consent *SMSConsent) Decision {
	switch ch {
	case model.ChannelEmail:
		return g.CheckEmail(contact)
	case model.ChannelSMS:
		return g.CheckSMS(contact, consent)
	case model.ChannelLinkedIn:
		// Never gated by jurisdiction, and never automated either. The
		// step type carries the restriction (model.ModeManualTask); this
		// is here so that a caller reaching for a jurisdiction check on
		// LinkedIn gets told where the actual constraint lives.
		return Decision{
			Allowed: true,
			Reason:  "LinkedIn steps are manual tasks by construction; automation violates the User Agreement and the rep's own account is the asset at risk",
		}
	case model.ChannelCall:
		return Decision{
			Allowed: true,
			Reason:  "phone steps are manual tasks with a dial-out link, which sidesteps the TCPA autodialer rules; DNC and calling-window checks apply at the task level",
		}
	default:
		return Decision{Reason: "unknown channel", Err: ErrBlocked}
	}
}

func stanceText(s Stance) string {
	switch s {
	case Permitted:
		return "cold B2B email is permitted subject to conditions"
	case Contested:
		return "the lawful basis for cold B2B email is contested"
	case Prohibited:
		return "cold B2B email is effectively prohibited without consent"
	default:
		return "the position is unrecorded"
	}
}

// EnabledRegions lists what has been opened, for the audit view.
func (g *Gate) EnabledRegions() []LawfulBasis {
	g.mu.RLock()
	defer g.mu.RUnlock()

	out := make([]LawfulBasis, 0, len(g.bases))
	for _, b := range g.bases {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Country != out[j].Country {
			return out[i].Country < out[j].Country
		}
		return out[i].Channel < out[j].Channel
	})
	return out
}

// DueForReview lists assertions that need looking at again.
func (g *Gate) DueForReview(within time.Duration) []LawfulBasis {
	g.mu.RLock()
	defer g.mu.RUnlock()

	cutoff := g.now().Add(within)
	var out []LawfulBasis
	for _, b := range g.bases {
		if !b.ReviewBy.IsZero() && b.ReviewBy.Before(cutoff) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReviewBy.Before(out[j].ReviewBy) })
	return out
}
