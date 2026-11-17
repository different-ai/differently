// The data behind /compare/: one record per sandbox provider.
//
// Where it comes from:
//   - Features and the Differently status: docs/providers/*.md, which name the
//     API pages read and the date. Keep the two in step.
//   - Prices: each provider's public pricing page or, where marked, a
//     third-party comparison, read on PRICES_READ. They are list prices.
//   - Compliance: what the provider states on its own security or trust page.
//
// Rules for editing: never state a figure that was not read from a source, mark
// what is unconfirmed, and use null for "not collected" rather than guessing.

export const PRICES_READ = '2026-11-16';
export const PRICES_READ_LABEL = '16 November 2026';

/** How a provider relates to Differently today. */
export type Support = 'verified' | 'experimental' | 'plugin' | 'unassessed';

/** yes: the provider states it. unconfirmed: claimed elsewhere, or true of the wider platform only. none: nothing found. own: your own audit scope. unchecked: not looked at. */
export type Evidence = 'yes' | 'unconfirmed' | 'none' | 'own' | 'unchecked';

/** wall: billed while running. usage: billed for CPU actually used. tier: bundled sizes. infra: your own infrastructure. */
export type Billing = 'wall' | 'usage' | 'tier' | 'infra';

export interface Provider {
  id: string;
  name: string;
  /** File in public/providers/. */
  logo: string;
  url: string;
  kind: 'Hosted service' | 'Open-source runtime' | 'In your cloud account';
  support: Support;
  /** Page under /docs/providers/. */
  docs?: string;
  supportNote: string;
  isolation: string;

  billing: Billing;
  cpu: string | null;
  memory: string | null;
  /** List price of one running hour of 1 vCPU and 2 GiB: [low] or [low, high]. For usage billing, low is 25% busy and high is 100% busy. */
  slot: [number] | [number, number] | null;
  slotNote?: string;
  fees: string;
  priceSource: string;

  keepsState: string | null;
  snapshots: string | null;
  ports: string | null;
  maxSize: string | null;
  runItYourself: string;

  soc2: Evidence;
  hipaa: Evidence;
  iso: Evidence;
  complianceNote: string;

  /** One or two sentences for the detail card. */
  note: string;
}

const own: Pick<Provider, 'soc2' | 'hipaa' | 'iso'> = { soc2: 'own', hipaa: 'own', iso: 'own' };
const unchecked: Pick<Provider, 'soc2' | 'hipaa' | 'iso'> = { soc2: 'unchecked', hipaa: 'unchecked', iso: 'unchecked' };

