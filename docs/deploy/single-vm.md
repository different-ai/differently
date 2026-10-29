# One Linux VM on any cloud

The quickest way to run Differently for a team: one Ubuntu VM with
[k3s](https://k3s.io), OpenSandbox's own Helm charts (with its Kubernetes
runtime) and Differently's Helm chart, all from public release artifacts.
It works the same on Azure, AWS, Google Cloud or your own hardware; only the
command that creates the VM differs.

> **Verified 2026-10-28** on an Azure `Standard_D8s_v6`: k3s v1.36.5,
> OpenSandbox 1.2.0 and Differently `v0.1.0-alpha.1`. A sandbox ran within
> about 2 s, and the end-to-end suite passed 12/12 against this setup; with
> a snapshot registry, the ingress gateway and egress policy added
> (2026-10-29), it passed 14/14. The commands below install `v0.1.0-alpha.2`,
> which has not itself been installed on this setup yet. The AWS, Google
> Cloud and bare-metal commands are the equivalent steps, not yet run there.

This is a single-node evaluation or team setup, not a highly available one.
Sandboxes are containers that share the VM's kernel. For production, see
[Kubernetes](kubernetes.md), an external PostgreSQL, and the
[hardening checklist](../security/hardening-checklist.md).

## 1. Create the VM

