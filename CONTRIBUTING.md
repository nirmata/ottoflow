# Contributing to OttoFlow

Thanks for your interest in contributing! This is a short entry point — the
full development guide (architecture, build/test commands, code style, PR
checklist) lives in **[DEVELOPER.md](DEVELOPER.md)**.

## Before you start

- Read the [Code of Conduct](CODE_OF_CONDUCT.md).
- All dependencies are public — `go build ./...` and `go mod download` work
  without Nirmata org access. See [DEVELOPER.md](DEVELOPER.md#-status--roadmap)
  for status.

## Sign the CLA

Before your first pull request can be merged, you must accept the
[Contributor License Agreement](CLA.md). Once a CLA bot is enabled on this
repo, it will comment on your PR with a one-click link, and accepting is
recorded against your GitHub account and covers all your future
contributions. Until then, a maintainer will let you know how to record your
acceptance.

You keep ownership of your work. The CLA grants Nirmata the right to relicense
contributions, which is what lets us guarantee that every OttoFlow release
converts to Apache-2.0 on its Change Date. See the
[Licensing FAQ](LICENSING-FAQ.md).

If you contribute on behalf of an employer, make sure you have their permission
— or ask us about a Corporate CLA at
[hello@nirmata.com](mailto:hello@nirmata.com).

## Sign off your commits (DCO)

All commits must include a `Signed-off-by` line certifying you wrote the
change or otherwise have the right to submit it under the project's license —
the [Developer Certificate of Origin](https://developercertificate.org/):

```sh
git commit -s -m "Your commit message"
```

Forgot to sign off? Amend your last commit with `git commit --amend -s`
(then force-push your branch). An automated DCO check runs on every pull
request and must pass before merging.

## Development workflow

See [DEVELOPER.md](DEVELOPER.md) for build/test commands, project structure,
and the full PR checklist.

## Reporting bugs / requesting features

Use the [issue templates](.github/ISSUE_TEMPLATE/) when opening an issue.

## Reporting security issues

Do not open a public issue for a security vulnerability — see
[SECURITY.md](SECURITY.md).