export const providers: Provider[] = [
  {
    id: 'opensandbox', name: 'OpenSandbox', logo: 'opensandbox.png', url: 'https://github.com/opensandbox-group/OpenSandbox',
    kind: 'Open-source runtime', support: 'verified', docs: 'opensandbox',
    supportNote: 'In every build. Verified on the Docker and Kubernetes runtimes; Fast Sandbox microVMs ran live on Azure.',
    isolation: 'Containers, or Firecracker microVMs with Fast Sandbox',
    billing: 'infra', cpu: null, memory: null, slot: null, fees: 'Your own nodes', priceSource: '',
    keepsState: 'Pause keeps memory; stop/start on some runtimes', snapshots: 'Disk, fork', ports: 'Signed URLs', maxSize: 'Your nodes',
    runItYourself: 'Yes, Apache-2.0', ...own, complianceNote: 'It runs in your infrastructure, inside your own audit scope.',
    note: 'The default private runtime for Differently. You install it, choose its runtime and register it as a pool.',
  },
  {
    id: 'agent-sandbox', name: 'agent-sandbox', logo: 'kubernetes.png', url: 'https://github.com/kubernetes-sigs/agent-sandbox',
    kind: 'Open-source runtime', support: 'experimental', docs: 'agent-sandbox',
    supportNote: 'Experimental adapter. Its live test ran once, on a kind cluster with runc.',
    isolation: 'The pod RuntimeClass: runc, gVisor or Kata',
    billing: 'infra', cpu: null, memory: null, slot: null, fees: 'Your own cluster', priceSource: '',
    keepsState: 'Stop/start with a volume', snapshots: 'None', ports: 'In-cluster', maxSize: 'Your nodes and quota',
    runItYourself: 'Yes, Apache-2.0', ...own, complianceNote: 'It runs in your cluster, inside your own audit scope.',
    note: 'A Kubernetes SIG project that adds a Sandbox resource to a cluster. Isolation is whatever RuntimeClass the pod uses.',
  },
  {
    id: 'cubesandbox', name: 'CubeSandbox', logo: 'cubesandbox.png', url: 'https://github.com/TencentCloud/CubeSandbox',
    kind: 'Open-source runtime', support: 'experimental', docs: 'cubesandbox',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'KVM microVM',
    billing: 'infra', cpu: null, memory: null, slot: null, fees: 'Your own nodes', priceSource: '',
    keepsState: 'Pause keeps memory', snapshots: 'Memory, fork', ports: 'Opt-in URLs', maxSize: 'Set by the template',
    runItYourself: 'Yes, Apache-2.0', ...own, complianceNote: 'It runs in your infrastructure, inside your own audit scope.',
    note: 'Tencent\'s open-source microVM sandbox runtime, with an API in the shape of E2B\'s.',
  },
  {
    id: 'daytona', name: 'Daytona', logo: 'daytona.png', url: 'https://www.daytona.io',
    kind: 'Hosted service', support: 'verified', docs: 'daytona',
    supportNote: 'In every build. Verified against a live account (container class; the VM class has not run).',
    isolation: 'Container, or a VM class',
    billing: 'wall', cpu: '$0.0504 / vCPU-h', memory: '$0.0162 / GiB-h', slot: [0.0828], fees: '$200 starting credit. Disk $0.000108 / GiB-h after 5 GiB.', priceSource: 'https://www.daytona.io/pricing',
    keepsState: 'Stop/start; pause on VMs', snapshots: 'Disk, fork', ports: 'Signed URLs', maxSize: '4 vCPU, 8 GiB, 10 GiB by default',
    runItYourself: 'Your own runners; source closed in June 2026',
    soc2: 'yes', hipaa: 'yes', iso: 'unconfirmed', complianceNote: 'Its trust center lists SOC 2 Type II and a HIPAA attestation. ISO 27001 was listed as in progress.',
    note: 'Charges the same list rates as E2B to four decimals. A stopped sandbox bills its disk only.',
  },
  {
    id: 'freestyle', name: 'Freestyle', logo: 'freestyle.png', url: 'https://www.freestyle.sh',
    kind: 'Hosted service', support: 'verified', docs: 'freestyle',
    supportNote: 'In every build. Verified against a live account.',
    isolation: 'Hardware-virtualized microVM',
    billing: 'wall', cpu: '$0.04032 / vCPU-h', memory: '$0.0129 / GiB-h', slot: [0.0661], fees: 'Hobby $50 a month for persistent VMs.', priceSource: 'https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026',
    keepsState: 'Stop/start; pause keeps memory', snapshots: 'Memory, fork', ports: 'Public URLs', maxSize: '32 vCPU, 64 GiB, 256 GiB (by plan)',
    runItYourself: 'No', soc2: 'none', hipaa: 'none', iso: 'none', complianceNote: 'No audit report or region list found. It runs on its own bare metal.',
    note: 'A fork carries running processes with it. Runloop names it as a migration target.',
  },
  {
    id: 'e2b', name: 'E2B', logo: 'e2b.png', url: 'https://e2b.dev',
    kind: 'Hosted service', support: 'experimental', docs: 'e2b',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'Firecracker microVM',
    billing: 'wall', cpu: '$0.0504 / vCPU-h', memory: '$0.0162 / GiB-h', slot: [0.0828], fees: 'Pro $150 a month. Disk included.', priceSource: 'https://docs.e2b.dev/faq/calculate-sandbox-price',
    keepsState: 'Pause keeps memory, unbilled', snapshots: 'Memory, fork', ports: 'Public URLs, opt-in', maxSize: '8 vCPU, 8 GiB, 20 GiB (by plan)',
    runItYourself: 'Open-source runtime; managed in your AWS or GCP account',
    soc2: 'yes', hipaa: 'yes', iso: 'none', complianceNote: 'SOC 2 Type II report under NDA in its trust center. BAA on Enterprise.',
    note: 'Bills allocated resources, not use. Its protocol is also spoken by CubeSandbox and, by its own account, Novita.',
  },
  {
    id: 'boat', name: 'Boat', logo: 'boat.png', url: 'https://boat.dev',
    kind: 'Hosted service', support: 'experimental', docs: 'boat',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'Virtual machine',
    billing: 'tier', cpu: 'Bundled sizes', memory: 'Included', slot: [0.018], slotNote: 'Smallest size.', fees: '$0.018 to $0.20 an hour by size. $20 a month plan as usage credit.', priceSource: 'https://www.agent37.com/compare/boat',
    keepsState: 'Stop/start keeps the disk', snapshots: 'Disk, fork', ports: 'Token or public URLs', maxSize: '16 vCPU, 32 GB, 251 GB',
    runItYourself: 'No', soc2: 'none', hipaa: 'none', iso: 'none', complianceNote: 'Nothing found on audit reports or regions.',
    note: 'Full Ubuntu machines, so Docker and systemd work as on a normal host. Stopped sandboxes keep their state for 14 days.',
  },
  {
    id: 'morph', name: 'Morph Cloud', logo: 'morph.png', url: 'https://cloud.morph.so',
    kind: 'Hosted service', support: 'experimental', docs: 'morph',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'Virtual machine',
    billing: 'wall', cpu: '$0.05 / unit-h', memory: 'In the unit', slot: [0.05], fees: 'A compute unit is 1 vCPU, or 4 GB RAM, or 16 GB disk. Snapshot storage is extra.', priceSource: 'https://cloud.morph.so/web/subscribe',
    keepsState: 'Pause keeps memory', snapshots: 'Memory, fork', ports: 'Public URLs', maxSize: null,
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'none', iso: 'none', complianceNote: 'Its trust center lists a SOC 2 Type I report, not Type II. Regions are not disclosed.',
    note: 'Built around snapshotting a running machine and branching many copies from it.',
  },
  {
    id: 'northflank', name: 'Northflank', logo: 'northflank.png', url: 'https://northflank.com',
    kind: 'Hosted service', support: 'experimental', docs: 'northflank',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'MicroVM (Cloud Hypervisor), or gVisor',
    billing: 'wall', cpu: '$0.01667 / vCPU-h', memory: '$0.00833 / GB-h', slot: [0.0333], fees: 'Free tier up to $20 a month of resources.', priceSource: 'https://northflank.com/blog/ai-sandbox-pricing',
    keepsState: 'None', snapshots: 'None', ports: 'Public URLs', maxSize: 'By plan (default 2 vCPU, 4 GiB)',
    runItYourself: 'In your own AWS, GCP, Azure and others',
    soc2: 'yes', hipaa: 'none', iso: 'none', complianceNote: 'SOC 2 Type 2 covers its managed cloud and bring-your-own-cloud. Its security page lists HIPAA and ISO 27001 as no.',
    note: 'The lowest per-vCPU and per-GB list rates in this table.',
  },
  {
    id: 'blaxel', name: 'Blaxel', logo: 'blaxel.png', url: 'https://blaxel.ai',
    kind: 'Hosted service', support: 'experimental', docs: 'blaxel',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'MicroVM',
    billing: 'usage', cpu: 'With memory', memory: '$0.0414 / GB-h active', slot: [0.0828], slotNote: 'While active. Standby is not billed.', fees: 'Snapshot storage $0.20 / GB-month.', priceSource: 'https://blaxel.ai/pricing',
    keepsState: 'Automatic standby keeps memory', snapshots: 'Private preview', ports: 'Token URLs', maxSize: 'By tier',
    runItYourself: 'No', soc2: 'yes', hipaa: 'yes', iso: 'yes', complianceNote: 'SOC 2, ISO 27001 and a BAA add-on. Policies can keep work in US or EU regions.',
    note: 'Bought by Baseten in October 2026. Baseten published its own sandbox rates on 10 November; whether Blaxel\'s terms carry over is open.',
  },
  {
    id: 'vercel', name: 'Vercel Sandbox', logo: 'vercel.png', url: 'https://vercel.com/sandbox',
    kind: 'Hosted service', support: 'experimental', docs: 'vercel',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'Firecracker microVM',
    billing: 'usage', cpu: '$0.128 / CPU-h used', memory: '$0.0212 / GB-h', slot: [0.0744, 0.1704], fees: 'Session length is capped by plan.', priceSource: 'https://vercel.com/docs/vercel-sandbox/pricing',
    keepsState: 'Stop/start', snapshots: 'Disk, fork', ports: 'Public URLs', maxSize: '8 vCPU, 16 GiB',
    runItYourself: 'No', soc2: 'yes', hipaa: 'yes', iso: 'yes', complianceNote: 'SOC 2 Type 2, ISO 27001:2022 and a BAA. Sandboxes run in about 20 regions and can be limited to approved ones.',
    note: 'CPU is billed only while it is busy; memory is billed for the whole session.',
  },
  {
    id: 'fly', name: 'Fly.io Machines', logo: 'fly.png', url: 'https://fly.io',
    kind: 'Hosted service', support: 'experimental', docs: 'fly',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'Firecracker microVM',
    billing: 'tier', cpu: 'By machine size', memory: 'By machine size', slot: null, fees: 'From $0.0093 an hour for a shared CPU with 1 GB.', priceSource: 'https://fly.io/learn/ai-sandbox-pricing/',
    keepsState: 'Stop/start; suspend is best effort', snapshots: 'None', ports: 'None per machine', maxSize: '16 vCPU, 128 GiB',
    runItYourself: 'No', soc2: 'yes', hipaa: 'yes', iso: 'unconfirmed', complianceNote: 'Fly.io is SOC 2 Type 2 audited and offers a BAA. Its datacenters, not Fly.io, hold ISO 27001.',
    note: 'General-purpose machines rather than a sandbox product. Regions are chosen per machine.',
  },
  {
    id: 'aws-agentcore', name: 'AWS Bedrock AgentCore', logo: 'aws.png', url: 'https://aws.amazon.com/bedrock/agentcore/',
    kind: 'In your cloud account', support: 'experimental', docs: 'aws-agentcore',
    supportNote: 'Experimental adapter for the Code Interpreter, tested against a fake server.',
    isolation: 'MicroVM per session',
    billing: 'usage', cpu: '$0.0895 / vCPU-h used', memory: '$0.00945 / GB-h used', slot: [0.0413, 0.1084], fees: 'Idle and I/O wait are free.', priceSource: 'https://cloudburn.io/blog/amazon-bedrock-agentcore-pricing',
    keepsState: 'None', snapshots: 'None', ports: 'None', maxSize: '2 vCPU, 8 GB, 10 GB, fixed',
    runItYourself: 'Runs in your AWS account',
    soc2: 'unconfirmed', hipaa: 'yes', iso: 'unconfirmed', complianceNote: 'On AWS\'s HIPAA-eligible list. AWS says SOC coverage is under audit.',
    note: 'A session API for running code and commands, with one fixed size per session.',
  },
  {
    id: 'azure-sandboxes', name: 'Azure Container Apps Sandboxes', logo: 'azure.png', url: 'https://learn.microsoft.com/azure/container-apps/',
    kind: 'In your cloud account', support: 'experimental', docs: 'azure-sandboxes',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'A managed boundary; Microsoft does not name it',
    billing: 'wall', cpu: null, memory: null, slot: null, fees: 'Price not collected.', priceSource: 'https://azure.microsoft.com/pricing/details/container-apps/',
    keepsState: 'Stop/start', snapshots: 'Disk, fork', ports: 'Public URLs', maxSize: '16 cores, 32 GiB, 320 GiB',
    runItYourself: 'Runs in your Azure subscription',
    soc2: 'unconfirmed', hipaa: 'unconfirmed', iso: 'unconfirmed', complianceNote: 'Azure holds these for its platform. Coverage of this service was not checked.',
    note: 'Stop/start, disk snapshots, forks and egress control are all in its API.',
  },
  {
    id: 'azure-sessions', name: 'Azure dynamic sessions', logo: 'azure.png', url: 'https://learn.microsoft.com/azure/container-apps/sessions',
    kind: 'In your cloud account', support: 'experimental', docs: 'azure-sessions',
    supportNote: 'Experimental adapter for Python session pools, tested against a fake server.',
    isolation: 'Hyper-V per session',
    billing: 'tier', cpu: 'Per session', memory: 'Included', slot: [0.03], slotNote: 'Per session-hour, in whole hours. Third-party figure.', fees: 'Billed from allocation to release, rounded up to the hour.', priceSource: 'https://learn.microsoft.com/en-us/azure/container-apps/billing',
    keepsState: 'None', snapshots: 'None', ports: 'None', maxSize: 'Fixed by the pool',
    runItYourself: 'Runs in your Azure subscription',
    soc2: 'unconfirmed', hipaa: 'unconfirmed', iso: 'unconfirmed', complianceNote: 'Azure holds these for its platform. Coverage of this service was not checked.',
    note: 'Built for running snippets of code. Files live under one folder.',
  },
  {
    id: 'gcp-sandboxes', name: 'Google Cloud Agent Platform', logo: 'gcp.png', url: 'https://cloud.google.com/agent-builder',
    kind: 'In your cloud account', support: 'experimental', docs: 'gcp-sandboxes',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'Container sandboxing; Google does not name the runtime',
    billing: 'wall', cpu: '$0.085 / vCPU-h', memory: '$0.009 / GiB-h', slot: [0.103], fees: 'Rounded per second.', priceSource: 'https://cloud.google.com/vertex-ai/pricing',
    keepsState: 'Stop/start', snapshots: 'Memory, fork', ports: 'None', maxSize: 'Set by the template',
    runItYourself: 'Runs in your Google Cloud project',
    soc2: 'unconfirmed', hipaa: 'unconfirmed', iso: 'unconfirmed', complianceNote: 'Google Cloud holds these for its platform. Coverage of this service was not checked.',
    note: 'Sandboxes for agents built on Google Cloud. A snapshot captures memory.',
  },
  {
    id: 'modal', name: 'Modal', logo: 'modal.png', url: 'https://modal.com/products/sandboxes',
    kind: 'Hosted service', support: 'plugin', docs: 'modal',
    supportNote: 'Through a remote plugin: Modal has no public REST API for sandboxes.',
    isolation: 'gVisor; a VM runtime is optional',
    billing: 'wall', cpu: '$0.1419 / core-h', memory: '$0.024 / GiB-h', slot: [0.119], fees: 'A core is 2 vCPU. $30 a month in credit. Pinning a region costs more.', priceSource: 'https://modal.com/products/sandboxes',
    keepsState: 'None', snapshots: 'Disk, fork', ports: 'Tunnel URLs', maxSize: 'Not published; GPUs available',
    runItYourself: 'No', soc2: 'yes', hipaa: 'yes', iso: 'none', complianceNote: 'SOC 2 Type II. BAA on Enterprise, with Volumes excluded.',
    note: 'Bills the larger of what you request and what you use. Sandboxes cost about three times its standard function rate.',
  },
  {
    id: 'cloudflare', name: 'Cloudflare Sandbox', logo: 'cloudflare.png', url: 'https://developers.cloudflare.com/sandbox/',
    kind: 'Hosted service', support: 'plugin', docs: 'cloudflare',
    supportNote: 'Through a remote plugin: sandboxes are reached only through a Worker you deploy.',
    isolation: 'Firecracker microVM',
    billing: 'usage', cpu: '$0.072 / vCPU-h used', memory: '$0.009 / GiB-h', slot: [0.036, 0.09], fees: '$5 a month Workers Paid plan.', priceSource: 'https://developers.cloudflare.com/sandbox/platform/pricing',
    keepsState: 'None', snapshots: 'Disk, fork (beta)', ports: 'Through your Worker', maxSize: '4 vCPU, 12 GiB, 20 GB',
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'none', iso: 'unconfirmed', complianceNote: 'Cloudflare holds SOC 2 and ISO 27001 for parts of its platform. Containers and Sandbox are not in its public HIPAA material.',
    note: 'An instance stops shortly after its Worker goes quiet, so long idle sessions need care.',
  },
  {
    id: 'sprites', name: 'Sprites', logo: 'sprites.png', url: 'https://sprites.dev',
    kind: 'Hosted service', support: 'plugin', docs: 'sprites',
    supportNote: 'Through a remote plugin for now: exec is specified only over WebSocket.',
    isolation: 'MicroVM',
    billing: 'usage', cpu: '$0.03825 / CPU-h used', memory: '$0.021875 / GB-h used', slot: [0.0533, 0.082], fees: 'Plans are optional. Storage is extra.', priceSource: 'https://fly.io/pricing-update/',
    keepsState: 'Sleeps and wakes by itself; the disk persists', snapshots: 'Disk checkpoints', ports: 'One URL per sprite', maxSize: '8 vCPU, 100 GB',
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'unconfirmed', iso: 'unconfirmed', complianceNote: 'Fly.io holds SOC 2 Type 2 and offers a BAA. Whether Sprites is covered was not confirmed.',
    note: 'Fly.io\'s persistent sandbox. Both CPU and memory are billed on what is used.',
  },
  {
    id: 'codesandbox', name: 'CodeSandbox', logo: 'codesandbox.png', url: 'https://www.together.ai/code-sandbox',
    kind: 'Hosted service', support: 'plugin', docs: 'codesandbox',
    supportNote: 'Through a remote plugin: commands and files go through its SDK.',
    isolation: 'Firecracker microVM',
    billing: 'wall', cpu: '$0.0446 / vCPU-h', memory: '$0.0149 / GiB-h', slot: [0.0744], slotNote: 'Together Code Sandbox launch rates.', fees: 'Custom plans. The older SDK bills credits per minute by tier.', priceSource: 'https://docs.together.ai/docs/together-code-sandbox',
    keepsState: 'Hibernate keeps memory', snapshots: 'Hibernation snapshots, fork', ports: 'Public URLs', maxSize: '64 vCPU, 128 GiB',
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'unconfirmed', iso: 'none', complianceNote: 'Together AI completed SOC 2 Type 2 in 2025. Coverage of the sandbox was not confirmed.',
    note: 'Now sold as Together Code Sandbox. Forks carry memory and disk.',
  },
  {
    id: 'beam', name: 'Beam', logo: 'beam.png', url: 'https://www.beam.cloud',
    kind: 'Hosted service', support: 'plugin', docs: 'beam',
    supportNote: 'Through a remote plugin built on Beam\'s SDK.',
    isolation: 'Container; microVM on request',
    billing: 'wall', cpu: '$0.05 to $0.0675 / vCPU-h', memory: '$0.01 to $0.023 / GiB-h', slot: [0.07, 0.1136], slotNote: 'Two sources disagree; this is their span.', fees: 'Developer plan $20 a month.', priceSource: 'https://www.beam.cloud/pricing',
    keepsState: 'Pause on VMs', snapshots: 'Disk and memory, fork', ports: 'Public URLs', maxSize: 'Not published; GPUs available',
    runItYourself: 'Open-source engine (beta9)', ...unchecked, complianceNote: 'Not checked for this page.',
    note: 'Its engine is open source, though self-hosting does not give every hosted feature.',
  },
  {
    id: 'koyeb', name: 'Koyeb', logo: 'koyeb.png', url: 'https://www.koyeb.com',
    kind: 'Hosted service', support: 'plugin', docs: 'koyeb',
    supportNote: 'Through a remote plugin built on Koyeb\'s SDK.',
    isolation: 'MicroVM (Cloud Hypervisor)',
    billing: 'tier', cpu: 'By instance size', memory: 'Included', slot: null, fees: 'From $0.0036 an hour; 4 vCPU with 4 GB is $0.0576 an hour.', priceSource: 'https://www.koyeb.com/pricing',
    keepsState: 'Pause without memory', snapshots: 'Disk, fork', ports: 'Public URL', maxSize: '40 vCPU, 128 GB',
    runItYourself: 'No', ...unchecked, complianceNote: 'Not checked for this page.',
    note: 'Sandboxes are in public preview and Koyeb is joining Mistral AI, so terms may change.',
  },
  {
    id: 'gke-agent-sandbox', name: 'GKE Agent Sandbox', logo: 'gcp.png', url: 'https://cloud.google.com/kubernetes-engine',
    kind: 'In your cloud account', support: 'plugin', docs: 'gke-agent-sandbox',
    supportNote: 'Through a remote plugin, or OpenSandbox\'s Kubernetes runtime on the same cluster.',
    isolation: 'gVisor',
    billing: 'infra', cpu: null, memory: null, slot: null, fees: 'Your GKE nodes', priceSource: '',
    keepsState: 'With GKE Pod Snapshots', snapshots: 'Pod snapshots', ports: null, maxSize: 'The pod\'s resources',
    runItYourself: 'Runs in your GKE cluster', ...own, complianceNote: 'It runs in your cluster, inside your own audit scope.',
    note: 'A GKE add-on built on agent-sandbox, with gVisor isolation.',
  },
  {
    id: 'novita', name: 'Novita Agent Sandbox', logo: 'novita.png', url: 'https://novita.ai/sandbox',
    kind: 'Hosted service', support: 'unassessed',
    supportNote: 'No adapter yet. It says it speaks E2B\'s protocol, so the e2b adapter is the first thing to try.',
    isolation: 'Not confirmed',
    billing: 'wall', cpu: '$0.0353 / vCPU-h', memory: '$0.0115 / GiB-h', slot: [0.0583], fees: 'Storage $0.00009 / GB-h after an included amount.', priceSource: 'https://novita.ai/sandbox',
    keepsState: null, snapshots: null, ports: null, maxSize: null,
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'none', iso: 'unconfirmed', complianceNote: 'Its own blog says SOC 2 is not publicly documented; a directory lists SOC 2 and ISO 27001.',
    note: 'Prices itself about 30% under E2B on CPU. Its blog and its pricing page disagree on the memory rate; this is the pricing page.',
  },
  {
    id: 'tensorlake', name: 'Tensorlake', logo: 'tensorlake.png', url: 'https://tensorlake.ai',
    kind: 'Hosted service', support: 'unassessed',
    supportNote: 'No adapter yet. It has REST endpoints for lifecycle, suspend, resume and snapshot.',
    isolation: 'Firecracker or Cloud Hypervisor',
    billing: 'usage', cpu: '$0.07 / core-h used', memory: '$0.015 / GB-h', slot: [0.0475, 0.1], fees: 'Pro $250 a month lowers the rates to $0.042 and $0.009.', priceSource: 'https://tensorlake.ai/pricing',
    keepsState: 'Suspend and resume', snapshots: 'Disk or memory, clone', ports: null, maxSize: null,
    runItYourself: 'Hybrid and on-premises options', soc2: 'yes', hipaa: 'none', iso: 'none', complianceNote: 'SOC 2 Type 2 in its trust center.',
    note: 'Bills CPU that is used, with a floor of 20% of what is allocated. Runloop names it as a migration target.',
  },
  {
    id: 'upstash', name: 'Upstash Box', logo: 'upstash.png', url: 'https://upstash.com/pricing/box',
    kind: 'Hosted service', support: 'unassessed',
    supportNote: 'No adapter yet. It is SDK-first, so a remote plugin is the likely route.',
    isolation: 'Hardened container',
    billing: 'usage', cpu: '$0.10 / CPU-h used', memory: 'Included', slot: [0.025, 0.1], fees: 'For a box of 2 vCPU and 4 GB. Or $8 a month to keep a small box alive.', priceSource: 'https://upstash.com/pricing/box',
    keepsState: 'Pauses by itself; files persist', snapshots: null, ports: null, maxSize: 'Boxes of 2, 4 or 8 vCPU',
    runItYourself: 'No', soc2: 'none', hipaa: 'none', iso: 'none', complianceNote: 'Nothing found for Box.',
    note: 'Memory is not billed, only the CPU a box uses.',
  },
  {
    id: 'digitalocean', name: 'DigitalOcean Harness Runtime', logo: 'digitalocean.png', url: 'https://www.digitalocean.com/pricing/harness-runtime',
    kind: 'Hosted service', support: 'unassessed',
    supportNote: 'No adapter yet. In public preview; a raw command API was not confirmed.',
    isolation: 'Hardware-isolated microVM',
    billing: 'usage', cpu: '$0.044 / vCPU-h used', memory: '$0.0095 / GB-h', slot: [0.03, 0.063], fees: 'Prepaid balance.', priceSource: 'https://www.digitalocean.com/pricing/harness-runtime',
    keepsState: 'Pause and resume', snapshots: 'Fork', ports: null, maxSize: null,
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'unconfirmed', iso: 'none', complianceNote: 'DigitalOcean holds SOC 2 and SOC 3. Coverage of this preview product was not confirmed.',
    note: 'A managed runtime for agents that also offers sandboxes alone.',
  },
];

