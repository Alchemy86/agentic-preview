# Changelog

## Unreleased

### Added

- **Scheduled, headerless intercepts** — the one mode here that is not header-keyed, and off
  unless a window is declared. A `SCHEDULE_FILE` gives a workload a recurring window; while
  it is open agentic-preview raises a *global* intercept on it (no header filter at all, so
  the traffic-agent keeps the port on its raw TCP listener and diverts every connection,
  TLS or plaintext, without parsing a byte), and on close it removes it exactly as a graceful
  stop does. Built for a service whose dependency is switched off outside working hours: a
  stand-in answers for it, and nothing in the estate has to know about the swap.
  [`docs/SCHEDULES.md`](docs/SCHEDULES.md) is the page.
- Windows are `days` + `start` + either `end` or `duration`, in a named IANA zone (the zone
  database is compiled in). `end` at or before `start` is the next day, so an overnight reads
  as `18:32` → `07:21`; `duration` is what expresses a span longer than a day, so Friday
  evening through to Monday morning is one window and not three.
- **Both directions of the two-mode clash are refused loudly.** A workload runs in ONE
  intercept mode at a time — one header-filtered intercept switches the whole traffic-agent
  port to HTTP mode, where a filterless intercept qualifies for neither matching tier and is
  silently inert while still reporting `ACTIVE`. So a window opening on a workload that
  already carries a header-keyed preview is refused and alarmed rather than opened, and a
  `POST /previews` for a workload holding a scheduled intercept is refused with `409`. Both
  messages name the other side.
- **A self-health check on every open window**, because a global intercept that dies takes all
  of a workload's traffic with it rather than one header's worth, and a window is open when
  nobody is watching. Every `SCHEDULE_CHECK_INTERVAL` (30s) the controller asks the manager
  whether its own intercept still exists and in what state, and re-raises or alarms. Measured
  recovery: 1.6s from a `kill -9`, 11s from a force-deleted pod, 9s from the node-agent being
  deleted underneath it — none of the three hung, and none needed the expiry sweep.
- `GET /schedules` and `kubectl agentic-preview schedules` report every declared window,
  whether it is open, whether its intercept is up, and the problem if it is open and is not.
  Read-only: a global intercept is a thing to review in a repository, not a thing anything
  that can reach the Service may raise.
- The chart grows `schedules`, `scheduleDefaultLocation` and `scheduleCheckInterval`, refuses
  at render time to declare a schedule outside `allowedNamespaces`, and carries a checksum of
  the schedule ConfigMap so editing a window rolls the pod. `deploy/` grows `schedules.yaml`,
  deliberately not applied, with the three edits that switch it on in its header.

- **A second kind of schedule: `type: dns`.** An intercept needs a workload, so the mode above
  can only divert something that runs in the cluster. A dependency with no pod, Deployment or
  Service anywhere — a managed database behind Private Link, an appliance — has exactly one
  interception point that reaches it, the resolver. A `type: dns` schedule takes it: while its
  window is open one tagged `hosts` line (`10.42.0.9 name # agentic-preview:<schedule>`) is
  written into a CoreDNS-style ConfigMap, and on close exactly that line is removed. The
  windows, the zone, the reconcile loop, the drift check and the alarms are the SAME
  machinery — it is a second target for one scheduler, not a second scheduler.
  - Every entry now declares `type: intercept` or `type: dns`, and an existing schedule file
    needs `type: intercept` added to each of its entries. The kind is never inferred from
    which fields are filled in, and an entry that says one kind and carries the other's
    fields is refused at boot and at chart-render time, rather than quietly doing half of
    what it says.
  - `redirectTo` is a literal IP, not a Service name: resolving a name would put a Kubernetes
    lookup, and a way for it to fail at 18:32 with nobody watching, on the path of the one
    operation that must not be fragile.
  - Where it writes is configuration and has **no default** —
    `SCHEDULE_DNS_CONFIGMAP_NAMESPACE`/`_NAME`/`_KEY`, chart `scheduleDNSConfigMap`. In a
    cluster that is usually `kube-system/coredns-custom`, and defaulting to it would make
    declaring a window and editing the cluster's resolver the same act. The permission for it
    is a separate grant neither `deploy/` nor the chart creates; `deploy/rbac.yaml` carries
    the Role commented out. Without it the schedule alarms and `problem` names the ConfigMap
    it may not write — nothing panics and nothing crash-loops.
  - The drift check is the reason this is not a pair of CronJobs: while the window is open the
    line is re-read every `SCHEDULE_CHECK_INTERVAL`, and a line that has been reverted or
    rewritten is put back and counted in `reRaises`. A steady tick issues no write at all, so
    the resolver is not reloaded every 30s. Verified against CoreDNS 1.11.3: the trailing tag
    comment is ignored, `hosts` answers over `forward` regardless of the order the Corefile
    writes them in, and a neighbouring untagged entry is untouched.
