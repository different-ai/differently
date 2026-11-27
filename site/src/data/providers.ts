// The data behind /compare/: one record per sandbox provider. The console's
// Providers page reads it too (console/src/lib/catalog.ts): a pool's list
// price and what its provider states come from here.
//
// Where it comes from:
//   - Features and the Differently status: docs/providers/*.md, which name the
//     API pages read and the date. Keep the two in step.
//   - Prices: each provider's public pricing page or, where marked, a
//     third-party comparison, read on PRICES_READ. They are list prices. A
//     runtime you operate has none: where its authors publish an estimate, it
//     goes in `fees` with its source, never in `slot`.
//   - Compliance: what the provider states on its own security or trust page.
//
// Rules for editing: never state a figure that was not read from a source, mark
// what is unconfirmed, and use null for "not collected" rather than guessing.

export const PRICES_READ = '2026-11-16';
export const PRICES_READ_LABEL = '16 November 2026';
/** Entries read again, or for the first time, after PRICES_READ. */
export const PRICES_READ_LATER = { label: '18 November 2026', ids: ['fly', 'modal', 'sprites', 'gke-sandboxes'] };

/** How a provider relates to Differently today. */
export type Support = 'verified' | 'experimental' | 'plugin' | 'unassessed';

/** yes: the provider states it. unconfirmed: claimed elsewhere, or true of the wider platform only. none: nothing found. own: your own audit scope. unchecked: not looked at. */
export type Evidence = 'yes' | 'unconfirmed' | 'none' | 'own' | 'unchecked';

/** wall: billed while running. usage: billed for CPU actually used. tier: bundled sizes. infra: your own infrastructure. */
export type Billing = 'wall' | 'usage' | 'tier' | 'infra';

/**
 * One capability, as the provider documents it. yes: documented. part: with a
 * condition, which the matching text field names (a runtime, a class, a beta).
 * no: not offered, or not documented. null: not collected.
 */
export type Cap = 'yes' | 'part' | 'no' | null;

export interface Caps {
  /** Each sandbox has a kernel of its own: a microVM or a VM. */
  kernel: Cap;
  /** A stopped or paused sandbox comes back with its disk. */
  stop: Cap;
  /** A pause, or a snapshot, brings back the running processes. */
  memory: Cap;
  /** Snapshots you can keep and start from. */
  snapshot: Cap;
  /** A new sandbox started from another one's snapshot. */
  fork: Cap;
  /** A URL for a port inside the sandbox. */
  url: Cap;
  /** Runs in infrastructure or a cloud account you control. */
  own: Cap;
}

/**
 * A provider's list prices in the shape of a Differently pool's cost model,
 * where its pricing maps onto one. `basis` is what the provider meters: what a
 * sandbox is allocated, or what it uses. Under "used" these are the rates of a
 * sandbox that is busy all the time, so an estimate made from them is an upper
 * bound. The console offers them when an operator sets a pool's prices.
 */
