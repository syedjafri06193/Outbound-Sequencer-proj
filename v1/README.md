# Outbound Sequencer — v1

A multichannel outbound sequencer in Go, built the way the design document
argues it should be: **the guardrails are the product.**

A sequencer optimised for send volume builds a machine that destroys its
user's business. Every meaningful constraint here — the complaint budget, the
warmup schedule, the jurisdiction gate, the suppression list, the send-time
preflight — exists to refuse a send. That is the feature.

Standard library only. No dependencies.

```
go test ./... -race
go run ./cmd/engine -prospects 2000 -steps 4 -days 21 -mailboxes 1
go run ./cmd/postmaster-sync -csv test/fixtures/postmaster-spiral.csv -domain get-ours.example
```

264 tests across 12 packages, all passing under `-race`.

---

## The three things this gets right

### 1. The rate is complaints over *inboxed*

```
DATE           INBOXED  COMPLAINTS      RATE  ACTION
2026-02-16        9000           2    0.022%  continue
2026-02-18        7100           2    0.028%  continue
2026-02-20        3400           2    0.059%  throttle
2026-02-21        2200           2    0.091%  halt
```

Two complaints a day, every day. Nothing about the sending changed. The rate
quadrupled because filtering grew and the denominator collapsed underneath
it — and **a monitor watching complaint counts sees a flat line here.**

`deliverability.AnalyseTrend` looks for exactly that shape. `Metrics.ComplaintRate`
refuses to compute a rate from anything but Postmaster Tools data, rather than
returning a plausible wrong one from local counts.

The breaker halts at **0.08%**, not the 0.3% ceiling, because the ceiling is
where enforcement begins and not where risk begins. Lifting it needs a named
human and a note.

### 2. Out-of-office is detected from headers, and is not a reply

```
body-text matching missed 5 of 5 non-English out-of-office replies;
header detection caught all 5
```

German, Japanese, Finnish, Polish, French — all with correct
`Auto-Submitted: auto-replied`. An out-of-office is a **pause with a parsed
return date**, never a stop, and it is never counted as a reply. Treating an
auto-reply as engagement stops the sequence for someone who never saw the
email.

```
$ printf 'Auto-Submitted: auto-replied\n\nOlen lomalla.\n' | go run ./cmd/replywatcher

  class:            out_of_office
  counts as reply:  false
  stop sequence:    false
  pause until:      2026-03-09 00:00
```

### 3. The reply race is closed, not just narrowed

```
checking only at schedule time:  252 of 500 emails went out after the prospect replied
send-time preflight, no locking:  ~80 of 500
preflight under the enrolment lock: 0 of 500, widest window 400µs
```

The design document's send-time preflight shrinks the window "from minutes to
milliseconds", and it does — but milliseconds times thousands of sends is not
zero. A per-enrolment row lock held across the idempotency claim, the
preflight and the provider call closes it. The reply, booking and unsubscribe
paths take the same lock before they write.

`test/races/` runs each of these 500 times under `-race`.

## Layout

```
cmd/
  engine/            capacity planner and dry-run scheduler
  replywatcher/      classify a message from stdin
  postmaster-sync/   replay a Postmaster export through the breaker
internal/
  deliverability/    the circuit breaker, warmup, domains, Postmaster ingest
  sequence/          timezone-correct scheduling, backpressure
  suppression/       global, permanent, keyed on the canonical address
  governance/        account-level touch limits across all reps
  reply/             header threading, auto-reply detection, classification
  meeting/           account-level pause on a booking
  jurisdiction/      block-by-default geo gating with a recorded lawful basis
  unsubscribe/       RFC 8058 one-click endpoint
  ratelimit/         mailbox / domain / account caps plus inter-send interval
  send/              the send-time gate and the dispatcher
  store/             in-memory state with explicit atomicity boundaries
test/races/          reply-, booking-, unsubscribe- and halt-vs-send
docs/
  deliverability.md  the physics, for operators
  legal.md           per-channel, per-jurisdiction
  runbook-halt.md    what to do when the breaker trips
  notes-on-the-spec.md  where the design document is wrong, with measurements
```

## Everything else it refuses to do

**Never the primary domain.** If outbound burns `yourcompany.com`, invoices
and password resets stop being delivered. A validation error, not a warning.
Subdomains are refused too — subdomain reputation is not isolated from the
parent, and isolation is the point.

**Warmup is enforced.** Cap 0 for a mailbox that has not started. Resets a
week on a health event; halts outright on a provider rejection.

**30–50 sends per mailbox per day.** You scale by adding mailboxes. 2,000
prospects × 4 steps ÷ 21 days = 381/day = **ten mailboxes**, and `cmd/engine`
will tell you so before you buy anything. `Limits.Validate()` refuses a cap
above 100/day.

**Below 5,000/day per domain.** Crossing the Gmail bulk threshold once
classifies the domain permanently, so the default cap is 4,500.

**Blocked by default outside the US.** CASL is the one that catches people.
Opening a region needs a lawful basis with a named person, an assertion in
their own words, and a review date.

**No cold SMS without a written consent artifact.** $500 per message, trebled
for willful, no cap. No jurisdiction setting opens it and no operator
assertion can, because the consent belongs to the recipient.

**LinkedIn steps are manual tasks by construction.** Automation violates the
User Agreement and the rep's own account is the asset at risk. There is no
configuration path to automating it.

**Suppression is global, permanent, and checked three times** — import,
enrolment, and immediately before dispatch. Keyed on the canonical address so
it outlives contact deletion and re-import, with Gmail dot and plus variants
collapsed to one key.

**One-click unsubscribe, processed immediately.** Both RFC 8058 headers, a
prominent body link, no confirmation page, no login, no expiry on the token,
and never an error to the recipient once the suppression has landed.

**A meeting booking pauses the whole account**, across every rep and
sequence, as a pause with a cooldown — not a stop, so a resume picks up at
step 3.

**Always spill, never compress.** "247 sends deferred to tomorrow — you are
enrolling faster than your mailbox capacity can deliver" is the product doing
its job.

**Claim the idempotency key before dispatch.** A crash after the claim is a
missed send, which is recoverable and visible in `StaleClaims`. A crash
before it is a double send, which is not. When you must choose, fail toward
not sending.

**Open tracking is dead.** Apple MPP pre-fetches images. Nothing branches on
opens and nothing reports them.

## Where the design document is wrong

Four defects, measured rather than asserted — including a scheduler that
fails the document's own property test 960 times in 1,000, and a reply
matcher with three independent bugs. Plus one real bug the test suite found
in this implementation. All in
[docs/notes-on-the-spec.md](docs/notes-on-the-spec.md).

## Not implemented

Real Gmail and Microsoft Graph transports (`send.Provider` is the interface),
a durable workflow engine, persistence, and the UI. The store is in-memory
with the atomicity boundaries a real database would have to preserve drawn
explicitly — those boundaries are the design, and the race tests exercise
them.