- **Forcing a window on demand**, either kind: `kubectl agentic-preview override <name>
  open|closed|auto [--for 2h]`, or `POST /schedules/{name}/override`. It is a layer on top of
  the reconcile loop and not a replacement — a forced-open intercept that dies is still
  re-raised, a forced-open redirect that drifts is still re-applied and counted. It arrives on
  the same authenticated surface a preview is raised on, it cannot CREATE a schedule, and it
  lives in memory rather than surviving a restart: the schedule is the durable, reviewable
  thing, and an override is an intervention somebody is present for.

Nothing about a workload with no declared window changes: with `SCHEDULE_FILE` unset the
controller returns on its first line and no other code path is reached.

### Fixed

- **Every namespace in `ALLOWED_NAMESPACES` is now served, not just the first.** The process
  arrived at the manager once, in `allowedNamespaces[0]`, and used that one session for
  everything — but a client session is bound to the namespace it arrives in, and the manager
  answers `WatchAgentPods` for that namespace alone. Agent pods in every other allowed
  namespace were therefore never reported, no tunnel was ever opened to them, and their
  intercepts held their traffic while reporting themselves `ACTIVE`. Measured on a real
  cluster with two allowed namespaces: the first was served and the second's requests hung,
  and swapping the order moved which one hung. There is now one session per allowed
  namespace, each with its own `WatchAgentPods` stream feeding the one tunnel pool, and each
  reconnecting on its own so one namespace's manager trouble leaves the others up.
- **A multi-replica workload now gets a tunnel per replica.** The tunnel pool keyed by
  `"<workload>.<namespace>"`, so the second replica's node-agent looked like the first one
  having changed and its tunnel was rebuilt *over* the first rather than alongside it.
  Traffic reaching the replica left without one was held, not failed over: measured on a
  two-replica workload, 17 of 40 requests served and 23 hung, with `/schedules` reporting
  open, up and no problem throughout. The pool now keys by `"<podName>.<namespace>"`, as
  Telepresence's own client pool does, and holds every reported agent pod's tunnel at once —
  40 of 40 at two replicas and 60 of 60 at three, none hung. Nothing distributes traffic
  between them and nothing needs to: an intercepted request arrives on the loop belonging to
  the pod that received it.

### Changed

- `/readyz` reports **`sessions`** — one entry per allowed namespace, with that namespace's
  session id and whether its credential was minted — in place of the single `session` and
  `sessionCredential`, plus **`disconnectedNamespaces`** for any allowed namespace without a
  session. `connected` is *at least one* namespace connected rather than all of them, because
  `/readyz` is the readiness probe and failing it on one namespace's trouble would stop
  callers reaching the namespaces that are working.
- `GET /previews` reports **`agentPods`** (a list) and `agentPodsReported` in place of the
  single `agentPod`, and `/readyz`'s `tunnels` maps each workload to the list of agent pods
  carrying one. Fewer tunnels than the manager reported agent pods is the multi-replica
  failure confined to a fraction of the traffic, and a scheduled intercept now alarms on it
  rather than reporting itself up.

## v0.2.0

### Added

- **A Helm chart**, in [`charts/agentic-preview/`](charts/agentic-preview/). It
  parameterises the manifests in `deploy/` and changes nothing about what they install.
  `allowedNamespaces` is required and has no default — the chart refuses to render without
  it, with a message explaining why, rather than leaving the container to crash-loop. One
  attach Role and one build Role, with a RoleBinding each, are generated per namespace in
  that list, and the same list becomes `ALLOWED_NAMESPACES`; all three come off the one
  value so they cannot drift. Still never a ClusterRole, and there is no `replicaCount`:
  one replica is a correctness requirement, so the chart offers no way to get it wrong.
