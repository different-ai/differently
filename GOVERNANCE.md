# Governance

Differently is maintained by the OpenWork team at
[github.com/different-ai](https://github.com/different-ai) and is open to
outside contributors.

## Roles

- **Contributors** open issues and pull requests.
- **Committers** have write access and may merge into `pre-release` once CI
  passes. Committers are invited after sustained, high-quality contributions.
- **Maintainers** cut releases (merge `pre-release` into `main`), manage
  security advisories, and decide on significant technical changes when consensus is not reached.

## Decisions

Day-to-day decisions happen in pull requests. Significant technical changes
are proposed by PR, with the design described in `docs/architecture/`, and
accepted by lazy consensus after at least five business days, or by a
maintainer decision when consensus is not reached.

## Commitments

- The open-source edition must remain fully functional on its own: no hosted
  license server, OpenWork account or proprietary coordination service will
  ever be required to run a self-hosted installation.
- The adapter protocol and public API are versioned; breaking changes go
  through a documented proposal and a deprecation period after 1.0.

## Code of conduct

Participation is governed by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
