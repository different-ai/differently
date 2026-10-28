# Quickstart

Two ways to try Differently: a laptop with no isolation (minutes), or a
Kubernetes cluster with KVM nodes (the real thing).

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

## 2. Kubernetes with Firecracker nodes

Follow [deploy/kubernetes.md](../deploy/kubernetes.md). In short:

```bash
kubectl label node <kvm-node> differently.dev/kvm=true
helm install differently deploy/helm/differently -n differently --create-namespace
kubectl -n differently get secret differently-tokens -o jsonpath='{.data.bootstrap-token}' | base64 -d
```

Then create a tenant and a scoped key as described in
[organization-onboarding.md](organization-onboarding.md).

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