Pick a size with room for the sandboxes you want: 8 vCPU and 32 GiB fits
several 2 vCPU / 4 GiB computers. Use Ubuntu 24.04, a 128 GiB disk, and SSH
open to your own IP only. Every cloud below takes the same
[cloud-init](#cloud-init) file, which installs k3s and Helm on first boot.

<details open>
<summary><b>Azure</b> (verified)</summary>

```bash
az group create -n differently -l eastus
az network nsg create -g differently -n differently-nsg
az network nsg rule create -g differently --nsg-name differently-nsg -n ssh \
  --priority 100 --access Allow --protocol Tcp --destination-port-ranges 22 \
  --source-address-prefixes "$(curl -s https://api.ipify.org)/32"
az vm create -g differently -n differently --size Standard_D8s_v6 \
  --image Canonical:ubuntu-24_04-lts:server:latest --disk-controller-type NVMe \
  --os-disk-size-gb 128 --nsg differently-nsg --public-ip-sku Standard \
  --admin-username azureuser --ssh-key-values ~/.ssh/id_ed25519.pub \
  --custom-data cloud-init.yaml
```

A new subscription may first need `az provider register -n Microsoft.Compute --wait`.
Dsv6 sizes use NVMe disks, hence `--disk-controller-type NVMe`.
</details>

<details>
<summary><b>AWS</b></summary>

```bash
AMI=$(aws ssm get-parameter --query Parameter.Value --output text \
  --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id)
aws ec2 run-instances --image-id "$AMI" --instance-type m7i.2xlarge \
  --key-name <your-key> --security-group-ids <sg-allowing-ssh-from-your-ip> \
  --block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeSize=128,VolumeType=gp3}' \
  --user-data file://cloud-init.yaml \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=differently}]'
```
</details>

<details>
<summary><b>Google Cloud</b></summary>

```bash
gcloud compute instances create differently --machine-type n2-standard-8 \
  --image-family ubuntu-2404-lts-amd64 --image-project ubuntu-os-cloud \
  --boot-disk-size 128GB --metadata-from-file user-data=cloud-init.yaml
gcloud compute firewall-rules create differently-ssh --allow tcp:22 \
  --source-ranges "$(curl -s https://api.ipify.org)/32"
```
</details>

<details>
<summary><b>Bare metal or any other host</b></summary>

Install Ubuntu 24.04, then run the `runcmd` lines from the cloud-init file
below as root.
</details>

### cloud-init

```yaml
#cloud-config
package_update: true
packages: [git, jq, curl, ca-certificates]
runcmd:
  # Single-node k3s; its bundled ingress is off because nothing is public yet.
  - curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable INSTALL_K3S_EXEC="--disable traefik --write-kubeconfig-mode 644" sh -
  - curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

Wait for it on the VM (`cloud-init status --wait`), then check
`kubectl get nodes` shows the node as `Ready`.

## 2. Install OpenSandbox

OpenSandbox runs the sandboxes; Differently only places them. Install it
from its own charts, with its Kubernetes runtime and images from Docker Hub
(its charts default to a registry in mainland China):

```bash
git clone --depth 1 -b release-1.2.0 https://github.com/opensandbox-group/OpenSandbox.git
OSB_KEY=$(openssl rand -hex 24)

cat > osb-controller.yaml <<'EOF'
controller:
  image: {repository: docker.io/opensandbox/controller, tag: release-1.2.0}
  snapshot:
    imageCommitterImage: docker.io/opensandbox/image-committer:release-1.2.0
    containerdSocketPath: /run/k3s/containerd/containerd.sock
EOF

cat > osb-server.yaml <<EOF
server:
  image: {repository: docker.io/opensandbox/server, tag: release-1.2.0}
configToml: |
  [server]
  host = "0.0.0.0"
  port = 80
  api_key = "$OSB_KEY"
  max_sandbox_timeout_seconds = 2592000 # the longest TTL OpenSandbox accepts: above any client's (OpenWork: 14 days)
  [log]
  level = "INFO"
  [runtime]
  type = "kubernetes"
  execd_image = "docker.io/opensandbox/execd:release-1.2.0"
  [kubernetes]
  kubeconfig_path = ""
  namespace = "opensandbox"
  informer_resync_seconds = 300
  informer_watch_timeout_seconds = 60
  workload_provider = "batchsandbox"
  batchsandbox_template_file = "/etc/opensandbox/example.batchsandbox-template.yaml"
  [egress]
  image = "docker.io/opensandbox/egress:release-1.2.0"
  mode = "dns+nft"
EOF

cd OpenSandbox
helm install base ./manifests/charts/base          # the CRDs; it creates its own namespaces
kubectl create namespace opensandbox
helm install opensandbox-controller ./manifests/charts/controller -n opensandbox-system -f ../osb-controller.yaml
helm install opensandbox-server ./manifests/charts/server -n opensandbox-system -f ../osb-server.yaml
kubectl -n opensandbox-system rollout status deploy --timeout=300s
cd ..
```

Don't create `opensandbox-system` yourself before installing `base`, or `base`
refuses to install. Keep `$OSB_KEY` for the next step.

## 3. Install Differently

Use the chart and images from the
[latest release](https://github.com/different-ai/differently/releases):

```bash
VERSION=0.1.0-alpha.2
curl -fsSLO https://github.com/different-ai/differently/releases/download/v$VERSION/differently-$VERSION.tgz

kubectl create namespace differently
kubectl -n differently create secret generic opensandbox-api-key --from-literal=api-key="$OSB_KEY"

cat > differently.yaml <<'EOF'
config:
  store: {driver: postgres}
postgresql:
  embedded: {enabled: true}          # evaluation only; use postgresql.external for production
opensandbox:
  apiURL: http://opensandbox-server.opensandbox-system.svc/v1
  existingSecret: opensandbox-api-key
  lineEndings: kept                  # execd release-1.2.0 keeps line endings
  # Stop/start, snapshots and fork need a snapshot registry, and port URLs the
  # ingress gateway: see docs/guides/opensandbox-pools.md (Kubernetes runtime).
  features: [exec, exec.stream, files]
networkPolicy:
  server:
    egress:
      opensandbox:
        namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: opensandbox-system}}
        port: 80
EOF

helm install differently ./differently-$VERSION.tgz -n differently -f differently.yaml --wait
kubectl -n differently get pods
```

The `apiURL` and `networkPolicy` lines match OpenSandbox's chart. Since
`v0.1.0-alpha.2` they are the chart's defaults and can be left out; they
are spelled out here so the values file also works with `v0.1.0-alpha.1`.

## 4. Create your first sandbox

Get the operator token the chart generated, and reach the API:

```bash
kubectl -n differently get secret differently-tokens \
  -o jsonpath='{.data.bootstrap-token}' | base64 -d > operator-token
