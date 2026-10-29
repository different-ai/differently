# ADR 0010: Apache-2.0 licence

- Status: Accepted
- Date: 2026-10-28

## Context

Differently is meant to be run by organizations on their own infrastructure,
embedded in their platforms, and extended with community adapters. Its
building blocks are permissively licensed: Firecracker, Kata Containers, Cloud
Hypervisor, KubeVirt, containerd, Kueue and CloudNativePG are Apache-2.0
(Cloud Hypervisor is Apache-2.0/BSD-3-Clause). Comparable open-source sandbox
platforms (E2B runtime, OpenSandbox, CubeSandbox, agent-sandbox) are also
Apache-2.0. Daytona's former open-source code was AGPL-3.0 and is now archived.

## Decision

License the whole repository under Apache-2.0, including the server, agent,
guest, CLI, SDKs, Helm chart and Terraform. The OpenAPI document declares it
(`api/openapi.yaml` `info.license`).

## Consequences

- Organizations can run, modify and embed Differently without copyleft
  obligations, and the explicit patent grant reassures legal reviews.
- Competitors can fork it; the project's value has to come from policy-driven
  placement, operational honesty and community, not licence terms.
- Third-party material must be compatible. In particular Boat's public OpenAPI
  document has no stated licence and must not be vendored
  ([ADR 0013](0013-no-boat-compatible-facade-in-mvp.md)); Daytona and Freestyle
  adapters are written from their published API descriptions, not copied
  client code.
- Dependencies must stay Apache-2.0-compatible (today: pgx, Prometheus client,
  sigs.k8s.io/yaml, cobra, and their transitive dependencies). A licence check belongs in CI.

## Alternatives considered

- **AGPL-3.0.** Protects against hosted forks but deters the embedding and
  enterprise adoption the project is aiming for.
- **Business Source Licence or similar.** Not open source; contradicts the
  self-hosted, no-vendor promise.
- **MIT.** Permissive but without an explicit patent grant.
