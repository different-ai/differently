# Bare metal: k3s, RKE2, Talos and plain Linux hosts

Status: k3s, RKE2 and a plain Linux host running OpenSandbox are
**documented**; Talos is **unverified**. Nothing here has been run on real
hardware yet.

Differently's control plane needs no privilege or KVM, so any of these
distributions can run it. Sandboxes run on an OpenSandbox server
([ADR 0020](../adr/0020-opensandbox-replaces-node-agent.md)), installed in the
same cluster or on separate hosts. Bare metal matters most for OpenSandbox's
Fast Sandbox runtime: `/dev/kvm` comes straight from the CPU, with no nested
virtualization. Enable VT-x/AMD-V (and VT-d if you like) in firmware, and
confirm the host checks in
[prerequisites.md](prerequisites.md#kvm-hosts-fast-sandbox-only). Fast
Sandbox has not been run with Differently yet.

## Kubernetes distributions

Differently's chart installs unprivileged pods only. Each distribution
differs mainly in Pod Security Admission (PSA) defaults, which matter for
OpenSandbox's namespace rather than Differently's: its Docker runtime needs
the Docker socket, and Fast Sandbox needs `/dev/kvm` and `/dev/net/tun` on
its nodes. Label OpenSandbox's namespace as its documentation requires.

### k3s

- k3s embeds containerd 2.0 (since v1.31.6 / v1.32.2). Its NetworkPolicy
  controller enforces the chart's policies.
- **No PSA enforcement by default.**
- If you followed the k3s hardening guide, `/var/lib/rancher/k3s/server/psa.yaml`
  sets `restricted` as the cluster-wide default. OpenSandbox's namespace then
  needs a namespace label (namespace labels override the defaults) or an
  entry in its `exemptions.namespaces`.
- k3s ships RuntimeClasses for crun, nvidia and Wasm, but not Kata.
  Differently needs none of them.

```sh
kubectl create namespace differently
helm install dly deploy/helm/differently -n differently -f values.yaml
```

### RKE2

- The default PSA level is `privileged`.
- With the **CIS profile**, RKE2 enforces `restricted` and exempts only
  `kube-system`, `compliance-operator-system` and `tigera-operator`. If
  OpenSandbox's components need more, add its namespace to the exemptions in
  a custom PSA configuration file, set through the
  `pod-security-admission-config-file` option.
- How SELinux on RHEL-family hosts interacts with OpenSandbox's runtime is
  **unverified**. If you see AVC denials, check `ausearch -m avc`.
- Differently does not need containerd changes.

### Talos

- Talos is immutable and enforces PSA `baseline` by default, which blocks
  privileged pods outside `kube-system`. Differently's pods are unaffected;
  OpenSandbox's may need a namespace label.
- Host packages come from system extensions, not package installs.
- **Unverified:** whether OpenSandbox's runtimes work on Talos, and for Fast
  Sandbox whether the Talos kernel loads `kvm_intel` or `kvm_amd` and `tun`
  without extra machine-config `kernel.modules` entries, and whether Talos
  can provide an XFS image volume with reflink.
- Talos on bare metal needs firmware virtualization for Fast Sandbox. A Talos
  VM needs nested virtualization from its hypervisor.

## Register an OpenSandbox server on a plain Linux host

Scenario: the control plane already runs (on Kubernetes or elsewhere), and
you want an existing Linux server to take sandboxes without running
Kubernetes on it. You run an OpenSandbox server on that host and register it
as a pool. This is the same adapter and the same policies as an in-cluster
server.

### 1. Install OpenSandbox on the host

Follow OpenSandbox's documentation. Choose the Docker runtime for containers,
or Fast Sandbox for Firecracker microVMs if the host passes the KVM checks.
Set a strong server `api_key`, and pin the server and `execd` image versions.

[`test/opensandbox/`](../../test/opensandbox/README.md) is a working example
of the Docker-runtime shape: `compose.yaml` runs `opensandbox/server:release-1.2.0`
with the host's Docker socket, and `config.toml` sets `execd` `v1.1.0`, drops
container capabilities, sets `no_new_privileges` and a PID limit. That is the
configuration the adapter was checked against. Its API key is a test value;
never reuse it.

### 2. Open the network paths

| Path | Requirement |
|---|---|
| Control plane to host | TCP to the OpenSandbox server's port (8090 in the example). With the default `useServerProxy`, exec and file traffic also goes through this port |
| Firewall on the host | Accept the server port **only** from the control plane's egress addresses (node IPs or NAT gateway). Clients never need to reach it |
| Chart NetworkPolicy | Allow server egress to the host's address on that port: `networkPolicy.server.egress.opensandbox.cidrs` (and `port` if it is not 8090) |

- **Encryption.** Every request carries the OpenSandbox API key. Across any
  network you do not fully control, serve OpenSandbox over `https` or put the
  path inside a WireGuard or IPsec tunnel, or a private interconnect.
- **Sandbox egress** leaves through the host's networking. Differently does
  not restrict it. Configure it in OpenSandbox and the host firewall, and
  block metadata endpoints if the host is a cloud VM.

### 3. Declare the pool on the control plane

If this host is your OpenSandbox server, point the chart's `opensandbox`
block at it (preferred, because it is versioned with the deployment). Store
the host's API key in a Secret first:

```sh
kubectl -n differently create secret generic edge-linux-opensandbox --from-literal=api-key='<api_key>'
```

```yaml
opensandbox:
  apiURL: http://192.168.50.10:8090/v1
  trust: private                        # you operate the host
  isolation: container                  # microvm if the host runs Fast Sandbox
  existingSecret: edge-linux-opensandbox
  pool:
    name: edge-linux
    region: dc1
    labels: {site: dc1}
    maxConcurrency: 20
networkPolicy:
  server:
    egress:
      opensandbox:
        namespaceSelector: null         # no in-cluster server (null, not {}, removes the default)
        cidrs: ["192.168.50.10/32"]
        port: 8090
```

To add the host as a further pool next to an in-cluster server, declare it
under `config.pools` (`adapter: opensandbox`, `trust`, `config.apiURL`,
`config.isolation`, `credentials: {env: NAME}`) and expose the key to the
server as that environment variable, as the comments in `values.yaml`
describe. Register each OpenSandbox server as exactly one pool (see
[day-2.md](day-2.md#replacing-a-backend)).

Or use the admin API, which creates a pool with `source: api` that survives
restarts in PostgreSQL. With an `env:` reference, as below, the environment
variable must already be set on every server replica. Alternatively, store
the key in Differently first and use `"credentials":{"secret":"edge-osb-key"}`
([keys and providers](../guides/keys-and-providers.md)).

```sh
curl -fsS -X POST "$DIFFERENTLY_URL/v1/admin/pools" \
  -H "Authorization: Bearer $DIFFERENTLY_API_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"edge-linux","adapter":"opensandbox","trust":"private","region":"dc1",
       "config":{"apiURL":"http://192.168.50.10:8090/v1","isolation":"container"},
       "credentials":{"env":"DIFFERENTLY_EDGE_OSB_KEY"}}'
```

The pool is rejected without `trust`, without `config.apiURL`, or with an
`isolation` other than `container` or `microvm`. If someone else operates the
host, declare `trust: external`: the pool then receives work only when the
global policy approves it in `externalPools`.

### 4. Route sandboxes to the pool

Placement picks among every allowed private pool. To target this host
explicitly, narrow placement in the request:

```sh
curl -fsS -X POST "$DIFFERENTLY_URL/v1/sandboxes" -H "Authorization: Bearer $PROJECT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"profile":"small","placement":{"allowedPools":["edge-linux"]}}'
```

To pin projects to the pool, use a project policy instead: `PUT /v1/policies/project:<id>`
with `allowedPools`.

## Verify

```sh
# On the host (or from a machine allowed through its firewall)
curl -fsS -H "OPEN-SANDBOX-API-KEY: $OSB_KEY" "http://192.168.50.10:8090/v1/sandboxes?page=1&pageSize=1"

# From an operator workstation
differently admin pool get edge-linux                   # health ok
differently admin trust-report | jq '.pools[] | select(.name=="edge-linux") | {trust, isolation, endpoint, credentialRef}'
# then create a sandbox with placement.allowedPools=["edge-linux"] and exec `echo ok` (kubernetes.md, step 7)
```

To check the adapter itself against that server, run the live test from a
checkout. It creates, pauses, snapshots and deletes real sandboxes on that
server, and takes the key from `OPENSANDBOX_API_KEY` (without it, it uses the
test key from `test/opensandbox/config.toml`):

```sh
OPENSANDBOX_URL=http://192.168.50.10:8090/v1 OPENSANDBOX_API_KEY="$OSB_KEY" \
  go test -tags live -run TestLiveOpenSandbox ./internal/adapters/opensandbox/
```

For Kubernetes distributions, use the verification in
[kubernetes.md](kubernetes.md#verify).