kubectl -n differently port-forward svc/differently-server 8080:8080 &
```

From your laptop, tunnel to it (`ssh -L 8080:localhost:8080 <vm>`) and use
the CLI from the same release (`differently_v0.1.0-alpha.2_<os>_<arch>.tar.gz`):

```bash
export DIFFERENTLY_URL=http://localhost:8080 DIFFERENTLY_API_KEY="$(cat operator-token)"
differently doctor                                   # server, auth and pool health
differently admin org create my-team                 # an organization with a default project
differently key create dev --org <org-id> --key-project <project-id>
DIFFERENTLY_API_KEY=<the new key> differently sandbox create --profile small --wait
```

The dashboard is at <http://localhost:8080/dashboard/>; sign in with the
token. [Organization onboarding](../guides/organization-onboarding.md) covers
projects, quotas and keys.

## 5. Optional: HTTPS for your team

To give teammates or services an HTTPS address instead of a tunnel:

1. Give the VM a DNS name:
   - Azure: `az network public-ip update -g differently -n <ip-name> --dns-name <label>`
     gives `<label>.<region>.cloudapp.azure.com`;
   - elsewhere, point a record at the VM's static IP.
2. Open 443 to the addresses that need it, and 80 to everyone. Let's Encrypt's
   HTTP challenge needs port 80.
3. Run a TLS proxy in front of the API, for example Caddy on the VM
   (`sudo apt-get install -y docker.io` first if Docker isn't installed):

   ```bash
   SVC=$(kubectl -n differently get svc differently-server -o jsonpath='{.spec.clusterIP}')
   printf '<your-dns-name> {\n\treverse_proxy %s:8080\n}\n' "$SVC" > Caddyfile
   docker run -d --name caddy --restart unless-stopped --network host \
     -v caddy_data:/data -v "$PWD/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2
   ```

   Alternatively, enable the chart's `ingress` with cert-manager.

Keep the API reachable only from the networks that use it; every request
needs a Differently API key, but there's no reason to expose it widely.

## Costs and cleanup

The VM is the only cost: on the order of $0.40 an hour for 8 vCPU / 32 GiB
on demand. Stop or deallocate it when idle:

```bash
az vm deallocate -g differently -n differently      # Azure
aws ec2 stop-instances --instance-ids <id>          # AWS
gcloud compute instances stop differently           # Google Cloud
```

Delete everything with `az group delete -n differently` (or terminate the
instance and its disk). Back up the Differently secrets key first if you
stored provider keys in it ([keys and providers](../guides/keys-and-providers.md#back-up-the-secrets-key)).

## Troubleshooting

| Symptom | Fix |
|---|---|
| `helm install base` says the namespace exists | Don't create `opensandbox-system` before installing `base` |
| OpenSandbox pods `ImagePullBackOff` from a `cn-zhangjiakou` registry | Set the Docker Hub images as above |
| Create fails after 60 s with `POD_READY_TIMEOUT` | The first pull of a large image was slow. Pre-pull it on the node (`sudo k3s ctr images pull <image>`); Differently retries it meanwhile |
| The pool shows unhealthy | Check `apiURL`, the API key Secret, and that `networkPolicy.server.egress.opensandbox` allows `opensandbox-system` on port 80 |
| Stop/start or snapshots are refused | The Kubernetes runtime needs OpenSandbox's snapshot registry (`controller.snapshot.registry`); leave them out of `features` until it has one ([setup](../guides/opensandbox-pools.md#kubernetes-runtime-snapshot-registry-signed-ports-and-egress)) |

More in [OpenSandbox pools](../guides/opensandbox-pools.md) and
[day-2 operations](day-2.md).
