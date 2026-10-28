# Air-gapped installation

Status: **documented**. The steps have not been run.

Differently is built to run with no internet access:

- **No telemetry.** It has no telemetry, update checks or phone-home code
  paths.
- **No third-party dashboard requests.** The dashboard is served from the
  binary with `Content-Security-Policy: default-src 'self'`, so the browser
  loads nothing from third parties.
- **No outbound calls by default.** The only outbound connections the
  control plane ever makes go to PostgreSQL, to node agents and to
  external-provider APIs. Provider pools are disabled by default.

## 1. Mirror the images

| Image | Needed for |
|---|---|
| `ghcr.io/different-ai/differently-server:0.1.0` | Control plane, migrations, preflight |
| `ghcr.io/different-ai/differently-agent:0.1.0` | Node agents (includes Firecracker, per the chart's defaults) |
| `ghcr.io/different-ai/differently-cli:0.1.0` | `helm test` only |
| your guest-images image (`deploy/images/package-oci.sh`) | Only with `agent.guestImages.enabled` |
| `docker.io/library/postgres:17` | Only with the evaluation-only `postgresql.embedded` |
| `docker.io/library/busybox:1.37` | Only for the `kubectl debug` checks in these guides |

Copy by digest so the internal tags point at exactly what you reviewed. Use
`--all` (skopeo) or `crane copy` so multi-architecture indexes come along:

```sh
REG=registry.internal.example.com
for img in differently-server differently-agent differently-cli; do
  skopeo copy --all docker://ghcr.io/different-ai/$img:0.1.0 docker://$REG/different-ai/$img:0.1.0
done
skopeo inspect docker://$REG/different-ai/differently-server:0.1.0 | jq -r .Digest
```

`global.imageRegistry` replaces only the registry part of each image, so keep
the repository paths (`different-ai/differently-server`) in the mirror, or
override `*.image.repository` per component. Pin with `*.image.digest` in
production.

## 2. Mirror the chart

```sh
helm package deploy/helm/differently                 # differently-0.1.0.tgz
helm push differently-0.1.0.tgz oci://$REG/charts    # or copy the .tgz across the gap
helm show chart oci://$REG/charts/differently --version 0.1.0
```

## 3. Build guest images outside, carry them in

The kernel and rootfs builds download from `cdn.kernel.org`,
`raw.githubusercontent.com`, Debian mirrors and your base image registry.
Run them on a connected build host:

```sh
deploy/images/build-kernel.sh --version 6.1.<patch> --out out/
deploy/images/build-rootfs.sh --image <base-image> --guest-binary ./differently-guest --output out/default.ext4
(cd out && sha256sum vmlinux *.ext4 > SHA256SUMS)
```

Carry `out/` across the gap and check `SHA256SUMS`. Then either package it as
an OCI image into the internal registry (`package-oci.sh --tag $REG/...`) or
stage it on nodes. For systemd hosts, carry the official Firecracker release
tarball together with its published SHA-256, and pass `--firecracker-tarball`
and `--firecracker-sha256` to `install.sh`.

## 4. Install with no external egress

```yaml
# values-airgap.yaml
global:
  imageRegistry: registry.internal.example.com
  imagePullSecrets: [{name: internal-registry}]
config:
  store: {driver: postgres}
  policy:
    external: never          # the default; stated explicitly here
postgresql:
  external: {existingSecret: differently-database}
auth:
  existingSecret: differently-tokens
providers:
  daytona: {enabled: false}
  freestyle: {enabled: false}
networkPolicy:
  enabled: true
  server:
    egress:
      restricted: true       # DNS, agents and the database only
      databaseCIDRs: ["10.20.0.0/24"]
agent:
  guestImages:
    enabled: true
    image:
      repository: differently/guest-images
      tag: "2026-10-28"
```

```sh
helm install dly oci://$REG/charts/differently --version 0.1.0 -n differently -f values-airgap.yaml
```

With providers disabled, the server NetworkPolicy has no TCP 443 egress rule.
Even a misconfigured provider pool could not reach the internet from the
server pods, as long as the CNI enforces NetworkPolicy. For defence in depth,
add a namespace-wide default-deny egress policy as well.

Sandboxes are a separate matter. Their egress goes through the KVM node's
networking, not the server's, so close it with node or VPC firewalling if
sandboxes must not reach the internet.

## Verify

```sh
# 1. Every image comes from the internal registry.
kubectl -n differently get pods -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{range .spec.initContainers[*]}{.image}{"\n"}{end}{end}' | sort -u

# 2. The server reports no telemetry, no approved external pools, and no external pool at all.
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/trust-report" \
  | jq '{telemetry, external: .globalPolicy.external, pools: [.pools[] | {name, trust, endpoint, approvedForExternalPlacement}]}'
#   "telemetry": "none: Differently has no telemetry or phone-home code paths"
#   every pool has "trust": "private"

# 3. The server's egress is restricted to DNS, agents and the database.
kubectl -n differently get networkpolicy dly-differently-server -o yaml | sed -n '/egress:/,$p'

# 4. Everything still works with egress locked down.
helm test dly -n differently --logs
```

For an independent check, watch flows from the server pods with your CNI's
flow logs (for example Cilium Hubble, or VPC flow logs). Expect only DNS,
PostgreSQL and agent port 9090.
