# Questions people ask

Short, honest answers to the questions that come up when someone first hears
"one API in front of many sandbox providers". Each answer says what
Differently does about the concern and where it stops.

<!-- Remove this note in the release that ships `differently run` (the first after v0.1.0-alpha.5); do not bump its version. -->
> Two commands named below, `differently templates` and the side-by-side
> table from `differently capabilities`, are newer than `v0.1.0-alpha.5`,
> the latest release. What they show is true of that release too; the
> [baselines guide](guides/baselines.md) says how to see it there.

## Images, snapshots and moving between providers

### Do I need my image or snapshot on every provider?

Yes. Each backend boots its own images, and nothing copies one between
providers. You build the baseline once on each provider you want to use and
register them under **one name**. Your code asks for that name, and
Differently sends the request only to pools that have it, handing each
backend its own image. `differently templates` shows which pool has what,
and a request for a baseline that no permitted pool has fails at once with
that reason. [One baseline on every provider](guides/baselines.md) has the
steps.

### Are snapshots portable between providers?

No. A snapshot stays with the backend that took it. What you do not have to
do is remember where: the snapshot's record names its pool, and a restore or
a fork is sent there. If that pool is busy the new sandbox waits; if the
request cannot run there, it is refused with the reason rather than started
somewhere it could not work.

### Can a running sandbox move to another provider?

No. Differently chooses the pool once, when the sandbox is created, and the
sandbox stays there. Backends have different runtimes and memory formats, so
there is no live migration. This is also why adding, draining or removing a
provider never disturbs work that is already running. To move the contents
of a sandbox, copy its files through the API into a new one.

### Do all providers do the same things?

No, and Differently does not pretend they do. One backend takes memory
snapshots, another cannot fork, a third has no stop and start.
`differently capabilities` puts them side by side. When your code needs a
feature it says so, and only pools that have it are considered. If none
does, you get `unsupported_capability` with the feature named, not a
sandbox that fails later. The [provider catalog](providers/README.md) says
what each adapter supports and what has been tested against the real
provider.

## Your code and your operators

### Does my code change when we add or switch a provider?

No. Code asks for a size, a template, a feature or a data classification,
never a backend. Operators add a pool, drain one, or send new work
elsewhere (`differently admin pool use <pool>`), and running sandboxes stay
where they are. [Keys and providers](guides/keys-and-providers.md#switch-new-sandboxes-between-providers)
shows the commands.

### What happens when a provider is down?

Differently checks each pool's health every few seconds. New sandboxes go to
another pool that policy permits, or wait in the queue until one is ready.
Sandboxes already on the unreachable pool are not moved: calls to them fail
with `provider_unavailable` until it is back. A failure that belongs to one
sandbox, such as an image that cannot be pulled, fails that sandbox and
leaves the pool in use for the others.

### Why not call the provider's SDK directly?

With one team and one backend, do that. Differently is for the point where
there are several teams or more than one backend, and someone has to answer
who may create sandboxes and how many, which data may leave your network,
and where a given sandbox ran. [Differently explained](explainer.md) walks
through it.

## Trust and control

### Could my code end up on a provider I did not choose?

Not unless an operator allows it. The default is private pools only. An
external provider receives work only after an operator approves that pool
in policy, and data classified `private-only` never leaves private pools
whatever the policy says. Every sandbox keeps a record of which pools were
considered and why it landed where it did. See
[placement policy](scheduling/placement-policy.md).

### Is Differently another vendor in the path?

No. It is software you run: Apache-2.0, self-hosted, no telemetry, nothing
to sign up for. Provider keys are stored sealed in your installation or
referenced from your own secret store, and are never returned by the API.

### Does it run the sandboxes?

No. Isolation, boot time and density belong to the backend you choose, by
default an [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox)
server on your own hosts. Differently records what each pool declares and
reports it.

## Running it

### Do I need Kubernetes?

Not to try it: [three commands](guides/quickstart.md) on a laptop. For a
team, the tested path is one Linux VM, where the
[guide](deploy/single-vm.md) installs a single-node Kubernetes (k3s) for
you; it ran on Azure, last on 2026-10-29. The Helm chart also
installs on a [cluster you already have](deploy/kubernetes.md): that has run
on k3s, not yet on a managed cluster (EKS, GKE, AKS). A Docker host can run
the whole stack without Kubernetes, from a checkout
([test/e2e](../test/e2e/README.md)).

### Does it slow things down?

Commands and file transfers pass through Differently on their way to the
backend, which is one more hop. There are almost no measurements yet, and
[reliability and performance](reliability-and-performance.md) says so and
lists the ones to take.

### Is it ready for production?

No. It is a pre-release. The [deployment overview](deploy/README.md) says
what has run on real infrastructure and what has not, and the
[roadmap](roadmap.md) says what is planned.

## Something missing?

[Open an issue](https://github.com/different-ai/differently/issues) with the
question. If it comes up twice, it belongs on this page.
