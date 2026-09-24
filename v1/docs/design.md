# Outbound Sequencer — Design & Build Guide

**Project:** Multichannel sequencing engine with deliverability guardrails, reply detection, and automatic pause when a meeting is booked
**Language:** Go
**Status of this document:** planning + reference

> This document describes legal constraints on commercial email, SMS, and telephony because they determine the architecture. It is not legal advice. Section 3 is a list of questions for counsel, not answers.

---

## Table of contents

1. [Executive summary and scope](#1-executive-summary-and-scope)
2. [The deliverability physics](#2-the-deliverability-physics)
3. [Legal constraints by channel](#3-legal-constraints-by-channel)
4. [Sending architecture](#4-sending-architecture)
5. [The scheduling engine](#5-the-scheduling-engine)
6. [Suppression and contact governance](#6-suppression-and-contact-governance)
7. [Reply detection](#7-reply-detection)
8. [Meeting-booked pause](#8-meeting-booked-pause)
9. [Multichannel](#9-multichannel)
10. [Metrics that mean something](#10-metrics-that-mean-something)
11. [Tech stack and setup](#11-tech-stack-and-setup)
12. [Repository layout](#12-repository-layout)
13. [Milestone ladder](#13-milestone-ladder)
14. [Reference implementations](#14-reference-implementations)
15. [Testing](#15-testing)
16. [Stretch goals](#16-stretch-goals)
17. [References](#17-references)

---

## 1. Executive summary and scope

### The original statement

> Multichannel sequencing engine with deliverability guardrails, reply detection, and automatic pause when a meeting is booked.

Five findings reshape this:

1. **The spam complaint ceiling is a hard physical cap on volume, enforced by Google and Yahoo rather than by you.** The limit is 0.3%, the target is 0.1%, and it is **recalculated daily**, not averaged. At the target, that's one complaint per thousand inboxed emails. Google escalated enforcement in November 2025 from temporary delays to **permanent rejections** — messages bounce at SMTP, they don't land in spam. **The guardrails are not a feature of this product; they are the product.** See section 2.
2. **The complaint-rate denominator is inboxed mail, not sent mail, which creates a death spiral.** As filtering increases, the denominator shrinks, so the same number of complaints produces a worse rate, which triggers more filtering. A program can collapse in days once it starts. See section 2.3.
3. **Bulk-sender classification is permanent once triggered.** Cross 5,000 messages a day to Gmail once, and you are permanently subject to the stricter rules even if volume drops afterward. A single enthusiastic blast changes your regulatory category forever. See section 2.4.
4. **Two of the channels are legally or contractually unusable in their naive form.** Cold SMS to a US number without prior express written consent violates the TCPA ($500–$1,500 per message, class-action magnet). LinkedIn automation violates LinkedIn's User Agreement and gets accounts restricted. These are not "check with legal" footnotes — they determine whether those channels ship at all. See section 3.
5. **Open tracking is dead and actively harmful.** Apple Mail Privacy Protection pre-fetches images, so opens are noise, and the tracking pixel itself hurts deliverability. Any sequence logic branching on "opened but didn't reply" is branching on random numbers. See section 10.1.

### Revised project statement

> A volume-capped outbound engine whose primary function is preventing the operator from destroying their own sending reputation: isolated sending domains with enforced warmup ramps, a hard daily complaint budget that halts sending before the ceiling, timezone-aware scheduling with idempotent sends, send-time suppression checks, threaded reply detection with out-of-office handling, and account-level pause on meeting booking.

### The framing that matters

A sequencer that optimizes for send volume is building a machine that destroys its user's business. The 0.3% ceiling exists because the ecosystem decided to price spam, and it prices it at roughly *one complaint per three hundred*. There is no targeting strategy that survives that threshold at high volume with generic messaging.

So the design goal is inverted from the obvious one: **fewer, better-targeted sends, with the system actively refusing to let the operator exceed what their reputation can absorb.** Everything in section 2 follows from that.

### Explicit non-goals

- **Not a data provider.** You don't scrape or sell contact data. List quality is the operator's problem, though you'll measure it (§10.3).
- **Not an email service provider.** You send through the user's own mailbox (§4.2), and most ESP acceptable-use policies prohibit cold outreach anyway.
- **Not a CRM.** You read from and write to one.
- **Not a mail server.** No SMTP infrastructure, no IP pool management.
- **Not a LinkedIn automation tool.** See §3.4.
- **Not cold SMS.** See §3.3.

---

## 2. The deliverability physics ★

### 2.1 The requirements

Since February 2024, Google and Yahoo have enforced a coordinated baseline; Microsoft added comparable rules for Outlook, Hotmail, and Live in spring 2025.

**All senders:**

| Requirement | Consequence of failure |
|---|---|
| SPF or DKIM authentication | Reject or spam |
| Valid forward and reverse DNS (PTR) | Reject |
| TLS-encrypted transmission | Reject |
| RFC 5322-compliant formatting | Reject |
| Spam complaint rate below 0.3% | Throttle, then block |

**Bulk senders** (5,000+ messages/day to Gmail, per primary domain; Yahoo declines to publish a number and applies it to "significant volume"):

| Requirement | Detail |
|---|---|
| DMARC policy | Minimum `p=none`; Google signals full alignment on both SPF and DKIM is likely to become mandatory |
| Domain alignment | The `From:` header domain must align with the SPF or DKIM domain |
| One-click unsubscribe | Google accepts **only** RFC 8058 `List-Unsubscribe` headers; Yahoo also accepts a `mailto:` fallback |
| Unsubscribe honored within | 48 hours (Google) / 2 days (Yahoo) |
| DKIM key length | Yahoo requires a minimum of 1024 bits |

**Non-compliance means rejection at SMTP, not spam-folder placement.** The escalation ladder runs temporary reject (4xx) → permanent reject (5xx) → domain blocked, and Google moved from temporary delays to permanent rejections in November 2025.

### 2.2 What 0.1% actually means

The ceiling is 0.3%. Google's stated target is below 0.1%. Both are computed **daily**.

| Emails inboxed per day | Complaints allowed at 0.1% | At the 0.3% ceiling |
|---|---|---|
| 500 | 0 (one is 0.2%) | 1 |
| 1,000 | 1 | 3 |
| 5,000 | 5 | 15 |
| 10,000 | 10 | 30 |

**At 1,000 inboxed emails a day, two people clicking "report spam" puts you in the warning zone and four puts you over the ceiling.**

For context, well-run permission-based marketing lists run 0.02–0.05%. Poorly targeted cold outbound routinely runs 0.5–2%, which is five to twenty times over the ceiling. The threshold is not a technicality you engineer around; it is the ecosystem stating a maximum tolerable level of unwanted mail, and generic high-volume outbound is above it by construction.

### 2.3 The denominator death spiral ★

The rate is complaints divided by **inboxed** mail — not sent mail. Google's own example: send 10,000, 9,000 reach the inbox, 1,000 get filtered, 27 people complain. The rate is 27 / 9,000 = 0.3%.

Notice what happens when reputation starts to slip:

```
Day 1   send 10,000 · inbox 9,000 · 27 complaints → 0.30%  ← ceiling hit
Day 2   send 10,000 · inbox 6,000 · 27 complaints → 0.45%  ← filtering increased
Day 3   send 10,000 · inbox 3,000 · 27 complaints → 0.90%
Day 4   send 10,000 · inbox 1,200 · 27 complaints → 2.25%
```

**Identical sending behavior, identical complaint counts, and the rate quadruples in three days** because the denominator collapsed. Filtering shrinks the denominator, the worse rate triggers more filtering, and the loop closes.

This is why recovery is slow and why the system must stop *well before* the ceiling. By the time you see 0.3% in Postmaster Tools, the spiral has already started.

**Design response: a complaint budget that halts sending at a fraction of the ceiling**, with the halt automatic and requiring human acknowledgment to lift (§5.5).

### 2.4 Bulk-sender classification is permanent

Once a domain crosses 5,000 messages in a day to Gmail, the bulk-sender classification attaches and **does not expire when volume decreases.**

The practical consequence: a single enthusiastic blast permanently subjects that domain to DMARC alignment and one-click-unsubscribe requirements. If you were going to implement those anyway — and you should — it costs nothing. If you weren't, a one-off campaign has permanently changed your obligations.

**Design response: a hard per-domain daily cap, enforced by the scheduler, set below 5,000 by default**, with crossing it a deliberate configuration change rather than an emergent consequence of enrolling too many prospects.

### 2.5 The other physical limits

| Metric | Safe | Warning | Critical |
|---|---|---|---|
| Spam complaint rate | < 0.1% | 0.1–0.29% | ≥ 0.3% |
| Hard bounce rate | < 1% | 1–2% | > 2% |
| Per-mailbox daily volume (cold) | 30–50 | 50–100 | > 100 |

That last row deserves explanation. Google Workspace permits far higher daily sending, but a mailbox that sends 500 cold emails a day does not look like a human being, and provider heuristics are tuned on behavioral patterns as well as authentication. Established practice for cold outbound is a few dozen per mailbox per day, which is why operations scale by adding mailboxes rather than by raising per-mailbox volume.

**This is arithmetic the product must surface.** If a rep wants to touch 2,000 prospects a month and each gets a four-step email sequence, that's 8,000 sends. At 40/day/mailbox over 21 business days, one mailbox delivers 840. They need ten mailboxes, not one — and they should learn this at sequence-design time, not by watching their domain burn.

### 2.6 Spam traps

**Pristine traps** are addresses that were never real and have never opted in to anything. Hitting one is strong evidence of a purchased or scraped list, and it is a blacklisting event, not a reputation nudge. **Recycled traps** are abandoned real addresses repurposed by providers; hitting them signals a stale list.

You cannot detect traps. The only defense is list hygiene: verify before sending, remove hard bounces immediately and permanently, and treat any list the operator can't explain the provenance of as radioactive.

---

## 3. Legal constraints by channel ★

Jurisdiction is determined by the **recipient**, which means routing by recipient location is a core feature and not a settings page.

### 3.1 Email — United States (CAN-SPAM)

CAN-SPAM permits cold commercial email, with requirements:

- Accurate `From`, `Reply-To`, and routing information
- Non-deceptive subject lines
- Identification as an advertisement (in context — B2B sales email is generally read as satisfying this through content)
- A valid physical postal address
- A functioning opt-out honored **within 10 business days**
- No harvesting or dictionary attacks

Penalties run to roughly $53,000 per email, and each individual message is a separate violation.

Note the mismatch: CAN-SPAM allows 10 business days to honor an opt-out; Google and Yahoo require 48 hours. **Build to the 48-hour requirement** — and in practice, process unsubscribes immediately.

### 3.2 Email — elsewhere

| Jurisdiction | Cold B2B email | Notes |
|---|---|---|
| **EU (GDPR + ePrivacy)** | Needs a lawful basis | Legitimate interest is *sometimes* argued for B2B but is contested and varies by member state. Germany's UWG effectively requires prior consent. France's CNIL requires the message to relate to the recipient's professional function. |
| **UK (PECR)** | Permitted to corporate subscribers | Companies and LLPs may be emailed; sole traders and partnerships are treated as individuals and require consent |
| **Canada (CASL)** | **Effectively prohibited without consent** | Express or implied consent required; implied consent needs an existing business relationship or a conspicuously published business address relevant to the role. Penalties to CAD $10M. |
| **Australia (Spam Act)** | Consent required | Inferred consent possible from a conspicuously published business address |

**CASL is the one that catches people.** US teams routinely treat Canada as part of the domestic market and it is one of the strictest regimes in the world for commercial email.

**Design response: block by default on recipient jurisdiction**, with per-region enablement that requires the operator to assert a lawful basis and record it. Not a warning banner — a gate.

### 3.3 SMS — the naive version is illegal in the US

The TCPA requires **prior express written consent** for marketing text messages. Not implied consent, not a business relationship, not a published number. Written consent.

- Statutory damages: **$500 per message**, trebled to $1,500 for willful violations
- No cap, and it is among the most heavily class-actioned statutes in US consumer law
- A2P 10DLC registration with the carriers is separately required for business messaging, with its own content and consent attestations
- Quiet hours apply (generally 8am–9pm recipient local time), with stricter state rules in several places

**Cold SMS to a prospect who has not given written consent is not a gray area.** Any SMS capability in this product must be gated on a recorded consent artifact — where it came from, when, and what language the person agreed to — and must refuse to send without one.

The honest v1 answer is: **ship SMS only for opted-in contacts**, typically people who provided a mobile number and consent during a form fill or an existing conversation. That is a genuinely useful channel and a legal one.

### 3.4 LinkedIn — automation violates the User Agreement

LinkedIn's User Agreement prohibits automated access, scraping, and the use of bots or automation software to interact with the platform. Accounts detected doing so get restricted or permanently banned.

This is widely ignored by the category, and it's worth being clear about the actual exposure: **the user's personal LinkedIn account is the asset at risk**, and for a salesperson that account is a significant part of their professional identity and network.

Defensible options:

| Approach | Status |
|---|---|
| **Manual task in the sequence** — the system tells the rep to send a connection request, they do it by hand, then mark it done | Compliant. Preserves multichannel cadence. **Recommended.** |
| LinkedIn's official Sales Navigator APIs, within partner terms | Compliant, limited, requires partnership |
| Browser automation / unofficial APIs | Violates the User Agreement; account ban risk |

The manual-task pattern is not a cop-out. It keeps the cadence, the tracking, and the reporting, while leaving the action with a human. Most of the value of "multichannel" is the coordinated timing, not the automation of the click.

### 3.5 Phone

- TCPA restrictions on autodialers and prerecorded messages
- National and state Do Not Call registries
- Calling windows, generally 8am–9pm recipient local time
- Several states require call recording consent (see the separate call-recording analysis if you're recording)

For a sequencer, phone steps are normally **manual tasks with a dial-out link**, which sidesteps autodialer rules entirely.

---

## 4. Sending architecture

### 4.1 Never send from the primary domain ★

If outbound burns `yourcompany.com`, then invoices, contracts, support replies, and password resets stop being delivered. That is not a marketing setback; it is an existential business failure, and it is slow and painful to recover from.

Use dedicated domains:

```
yourcompany.com          ← primary. NEVER used for outbound sequencing.
get-yourcompany.com      ← outbound domain 1
yourcompany-team.com     ← outbound domain 2
tryyourcompany.com       ← outbound domain 3
```

Rules:

- **Separate registrable domains, not subdomains.** Subdomain reputation is not fully isolated from the parent, and the whole point is isolation.
- Each outbound domain needs its own SPF, DKIM (1024-bit minimum for Yahoo), DMARC, and a real website that resolves — a domain with no site is itself a spam signal.
- Each outbound domain needs its own **custom link-tracking domain**. Shared tracking domains mean you inherit every other sender's reputation, and a shared tracker on a blocklist takes you with it.
- **Cap each domain below the 5,000/day bulk threshold** (§2.4) unless crossing it is a deliberate decision.
- The system must refuse to attach the primary domain to a sequence. Make it a validation error, not a warning.

### 4.2 Send through the user's own mailbox

Two reasons this is the right architecture:

**Deliverability.** Cold outbound from a shared ESP IP pool is a reputation disaster for everyone on that pool, which is why it degrades fast.

**Acceptable-use policies.** Most transactional ESPs explicitly prohibit cold outreach — Postmark is unambiguous about it, and others enforce similar terms. Building on infrastructure whose terms you're violating means your sending disappears without notice one morning.

So: OAuth into the rep's Gmail/Workspace or Microsoft 365 mailbox and send via the Gmail API or Microsoft Graph. This also makes replies land naturally in the rep's inbox and keeps threading intact.

```go
type Mailbox struct {
    ID              string
    UserID          string
    Provider        Provider // gmail | microsoft
    Address         string
    Domain          string   // must not be the primary domain
    DailyCap        int      // 30-50 for cold
    WarmupStartedAt *time.Time
    WarmupDay       int
    HealthState     MailboxHealth
}
```

### 4.3 Warmup is enforced, not advised

A brand-new domain and mailbox sending fifty cold emails on day one is a textbook spam pattern. Ramp over weeks.

```go
// WarmupCap returns the maximum cold sends allowed today for a mailbox.
// The engine enforces this; it is not a suggestion in the UI.
func WarmupCap(m *Mailbox, target int) int {
    if m.WarmupStartedAt == nil {
        return 0 // not started → cannot send cold. Full stop.
    }
    day := m.WarmupDay
    switch {
    case day < 7:
        return min(5, target)
    case day < 14:
        return min(10+2*(day-7), target)
    case day < 28:
        return min(24+2*(day-14), target)
    default:
        return target
    }
}
```

**Warmup must reset on a health event.** If a mailbox starts bouncing or drawing complaints, it goes back to a reduced cap. A mailbox that degraded is not a mailbox that should keep sending at full volume while you investigate.

### 4.4 One-click unsubscribe

Required for bulk senders, and worth implementing regardless because it reduces complaints — a person who can unsubscribe in one click frequently does so *instead of* clicking "report spam," and those are radically different outcomes for your reputation.

```go
func unsubscribeHeaders(token string) map[string]string {
    return map[string]string{
        "List-Unsubscribe": fmt.Sprintf(
            "<https://unsub.example.com/u/%s>, <mailto:unsub@example.com?subject=%s>",
            token, token),
        "List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
    }
}
```

Both headers are required for RFC 8058 compliance. Google accepts only the RFC 8058 header form; Yahoo also accepts the `mailto:` fallback, so include both.

The endpoint must accept a `POST` with no confirmation page, no login, and no preference center, and it must suppress the address **immediately** — not within 48 hours, immediately. Then return a simple confirmation.

**Make the unsubscribe link prominent in the body too.** Hiding it to reduce opt-outs converts unsubscribes into spam complaints, which is trading a free action for a catastrophic one.

---

## 5. The scheduling engine

### 5.1 The model

```go
type Sequence struct {
    ID       string
    Steps    []Step
    Settings SequenceSettings
}

type Step struct {
    Index       int
    Channel     Channel       // email | call_task | linkedin_task | sms
    DelayAfter  time.Duration // from the previous step's send
    TemplateID  string
    Conditions  []Condition   // deliberately NOT open-based — see §10.1
}

type Enrollment struct {
    ID           string
    SequenceID   string
    ContactID    string
    AccountID    string
    MailboxID    string
    State        EnrollmentState // active | paused | replied | booked | finished | bounced | suppressed
    CurrentStep  int
    NextSendAt   time.Time
    PausedReason string
    Timezone     string
}
```

### 5.2 Timezone and business-day arithmetic

Send in the **recipient's** local business hours. Getting this wrong means emailing people at 3am, which is both ineffective and an obvious automation tell.

```go
func nextSendTime(after time.Time, tz *time.Location, w Window, cal HolidayCalendar) time.Time {
    t := after.In(tz)
    for {
        if !cal.IsBusinessDay(t) {           // weekends AND local holidays
            t = startOfDay(t.AddDate(0, 0, 1))
            continue
        }
        if t.Before(windowStart(t, w)) {
            t = windowStart(t, w)
        }
        if t.After(windowEnd(t, w)) {
            t = startOfDay(t.AddDate(0, 0, 1))
            continue
        }
        // Jitter so sends don't land on suspiciously round timestamps.
        return t.Add(time.Duration(rand.Intn(int(w.JitterMax))))
    }
}
```

Two details that matter more than they look:

**Holiday calendars are per-country.** Emailing a German prospect on a German public holiday, or a US prospect on July 4th, is a small but real signal that nobody is paying attention.

**Jitter is not decoration.** Sends at exactly 09:00:00.000 across hundreds of recipients is a machine signature. Randomize within a window.

### 5.3 Rate limiting across three dimensions

```go
type RateLimiter interface {
    // Allow returns whether this send may proceed right now.
    Allow(ctx context.Context, mailboxID, domainID string) (bool, RateLimitReason)
}
```

Three caps, all enforced:

| Level | Cap | Why |
|---|---|---|
| **Mailbox** | Warmup cap, or 30–50/day | Behavioral plausibility |
| **Domain** | Below 5,000/day | Bulk-sender classification is permanent (§2.4) |
| **Account (recipient)** | N touches per company per week, across all reps | The "five reps emailed me" problem (§6.3) |

Plus a minimum inter-send interval per mailbox — sending forty emails in ninety seconds from one mailbox is not human.

### 5.4 Backpressure

When the queue can't drain within the sending window, you have two options and only one is correct.

| Option | Consequence |
|---|---|
| Compress — send faster to fit | Exceeds rate limits, looks robotic, damages reputation |
| **Spill to tomorrow** | Sequence timing slips |

**Always spill.** Sequence timing is a preference; reputation is the business. And surface it loudly: "247 sends deferred to tomorrow — you are enrolling faster than your mailbox capacity can deliver." That message is the product doing its job, telling the operator the arithmetic in §2.5 doesn't work for their plan.

### 5.5 The complaint budget circuit breaker ★

The most important control in the system.

```go
type ComplaintBudget struct {
    DomainID       string
    WindowDays     int      // 1 — Google calculates daily
    HaltThreshold  float64  // 0.0008 (0.08%) — well below the 0.1% target
    WarnThreshold  float64  // 0.0005
    MinVolume      int      // don't act on tiny samples
}

func (b *ComplaintBudget) Evaluate(m Metrics) BudgetDecision {
    if m.Inboxed < b.MinVolume {
        return BudgetDecision{Action: Continue, Reason: "insufficient volume"}
    }
    // Denominator is INBOXED, not sent — see §2.3.
    rate := float64(m.Complaints) / float64(m.Inboxed)
    switch {
    case rate >= b.HaltThreshold:
        return BudgetDecision{
            Action: HaltDomain,
            Reason: fmt.Sprintf("complaint rate %.3f%% at or above halt threshold %.3f%%",
                rate*100, b.HaltThreshold*100),
            RequiresHumanAck: true,
        }
    case rate >= b.WarnThreshold:
        return BudgetDecision{Action: ThrottleDomain, Factor: 0.5}
    }
    return BudgetDecision{Action: Continue}
}
```

Design choices worth defending:

**Halt at 0.08%, not 0.3%.** By the time Postmaster Tools shows 0.3%, the denominator spiral (§2.3) is already running. The ceiling is where enforcement begins, not where risk begins.

**Halting requires human acknowledgment to lift.** An automatic resume means the same behavior resumes and the spiral continues. A human has to look at what happened.

**Halt the domain, not the sequence.** Reputation is a domain-level property. A bad sequence on a shared domain damages every sequence on it.

**Also monitor bounce rate** with the same machinery. Above 2% is critical, and high bounces usually mean list quality problems that will produce complaints next.

### 5.6 Idempotency

Sending the same email to a prospect twice is embarrassing, visible, and damages the relationship you're trying to build.

```go
func sendKey(enrollmentID string, stepIndex int) string {
    return fmt.Sprintf("send:%s:%d", enrollmentID, stepIndex)
}

func (e *Engine) Send(ctx context.Context, job SendJob) error {
    key := sendKey(job.EnrollmentID, job.StepIndex)
    // Claim the key BEFORE dispatch. A crash after claiming means a
    // missed send, which is recoverable. A crash before means a double
    // send, which is not.
    claimed, err := e.store.ClaimSendKey(ctx, key)
    if err != nil {
        return err
    }
    if !claimed {
        e.log.Info("send already dispatched, skipping", "key", key)
        return nil
    }
    ...
}
```

The ordering comment is the important part. When you must choose, fail toward not sending.

---

## 6. Suppression and contact governance

### 6.1 The most important data structure

```go
type SuppressionReason string

const (
    Unsubscribed       SuppressionReason = "unsubscribed"
    SpamComplaint      SuppressionReason = "spam_complaint"
    HardBounce         SuppressionReason = "hard_bounce"
    DoNotContactCRM    SuppressionReason = "crm_dnc"
    ExistingCustomer   SuppressionReason = "existing_customer"
    OpenOpportunity    SuppressionReason = "open_opportunity"
    Competitor         SuppressionReason = "competitor"
    RecentlyContacted  SuppressionReason = "recently_contacted"
    JurisdictionBlocked SuppressionReason = "jurisdiction_blocked"
    HostileReply       SuppressionReason = "hostile_reply"
)
```

Suppression is checked at **three points**, and the third is the one people skip:

1. At list import
2. At enrollment
3. **At send time, immediately before dispatch** ★

The third check is essential because everything can change between enrollment and send. A prospect unsubscribes from one sequence on Tuesday; a different sequence has a step queued for Wednesday. Without a send-time check, it goes out, and you have emailed someone who explicitly opted out — a CAN-SPAM violation and a near-certain spam complaint.

```go
func (e *Engine) preflightSend(ctx context.Context, job SendJob) (bool, string) {
    if s := e.suppression.Check(ctx, job.ContactEmail); s != nil {
        return false, "suppressed: " + string(s.Reason)
    }
    if e.replies.HasReplied(ctx, job.EnrollmentID) {
        return false, "replied"                      // §7.4
    }
    if e.meetings.AccountHasBooking(ctx, job.AccountID) {
        return false, "meeting booked"               // §8
    }
    if !e.jurisdiction.Allowed(ctx, job.ContactID) {
        return false, "jurisdiction not enabled"
    }
    if ok, reason := e.limiter.Allow(ctx, job.MailboxID, job.DomainID); !ok {
        return false, "rate limited: " + string(reason)
    }
    return true, ""
}
```

Every one of those checks exists because of a specific way this goes wrong.

### 6.2 Unsubscribe is global and permanent

An unsubscribe applies to **every sequence from every rep**, not just the one it came from. Per-sequence opt-out is how a person unsubscribes three times and then files a complaint.

Store suppressions on the normalized email address, and never delete them. Suppression records outlive contact records, list imports, and CRM deletions. If a contact is deleted and re-imported, the suppression must still apply.

### 6.3 The "five reps emailed me" problem

Without account-level governance, four reps working the same target account each enroll two contacts, and the prospect receives eight cold emails in a week from one company. That is a complaint, and it is entirely self-inflicted.

```go
type AccountPolicy struct {
    MaxTouchesPerWeek    int  // across ALL reps and sequences
    MaxContactsInFlight  int  // simultaneously enrolled at one account
    CooldownAfterReply   time.Duration
    CooldownAfterBooking time.Duration
}
```

Enforced at enrollment and re-checked at send time. This is one of the highest-value features in the product and almost nobody builds it, because it makes individual reps' numbers look worse while making the company's outcomes better.

---

## 7. Reply detection

### 7.1 Thread by headers, never by subject

Subject-line matching breaks on `Re:` prefixes, localized prefixes (`AW:`, `SV:`, `Antw:`), forwards, and subject edits.

Store the `Message-ID` of every send. Match incoming mail on `In-Reply-To` and `References`.

```go
type SentMessage struct {
    EnrollmentID string
    StepIndex    int
    MessageID    string   // RFC 5322 Message-ID we generated
    ThreadID     string   // provider thread id (Gmail/Graph)
    SentAt       time.Time
}

func (d *Detector) match(msg InboundMessage) (*Enrollment, bool) {
    for _, ref := range append(msg.References, msg.InReplyTo...) {
        if sent, ok := d.store.BySentMessageID(ref); ok {
            return d.store.Enrollment(sent.EnrollmentID), true
        }
    }
    // Fall back to the provider thread id, which survives some client mangling.
    if sent, ok := d.store.BySentThreadID(msg.ThreadID); ok {
        return d.store.Enrollment(sent.EnrollmentID), true
    }
    return nil, false
}
```

### 7.2 Not every reply is a reply

```go
type ReplyClass string

const (
    ReplyPositive     ReplyClass = "positive"      // interested
    ReplyNegative     ReplyClass = "negative"      // not interested
    ReplyHostile      ReplyClass = "hostile"       // stop / unsubscribe / angry
    ReplyReferral     ReplyClass = "referral"      // "talk to my colleague"
    ReplyNotNow       ReplyClass = "not_now"       // "circle back in Q3"
    ReplyOutOfOffice  ReplyClass = "out_of_office"
    ReplyBounceNotice ReplyClass = "bounce"
    ReplyLeftCompany  ReplyClass = "left_company"
)
```

Each needs different handling:

| Class | Action |
|---|---|
| Positive / negative / referral | Stop the sequence, notify the rep |
| **Hostile** | Stop, **suppress globally**, notify, and flag for review — this is the strongest possible complaint precursor |
| **Not now** | Stop and schedule a re-enrollment at the stated date |
| **Out of office** | **Do not count as a reply.** Pause, parse the return date, resume after it. |
| Bounce notice | Hard → suppress permanently. Soft → retry with backoff, suppress after N. |
| Left company | Suppress the contact, flag the account for a new contact |

**Out-of-office handling is the one that visibly separates good sequencers from bad ones.** Treating an auto-reply as engagement stops the sequence for someone who never saw the email. Detect via `Auto-Submitted`, `X-Autoreply`, `X-Auto-Response-Suppress`, and `Precedence: auto_reply` headers first — header-based detection is far more reliable than body text matching, which fails across languages.

Then parse the return date and resume. A sequence that pauses for a vacation and picks up the day after is a small thing that reads as attentive rather than automated.

### 7.3 Delivery: webhooks plus polling

Gmail push notifications (via Pub/Sub `watch`) and Microsoft Graph subscriptions both deliver reply events quickly and both drop messages.

Run a polling reconciler alongside — every few minutes, check mailboxes for messages newer than the last processed cursor. Log the discrepancy rate: a rising rate means your webhook path is broken, and you want to know before a rep does.

### 7.4 The reply race condition ★

The classic bug:

```
09:00:00  scheduler queues step 3 for contact X
09:00:15  contact X replies "please stop emailing me"
09:00:30  the queued job fires and sends step 3
```

The check happened at schedule time. The world changed before dispatch.

**The fix is the send-time preflight in §6.1** — `HasReplied` is checked immediately before dispatch, inside the same operation that claims the idempotency key. The window shrinks from minutes to milliseconds.

Belt and braces: when a reply is detected, actively cancel queued jobs for that enrollment as well as marking the state. Durable workflow engines make cancellation straightforward and it closes the remaining gap.

---

## 8. Meeting-booked pause

### 8.1 Detection sources

| Source | Reliability | Latency |
|---|---|---|
| Scheduling-link webhook (Calendly, Chili Piper, etc.) | High | Seconds |
| Calendar event created with the prospect as attendee | High | Seconds to minutes |
| CRM meeting/event object | Medium | Depends on sync |
| Reply classified as positive with a proposed time | Low | Needs confirmation |

Use webhooks where available, calendar watch as the backstop, and treat reply-inferred bookings as a prompt to the rep rather than an automatic pause.

### 8.2 Pause at the account level, not the contact level ★

If someone at Acme books a meeting, continuing to cold-email three other people at Acme is the single most visible failure a sequencer can produce. The prospect mentions it on the call, and it reads as an organization that doesn't talk to itself.

```go
func (h *MeetingHandler) OnBooked(ctx context.Context, ev MeetingBooked) error {
    contact, err := h.crm.ContactByEmail(ctx, ev.AttendeeEmail)
    if err != nil {
        return err
    }

    // Everyone at the account, across all reps and sequences.
    enrollments, err := h.store.ActiveEnrollmentsByAccount(ctx, contact.AccountID)
    if err != nil {
        return err
    }
    for _, en := range enrollments {
        if err := h.store.Pause(ctx, en.ID, PauseReason{
            Kind:      "meeting_booked",
            Evidence:  ev.SourceRef,
            ExpiresAt: ev.StartsAt.Add(h.policy.CooldownAfterMeeting),
        }); err != nil {
            return err
        }
        h.cancelQueued(ctx, en.ID)      // §7.4 — cancel, don't just mark
    }
    return h.notify(ctx, contact.AccountID, ev)
}
```

Make account-level the default with a per-sequence override, rather than the other way round.

### 8.3 The cases that need decisions

| Case | Handling |
|---|---|
| Meeting cancelled | Resume after a cooldown — a few days, not immediately. An instant resume after a cancellation reads badly. |
| No-show | Different from cancelled. Usually a specific short follow-up sequence, not a resume of cold outreach. |
| Meeting booked with a different rep | Still pause. The account has engaged; that's what matters. |
| Meeting in the distant future | Pause now, don't wait. |
| Race: booked at 09:00:30, send at 09:00:31 | Same send-time preflight as §7.4 |

### 8.4 What "pause" means

Be precise, because the choice is user-visible:

- **Pause** — stop, remember the position, resumable
- **Stop** — terminal, requires re-enrollment
- **Finish** — completed all steps normally

A meeting booking is a **pause** with a cooldown expiry, not a stop. If the meeting falls through and the deal goes quiet, you want the option to resume from step 3 rather than starting over.

---

## 9. Multichannel

### 9.1 What multichannel actually buys you

The value is **coordinated timing across channels**, not automating every channel. A sequence of email → wait → LinkedIn view → email → call task is effective because the touches are spaced and varied. Whether the LinkedIn action is automated or performed by a human in ten seconds changes nothing about the prospect's experience — and it changes everything about the rep's account safety (§3.4).

### 9.2 Manual tasks as first-class steps

```go
type Step struct {
    Channel  Channel
    Mode     StepMode // automatic | manual_task
    ...
}
```

Manual-task steps appear in the rep's task queue with the context they need: the prospect, the account, prior touches, the suggested message, and a one-click "done" or "skip." The sequence advances when the task is completed or expires.

This handles LinkedIn, phone, direct mail, and gifting without any ToS exposure, and it's a better product for the rep than pretending a bot is them.

### 9.3 Channel-specific gates

```go
func (e *Engine) channelAllowed(ctx context.Context, step Step, c Contact) (bool, string) {
    switch step.Channel {
    case ChannelSMS:
        consent := e.consent.SMSConsent(ctx, c.ID)
        if consent == nil {
            return false, "TCPA: no recorded prior express written consent"
        }
        if !withinQuietHours(time.Now(), c.Timezone) {
            return false, "outside permitted messaging hours"
        }
    case ChannelLinkedIn:
        if step.Mode != ManualTask {
            return false, "LinkedIn steps must be manual tasks (User Agreement)"
        }
    case ChannelCall:
        if e.dnc.Listed(ctx, c.Phone) {
            return false, "on Do Not Call registry"
        }
        if !withinCallWindow(time.Now(), c.Timezone) {
            return false, "outside permitted calling hours"
        }
    }
    return true, ""
}
```

The LinkedIn case is a hard structural refusal rather than a setting. That's deliberate — it means nobody can configure their way into an account ban.

---

## 10. Metrics that mean something

### 10.1 Open tracking is dead ★

Apple Mail Privacy Protection pre-fetches remote images through a proxy regardless of whether the recipient opened anything. With Apple Mail's share of email clients, a large fraction of your "opens" are a machine loading a pixel.

Two consequences:

**Open rate is noise.** It is not a weak signal, it is contaminated in a way you cannot correct for, because you don't know which recipients use MPP.

**The tracking pixel hurts deliverability.** A remote image loaded from a tracking domain is a spam-filter signal, and you're paying that cost for a metric that means nothing.

**Any sequence logic branching on opens is branching on random numbers.** "If opened but no reply, send the follow-up" is extremely common in this category and it is now pure superstition.

**Recommendation: don't ship open tracking.** If competitive pressure forces it, ship it off by default, label it as unreliable in the UI, and never allow sequence branching on it.

Link-click tracking is more meaningful — it requires an actual click — but it still needs a custom tracking domain (§4.1) and still adds a small deliverability cost. Worth it for genuine intent signals; not worth it on every link.

### 10.2 What to measure instead

| Metric | Why |
|---|---|
| **Reply rate** | The actual goal. Uncontaminated. |
| **Positive reply rate** | Better still — classification from §7.2 |
| **Meeting booked rate** | The business outcome |
| **Complaint rate** | The constraint (§2.2). Track per domain, per sequence, daily. |
| **Bounce rate** | List quality leading indicator |
| **Unsubscribe rate** | A *healthy* signal — people opting out instead of complaining |
| **Deliverability by domain** | From Postmaster Tools, per sending domain |

Note the unsubscribe framing. A rising unsubscribe rate with a flat complaint rate is the system working correctly: you gave people an easy exit and they took it instead of the destructive one.

### 10.3 List quality as a measured input

```go
type ListQualityReport struct {
    SourceID          string
    HardBounceRate    float64  // > 5% suggests scraped or stale
    ComplaintRate     float64
    ReplyRate         float64
    CatchAllRate      float64  // unverifiable domains
    RoleAddressRate   float64  // info@, sales@ — low value, high complaint risk
}
```

Attribute these back to the list source. When a particular source consistently produces high bounces and complaints, that's actionable and specific — and it's the conversation that actually improves outcomes, as opposed to tweaking subject lines.

---

## 11. Tech stack and setup

| Layer | Choice | Why |
|---|---|---|
| **Language** | Go | Concurrency, durable workers, straightforward deployment |
| **Workflow** | Temporal | Long-running, multi-step, cancellable, with durable state and visibility. Cancellation matters for §7.4. |
| **Database** | PostgreSQL | Transactional state, suppression lists, send keys |
| **Queue** | Temporal task queues, or Redis + a durable outbox | |
| **Email send** | Gmail API / Microsoft Graph via OAuth | Not an ESP (§4.2) |
| **Reply detection** | Gmail Pub/Sub watch + Graph subscriptions, plus polling | Webhooks drop (§7.3) |
| **Deliverability data** | Google Postmaster Tools API | The complaint rate that matters is Google's number, not yours |
| **Email verification** | A third-party verifier at import | Cheaper than a bounce |
| **Scheduling links** | Calendly / Chili Piper webhooks | |
| **CRM** | Salesforce / HubSpot | |

Two setup notes:

**Postmaster Tools is the source of truth for complaint rate.** You cannot compute it yourself — you don't know what was inboxed versus filtered, which is the denominator (§2.3). Pull it via API and drive the circuit breaker from Google's number.

**Get a dedicated test domain and mailbox early**, separate from anything that matters, and accept that you will burn it while developing. Testing a sequencer against production mailboxes is how you discover the double-send bug the expensive way.

---

## 12. Repository layout

```
Outbound-Sequencer/
├── README.md
├── docs/
│   ├── design.md                ← this document
│   ├── deliverability.md        ← ★ the §2 physics, for operators
│   ├── legal.md                 ← per-channel, per-jurisdiction
│   └── runbook-halt.md          ← what to do when the breaker trips
├── cmd/
│   ├── engine/
│   ├── replywatcher/
│   └── postmaster-sync/
├── internal/
│   ├── sequence/
│   │   ├── model.go
│   │   ├── scheduler.go         ← timezone, business days, jitter
│   │   └── backpressure.go      ← always spill, never compress
│   ├── deliverability/
│   │   ├── budget.go            ← ★ the circuit breaker
│   │   ├── warmup.go
│   │   ├── postmaster.go
│   │   └── domains.go           ← refuses the primary domain
│   ├── send/
│   │   ├── preflight.go         ← ★ the send-time gate
│   │   ├── idempotency.go
│   │   ├── gmail.go
│   │   └── graph.go
│   ├── suppression/
│   ├── governance/              ← account-level touch limits
│   ├── reply/
│   │   ├── threading.go         ← Message-ID / References
│   │   ├── classify.go
│   │   ├── autoreply.go         ← header-based OOO detection
│   │   └── reconcile.go
│   ├── meeting/
│   ├── jurisdiction/            ← geo gating
│   └── unsubscribe/             ← RFC 8058 endpoint
└── test/
    ├── races/                   ← ★ reply-vs-send, booking-vs-send
    └── fixtures/
```

---

## 13. Milestone ladder

### M0 — Deliverability and legal design ★ **before any sending code**
**Est. 1 week**

Write `docs/deliverability.md` and `docs/legal.md`. Decide: domain strategy, per-mailbox caps, warmup schedule, complaint budget thresholds, which jurisdictions are enabled, SMS consent requirements, LinkedIn as manual-task-only.

Do the arithmetic from §2.5 for your intended volume. If it doesn't work, the answer is fewer prospects or more mailboxes — decide now, not after building.

**Done when:** an operator can read `deliverability.md` and understand why their volume is capped.

---

### M1 — Sending infrastructure
**Est. 1.5 weeks**

Domain registration and DNS (SPF, DKIM, DMARC, custom tracking domain), OAuth mailbox connection, warmup enforcement, the RFC 8058 unsubscribe endpoint.

**Done when:** the system refuses to attach the primary domain, refuses to send from a mailbox with no warmup, and one-click unsubscribe suppresses immediately.

---

### M2 — The scheduler
**Est. 2 weeks**

Enrollment, step progression, timezone and business-day arithmetic with holiday calendars, jitter, three-level rate limiting, backpressure that spills.

**Done when:** a sequence enrolled across ten timezones sends in each recipient's local business hours, and over-enrollment defers with a visible message rather than compressing.

---

### M3 — Suppression and governance ★
**Est. 1 week**

Suppression list with all reasons, checks at import/enrollment/**send time**, account-level touch limits, jurisdiction gating.

**Done when:** a contact who unsubscribes from sequence A is provably not sent sequence B's queued step, verified by a race test.

---

### M4 — Idempotency and the send path
**Est. 1 week**

Send-key claiming before dispatch, the preflight gate, provider send, failure handling.

**Done when:** a chaos test that kills the worker mid-send at every point produces zero duplicate sends.

---

### M5 — Reply detection ★
**Est. 2 weeks**

Header-based threading, classification, header-based out-of-office detection with return-date parsing, webhook plus polling reconciliation, queued-job cancellation.

**Done when:** an out-of-office auto-reply pauses rather than stops, resumes after the return date, and a reply arriving 200ms before a queued send prevents that send.

---

### M6 — Meeting-booked pause
**Est. 1 week**

Webhook and calendar detection, account-level pause, cancellation handling, cooldowns.

**Done when:** one person at an account booking a meeting pauses every enrollment at that account across all reps.

---

### M7 — The complaint budget circuit breaker ★
**Est. 1 week**

Postmaster Tools sync, daily rate evaluation on the inboxed denominator, throttle and halt, human acknowledgment to resume, the runbook.

**Done when:** a simulated complaint spike halts the domain before 0.1% and cannot auto-resume.

---

### M8 — Multichannel
**Est. 1.5 weeks**

Manual task steps, task queue UI, channel gates, SMS consent enforcement, DNC and calling-window checks.

**Done when:** an automatic LinkedIn step is structurally impossible to configure.

---

### M9 — Metrics
**Est. 1 week**

Reply and positive-reply rates, meeting rate, complaint and bounce rates per domain and sequence, list-quality attribution by source. No open tracking.

---

## 14. Reference implementations

### 14.1 Out-of-office detection

```go
// IsAutoReply detects automated responses from headers first. Body-text
// matching fails across languages and is only a last resort.
func IsAutoReply(msg InboundMessage) bool {
    if v := msg.Header("Auto-Submitted"); v != "" && !strings.EqualFold(v, "no") {
        return true
    }
    if msg.Header("X-Autoreply") != "" || msg.Header("X-Autorespond") != "" {
        return true
    }
    if strings.Contains(strings.ToLower(msg.Header("Precedence")), "auto_reply") {
        return true
    }
    if msg.Header("X-Auto-Response-Suppress") != "" {
        return true
    }
    // RFC 3834: automatic responses should have a null Return-Path.
    if msg.Header("Return-Path") == "<>" {
        return true
    }
    return false
}

// ParseReturnDate extracts a return date so the sequence resumes at the
// right time. Returns zero if no confident parse — then use a default delay.
func ParseReturnDate(body string, received time.Time) time.Time { ... }
```

Prefer a conservative default (resume in 7 days) over a confident wrong parse. Resuming while someone is still away wastes a touch.

### 14.2 The send-time gate, in order

```go
func (e *Engine) ExecuteSend(ctx context.Context, job SendJob) error {
    // 1. Claim the idempotency key first — fail toward not sending.
    claimed, err := e.store.ClaimSendKey(ctx, sendKey(job.EnrollmentID, job.StepIndex))
    if err != nil || !claimed {
        return err
    }

    // 2. Re-check everything. The world changed since scheduling.
    if ok, reason := e.preflightSend(ctx, job); !ok {
        e.store.RecordSkipped(ctx, job, reason)
        return nil                          // not an error — correct behavior
    }

    // 3. Budget check at the domain level.
    if d := e.budget.Current(ctx, job.DomainID); d.Action == HaltDomain {
        e.store.RecordSkipped(ctx, job, "domain halted: "+d.Reason)
        return nil
    }

    // 4. Send.
    res, err := e.provider.Send(ctx, job.Message)
    if err != nil {
        return e.handleSendFailure(ctx, job, err)
    }

    // 5. Persist Message-ID for threading (§7.1).
    return e.store.RecordSent(ctx, job, res.MessageID, res.ThreadID)
}
```

Step 2 returning `nil` rather than an error is deliberate. A skipped send is the system working, not failing, and treating it as an error pollutes your alerting with correct behavior.

---

## 15. Testing

### 15.1 Race tests are the important ones

```go
func TestReplyBeatsQueuedSend(t *testing.T) {
    for i := 0; i < 500; i++ {
        env := newTestEnv(t)
        en := env.Enroll(contact, sequence)
        env.AdvanceTo(en.NextSendAt.Add(-1 * time.Millisecond))

        var wg sync.WaitGroup
        wg.Add(2)
        go func() { defer wg.Done(); env.DeliverReply(en.ID, "not interested") }()
        go func() { defer wg.Done(); env.RunScheduler() }()
        wg.Wait()

        // Either the send happened before the reply, or it was skipped.
        // What must NEVER happen: a send dispatched after the reply landed.
        require.False(t, env.SentAfter(en.ID, env.ReplyTime(en.ID)),
            "sent an email after the prospect replied")
    }
}
```

Run it hundreds of times. Race bugs that appear one time in fifty are exactly the ones that reach production.

Also test: booking-versus-send, unsubscribe-versus-send, and concurrent enrollment of the same contact into two sequences.

### 15.2 Crash tests

Kill the worker at every point in `ExecuteSend` and assert zero duplicate sends across a full replay. This is the same discipline as the deployment orchestrator's chaos suite, and for the same reason — a double send is not recoverable.

### 15.3 Scheduler property tests

```go
func TestSendsAlwaysInBusinessHours(t *testing.T) {
    rapid.Check(t, func(t *rapid.T) {
        tz := genTimezone().Draw(t, "tz")
        at := genTime().Draw(t, "at")
        next := nextSendTime(at, tz, defaultWindow, holidays)

        local := next.In(tz)
        require.True(t, holidays.IsBusinessDay(local))
        require.GreaterOrEqual(t, local.Hour(), defaultWindow.StartHour)
        require.Less(t, local.Hour(), defaultWindow.EndHour)
    })
}
```

Generate timezones including the awkward ones — half-hour and 45-minute offsets, southern-hemisphere DST, and regions that changed their rules recently.

### 15.4 Test the budget breaker with real shapes

Replay a complaint spike against the circuit breaker and assert it halts before 0.1% and does not auto-resume. Include the denominator-collapse shape from §2.3 specifically — a breaker that only watches raw complaint counts will miss it entirely.

---

## 16. Stretch goals

| Feature | Effort | Value |
|---|---|---|
| **Mailbox rotation with health scoring** | Medium | Distribute across mailboxes weighted by current health rather than round-robin |
| **Automated domain health monitoring** | Medium | Blocklist checks, DNS validation, DMARC report parsing |
| **A/B testing with proper stats** | Medium | Sequential testing with correct stopping rules — most A/B in this category peeks and stops early |
| **Reply sentiment beyond classification** | Small | Extract objections and referral names |
| **Re-engagement after "not now"** | Small | Auto re-enroll at the date the prospect stated. High value, low effort. |
| **Deliverability score per sequence** | Medium | Predict complaint risk from content before sending |
| **Send-time optimization** | Medium | Per-contact best time from historical reply data — only meaningful with reply data, not opens |
| **Warmup network** | Large | Mutual inbox warming. Note that some providers treat this as manipulation. |
| **Intent-based enrollment** | Medium | Trigger from website visits or product signals rather than list uploads — the single biggest lever on complaint rate |

That last one deserves attention. Every problem in section 2 gets easier with better targeting, and triggering on real signals rather than a bought list is the difference between a 0.05% and a 0.5% complaint rate. It's the highest-leverage feature on the list.

---

## 17. References

### Deliverability

| Source | For |
|---|---|
| Google Email Sender Guidelines | The requirements and the 0.1% / 0.3% thresholds |
| Yahoo Sender Requirements | The parallel rules and the DKIM key length minimum |
| Microsoft Outlook sender requirements (2025) | The third provider's version |
| Google Postmaster Tools + API | The complaint rate that actually counts |
| **RFC 8058** | One-click unsubscribe — the exact header form Google requires |
| RFC 7489 (DMARC), 7208 (SPF), 6376 (DKIM) | Authentication |
| RFC 3834 | Automatic response behavior, for §14.1 |
| M3AAWG Sender Best Common Practices | The industry consensus document |

### Legal

> Reading list, not advice.

| Source | For |
|---|---|
| CAN-SPAM Act, 15 U.S.C. §§ 7701–7713 | US email |
| TCPA, 47 U.S.C. § 227 + FCC rules | SMS and calling. Prior express written consent. |
| CASL (S.C. 2010, c. 23) | Canada — the strict one |
| GDPR Arts. 6, 21 + ePrivacy Directive | EU |
| PECR (UK) | Corporate vs individual subscribers |
| LinkedIn User Agreement, § 8.2 | The automation prohibition |
| A2P 10DLC registration requirements | US business SMS |

### Technical

- Gmail API push notifications (Pub/Sub `watch`) and Microsoft Graph change notifications
- Temporal documentation on cancellation and signals — relevant to §7.4
- Apple Mail Privacy Protection technical notes — why §10.1 is true
- IANA time zone database and its update cadence, for §15.3

---

## Appendix A — Decision record

| Decision | Rationale |
|---|---|
| **The guardrails are the product, not a feature** | The 0.3% ceiling caps volume by physics. A sequencer optimizing for send volume builds a machine that destroys its user's domain. |
| **Halt at 0.08%, not at the 0.3% ceiling** | The ceiling is where enforcement begins, not where risk begins. The denominator spiral is already running by then. |
| Halt requires human acknowledgment | Auto-resume restarts the same behavior and the spiral continues |
| Halt the domain, not the sequence | Reputation is a domain-level property |
| Complaint rate from Postmaster Tools, not computed locally | The denominator is inboxed mail, which you cannot observe |
| **Hard per-domain cap below 5,000/day** | Bulk-sender classification is permanent once triggered; a single blast changes your regulatory category forever |
| **Never send from the primary domain** | A burned primary domain stops invoices, contracts, and support mail. Existential, not a marketing setback. |
| Separate registrable domains, not subdomains | Subdomain reputation isn't fully isolated from the parent |
| Send via the user's own mailbox, not an ESP | Shared ESP pools burn fast, and most transactional ESP acceptable-use policies prohibit cold outreach outright |
| Warmup enforced by the engine, resets on health events | A new mailbox sending 50 cold emails on day one is a textbook spam pattern |
| Custom tracking domain per sending domain | A shared tracker means inheriting every other sender's reputation |
| **RFC 8058 one-click unsubscribe, processed immediately** | Google accepts only that header form. And an easy unsubscribe is a complaint that didn't happen. |
| Prominent in-body unsubscribe too | Hiding it converts unsubscribes into spam complaints — trading a free action for a catastrophic one |
| **Suppression checked at send time, not just enrollment** | Everything can change between scheduling and dispatch; this is where opted-out people get emailed |
| Unsubscribe is global and permanent, outliving contact records | Per-sequence opt-out is how someone unsubscribes three times then complains |
| **Account-level touch limits across all reps** | Four reps × two contacts = eight cold emails from one company in a week. Self-inflicted complaint. |
| **Backpressure spills to tomorrow, never compresses** | Sequence timing is a preference; reputation is the business |
| Claim the idempotency key before dispatch | When you must choose, fail toward not sending |
| Thread replies on Message-ID / References, never subject | `Re:` prefixes, localized prefixes, forwards, and edits all break subject matching |
| **Out-of-office detected from headers, pauses rather than stops** | Body-text matching fails across languages; treating an auto-reply as engagement stops a sequence for someone who never read it |
| Hostile replies suppress globally and flag | The strongest available complaint precursor |
| Reply webhooks plus polling reconciliation | Gmail and Graph notifications both drop messages |
| Cancel queued jobs on reply, don't just mark state | Closes the remaining race window after the send-time check |
| **Meeting booking pauses the whole account by default** | Continuing to cold-email three colleagues after one books is the most visible failure a sequencer can produce |
| Booking is a pause with cooldown expiry, not a stop | If the meeting falls through you want to resume from step 3, not restart |
| **LinkedIn steps are structurally manual-task-only** | Automation violates the User Agreement and the asset at risk is the rep's personal account |
| **SMS requires a recorded written-consent artifact** | TCPA prior express written consent; $500–$1,500 per message and heavily class-actioned |
| Jurisdiction gating blocks by default | CASL is effectively prohibitive without consent and US teams routinely treat Canada as domestic |
| **No open tracking** | Apple MPP makes opens noise, the pixel hurts deliverability, and branching on opens is branching on random numbers |
| Unsubscribe rate reported as a healthy signal | Rising unsubscribes with flat complaints means the easy exit is working |
| List quality attributed back to source | The conversation that improves outcomes, versus tweaking subject lines |

---

## Appendix B — Quick reference card

```
THE CEILING (Google/Yahoo/Microsoft, recalculated DAILY)
  spam complaint rate   target < 0.10%   ceiling 0.30%
  denominator = INBOXED, not sent  ← the death spiral
  bounce rate           < 1% safe · > 2% critical
  Nov 2025: enforcement escalated to PERMANENT rejection
  non-compliance = SMTP reject, not spam folder

  inboxed/day    complaints allowed @0.1%    @0.3%
     1,000              1                      3
     5,000              5                     15
    10,000             10                     30

BULK SENDER = 5,000+/day to Gmail, per primary domain
  classification is PERMANENT once triggered
  requires: DMARC (≥ p=none), From: aligned with SPF or DKIM,
            RFC 8058 one-click unsub, honored within 48h
  Yahoo: DKIM key ≥ 1024 bits

ALL SENDERS
  SPF or DKIM · valid forward+reverse DNS · TLS · RFC 5322 · < 0.3%

DOMAINS
  NEVER send outbound from the primary domain
  separate registrable domains, not subdomains
  own SPF/DKIM/DMARC + own tracking domain + a real website
  cap each below 5,000/day

MAILBOXES
  cold: 30–50/day per mailbox — scale by adding mailboxes
  warmup: 5/day week 1 → ramp over ~4 weeks
  warmup RESETS on a health event

SEND-TIME PREFLIGHT (re-check everything; the world changed)
  suppressed? → replied? → account booked? → jurisdiction?
  → rate limit? → domain halted? → THEN send
  claim the idempotency key BEFORE dispatch

REPLIES
  thread on Message-ID / References — never subject
  auto-reply detection from HEADERS:
    Auto-Submitted · X-Autoreply · Precedence: auto_reply
    X-Auto-Response-Suppress · Return-Path: <>
  OOO = pause + resume after return date, NOT a stop
  hostile reply = stop + suppress globally + flag

LEGAL BY CHANNEL
  US email    CAN-SPAM: opt-out in 10 business days (but build to 48h)
  EU          lawful basis; DE effectively needs consent
  UK          corporate subscribers OK; sole traders need consent
  CANADA      CASL — effectively prohibited without consent, CAD $10M
  SMS         TCPA prior express WRITTEN consent · $500–1,500/message
  LinkedIn    automation violates the User Agreement → manual tasks only
  Phone       DNC · 8am–9pm local · no autodialer

DEAD METRICS
  open rate — Apple MPP pre-fetches pixels. Noise.
  never branch a sequence on opens.
```
