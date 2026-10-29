# Outbound Sequencer

[![CI](https://github.com/syedjafri06193/Outbound-Sequencer-proj/actions/workflows/ci.yml/badge.svg)](https://github.com/syedjafri06193/Outbound-Sequencer-proj/actions/workflows/ci.yml)

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
├── 1—Documentation/   ← all documentation (index in its README)
│   ├── design.md      ← full design guide (the spec code comments cite)
│   ├── design.pdf     ← same guide, PDF
│   └── v1/            deliverability, legal, halt runbook, spec errata
└── v1/                ← first implementation
    ├── cmd/           engine, postmaster-sync, replywatcher
    ├── internal/      deliverability, governance, jurisdiction, meeting, model,
    │                  ratelimit, reply, send, sequence, store, suppression, unsubscribe
    └── test/          race tests + Postmaster fixture
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

## Documentation

All documentation lives in [`1—Documentation/`](1—Documentation/README.md) — start with its index.

<!-- project-metrics:start -->
## Project Metrics

[![Stars](https://img.shields.io/github/stars/syedjafri06193/Outbound-Sequencer-proj?style=flat-square)](https://github.com/syedjafri06193/Outbound-Sequencer-proj/stargazers) [![Forks](https://img.shields.io/github/forks/syedjafri06193/Outbound-Sequencer-proj?style=flat-square)](https://github.com/syedjafri06193/Outbound-Sequencer-proj/forks) [![Open issues](https://img.shields.io/github/issues/syedjafri06193/Outbound-Sequencer-proj?style=flat-square)](https://github.com/syedjafri06193/Outbound-Sequencer-proj/issues) [![Last commit](https://img.shields.io/github/last-commit/syedjafri06193/Outbound-Sequencer-proj/improvements?style=flat-square)](https://github.com/syedjafri06193/Outbound-Sequencer-proj/commits/improvements) [![Commits](https://img.shields.io/github/commit-activity/t/syedjafri06193/Outbound-Sequencer-proj?style=flat-square)](https://github.com/syedjafri06193/Outbound-Sequencer-proj/commits/improvements) [![Top language](https://img.shields.io/github/languages/top/syedjafri06193/Outbound-Sequencer-proj?style=flat-square)](https://github.com/syedjafri06193/Outbound-Sequencer-proj) [![Repo size](https://img.shields.io/github/repo-size/syedjafri06193/Outbound-Sequencer-proj?style=flat-square)](https://github.com/syedjafri06193/Outbound-Sequencer-proj)

### Code

| Metric | Value |
|---|---|
| Lines of code | 6,209 |
| Source files | 36 |
| Tracked files | 50 |
| Documentation | 9 Markdown files (15,913 words), 1 PDF |
| Commits | 8 |
| Contributors | 1 |
| Active | 2026-09-11 → 2026-10-28 |

| Language | Lines | Share |
|---|---:|---:|
| Go | 6,207 | 100.0% |
| Modula-2 | 2 | 0.0% |

### Performance

| Metric | Result |
|---|---|
| Emails sent after prospect replied (500 trials) | 252 → ~80 → **0** |
| Widest race window with enrolment lock | 400 µs |

<sub>Measured figures recorded in [`v1/README.md`](v1/README.md).</sub>

<sub>Code stats as of 2026-10-28, counted with pygount (comments and blank lines excluded). Badges update live.</sub>
<!-- project-metrics:end -->
