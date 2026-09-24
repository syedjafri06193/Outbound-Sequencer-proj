# Notes on the spec

Places where `Documentation/README.md` says something the implementation had
to depart from, with the measurement that justified the departure. Everything
here is reproducible from the test suite.

The document is unusually good — the physics in §2 and the ordering argument
in §14.2 are correct and load-bearing. What follows is the reference code,
not the reasoning.

---

## 1. §5.2's `nextSendTime` puts sends outside the sending window

**Severity: high.** It breaks the invariant the function exists to
guarantee, and §15.3's own property test contradicts it.

The reference implementation applies jitter as the last step, after the
window-end check has already passed:

```go
// §5.2, paraphrased
if t.After(end) { t = nextDay(...) }
...
return t.Add(jitter(w.JitterMax))
```

A slot found at 16:58 plus up to 30 minutes of jitter lands at 17:28.

§15.3 then asserts:

```go
require.Less(t, local.Hour(), defaultWindow.EndHour)
```

So the document's scheduler fails the document's own property test.

**Measured.** `TestDocumentsOwnImplementationViolatesItsOwnPropertyTest`
reproduces §5.2 verbatim as `docOrderNextSendTime` and runs it 1,000 times:

```
the document's ordering put 960 of 1000 sends past the 17:00 window end
```

It survives casual testing because it only misbehaves near the window edge —
in production it shows up as a trickle of 5pm emails nobody can account for.

**What we do.** Bound the jitter to the remaining window, then re-check:

```go
remaining := end.Sub(t)
jitter := w.JitterMax
if jitter > remaining { jitter = remaining }
if jitter > 0 { t = t.Add(time.Duration(rng.Int63n(int64(jitter)))) }
// Re-check: a DST transition inside the window moves the wall-clock end.
if !t.Before(atHour(t, w.EndHour, tz)) { t = startOfDay(t.AddDate(0,0,1), tz); continue }
```

`model.Window.Valid()` additionally refuses a jitter that does not fit inside
the window at all, so the configuration cannot express the bug.

Verified by a 20,000-iteration property test across twelve timezones
including Kathmandu (+05:45), Chatham (+12:45), Tehran, Sydney and
São Paulo.

## 2. §7.1's `match` has three defects

**Severity: medium.** All three silently mis-thread, which is worse than
failing to thread.

```go
// §7.1
for _, ref := range append(msg.References, msg.InReplyTo...) {
    if sent, ok := d.store.BySentMessageID(ref); ok { ... }
}
```

**Measured** by `TestDocumentsOwnMatchHasThreeDefects`, which runs §7.1
verbatim:

```
ordering:      the document's version matched "old-sequence"; the reply was to "current"
aliasing:      the caller's References backing array was overwritten with "<step3@ours>"
normalisation: 2 of 2 bracket-free references failed to thread
```

1. **References are searched before In-Reply-To, oldest-first.** `In-Reply-To`
   names the immediate parent and is the most specific answer; `References`
   is the whole ancestry and can contain messages from a different enrolment
   if a thread was forwarded. Searching the ancestry first, from the oldest
   end, maximises the chance of matching the wrong one.
2. **`append(msg.References, ...)` writes into the caller's slice** whenever
   it has spare capacity. A caller that reuses the message struct gets its
   `References` silently rewritten.
3. **Angle brackets are compared literally.** Some clients include them in
   `References` and some do not. Storing one form and comparing the other
   breaks threading for a whole class of clients, and the failure is
   invisible.

**What we do.** `reply.Match` checks `In-Reply-To` first, walks `References`
newest-first, never appends to the caller's slice, and normalises every id
through `normalizeMessageID` (strip brackets, trim, lowercase).

## 3. §7.2's out-of-office advice is right; a phrase list is not enough

The document is correct that header detection beats body matching. It is
worth quantifying how much, because the temptation to add "just a few
phrases" as a fallback is strong.

**Measured** by `TestBodyMatchingWouldMissEveryOneOfThem`:

