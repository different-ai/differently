# Quickstart

Two ways to try Differently: a laptop with no isolation (minutes), or a
Kubernetes cluster with an OpenSandbox server as the sandbox runtime (the
real thing).

## 1. Laptop (dev mode)

Dev mode runs an in-memory store and a `devhost` pool. Commands run **directly
on your machine as your user** — never point it at untrusted code.

```bash
git clone https://github.com/different-ai/differently && cd differently
make build
./bin/differently-server --dev
```

The server writes an operator token to `~/.differently/dev/token`, creates an
organization `org_default` with project `prj_default`, and serves:

- API: `http://localhost:8080/v1/...` ([OpenAPI](../../api/openapi.yaml))
- Dashboard: `http://localhost:8080/dashboard/`
- Metrics: `http://localhost:8080/metrics`

In a second terminal:

```bash
./bin/differently doctor
ID=$(./bin/differently sandbox create --profile small --wait -o json | jq -r .id)
./bin/differently sandbox exec "$ID" -- 'echo "hello from $(hostname)"'
./bin/differently sandbox files put "$ID" ./go.mod /work/go.mod
./bin/differently sandbox files cat "$ID" /work/go.mod
FORK=$(./bin/differently sandbox fork "$ID" -o json | jq -r .id)
./bin/differently sandbox placement "$ID"
./bin/differently sandbox delete "$ID" "$FORK"
./bin/differently admin verify-audit
```

Ask for something no pool provides and you get a precise error instead of a
sandbox that never starts:

```bash
./bin/differently sandbox create --capability gpu
# error [unsupported_capability]: no permitted pool supports the requested capabilities
```

## 2. Kubernetes with OpenSandbox

Differently does not run sandboxes itself. Install an
[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) server first,
with its own chart or manifests, and choose its runtime: Docker or
Kubernetes containers, or Fast Sandbox microVMs on KVM nodes. Only the Docker
runtime has been run with Differently so far. Then follow
[deploy/kubernetes.md](../deploy/kubernetes.md). In short:

```bash
kubectl create namespace differently
kubectl -n differently create secret generic opensandbox-api-key \
  --from-literal=api-key='<the OpenSandbox server api_key>'
helm install differently deploy/helm/differently -n differently \
  --set opensandbox.apiURL=http://opensandbox-server.opensandbox-system.svc/v1 \
  --set opensandbox.existingSecret=opensandbox-api-key
kubectl -n differently get secret differently-tokens -o jsonpath='{.data.bootstrap-token}' | base64 -d
```

The chart registers the server as a pool named `opensandbox` with
`trust: private`. Then create a tenant and a scoped key as described in
[organization-onboarding.md](organization-onboarding.md). Pool options and
what OpenSandbox supports are in [opensandbox-pools.md](opensandbox-pools.md).

To try a real runtime on one machine instead, `test/opensandbox/compose.yaml`
starts an OpenSandbox server with the Docker runtime, and
`test/e2e/config.yaml` shows a server config with an `opensandbox` pool
pointing at one.

## 3. Use an SDK

```ts
import { Differently } from "@differently/sdk";
const differently = new Differently(); // reads DIFFERENTLY_URL and DIFFERENTLY_API_KEY
const sandbox = await differently.sandboxes.create({ profile: "small" });
for await (const ev of sandbox.execStream("npm test")) {
  if (ev.type === "stdout") process.stdout.write(ev.data);
}
await sandbox.delete();
```

```python
from differently import Differently
with Differently().sandboxes.create(profile="small") as sandbox:
    print(sandbox.exec("python3 --version").stdout)
```
