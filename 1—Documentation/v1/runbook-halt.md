# Runbook: the complaint budget breaker has tripped

A domain is halted. Nothing is going out from it. That is the system working.

Your job is not to get sending again quickly. Your job is to find out what
changed, because an automatic resume means the same behaviour resumes and
the spiral continues.

---

## 0. Before anything else

**Do not lift the halt to "see if it clears".** It will not clear. Complaint
rates are recalculated daily against a denominator that is already shrinking,
and resuming at the previous volume is how a recoverable problem becomes
permanent SMTP rejection.

## 1. Read what the breaker said

```go
for _, h := range breaker.HaltedDomains() {
    fmt.Println(h.DomainID, h.HaltedAt, h.Reason)
}
```

The reason is the **first** one recorded and is never overwritten by later
observations. That matters: by the time you look, the numbers will have moved,
and you want the state at the moment of the decision.

Two shapes:

- **`complaint rate X% at or above halt threshold 0.080%`** — go to §2.
- **`bounce rate X% at or above ...`** — go to §5. Bounces are checked first
  because high bounces usually mean list quality problems that produce
  complaints next.

## 2. Is it the numerator or the denominator?

This is the question, and getting it wrong sends you looking in the wrong
place for a week.

```
go run ./cmd/postmaster-sync -csv <your-export.csv> -domain <domain>
```

Read the `INBOXED` column, not the `RATE` column.

**If complaints rose and inboxed held steady** — the numerator moved. Something
about the sending changed: a new sequence, new copy, a new list, a new rep.
Go to §3.

**If complaints held steady and inboxed fell** — the denominator moved. This
is the death spiral. Nothing about your sending got worse; filtering grew and
the same behaviour now reads as a higher rate. Go to §4.

`AnalyseTrend` names this shape explicitly if it sees it:

> denominator collapse over 6 days: inboxed volume fell to 24% of where it
> started (9000 to 2200) while complaints went from 2 to 2.

## 3. The numerator moved

Find what is new. In rough order of likelihood:

1. **A new list.** Check `Store.SkipsByReason()` for a rise in `suppression`
   at import time, and the bounce rate for the same period. A bought list
   shows up in both.
2. **A new sequence or new copy.** Which sequences sent to this domain in the
   window? A single badly-worded first touch can carry a domain.
3. **A new rep.** `Ledger.BusiestAccounts()` shows touches per account with
   the rep count. Four reps on one account is a self-inflicted complaint.
4. **Targeting.** Every problem here gets easier with better targeting.
   Triggering on real signals rather than a bought list is the difference
   between 0.05% and 0.5%.

Fix the cause. Then §6.

## 4. The denominator moved

Filtering grew. Find out why mail stopped reaching inboxes.

1. **Authentication.** Check the DKIM, SPF and DMARC pass rates in the
   Postmaster export. `postmaster-sync` prints a line for anything under 95%.
   A DNS change, an expired key or a new sending IP will show here first.
2. **Domain and IP reputation.** "Medium" already counts as degraded. By the
   time Google says "low", mail is going to spam.
3. **Blocklists.** Check the sending domain and the **tracking domain**
   separately. A shared tracking domain on a blocklist takes you with it,
   which is why each outbound domain needs its own.
4. **Volume shape.** Did volume jump? A step change in daily volume looks
   like a compromise even when it is a new hire.
5. **Content.** Link shorteners, image-heavy templates, attachment-like
   markup, and a tracking domain that does not match the sending domain all
   push mail toward spam.

## 5. Bounces

Above 1% is a warning, above 2% is critical.

- **Hard bounces** mean the addresses do not exist: a stale or bought list.
  These are suppressed permanently on sight.
- **Soft bounces** are transient and retried with backoff, suppressed after
  five consecutive failures.
- Note that `5.2.2` (mailbox full) is permanent by class and transient in
  fact; the classifier treats it as soft, because suppressing on it loses a
  real prospect whose inbox was full for a week.

A high hard-bounce rate is a list problem, and a list that bounces will also
complain. Verify the list before you resume, not after.

## 6. Resuming

```go
err := breaker.Acknowledge(domainID, "jane@ours.example", "Bought list from the Feb import; removed 3,400 contacts and disabled the sequence. Auth was clean.")
```

An operator name and a note are both required; an empty one is refused. The
note is not paperwork — it is what the next person reads when this happens
again in four months.

**Resume comes back throttled, not at full speed.** `Breaker.ThrottleFactor`
returns a fraction, and the backpressure planner applies it. Expect to spend
several days back at reduced volume.

## 7. If the domain is already burnt

Sometimes the answer is that the domain does not recover on a useful
timescale.

- Stand up a new outbound domain — a **separate registrable domain**, not a
  subdomain, with its own SPF, DKIM, DMARC, tracking domain and a real
  website.
- Warm it from zero. Twenty-eight days. The engine enforces this and will
  return a cap of 0 for a mailbox whose warmup has not started.
- Do not move the same list to the new domain. The list is why you are here.

## What must never happen

- **Never send from the primary domain**, not even temporarily, not even
  "just for the important ones". If outbound burns `yourcompany.com`, your
  invoices and password resets stop being delivered. The registry refuses it
  as a validation error and that refusal should not be worked around.
- **Never raise the per-mailbox cap to catch up** on a backlog. Add
  mailboxes. `ratelimit.Limits.Validate()` refuses a cap above 100/day.
- **Never let a domain cross 5,000/day to Gmail** to clear a queue. That
  classification is permanent.
- **Never compress the queue to fit the window.** Always spill to tomorrow.
  Sequence timing is a preference; reputation is the business.