```
body-text matching missed 5 of 5 non-English out-of-office replies;
header detection caught all 5
```

German, Japanese, Finnish, Polish and French, all with correct
`Auto-Submitted: auto-replied` headers.

Two details the reference does not mention:

- **`Auto-Submitted: no` means this is NOT an automatic response** (RFC
  3834). Checking only for the header's presence turns every well-behaved
  human client that sets it into a permanent out-of-office.
- **Header names are case-insensitive and providers disagree.** Gmail returns
  `Auto-Submitted`, Graph sometimes `auto-submitted`. A case-sensitive map
  lookup silently misses every auto-reply from one of them.
  `InboundMessage.Header` does a case-insensitive lookup.

## 4. §5.5's `Evaluate` reads the rate off whatever metrics it is handed

**Severity: medium.** The reference computes `complaints / inboxed`
unconditionally. In practice the inputs come from several places — Postmaster
Tools, feedback loops, locally-counted unsubscribes — and only one of them is
the number Google is using.

Computing a rate from local data produces a figure with the right units, the
wrong value, and an error in the reassuring direction.

**What we do.** `Metrics` carries a `Source`, and `ComplaintRate` returns
`ok=false` for anything that is not `SourcePostmaster` rather than a
plausible wrong number. `Evaluate` reports "insufficient or non-authoritative
data" instead of "continue".

`BounceRate` deliberately uses `Sent` as its denominator, because a bounce is
something the sender observes directly and does not have the same problem.

## 5. §5.5 checks complaints before bounces

The reference evaluates the complaint rate and returns; bounces are mentioned
afterwards as something to "also monitor".

**What we do.** Bounces are evaluated first. High bounces usually mean list
quality problems that will produce complaints next, and halting on the
earlier signal is the point of having two.

## 6. `ComplaintsAllowed` must floor, not round

Not a defect in the document, but the obvious implementation rounds, and at
these volumes rounding is the difference between a correct answer and a
dangerous one.

At 500 inboxed and a 0.08% threshold, the allowance is 0.4 complaints.
Rounding gives "you may have 1", which is 0.2% — already over. Flooring gives
0, which is the truth.

## 7. §6.1's `preflightSend` checks the rate limiter before the budget

The reference order is suppression, reply, meeting, jurisdiction, rate limit —
with the budget check outside, in §14.2 step 3, *after* the preflight.

The rate limiter is the only check with a **side effect**: `Allow` consumes
allowance. Checking a halted domain after the limiter has already spent a
mailbox's daily slot throws away capacity on a send that will not happen.

**What we do.** `Preflight.Check` evaluates the budget at step 6 and the rate
limiter at step 7, last. `TestBudgetIsCheckedBeforeTheLimiterConsumes`
asserts the allowance is untouched when a halted domain refuses a send.

## 8. §7.4's fix narrows the race; it does not close it

**This is the most interesting finding, and the document is honest about it**
— "the window shrinks from minutes to milliseconds". Worth measuring, because
milliseconds times thousands of sends is not zero.

**Measured** by the tests in `test/races/`. First, the control — checking only
at schedule time, which is what the product looks like without §6.1's third
checkpoint:

```
checking only at schedule time: 252 of 500 emails went out after the prospect had replied
```

With the send-time preflight but no serialisation, the residual was around
**80 in 500** for replies, **70 in 500** for unsubscribes: the preflight reads
"no reply", the reply lands, and the provider then accepts a message the
prospect has already answered.

**What we do, beyond the document.** Hold a per-enrolment row lock across the
idempotency claim, the preflight and the provider call; the reply, booking
and unsubscribe paths take the same lock before they write. In a real
deployment this is `SELECT ... FOR UPDATE` on the enrolment row, held across
the provider call — one row locked for an API round-trip, which at 40 sends
per mailbox per day is not a contention problem.

There are two lock namespaces, always taken in the order contact → enrolment,
because a global unsubscribe is keyed on the address and knows nothing about
enrolments. The contact lock is keyed on the canonical address, so Gmail
variants share one.

