# Changelog

## Unreleased

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

The README's [Honest limits](README.md#honest-limits) has the measurements behind all four.

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
