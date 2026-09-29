# Outbound Sequencer

A multichannel outbound sequencing engine in Go with deliverability
guardrails, reply detection, and automatic pause when a meeting is booked.

**The guardrails are the product.** The complaint budget, warmup schedule,
jurisdiction gate, suppression list and send-time preflight all exist to
*refuse* a send, because a sequencer optimised for volume burns its user's
domain.

```
 prospects ──► scheduler (backpressure) ──► send-time preflight ──► dispatch
                                               │  refuses if:
                                               ├─ suppressed / unsubscribed (RFC 8058 one-click)
                                               ├─ jurisdiction gate fails
                                               ├─ over mailbox / domain / account rate limits
                                               ├─ complaint budget spent or warmup not done (Postmaster data)
                                               └─ reply received or meeting booked  ──► sequence paused
```

## Quick start

Needs Go 1.24. Standard library only, no dependencies:

```bash
cd v1
go test ./... -race                                   # 264 tests, 12 packages
go run ./cmd/engine -prospects 2000 -steps 4 -days 21 -mailboxes 1
go run ./cmd/postmaster-sync -csv test/fixtures/postmaster-spiral.csv -domain get-ours.example
```

CI runs gofmt, vet and the race-enabled tests on every push
([`.github/workflows/ci.yml`](.github/workflows/ci.yml)).

## Repository layout

```
.
├── README.md          ← you are here
├── .github/workflows/ ← CI: gofmt, vet, tests with -race
├── docs/
│   ├── design.md      ← full design guide (the spec code comments cite)
│   └── design.pdf     ← same guide, PDF
└── v1/                ← first implementation
    ├── cmd/           engine, postmaster-sync, replywatcher
    ├── internal/      deliverability, governance, jurisdiction, meeting, model,
    │                  ratelimit, reply, send, sequence, store, suppression, unsubscribe
    ├── test/          race tests + Postmaster fixture
    └── docs/          deliverability, legal, halt runbook, spec errata
```

Each `vN/` directory is a self-contained iteration. Start with
[`v1/README.md`](v1/README.md).

> Legal constraints on email, SMS and telephony shape the architecture; the
> docs describe them but are **not legal advice**.

## Versions and feedback

| Version | Summary | Feedback |
|---|---|---|
| [v1](v1/) | Guardrail-first Go engine: complaint budget, warmup, jurisdiction gate, suppression, preflight, reply/meeting pause | — |

Add a row per version as new iterations land.