/** Providers that cannot be used, and why. */
export const unavailable = [
  { name: 'Runloop', logo: 'runloop.png', why: 'Shut down. Support for existing customers ended on 13 November 2026; its docs point to Tensorlake and Freestyle.' },
  { name: 'microsandbox', logo: 'microsandbox.png', why: 'Since its 2026 rewrite it has no server API: the runtime is embedded in each SDK process, so nothing can place work on it.' },
];

export const supportLabel: Record<Support, string> = {
  verified: 'Verified live',
  experimental: 'Adapter, experimental',
  plugin: 'Through a plugin',
  unassessed: 'Not yet',
};

export const billingLabel: Record<Billing, string> = {
  wall: 'While running',
  usage: 'CPU used',
  tier: 'Bundled sizes',
  infra: 'Your infrastructure',
};

export const evidenceLabel: Record<Evidence, string> = {
  yes: 'Stated',
  unconfirmed: 'Unconfirmed',
  none: 'Not found',
  own: 'Your scope',
  unchecked: 'Not checked',
};

/** "$0.083" or "$0.036 to $0.090". */
export function slotText(p: Provider): string {
  if (!p.slot) return '';
  const f = (n: number) => `$${n.toFixed(3)}`;
  return p.slot.length === 2 ? `${f(p.slot[0])} to ${f(p.slot[1])}` : f(p.slot[0]);
}