Result:

```
500 interleavings: 209 emails went out (the reply arrived first in the rest);
189 landed in the gap between arrival and the write committing, the widest
400.219µs; 0 after the write committed
```

The remaining 189 are not violations — the system had not been told about the
reply yet — and the widest gap is 400 microseconds rather than the fifteen
seconds §7.4 opens with. The test asserts both: zero after commit, and a
sub-millisecond arrival-to-dispatch window.

**One case is not closed, and is documented rather than papered over.** The
complaint budget breaker is domain-level and has no enrolment or contact key,
so nothing serialises a halt against an in-flight dispatch:

```
500 interleavings: 153 emails went out; ... 126 after the write committed
```

A halt landing during a provider round-trip lets that one message out.
`TestHaltBeatsQueuedSend` measures the window instead of claiming otherwise,
and asserts the guarantee that does hold: once a domain is halted, nothing
new goes out.

## 9. A real bug found by the test suite, in our own code

Recorded because it is exactly the class of mistake this product is about.

`model.CanonicalEmail` originally read the canonical form off
`GmailVariants` by position:

```go
v := GmailVariants(addr)
if len(v) > 1 { return v[1] }
return v[0]
```

`GmailVariants` skips the entry identical to the input, so for an address
that is **already canonical** the second element is the `googlemail.com`
form. That meant:

```
CanonicalEmail("first.last@gmail.com") -> "firstlast@gmail.com"
CanonicalEmail("firstlast@gmail.com")  -> "firstlast@googlemail.com"
```

Two spellings of one inbox, filed under two suppression keys. An unsubscribe
from one would not have stopped mail to the other — precisely the
circumvention `GmailVariants` exists to prevent.

Caught by `TestUnsubscribeIsGlobalNotPerSequence`. `CanonicalEmail` now
computes the key directly and `TestGmailVariantsCollapseToOneKey` checks
seven spellings against it.

## 10. Smaller notes

- **§5.2 `startOfDay`.** Truncating to 24 hours is wrong across a DST
  transition: a 23-hour day truncates into the previous day and a 25-hour day
  lands mid-morning. Built from date parts instead.
- **No bound on the forward scan.** A calendar that marks every day a holiday
  — a misconfiguration, or a country code nobody implemented — spins the
  scheduler forever. `MaxSearchDays = 400`.
- **§8.2's `OnBooked` pauses unconditionally.** A terminal enrolment must not
  be paused back into life, or a "resume" button reanimates someone who
  unsubscribed. `Store.Pause` is a no-op on terminal states.
- **§8.1 treats reply-inferred bookings as a source.** They are, but acting on
  one pauses an entire account on the strength of "how about Thursday?".
  `DetectionSource.Reliable()` gates automatic pausing; unreliable sources
  prompt the rep instead.
- **Bounce `5.2.2` is mailbox-full**: permanent by class, transient in fact.
  Classified soft, because suppressing on it permanently loses a prospect
  whose inbox was full for a week.
- **Unsubscribe tokens must never expire.** Refusing a three-year-old link
  converts an unsubscribe into a spam complaint. They are HMAC-signed rather
  than stored, so the endpoint can suppress with no database read — an
  unsubscribe that returns 500 is an unsubscribe that becomes a complaint.
- **The Postmaster CSV export's columns have moved before.** A positional
  parser that reads the DKIM rate as the spam rate produces a plausible
  number, which is the worst possible failure here. `ParseCSV` reads the
  header and refuses to guess at a missing column.

## What we did not implement

- Real Gmail API and Microsoft Graph transports. `send.Provider` is the
  interface; there is no OAuth flow and no HTTP client. The environment this
  was built in has no module proxy access, so the whole implementation is
  standard-library only.
- A durable workflow engine. `internal/store` is in-memory, with the
  atomicity boundaries a real database would have to preserve drawn
  explicitly — those boundaries are the design, and the race tests exercise
  them.
- Persistence, HTTP API surface beyond the unsubscribe endpoint, and the UI.
