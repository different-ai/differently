# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through
[GitHub Security Advisories](https://github.com/different-ai/differently/security/advisories/new)
("Report a vulnerability"). Do not open a public issue.

Include the affected version or commit, a description, reproduction steps and
the impact you expect. We aim to acknowledge reports within 3 business days and
to agree on a disclosure timeline with you; we credit reporters who want credit.

## Supported versions

Differently is pre-release. Until 1.0, only the latest release candidate and
the `pre-release` branch receive security fixes.

## Scope

In scope: the control plane, node agent, guest agent, adapters, CLI, SDKs,
Helm chart and image build scripts in this repository.

Out of scope: vulnerabilities in Firecracker, the Linux kernel, Kubernetes or
external providers themselves (report those upstream), and issues that require
an attacker who already controls the infrastructure Differently runs on — see
the residual risks in [docs/security/trust-model.md](docs/security/trust-model.md).

## Hardening

Operators should follow [docs/security/hardening-checklist.md](docs/security/hardening-checklist.md).