- **A chart repository**, published to the `gh-pages` branch and served by GitHub Pages at
  `https://alchemy86.github.io/agentic-preview`, ready for
  [Artifact Hub](https://artifacthub.io) to index. `charts/artifacthub-repo.yml` is
  published beside `index.yaml`, where Artifact Hub reads it from.
- The `chart` job in `release.yml` packages and publishes it on every `v*` tag, after the
  image job and stamped from the same tag, so the chart cannot advertise an image that was
  not pushed.
- A `chart` job in CI: `helm lint --strict`, `helm template`, and an assertion that
  rendering without `allowedNamespaces` still fails.
- `brand/agentic-preview-icon.png`, derived from the icon SVG — the chart's `icon`, which
  is what Artifact Hub renders on the package page.

## v0.1.0

Header-routed Telepresence preview environments in Kubernetes, raised by an HTTP call from
inside the cluster itself.

`POST` a work id, a service and an image. agentic-preview builds a preview of that service,
raises an intercept on the live workload, and diverts every request carrying that header
value to the preview. Unmarked traffic is untouched. It runs as an ordinary Deployment —
no Telepresence CLI, no connector daemon, no TUN device, no root, no laptop, no human in
the loop.

### What is in it

- **A preview is a copy of the live Deployment.** Environment, config and secret
  references, pull credentials, probes and service account are carried across because they
  were copied off the running object, not guessed at from a template.
- **The image reference is taken verbatim.** No tag conventions, no registry assumptions.
- **One work id spans many services.** Every preview under it shares the header value, and
  any contact with the work id extends all of them.
- **Survives a rollout of the workload being previewed.** The tunnel to the node-agent is
  rebuilt as the agent pod set changes. There is an 18s gap while it does; measured.
- **Refuses to build a preview that live traffic could claim**, and refuses to raise an
  intercept pointing at a Service with no ready endpoints — a create waits for the pods and
  fails the `POST` with the reason Kubernetes gave.
- **Namespaced RBAC only, never a ClusterRole.** Nothing on ConfigMaps or Secrets: the
  preview references the live ones and never reads them. `update` and `delete` are guarded
  in code by the tool's own `managed-by` label, so an object it did not create is never
  written to and never removed.
- **Clean shutdown removes every intercept and departs the session first.** Measured, every
  header falls back to the live pod in under 0.31s.

### What it deliberately does not do

Boundaries, not gaps.

- It does not trigger anything. It receives calls. Nothing watches a repository, a
  registry, a webhook or a queue.
- It has no notion of a pull request. A work id is an opaque string used as a header value
  and a label, never parsed. Nothing tears a preview down because a branch merged;
  teardown is explicit.
- It does not build or push images, manage DNS, ingress or certificates, or install
  Telepresence.

### Limits you should read before deploying it

- **It does not authenticate its callers.** ClusterIP, no Ingress, no API key. Anything
  that can reach it can raise a preview and build one. Put it where only your pipeline can
  reach it.
- **`ALLOWED_NAMESPACES` is the only fence on the forward target.** Forwarding is a plain
  `net.Dial` from this pod; Kubernetes is not in that path, so no Role can bound it. That
  variable and both sets of Roles in `deploy/rbac.yaml` must name the same set.
- **A `SIGKILL` leaves intercepts hanging.** Requests carrying those specific header values
  hang rather than falling back; nothing else is affected, and the next `POST` for that
  service clears it.
- **One replica, `strategy: Recreate`.** It does not scale out.

[Honest limits](docs/LIMITS.md) has the measurements behind all four.

### Requirements

A Telepresence traffic-manager already running in the cluster, **v2.30.0 or later** — the
node-agent reconciler that lets a preview survive a target-pod rollout landed in v2.30.0.

### Image

```
ghcr.io/alchemy86/agentic-preview:0.1.0
```

`linux/amd64` and `linux/arm64`, a static binary on `distroless/static-debian12:nonroot`.
Pin the digest rather than the tag in a cluster manifest — the digest is in the release
workflow's summary, and the README says why.

### Versioning

Pre-1.0. It works and it is proven on a real cluster, but the HTTP API may still move.
