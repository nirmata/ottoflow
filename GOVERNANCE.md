# Governance

OttoFlow is a young, company-backed open-source project. This document
describes how the project actually runs today, not an aspirational model. It
will be revised as the contributor base grows.

## Who maintains the project

OttoFlow is created and maintained by [Nirmata, Inc.](https://nirmata.com).
Nirmata engineers are the primary maintainers, review and merge pull requests,
and are responsible for releases. There is no separate steering committee,
maintainer election process, or formal voting body at this stage — decisions
are made by the Nirmata engineering team responsible for the project.

Per-path code ownership is tracked in [`.github/CODEOWNERS`](.github/CODEOWNERS).
That file is intentionally sparse today; it will be filled in with named
owners as the maintainer group grows, rather than auto-populated from commit
history.

## How decisions get made

- Day-to-day technical decisions (design, code review, what merges) are made
  by Nirmata maintainers through normal GitHub pull request review.
- Larger or user-facing changes should be raised as a GitHub issue first so
  they can be discussed before significant implementation work starts.
- Licensing and legal questions (including anything about the
  [Business Source License](LICENSE.md) grant) are decided by Nirmata and
  routed through [hello@nirmata.com](mailto:hello@nirmata.com) — they
  are not something a pull request review can resolve.
- There is no formal RFC process or vote today. As the project and external
  contributor base grow, this document will be updated to describe one.

## How to become a contributor

Anyone can contribute:

1. Read [CONTRIBUTING.md](CONTRIBUTING.md) and the
   [Code of Conduct](CODE_OF_CONDUCT.md).
2. Open an issue or pick up an existing one, and open a pull request.
3. Sign off your commits (DCO) and accept the [CLA](CLA.md) as described in
   CONTRIBUTING.md.
4. Get your change reviewed and merged by a maintainer.

There is currently no separate "become a maintainer" ladder or nomination
process. Consistent, high-quality contributions are the way to build trust
with the maintainer team; if and when the project takes on outside
maintainers, that process will be documented here rather than handled
informally.

## How to escalate

- **Technical disagreements or stuck pull requests / issues:** ping a
  maintainer on the issue or PR. If that doesn't resolve it, email
  [support@nirmata.com](mailto:support@nirmata.com).
- **Code of Conduct concerns:** report to
  [support@nirmata.com](mailto:support@nirmata.com) as described in
  [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). There is no separate CoC committee
  today — reports go to Nirmata, who are responsible for reviewing and acting
  on them.
- **Security vulnerabilities:** follow [SECURITY.md](SECURITY.md); do not open
  a public issue.
- **Licensing questions:** [hello@nirmata.com](mailto:hello@nirmata.com).

## Changing this document

This document is maintained the same way as the rest of the project: propose
changes via a pull request, reviewed by a Nirmata maintainer.
