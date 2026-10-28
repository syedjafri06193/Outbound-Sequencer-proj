# Deliverability, for operators

This is the physics. None of it is negotiable by configuration, and most of
the product exists to stop you violating it.

If you read one section, read [the denominator](#the-denominator-is-what-moves).

---

## The numbers

| Thing | Value | Where it is enforced |
|---|---|---|
| Complaint rate target | **0.1%** | `deliverability.GoogleTargetRate` |
| Complaint rate ceiling | **0.3%** | `deliverability.GoogleCeilingRate` |
| Our halt threshold | **0.08%** | `deliverability.DefaultBudget().HaltThreshold` |
| Our warn threshold | **0.05%** | `deliverability.DefaultBudget().WarnThreshold` |
| Bounce warning | 1% | `deliverability.BounceWarnRate` |
| Bounce critical | 2% | `deliverability.BounceCriticalRate` |
| Bulk-sender threshold | **5,000/day to Gmail** | `ratelimit.BulkThreshold` |
| Our domain cap | 4,500/day | `ratelimit.DefaultLimits().DomainDaily` |
| Cold sends per mailbox | 30–50/day | `ratelimit.DefaultLimits().MailboxDaily` |
| Minimum gap between sends | 90 seconds | `ratelimit.DefaultLimits().MinInterval` |
| Warmup | 28 days, enforced | `deliverability.DefaultWarmup()` |

The complaint rate is **recalculated daily**. There is no weekly average to
hide behind.

## What 0.1% actually means

At 1,000 inboxed messages a day:

- **1 complaint** is the target.
- **3 complaints** is the ceiling.
- **4 complaints** is over it.

At 500 inboxed, a single complaint is 0.2% — already double the target. The
engine's `ComplaintsAllowed` floors rather than rounds, so it will tell you
the allowance is zero rather than implying you have one to spend.

Since November 2025, Google escalates sustained violations to permanent SMTP
rejection. Not filtering. Rejection.

## The denominator is what moves

The rate is **complaints ÷ inboxed**. Not complaints ÷ sent.

This is the single most important sentence in this document, because the
denominator is not under your control and it moves faster than the
numerator.

Run `cmd/postmaster-sync` against a real export and you will see the shape:

```
DATE           INBOXED  COMPLAINTS      RATE  ACTION
2026-02-16        9000           2    0.022%  continue
2026-02-17        8800           2    0.023%  continue
2026-02-18        7100           2    0.028%  continue
2026-02-19        5200           2    0.038%  continue
2026-02-20        3400           2    0.059%  throttle
2026-02-21        2200           2    0.091%  halt
```

Two complaints a day, every day. Nothing about the sending changed. The rate
quadrupled because filtering grew and the denominator collapsed underneath
it.

**A monitor watching complaint counts sees a flat line here.** It will keep
reporting "two complaints, same as yesterday" right up to the point the
domain stops delivering. `deliverability.AnalyseTrend` looks for exactly this
shape — inboxed volume falling while the count holds — and says so in words.

The practical consequence: a complaint rate you compute yourself from your
own send counts is not the number Google is using, and it will be wrong in
the reassuring direction. `Metrics.ComplaintRate` refuses to return a rate
for anything that is not Postmaster data, rather than returning a plausible
wrong one.

## Why we halt at 0.08% and not 0.3%

By the time Postmaster Tools shows 0.3%, the spiral above has been running
for days. The ceiling is where **enforcement** begins, not where **risk**
begins.

Halting at 0.08% means halting while the numbers still look fine to a person
reading the dashboard. That is the point.

Three design choices that follow:

- **Halting requires a human to lift it.** An automatic resume means the
  same behaviour resumes and the spiral continues. `Breaker.Acknowledge`
  needs an operator name and a note, and refuses an empty one.
- **Resume is throttled, not full speed.** You come back at a fraction of
  the previous rate.
- **We halt the domain, not the sequence.** Reputation is a domain-level
  property. A bad sequence on a shared domain damages every sequence on it.

## Bulk-sender classification is permanent

Cross 5,000 messages a day to Gmail **once** and the domain is permanently
subject to the stricter bulk-sender rules. There is no way back.

The default domain cap is 4,500, not 4,999, because a counter racing a send
in flight should not be the only thing between you and an irreversible
classification. `Limits.Validate()` refuses a configuration at or above the
threshold.

## Volume arithmetic

Per mailbox, cold: **30–50 a day**. Above that a mailbox stops looking like a
person, whatever your warmup says.

You scale by adding mailboxes, not by raising the cap. The arithmetic:

```
2,000 prospects × 4 steps ÷ 21 business days = 381 sends a day
381 ÷ 40 per mailbox = 10 mailboxes
```

**Ten mailboxes, not one.** Run `cmd/engine` with your own numbers before you
buy anything:

```
go run ./cmd/engine -prospects 2000 -steps 4 -days 21 -mailboxes 1
```

It will tell you the plan does not fit, how many mailboxes it needs, and
that raising the per-mailbox cap is not one of the options.

## Domains

- **Never the primary domain.** If outbound burns `yourcompany.com`, then
  invoices, contracts, support replies and password resets stop being
  delivered. That is an existential business failure, and recovering from it
  is slow. The registry refuses it as a validation error, not a warning.
- **Separate registrable domains, not subdomains.** Subdomain reputation is
  not fully isolated from the parent, and isolation is the entire point.
- Each outbound domain needs its own SPF, DKIM (1024-bit minimum for Yahoo),
  DMARC, **its own tracking domain**, and a real website that resolves. A
  domain with no site is itself a spam signal, and a shared tracking domain
  means inheriting every other sender's reputation.

## Warmup

Enforced by the engine, not suggested in the UI.

| Days | Cap |
|---|---|
| 0 (not started) | **0** — cannot send cold at all |
| 1–7 | 5/day |
| 8–14 | 10 + 2 per day |
| 15–28 | 24 + 2 per day |
| 28+ | your target |

**Warmup resets on a health event.** A mailbox that starts bouncing or
drawing complaints goes back a week. A provider rejection or an auth failure
halts it outright — cap zero — until someone looks.

## Authentication is the leading indicator

Watch DKIM, SPF and DMARC pass rates before you watch the complaint rate.
Unauthenticated mail lands in spam, which shrinks the inboxed denominator,
which raises the complaint rate on unchanged behaviour. Seeing the auth drop
first is the difference between fixing a DNS record and explaining a halted
domain.

`PostmasterRow.AuthHealth()` flags anything below a 95% pass rate.

## Open tracking is dead

Apple Mail Privacy Protection pre-fetches images, so opens are noise. Do not
branch on them, do not report them, and be suspicious of any tool that shows
you an open rate as a headline number.

What to measure instead:

- **Reply rate** — the only engagement signal that survived.
- **Positive reply rate** — the one that correlates with revenue.
- **Complaint rate per domain, daily** — from Postmaster, not computed.
- **Inboxed volume per domain, daily** — the denominator. Watch it.
- **Bounce rate** — list quality, and a leading indicator of complaints.
- **Skipped sends by reason** — `Store.SkipsByReason()`. A spike in
  `suppression` means a list problem; a spike in `ratelimit` means the
  capacity arithmetic does not work.

## When the breaker trips

See [runbook-halt.md](runbook-halt.md).
