# Security Policy

## Supported Versions

OttoFlow follows [semantic versioning](https://semver.org/). Security fixes are
backported to the latest minor release; older releases are not supported.

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, report it privately via [GitHub Security Advisories](https://github.com/nirmata/ottoflow/security/advisories/new).

Include, if possible:

- A description of the vulnerability and its potential impact
- Steps to reproduce
- Affected version(s)

We aim to acknowledge new reports within 5 business days and to keep you
informed as we investigate and prepare a fix. Once a fix is available, we'll
coordinate disclosure timing with you and credit reporters (unless anonymity is
requested) in the release notes and/or a GitHub Security Advisory.

## Supply-chain trust

Release archives publish a `checksums.txt`; verify a download before running it:

```sh
sha256sum --ignore-missing -c checksums.txt
```

Every change is scanned by [CodeQL](.github/workflows/codeql.yml) and kept current
by [Dependabot](.github/dependabot.yml).

<!-- TODO: cosign signatures, SBOMs, and SLSA provenance are not yet produced by the
     release pipeline. Add them to the supply-chain story here once the release
     workflow emits them — do not claim them until then. -->
