# Legal constraints, by channel and jurisdiction

Not legal advice. This is what the system enforces and why, so that you know
what you are overriding when you override it.

**Jurisdiction is determined by the recipient.** That makes routing by
recipient location a core feature, not a settings page — and it means an
unknown recipient country is a blocked send, because it cannot be inferred
from an email domain.

---

## Email

### United States — CAN-SPAM

Cold commercial email is permitted, with requirements:

- Accurate `From`, `Reply-To` and routing information
- Non-deceptive subject lines
- A valid physical postal address
- A functioning opt-out, honoured within 10 business days
- No harvesting or dictionary attacks

Penalties run to roughly **$53,000 per email**, and each message is a
separate violation.

Note the mismatch: CAN-SPAM allows 10 business days to honour an opt-out;
Google and Yahoo require 48 hours. **The system processes unsubscribes
immediately** — see [Unsubscribe](#unsubscribe-rfc-8058).

This is the only regime the gate allows by default.

### Everywhere else — blocked until you say otherwise

| Jurisdiction | Cold B2B email | Max penalty |
|---|---|---|
| **Canada (CASL)** | **Effectively prohibited without consent** | CAD $10M |
| **Germany (UWG)** | Prohibited — prior consent required | EUR 20M / 4% turnover |
| **France (GDPR + CNIL)** | Contested — must relate to the recipient's professional function | EUR 20M / 4% turnover |
| **Rest of EU (GDPR + ePrivacy)** | Contested — legitimate interest is argued for B2B and varies by member state | EUR 20M / 4% turnover |
| **UK (PECR)** | Permitted to **corporate subscribers** only | GBP 500,000 + UK GDPR |
| **Australia (Spam Act)** | Consent required; inferred consent possible from a conspicuously published business address | AUD 2.2M/day |

**CASL is the one that catches people.** US teams routinely treat Canada as
part of the domestic market, and it is one of the strictest regimes in the
world for commercial email.

**The UK entry is narrower than it looks.** PECR permits corporate
subscribers — companies and LLPs. Sole traders and partnerships are treated
as individuals and require consent. A list that does not distinguish entity
type is not covered by the corporate-subscriber exemption, which is why the
gate asks someone to assert that it is.

A country not in the table is **blocked**, because the table is known to be
incomplete and the correct default for an incomplete table is refusal.

### How to open a region

Not a warning banner. A gate.

```go
gate.Enable(jurisdiction.LawfulBasis{
    Country:    "CA",
    Channel:    model.ChannelEmail,
    Basis:      "express consent",
    Assertion:  "every Canadian contact came from a double opt-in form; records in the CRM",
    AssertedBy: "jane@ours.example",
})
```

All three of `Basis`, `Assertion` and `AssertedBy` are required. A named
person has to own it, because "we decided this was fine" is not an answer to
a regulator and a checkbox cannot be one either.

Assertions carry a **review date**, defaulting to a year. Past it, the region
closes again. A lawful basis asserted in 2024 and never revisited is a record
of what somebody used to believe.

## SMS — the naive version is illegal in the US

The TCPA requires **prior express written consent** for marketing texts. Not
implied consent. Not a business relationship. Not a published number.
Written.

- **$500 per message**, trebled to **$1,500** for willful violations
- No cap, and among the most heavily class-actioned statutes in US consumer
  law
- A2P 10DLC registration is separately required, with its own attestations
- Quiet hours apply (generally 8am–9pm recipient local), with stricter state
  rules in places

**There is no jurisdiction setting that opens cold SMS**, and this is
deliberate. An operator assertion cannot open it either, because the consent
the TCPA requires belongs to the recipient and an operator cannot assert it
on their behalf. `Gate.CheckSMS` takes a consent artifact and refuses
without one:

```go
jurisdiction.SMSConsent{
    Source:     "pricing page form",
    Language:   "I agree to receive text messages from Example Ltd about my enquiry.",
    CapturedAt: capturedAt,
    IPAddress:  "203.0.113.7",
}
```

The `Language` field is the exact text the person agreed to. Without it there
is nothing to show anyone.

The honest v1 answer: **ship SMS only for opted-in contacts** — people who
gave a mobile number and consent during a form fill or an existing
conversation. That is a genuinely useful channel and a legal one.

## LinkedIn — automation violates the User Agreement

LinkedIn prohibits automated access, scraping, and bots. Accounts detected
doing it get restricted or permanently banned.

The exposure is worth being precise about: **the rep's personal LinkedIn
account is the asset at risk**, and for a salesperson that account is a
significant part of their professional identity and network.

So LinkedIn steps are **manual tasks by construction**. `model.ModeManualTask`
is a type-level constraint, and `send.ManualTaskGate` refuses an automatic
LinkedIn step at runtime with an error that says why. There is no
configuration path to automating it.

This is not a cop-out. Most of the value of multichannel is the coordinated
timing, not the automation of the click — the sequence still tells the rep
what to do and when, and still tracks it.

## Phone

- TCPA restrictions on autodialers and prerecorded messages
- National and state Do Not Call registries
- Calling windows, generally 8am–9pm recipient local
- Several states require call recording consent

Phone steps are **manual tasks with a dial-out link**, which sidesteps the
autodialer rules entirely.

## Unsubscribe (RFC 8058)

Required for bulk senders and worth implementing regardless, because a person
who can unsubscribe in one click frequently does so **instead of** clicking
"report spam". One of those is free and the other is measured in tenths of a
percent against a budget you cannot afford.

Both headers, on every send:

```
List-Unsubscribe: <https://unsub.example.com/u/TOKEN>, <mailto:unsub@example.com?subject=TOKEN>
List-Unsubscribe-Post: List-Unsubscribe=One-Click
```

Google accepts only the RFC 8058 header form; Yahoo also accepts the
`mailto:` fallback, so both are present.

The endpoint:

- Accepts `POST` with **no confirmation page, no login, no preference
  centre**
- Suppresses **immediately** — not within 48 hours
- Also suppresses on `GET`, because a person who believes they unsubscribed
  and did not reaches for the spam button next. The cost is that a
  link-scanning security appliance can unsubscribe someone who never clicked;
  that trade is correct, because a false unsubscribe loses one prospect and a
  missed one costs reputation across every recipient.
- Never returns an error to the recipient once the suppression has landed.
  An unsubscribe that 500s becomes a spam complaint.
- Never expires a token. Refusing a three-year-old link converts an
  unsubscribe into a complaint.

**And the link is prominent in the body.** Hiding it to reduce opt-outs
trades a free action for a catastrophic one.

## Suppression is global and permanent

An unsubscribe applies to **every sequence from every rep**. Per-sequence
opt-out is how a person unsubscribes three times and then files a complaint.

- Keyed on the normalised address, not a contact id, so suppressions outlive
  contact records, list imports and CRM deletions.
- Gmail dot and plus-address variants collapse to one key, so an unsubscribe
  cannot be circumvented by a differently-formatted import of the same list.
  Dots are **not** collapsed at other domains, where they are significant.
- `AddWithExpiry` refuses an expiry on a permanent reason. An unsubscribe
  that lapses after ninety days is a CAN-SPAM violation with a timer on it.
- Checked at three points: import, enrolment, and **immediately before
  dispatch**. The third is the one people skip, and it is the one that
  matters — see [runbook-halt.md](runbook-halt.md) and `internal/send/preflight.go`.