export interface Rates {
  cpuHour?: number;
  memoryGiBHour?: number;
  sandboxHour?: number;
  basis: 'allocated' | 'used';
  note?: string;
}

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
  /** Null where the pricing does not map onto per-hour rates, or was not collected. */
  rates: Rates | null;
  /** The adapter kind of a pool on this provider (`adapter:` in a pool's config), where this repository has one. */
  adapter?: string;
  cpu: string | null;
  memory: string | null;
  /** List price of one running hour of 1 vCPU and 2 GiB: [low] or [low, high]. For usage billing, low is 25% busy and high is 100% busy. Any other span (two sizes, two sources) is named in slotNote. */
  slot: [number] | [number, number] | null;
  slotNote?: string;
  fees: string;
  priceSource: string;

  keepsState: string | null;
  snapshots: string | null;
  ports: string | null;
  maxSize: string | null;
  /** The same facts as the text fields beside it, as marks for the feature grid. Keep the two in step. */
  caps: Caps;
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
    rates: null,
    adapter: 'opensandbox',
    keepsState: 'Pause keeps memory (Docker, Fast Sandbox); stop/start on Kubernetes', snapshots: 'Disk, fork', ports: 'Signed URLs on Kubernetes', maxSize: 'Your nodes',
    caps: { kernel: 'part', stop: 'yes', memory: 'part', snapshot: 'yes', fork: 'yes', url: 'part', own: 'yes' },
    runItYourself: 'Yes, Apache-2.0', ...own, complianceNote: 'It runs in your infrastructure, inside your own audit scope.',
    note: 'The default private runtime for Differently. You install it, choose its runtime and register it as a pool.',
  },
  {
    id: 'agent-sandbox', name: 'agent-sandbox', logo: 'kubernetes.png', url: 'https://github.com/kubernetes-sigs/agent-sandbox',
    kind: 'Open-source runtime', support: 'experimental', docs: 'agent-sandbox',
    supportNote: 'Experimental adapter. Its live test ran once, on a kind cluster with runc.',
    isolation: 'The pod RuntimeClass: runc, gVisor or Kata',
    billing: 'infra', cpu: null, memory: null, slot: null, fees: 'Your own cluster', priceSource: '',
    rates: null,
    adapter: 'agentsandbox',
    keepsState: 'Stop/start with a volume; pause on GKE (gVisor), warm pools too', snapshots: 'Memory + fork on GKE (gVisor)', ports: 'In-cluster', maxSize: 'Your nodes and quota',
    caps: { kernel: 'part', stop: 'part', memory: 'part', snapshot: 'part', fork: 'part', url: 'part', own: 'yes' },
    runItYourself: 'Yes, Apache-2.0', ...own, complianceNote: 'It runs in your cluster, inside your own audit scope.',
    note: 'A Kubernetes SIG project that adds a Sandbox resource to a cluster. Isolation is whatever RuntimeClass the pod uses.',
  },
  {
    id: 'cubesandbox', name: 'CubeSandbox', logo: 'cubesandbox.png', url: 'https://github.com/TencentCloud/CubeSandbox',
    kind: 'Open-source runtime', support: 'experimental', docs: 'cubesandbox',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'KVM microVM',
    billing: 'infra', cpu: null, memory: null, slot: null, fees: 'Your own nodes', priceSource: '',
    rates: null,
    adapter: 'cubesandbox',
    keepsState: 'Pause keeps memory', snapshots: 'Memory, fork', ports: 'Opt-in URLs', maxSize: 'Set by the template',
    caps: { kernel: 'yes', stop: 'yes', memory: 'yes', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'yes' },
    runItYourself: 'Yes, Apache-2.0', ...own, complianceNote: 'It runs in your infrastructure, inside your own audit scope.',
    note: 'Tencent\'s open-source microVM sandbox runtime, with an API in the shape of E2B\'s.',
  },
  {
    id: 'daytona', name: 'Daytona', logo: 'daytona.png', url: 'https://www.daytona.io',
    kind: 'Hosted service', support: 'verified', docs: 'daytona',
    supportNote: 'In every build. Verified against a live account (container class; the VM class has not run).',
    isolation: 'Container, or a VM class',
    billing: 'wall', cpu: '$0.0504 / vCPU-h', memory: '$0.0162 / GiB-h', slot: [0.0828], fees: '$200 starting credit. Disk $0.000108 / GiB-h after 5 GiB.', priceSource: 'https://www.daytona.io/pricing',
    rates: { cpuHour: 0.0504, memoryGiBHour: 0.0162, basis: 'allocated' },
    adapter: 'daytona',
    keepsState: 'Stop/start; pause on VMs', snapshots: 'Disk, fork', ports: 'Signed URLs', maxSize: '4 vCPU, 8 GiB, 10 GiB by default',
    caps: { kernel: 'part', stop: 'yes', memory: 'part', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'part' },
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
    rates: { cpuHour: 0.04032, memoryGiBHour: 0.0129, basis: 'allocated' },
    adapter: 'freestyle',
    keepsState: 'Stop/start; pause keeps memory', snapshots: 'Memory, fork', ports: 'Public URLs', maxSize: '32 vCPU, 64 GiB, 256 GiB (by plan)',
    caps: { kernel: 'yes', stop: 'yes', memory: 'yes', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'no' },
    runItYourself: 'No', soc2: 'none', hipaa: 'none', iso: 'none', complianceNote: 'No audit report or region list found. It runs on its own bare metal.',
    note: 'A fork carries running processes with it. Runloop names it as a migration target.',
  },
  {
    id: 'e2b', name: 'E2B', logo: 'e2b.png', url: 'https://e2b.dev',
    kind: 'Hosted service', support: 'experimental', docs: 'e2b',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'Firecracker microVM',
    billing: 'wall', cpu: '$0.0504 / vCPU-h', memory: '$0.0162 / GiB-h', slot: [0.0828], fees: 'Pro $150 a month. Disk included.', priceSource: 'https://docs.e2b.dev/faq/calculate-sandbox-price',
    rates: { cpuHour: 0.0504, memoryGiBHour: 0.0162, basis: 'allocated' },
    adapter: 'e2b',
    keepsState: 'Pause keeps memory, unbilled', snapshots: 'Memory, fork', ports: 'Public URLs, opt-in', maxSize: '8 vCPU, 8 GiB, 20 GiB (by plan)',
    caps: { kernel: 'yes', stop: 'yes', memory: 'yes', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'yes' },
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
    rates: { sandboxHour: 0.018, basis: 'allocated', note: 'The smallest size. Larger sizes cost up to $0.20 an hour.' },
    adapter: 'boat',
    keepsState: 'Stop/start keeps the disk', snapshots: 'Disk, fork', ports: 'Token or public URLs', maxSize: '16 vCPU, 32 GB, 251 GB',
    caps: { kernel: 'yes', stop: 'yes', memory: 'no', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'no' },
    runItYourself: 'No', soc2: 'none', hipaa: 'none', iso: 'none', complianceNote: 'Nothing found on audit reports or regions.',
    note: 'Full Ubuntu machines, so Docker and systemd work as on a normal host. Stopped sandboxes keep their state for 14 days.',
  },
  {
    id: 'morph', name: 'Morph Cloud', logo: 'morph.png', url: 'https://cloud.morph.so',
    kind: 'Hosted service', support: 'experimental', docs: 'morph',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'Virtual machine',
    billing: 'wall', cpu: '$0.05 / unit-h', memory: 'In the unit', slot: [0.05], fees: 'A compute unit is 1 vCPU, or 4 GB RAM, or 16 GB disk. Snapshot storage is extra.', priceSource: 'https://cloud.morph.so/web/subscribe',
    rates: { cpuHour: 0.05, basis: 'allocated', note: 'One compute unit: 1 vCPU with up to 4 GB of memory. More memory or disk adds units.' },
    adapter: 'morph',
    keepsState: 'Pause keeps memory', snapshots: 'Memory, fork', ports: 'Public URLs', maxSize: null,
    caps: { kernel: 'yes', stop: 'yes', memory: 'yes', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'no' },
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'none', iso: 'none', complianceNote: 'Its trust center lists a SOC 2 Type I report, not Type II. Regions are not disclosed.',
    note: 'Built around snapshotting a running machine and branching many copies from it.',
  },
  {
    id: 'northflank', name: 'Northflank', logo: 'northflank.png', url: 'https://northflank.com',
    kind: 'Hosted service', support: 'experimental', docs: 'northflank',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'MicroVM (Cloud Hypervisor), or gVisor',
    billing: 'wall', cpu: '$0.01667 / vCPU-h', memory: '$0.00833 / GB-h', slot: [0.0333], fees: 'Free tier up to $20 a month of resources.', priceSource: 'https://northflank.com/blog/ai-sandbox-pricing',
    rates: { cpuHour: 0.01667, memoryGiBHour: 0.00833, basis: 'allocated' },
    adapter: 'northflank',
    keepsState: 'None', snapshots: 'None', ports: 'Public URLs', maxSize: 'By plan (default 2 vCPU, 4 GiB)',
    caps: { kernel: 'part', stop: 'no', memory: 'no', snapshot: 'no', fork: 'no', url: 'yes', own: 'yes' },
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
    rates: { memoryGiBHour: 0.0414, basis: 'used', note: 'Billed on memory while the sandbox is active. Standby is not billed.' },
    adapter: 'blaxel',
    keepsState: 'Automatic standby keeps memory', snapshots: 'Private preview', ports: 'Token URLs', maxSize: 'By tier',
    caps: { kernel: 'yes', stop: 'yes', memory: 'yes', snapshot: 'part', fork: 'part', url: 'yes', own: 'no' },
    runItYourself: 'No', soc2: 'yes', hipaa: 'yes', iso: 'yes', complianceNote: 'SOC 2, ISO 27001 and a BAA add-on. Policies can keep work in US or EU regions.',
    note: 'Bought by Baseten in October 2026. Baseten published its own sandbox rates on 10 November; whether Blaxel\'s terms carry over is open.',
  },
  {
    id: 'groundbox', name: 'Groundbox', logo: 'groundbox.png', url: 'https://groundbox.dev',
    kind: 'Hosted service', support: 'experimental', docs: 'groundbox',
    supportNote: 'Experimental adapter. Its live test ran against the hosted API, run by a Groundbox maintainer.',
    isolation: 'Firecracker microVM',
    billing: 'wall', cpu: '$0.04 / vCPU-h', memory: 'Included, up to 4 GB per vCPU', slot: [0.04], fees: 'Paid plan $20 a month, with $20 of usage included. Paused and stopped sandboxes and kept snapshots $0.05 per GB-month.', priceSource: 'https://groundbox.dev/pricing',
    rates: { cpuHour: 0.04, basis: 'allocated', note: 'Up to 4 GB of memory per vCPU is included.' },
    adapter: 'groundbox',
    keepsState: 'Stop keeps the disk; pause keeps memory too, and idle sandboxes pause and wake by themselves. Both billed as storage', snapshots: 'Memory, fork; kept until deleted or 30 days unused', ports: 'Share-link or public URLs', maxSize: '16 vCPU, 64 GB, 100 GB',
    caps: { kernel: 'yes', stop: 'yes', memory: 'yes', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'no' },
    runItYourself: 'No', soc2: 'none', hipaa: 'none', iso: 'none', complianceNote: 'No audit report or region list found.',
    note: 'Sandboxes and GitHub Actions runners on one account. A snapshot is kept on its own and a fork is a new sandbox started from one.',
  },
  {
    id: 'vercel', name: 'Vercel Sandbox', logo: 'vercel.png', url: 'https://vercel.com/sandbox',
    kind: 'Hosted service', support: 'experimental', docs: 'vercel',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'Firecracker microVM',
    billing: 'usage', cpu: '$0.128 / CPU-h used', memory: '$0.0212 / GB-h', slot: [0.0744, 0.1704], fees: 'Session length is capped by plan.', priceSource: 'https://vercel.com/docs/vercel-sandbox/pricing',
    rates: { cpuHour: 0.128, memoryGiBHour: 0.0212, basis: 'used', note: 'CPU is billed while it is busy; memory for the whole session.' },
    adapter: 'vercel',
    keepsState: 'Stop/start', snapshots: 'Disk, fork', ports: 'Public URLs', maxSize: '8 vCPU, 16 GiB',
    caps: { kernel: 'yes', stop: 'yes', memory: 'no', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'no' },
    runItYourself: 'No', soc2: 'yes', hipaa: 'yes', iso: 'yes', complianceNote: 'SOC 2 Type 2, ISO 27001:2022 and a BAA. Sandboxes run in about 20 regions and can be limited to approved ones.',
    note: 'CPU is billed only while it is busy; memory is billed for the whole session.',
  },
  {
    id: 'fly', name: 'Fly.io Machines', logo: 'fly.png', url: 'https://fly.io',
    kind: 'Hosted service', support: 'experimental', docs: 'fly',
    supportNote: 'Experimental adapter, tested against a fake server.',
    isolation: 'Firecracker microVM',
    billing: 'tier', cpu: 'By machine size', memory: 'By machine size', slot: [0.0186, 0.0458], slotNote: 'A shared-cpu-2x Machine with 2 GB, up to a performance-1x with 2 GB.', fees: 'Prices are for its two lowest-priced regions; others cost more. A stopped Machine pays $0.15 / GB-month for its root filesystem.', priceSource: 'https://fly.io/docs/about/pricing/',
    rates: null,
    adapter: 'fly',
    keepsState: 'Stop/start; suspend is best effort', snapshots: 'None', ports: 'None per machine', maxSize: '16 vCPU, 128 GiB',
    caps: { kernel: 'yes', stop: 'yes', memory: 'part', snapshot: 'no', fork: 'no', url: 'no', own: 'no' },
    runItYourself: 'No', soc2: 'yes', hipaa: 'yes', iso: 'unconfirmed', complianceNote: 'Fly.io is SOC 2 Type 2 audited and offers a BAA. Its datacenters, not Fly.io, hold ISO 27001.',
    note: 'General-purpose machines rather than a sandbox product. Regions are chosen per machine.',
  },
  {
    id: 'aws-agentcore', name: 'AWS Bedrock AgentCore', logo: 'aws.png', url: 'https://aws.amazon.com/bedrock/agentcore/',
    kind: 'In your cloud account', support: 'experimental', docs: 'aws-agentcore',
    supportNote: 'Experimental adapter for the Code Interpreter, tested against a fake server.',
    isolation: 'MicroVM per session',
    billing: 'usage', cpu: '$0.0895 / vCPU-h used', memory: '$0.00945 / GB-h used', slot: [0.0413, 0.1084], fees: 'Idle and I/O wait are free.', priceSource: 'https://cloudburn.io/blog/amazon-bedrock-agentcore-pricing',
    rates: { cpuHour: 0.0895, memoryGiBHour: 0.00945, basis: 'used' },
    adapter: 'agentcore',
    keepsState: 'None', snapshots: 'None', ports: 'None', maxSize: '2 vCPU, 8 GB, 10 GB, fixed',
    caps: { kernel: 'yes', stop: 'no', memory: 'no', snapshot: 'no', fork: 'no', url: 'no', own: 'yes' },
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
    rates: null,
    adapter: 'azuresandboxes',
    keepsState: 'Stop/start', snapshots: 'Disk, fork', ports: 'Public URLs', maxSize: '16 cores, 32 GiB, 320 GiB',
    caps: { kernel: null, stop: 'yes', memory: 'no', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'yes' },
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
    rates: { sandboxHour: 0.03, basis: 'allocated', note: 'Per session-hour, rounded up to the hour. A third-party figure.' },
    adapter: 'azuresessions',
    keepsState: 'None', snapshots: 'None', ports: 'None', maxSize: 'Fixed by the pool',
    caps: { kernel: 'yes', stop: 'no', memory: 'no', snapshot: 'no', fork: 'no', url: 'no', own: 'yes' },
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
    rates: { cpuHour: 0.085, memoryGiBHour: 0.009, basis: 'allocated' },
    adapter: 'gcpsandboxes',
    keepsState: 'Stop/start', snapshots: 'Memory, fork', ports: 'None', maxSize: 'Set by the template',
    caps: { kernel: null, stop: 'yes', memory: 'yes', snapshot: 'yes', fork: 'yes', url: 'no', own: 'yes' },
    runItYourself: 'Runs in your Google Cloud project',
    soc2: 'unconfirmed', hipaa: 'unconfirmed', iso: 'unconfirmed', complianceNote: 'Google Cloud holds these for its platform. Coverage of this service was not checked.',
    note: 'Sandboxes for agents built on Google Cloud. A snapshot captures memory.',
  },
  {
    id: 'modal', name: 'Modal', logo: 'modal.png', url: 'https://modal.com/products/sandboxes',
    kind: 'Hosted service', support: 'plugin', docs: 'modal',
    supportNote: 'Through a remote plugin: Modal has no public REST API for sandboxes.',
    isolation: 'gVisor; a VM runtime is optional',
    billing: 'usage', cpu: '$0.1419 / core-h', memory: '$0.024 / GiB-h', slot: [0.0657, 0.119], slotNote: 'With nothing reserved above the default. A sandbox that reserves its size pays the high figure all the time.', fees: 'A core is 2 vCPU. Bills the larger of what is reserved and what is used; the default reservation is 0.125 core and 128 MiB. $30 a month in credit. Pinning a region costs more.', priceSource: 'https://modal.com/pricing',
    rates: { cpuHour: 0.07095, memoryGiBHour: 0.024, basis: 'used', note: 'Modal prices a physical core, which is 2 vCPU; the CPU rate here is half of that. It bills the larger of what a sandbox reserves and what it uses.' },
    keepsState: 'None', snapshots: 'Disk, fork', ports: 'Tunnel URLs', maxSize: 'Not published; GPUs available',
    caps: { kernel: 'part', stop: 'no', memory: 'no', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'no' },
    runItYourself: 'No', soc2: 'yes', hipaa: 'yes', iso: 'none', complianceNote: 'SOC 2 Type II. BAA on Enterprise, with Volumes excluded.',
    note: 'Bills the larger of what you reserve and what you use, down to a default of 0.125 core. Sandboxes cost about three times its standard function rate.',
  },
  {
    id: 'cloudflare', name: 'Cloudflare Sandbox', logo: 'cloudflare.png', url: 'https://developers.cloudflare.com/sandbox/',
    kind: 'Hosted service', support: 'plugin', docs: 'cloudflare',
    supportNote: 'Through a remote plugin: sandboxes are reached only through a Worker you deploy.',
    isolation: 'Firecracker microVM',
    billing: 'usage', cpu: '$0.072 / vCPU-h used', memory: '$0.009 / GiB-h', slot: [0.036, 0.09], fees: '$5 a month Workers Paid plan.', priceSource: 'https://developers.cloudflare.com/sandbox/platform/pricing',
    rates: { cpuHour: 0.072, memoryGiBHour: 0.009, basis: 'used' },
    keepsState: 'None', snapshots: 'Disk, fork (beta)', ports: 'Through your Worker', maxSize: '4 vCPU, 12 GiB, 20 GB',
    caps: { kernel: 'yes', stop: 'no', memory: 'no', snapshot: 'part', fork: 'part', url: 'part', own: 'no' },
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'none', iso: 'unconfirmed', complianceNote: 'Cloudflare holds SOC 2 and ISO 27001 for parts of its platform. Containers and Sandbox are not in its public HIPAA material.',
    note: 'An instance stops shortly after its Worker goes quiet, so long idle sessions need care.',
  },
  {
    id: 'sprites', name: 'Sprites', logo: 'sprites.png', url: 'https://sprites.dev',
    kind: 'Hosted service', support: 'plugin', docs: 'sprites',
    supportNote: 'Through a remote plugin for now: exec is specified only over WebSocket.',
    isolation: 'MicroVM',
    billing: 'usage', cpu: '$0.0385 / CPU-h used', memory: '$0.021875 / GB-h used', slot: [0.0534, 0.0823], fees: 'Plans are optional. Storage is extra.', priceSource: 'https://fly.io/pricing-update/',
    rates: { cpuHour: 0.0385, memoryGiBHour: 0.021875, basis: 'used' },
    keepsState: 'Sleeps and wakes by itself; the disk persists, memory while warm', snapshots: 'Disk checkpoints', ports: 'One URL per sprite', maxSize: '8 vCPU, 100 GB',
    caps: { kernel: 'yes', stop: 'yes', memory: 'part', snapshot: 'yes', fork: 'no', url: 'yes', own: 'no' },
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'unconfirmed', iso: 'unconfirmed', complianceNote: 'Fly.io holds SOC 2 Type 2 and offers a BAA. Whether Sprites is covered was not confirmed.',
    note: 'Fly.io\'s persistent sandbox. Both CPU and memory are billed on what is used.',
  },
  {
    id: 'codesandbox', name: 'CodeSandbox', logo: 'codesandbox.png', url: 'https://www.together.ai/code-sandbox',
    kind: 'Hosted service', support: 'plugin', docs: 'codesandbox',
    supportNote: 'Through a remote plugin: commands and files go through its SDK.',
    isolation: 'Firecracker microVM',
    billing: 'wall', cpu: '$0.0446 / vCPU-h', memory: '$0.0149 / GiB-h', slot: [0.0744], slotNote: 'Together Code Sandbox launch rates.', fees: 'Custom plans. The older SDK bills credits per minute by tier.', priceSource: 'https://docs.together.ai/docs/together-code-sandbox',
    rates: { cpuHour: 0.0446, memoryGiBHour: 0.0149, basis: 'allocated' },
    keepsState: 'Hibernate keeps memory', snapshots: 'Hibernation snapshots, fork', ports: 'Public URLs', maxSize: '64 vCPU, 128 GiB',
    caps: { kernel: 'yes', stop: 'yes', memory: 'yes', snapshot: 'part', fork: 'yes', url: 'yes', own: 'no' },
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'unconfirmed', iso: 'none', complianceNote: 'Together AI completed SOC 2 Type 2 in 2025. Coverage of the sandbox was not confirmed.',
    note: 'Now sold as Together Code Sandbox. Forks carry memory and disk.',
  },
  {
    id: 'beam', name: 'Beam', logo: 'beam.png', url: 'https://www.beam.cloud',
    kind: 'Hosted service', support: 'plugin', docs: 'beam',
    supportNote: 'Through a remote plugin built on Beam\'s SDK.',
    isolation: 'Container; microVM on request',
    billing: 'wall', cpu: '$0.05 to $0.0675 / vCPU-h', memory: '$0.01 to $0.023 / GiB-h', slot: [0.07, 0.1136], slotNote: 'Two sources disagree; this is their span.', fees: 'Developer plan $20 a month.', priceSource: 'https://www.beam.cloud/pricing',
    rates: null,
    keepsState: 'Pause on VMs', snapshots: 'Disk and memory, fork', ports: 'Public URLs', maxSize: 'Not published; GPUs available',
    caps: { kernel: 'part', stop: 'part', memory: 'part', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'part' },
    runItYourself: 'Open-source engine (beta9)', ...unchecked, complianceNote: 'Not checked for this page.',
    note: 'Its engine is open source, though self-hosting does not give every hosted feature.',
  },
  {
    id: 'koyeb', name: 'Koyeb', logo: 'koyeb.png', url: 'https://www.koyeb.com',
    kind: 'Hosted service', support: 'plugin', docs: 'koyeb',
    supportNote: 'Through a remote plugin built on Koyeb\'s SDK.',
    isolation: 'MicroVM (Cloud Hypervisor)',
    billing: 'tier', cpu: 'By instance size', memory: 'Included', slot: null, fees: 'From $0.0036 an hour; 4 vCPU with 4 GB is $0.0576 an hour.', priceSource: 'https://www.koyeb.com/pricing',
    rates: null,
    keepsState: 'Pause without memory; disk not documented', snapshots: 'Disk, fork', ports: 'Public URL', maxSize: '40 vCPU, 128 GB',
    caps: { kernel: 'yes', stop: 'part', memory: 'no', snapshot: 'yes', fork: 'yes', url: 'yes', own: 'no' },
    runItYourself: 'No', ...unchecked, complianceNote: 'Not checked for this page.',
    note: 'Sandboxes are in public preview and Koyeb is joining Mistral AI, so terms may change.',
  },
  {
    id: 'gke-agent-sandbox', name: 'GKE Agent Sandbox', logo: 'gcp.png', url: 'https://cloud.google.com/kubernetes-engine',
    kind: 'In your cloud account', support: 'experimental', docs: 'gke-agent-sandbox',
    supportNote: 'Through the experimental agent-sandbox adapter, with GKE Pod Snapshots for memory snapshots, fork and pause. Tested with a fake server, not run on GKE.',
    isolation: 'gVisor',
    billing: 'infra', cpu: null, memory: null, slot: null, fees: 'Your GKE nodes', priceSource: '',
    rates: null,
    keepsState: 'Suspend keeps volumes; memory with GKE Pod Snapshots', snapshots: 'Pod snapshots, fork', ports: 'Its Service; exposing it is up to you', maxSize: 'The pod\'s resources',
    caps: { kernel: 'no', stop: 'part', memory: 'part', snapshot: 'yes', fork: 'yes', url: 'part', own: 'yes' },
    runItYourself: 'Runs in your GKE cluster', ...own, complianceNote: 'It runs in your cluster, inside your own audit scope.',
    note: 'A GKE add-on built on agent-sandbox, with gVisor isolation.',
  },
  {
    id: 'gke-sandboxes', name: 'gke-sandboxes', logo: 'gcp.png', url: 'https://github.com/comp-work/gke-sandboxes',
    kind: 'Open-source runtime', support: 'unassessed',
    supportNote: 'No adapter yet. It is a TypeScript library that drives the Kubernetes API, with no server of its own, so a remote plugin is the route.',
    isolation: 'gVisor',
    billing: 'infra', cpu: null, memory: null, slot: null, fees: 'Your GKE nodes. Its authors estimate $0.019 for each hour a sandbox is awake: a whole cluster bill shared by 520 sandboxes that each use about 4% of 2 vCPU.', priceSource: 'https://github.com/comp-work/gke-sandboxes/blob/main/docs/comparison.md',
    rates: null,
    keepsState: 'Sleep keeps the files, not the processes', snapshots: 'Files, restored into the same sandbox', ports: 'A private address; an example proxy signs links', maxSize: 'Your nodes (2 vCPU, 4 GiB, 20 GiB by default)',
    caps: { kernel: 'no', stop: 'yes', memory: 'no', snapshot: 'part', fork: 'no', url: 'part', own: 'yes' },
    runItYourself: 'Yes, Apache-2.0, on GKE', ...own, complianceNote: 'It runs in your Google Cloud project, inside your own audit scope.',
    note: 'Energy\'s setup for GKE, published in November 2026: a gVisor pod for each sandbox, which sleeps after five idle minutes and wakes with its files. The cost is its authors\' estimate at their scale, not a list price.',
  },
  {
    id: 'novita', name: 'Novita Agent Sandbox', logo: 'novita.png', url: 'https://novita.ai/sandbox',
    kind: 'Hosted service', support: 'unassessed',
    supportNote: 'No adapter yet. It says it speaks E2B\'s protocol, so the e2b adapter is the first thing to try.',
    isolation: 'Not confirmed',
    billing: 'wall', cpu: '$0.0353 / vCPU-h', memory: '$0.0115 / GiB-h', slot: [0.0583], fees: 'Storage $0.00009 / GB-h after an included amount.', priceSource: 'https://novita.ai/sandbox',
    rates: { cpuHour: 0.0353, memoryGiBHour: 0.0115, basis: 'allocated' },
    keepsState: null, snapshots: null, ports: null, maxSize: null,
    caps: { kernel: null, stop: null, memory: null, snapshot: null, fork: null, url: null, own: 'no' },
    runItYourself: 'No', soc2: 'unconfirmed', hipaa: 'none', iso: 'unconfirmed', complianceNote: 'Its own blog says SOC 2 is not publicly documented; a directory lists SOC 2 and ISO 27001.',
    note: 'Prices itself about 30% under E2B on CPU. Its blog and its pricing page disagree on the memory rate; this is the pricing page.',
  },
  {
    id: 'tensorlake', name: 'Tensorlake', logo: 'tensorlake.png', url: 'https://tensorlake.ai',
    kind: 'Hosted service', support: 'unassessed',
    supportNote: 'No adapter yet. It has REST endpoints for lifecycle, suspend, resume and snapshot.',
    isolation: 'Firecracker or Cloud Hypervisor',
    billing: 'usage', cpu: '$0.07 / core-h used', memory: '$0.015 / GB-h', slot: [0.0475, 0.1], fees: 'Pro $250 a month lowers the rates to $0.042 and $0.009.', priceSource: 'https://tensorlake.ai/pricing',
    rates: { cpuHour: 0.07, memoryGiBHour: 0.015, basis: 'used', note: 'CPU is billed on use, with a floor of 20% of what is allocated.' },
    keepsState: 'Suspend and resume', snapshots: 'Disk or memory, clone', ports: null, maxSize: null,
    caps: { kernel: 'yes', stop: 'yes', memory: 'yes', snapshot: 'yes', fork: 'yes', url: null, own: 'yes' },
    runItYourself: 'Hybrid and on-premises options', soc2: 'yes', hipaa: 'none', iso: 'none', complianceNote: 'SOC 2 Type 2 in its trust center.',
    note: 'Bills CPU that is used, with a floor of 20% of what is allocated. Runloop names it as a migration target.',
  },
  {
    id: 'upstash', name: 'Upstash Box', logo: 'upstash.png', url: 'https://upstash.com/pricing/box',
    kind: 'Hosted service', support: 'unassessed',
    supportNote: 'No adapter yet. It is SDK-first, so a remote plugin is the likely route.',
    isolation: 'Hardened container',
    billing: 'usage', cpu: '$0.10 / CPU-h used', memory: 'Included', slot: [0.025, 0.1], fees: 'For a box of 2 vCPU and 4 GB. Or $8 a month to keep a small box alive.', priceSource: 'https://upstash.com/pricing/box',
    rates: { cpuHour: 0.1, basis: 'used', note: 'Memory is not billed.' },
    keepsState: 'Pauses by itself; files persist', snapshots: null, ports: null, maxSize: 'Boxes of 2, 4 or 8 vCPU',
    caps: { kernel: 'no', stop: 'yes', memory: 'no', snapshot: null, fork: null, url: null, own: 'no' },
    runItYourself: 'No', soc2: 'none', hipaa: 'none', iso: 'none', complianceNote: 'Nothing found for Box.',
    note: 'Memory is not billed, only the CPU a box uses.',
  },
  {
    id: 'digitalocean', name: 'DigitalOcean Harness Runtime', logo: 'digitalocean.png', url: 'https://www.digitalocean.com/pricing/harness-runtime',
    kind: 'Hosted service', support: 'unassessed',
    supportNote: 'No adapter yet. In public preview; a raw command API was not confirmed.',
    isolation: 'Hardware-isolated microVM',
    billing: 'usage', cpu: '$0.044 / vCPU-h used', memory: '$0.0095 / GB-h', slot: [0.03, 0.063], fees: 'Prepaid balance.', priceSource: 'https://www.digitalocean.com/pricing/harness-runtime',
    rates: { cpuHour: 0.044, memoryGiBHour: 0.0095, basis: 'used' },
    keepsState: 'Pause and resume', snapshots: 'Fork', ports: null, maxSize: null,
    caps: { kernel: 'yes', stop: 'yes', memory: null, snapshot: null, fork: 'yes', url: null, own: 'no' },
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
