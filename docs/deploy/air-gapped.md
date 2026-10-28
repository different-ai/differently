# Air-gapped installation

Status: **documented**. The steps have not been run.

Differently is built to run with no internet access:

- **No telemetry.** It has no telemetry, update checks or phone-home code
  paths.
- **No third-party dashboard requests.** The dashboard is served from the
  binary with `Content-Security-Policy: default-src 'self'`, so the browser
  loads nothing from third parties.
- **No outbound calls by default.** The only outbound connections the
  control plane ever makes go to PostgreSQL, to the backends of its pools
  (OpenSandbox servers, `remote` plugins) and to external-provider APIs.
  Provider pools are disabled by default.

OpenSandbox is a separate product with its own images and configuration.
Whether it runs fully offline, and which of its components reach the
network, is for its documentation to answer; it has not been checked here.

## 1. Mirror the images

| Image | Needed for |
|---|---|
| `ghcr.io/different-ai/differently-server:0.1.0` | Control plane, migrations, preflight |
| `ghcr.io/different-ai/differently-cli:0.1.0` | `helm test` only |
| `docker.io/library/postgres:17` | Only with the evaluation-only `postgresql.embedded` |
| `docker.io/library/busybox:1.37` | Only for the `kubectl debug` checks in these guides |
| OpenSandbox's server, `execd` and other component images | Your OpenSandbox installation; see step 3 |
| The sandbox images your pools create sandboxes from | Pulled by OpenSandbox's runtime, not by Differently |

Copy by digest so the internal tags point at exactly what you reviewed. Use
`--all` (skopeo) or `crane copy` so multi-architecture indexes come along:

```sh
REG=registry.internal.example.com
for img in differently-server differently-cli; do
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
helm package deploy/helm/differently                 # differently-0.2.0.tgz
helm push differently-0.2.0.tgz oci://$REG/charts    # or copy the .tgz across the gap
helm show chart oci://$REG/charts/differently --version 0.2.0
```

## 3. Mirror and configure OpenSandbox

Install OpenSandbox from its own chart or manifests, mirrored the same way,
following its documentation. Point every image it uses at the internal
registry. In the configuration the adapter was checked against
(`test/opensandbox/config.toml`), those are at least the server image, the
`execd` image (`[runtime] execd_image`) and the egress image
(`[egress] image`); a Fast Sandbox installation adds its own components and
templates. Pin the same versions you tested.

Sandboxes are created from an image URI (or a Fast Sandbox template). The
pool's default image is `ubuntu:24.04` unless you set `opensandbox.image`
(or a Fast Sandbox `opensandbox.templateId`), or the request names a
template. Mirror those images and use internal URIs,
or creation fails when OpenSandbox's runtime cannot pull them.

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
opensandbox:
  apiURL: http://opensandbox-server.opensandbox.svc:8090/v1
  trust: private
  isolation: container
  image: registry.internal.example.com/library/ubuntu:24.04   # mirrored sandbox image
  existingSecret: opensandbox-api-key
providers:
  daytona: {enabled: false}
  freestyle: {enabled: false}
networkPolicy:
  enabled: true
  server:
    egress:
      restricted: true       # DNS, the database and OpenSandbox only
      databaseCIDRs: ["10.20.0.0/24"]
```

```sh
helm install dly oci://$REG/charts/differently --version 0.2.0 -n differently -f values-airgap.yaml
```

With providers disabled, the server NetworkPolicy has no TCP 443 egress rule;
its OpenSandbox rule (`networkPolicy.server.egress.opensandbox`) allows only
the OpenSandbox namespace (or `cidrs`) on port 8090.
Even a misconfigured provider pool could not reach the internet from the
server pods, as long as the CNI enforces NetworkPolicy. For defence in depth,
add a namespace-wide default-deny egress policy as well.

Sandboxes are a separate matter. Their egress goes through OpenSandbox's
runtime and the network under it, not through Differently, which does not
configure OpenSandbox's network policy. Close it with OpenSandbox's own
egress settings, NetworkPolicy in OpenSandbox's namespace, or node and VPC
firewalling if sandboxes must not reach the internet.

## Verify

```sh
# 1. Every image comes from the internal registry (repeat for OpenSandbox's namespace).
kubectl -n differently get pods -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{range .spec.initContainers[*]}{.image}{"\n"}{end}{end}' | sort -u

# 2. The server reports no telemetry, no approved external pools, and no external pool at all.
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/trust-report" \
  | jq '{telemetry, external: .globalPolicy.external, pools: [.pools[] | {name, trust, endpoint, approvedForExternalPlacement}]}'
#   "telemetry": "none: Differently has no telemetry or phone-home code paths"
#   every pool has "trust": "private", and the opensandbox endpoint is an internal address

# 3. The server's egress is restricted to DNS, the database and OpenSandbox.
kubectl -n differently get networkpolicy dly-differently-server -o yaml | sed -n '/egress:/,$p'

# 4. Everything still works with egress locked down.
helm test dly -n differently --logs
```

For an independent check, watch flows from the server pods with your CNI's
flow logs (for example Cilium Hubble, or VPC flow logs). Expect only DNS,
PostgreSQL and the OpenSandbox server's port (8090 by default).
