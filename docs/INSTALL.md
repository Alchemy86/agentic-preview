# Installing agentic-preview

The front page has the short version of both paths. This is everything else: the
prerequisite in detail, how to pin the image you tested, the four placeholders in
`deploy/`, and how to check what came up.

## Prerequisite: a traffic-manager, v2.30.0 or later

agentic-preview does not install Telepresence — see
[the Telepresence install docs](https://www.telepresence.io/docs/install/manager).

The version floor is not arbitrary. The node-agent reconciler that makes a preview survive
a target-pod rollout landed in v2.30.0; earlier managers will raise an intercept and then
lose it on the first roll. Find your manager with:

```bash
kubectl get deploy -A | grep traffic-manager
```

## With Helm

```bash
helm repo add agentic-preview https://alchemy86.github.io/agentic-preview
helm repo update

helm install agentic-preview agentic-preview/agentic-preview \
  --namespace agentic-preview --create-namespace \
  --set 'allowedNamespaces={shop,warehouse}'
```

`shop` and `warehouse` are **fictional example namespaces**, not a default worth keeping.
Replace them with the namespaces holding the workloads you actually want to preview.

If your traffic-manager is not in the `telepresence` namespace, say where it is — the
ConnectReview Role and the manager's gRPC address are both derived from this one value:

```bash
  --set trafficManager.namespace=YOUR-MANAGER-NAMESPACE
```

A realistic values file is [`charts/example-values.yaml`](../charts/example-values.yaml),
and the full table of values, with the reasoning behind each, is in
[the chart's own README](../charts/agentic-preview/README.md). Two things are deliberately
*not* values:

- **`replicaCount`.** Exactly one replica is a correctness requirement, not a preference:
  two would each hold their own manager session and collide on identical header filters.
  It is hard-coded, with a `Recreate` strategy, so the chart cannot be asked to get it
  wrong.
- **An Ingress.** Anything that can reach the service can raise an intercept on any
  workload in `allowedNamespaces`. It is a ClusterIP for in-cluster callers, on purpose.

`image.tag` defaults to the chart's `appVersion`, and the release workflow stamps chart
version, `appVersion` and the image tag from the same `v*` tag — so the chart and the image
are the same release by construction rather than by anyone remembering. Pin the bytes with
`--set image.digest=sha256:…`; see [Pin the digest](#pin-the-digest-not-the-tag) below.

Check what you are about to install before you install it:

```bash
helm template agentic-preview agentic-preview/agentic-preview \
  --set 'allowedNamespaces={shop}' | less
```

`helm uninstall` removes the service and its Roles. It does **not** remove previews that
are still up — those are objects in your own namespaces. Drop them first, or find the
strays afterwards with:

```bash
kubectl get deploy,svc -A -l app.kubernetes.io/managed-by=agentic-preview
```

## With the raw manifests

### 1. Get the image

Every `v*` tag publishes one to GitHub's registry, for `linux/amd64` and `linux/arm64`:

```
ghcr.io/alchemy86/agentic-preview:0.1.0
```

A static binary on `distroless/static-debian12:nonroot`. The Telepresence dependency is
pinned to an exact commit in `go.mod` and fetched from the module proxy, so no local
Telepresence checkout is needed. The package is public: `docker pull` needs no login.

Building it yourself is a plain `docker build -t <your-registry>/agentic-preview .` away,
and nothing in `deploy/` assumes where the image came from.

#### Pin the digest, not the tag

A tag is a pointer that the person who owns the registry can move; `:0.1.0` today and
`:0.1.0` next month are not promised to be the same bytes, and `:latest` is not even
trying. A digest *is* the bytes — it is the content hash, so a manifest that names one
either gets exactly the image you tested or fails to pull:

```bash
docker buildx imagetools inspect ghcr.io/alchemy86/agentic-preview:0.1.0 \
  --format '{{.Manifest.Digest}}'
# ghcr.io/alchemy86/agentic-preview@sha256:...
```

The digest for each published version is printed in that release's workflow summary, under
**[Actions → Release](https://github.com/Alchemy86/agentic-preview/actions/workflows/release.yml)**.
Use the tag to find out what is current; put the digest in the YAML. With Helm, that is
`image.digest`.

### 2. Replace the four placeholders in `deploy/`

They are listed, with what each one is and where it appears, in the header comment of
[`deploy/kustomization.yaml`](../deploy/kustomization.yaml):

| Placeholder | Replace with |
| :--- | :--- |
| `telepresence` | The namespace your traffic-manager runs in — `kubectl get deploy -A \| grep traffic-manager` |
| `shop` | Each namespace agentic-preview may intercept in, build previews in, and forward to |
| `registry.example.com/agentic-preview` | `ghcr.io/alchemy86/agentic-preview@sha256:...`, or wherever you pushed your own build |
| `agentic-preview` (namespace) | Where the service itself should run, if not there |

`shop` and `checkout-api` throughout this repo are a **fictional example service**, not a
default worth keeping.

Three things have to name the same set of namespaces for the life of the deploy: the
**attach** Roles in `deploy/rbac.yaml`, the **build** Roles beside them, and
`ALLOWED_NAMESPACES` in `deploy/deployment.yaml`. They bound three different ends of a
preview — intercepting, building, and forwarding — and the third is bounded by that env var
and nothing else. The reason is in
[Limits → `ALLOWED_NAMESPACES` is the only fence on the forward target](LIMITS.md#allowed_namespaces-is-the-only-fence-on-the-forward-target),
and it matters. This is the bookkeeping the chart does for you, off one list.

### 3. Apply it

```bash
kubectl apply -k deploy/
```

## What the permissions allow, in one paragraph

The same on both paths. In the listed namespaces and nowhere else: `get`, `list`, `create`,
`update` and `delete` on Deployments and Services, and `list` on Pods. Never a ClusterRole.
Nothing on ConfigMaps or Secrets — the preview *references* the live ones, it never reads
their contents. Every verb is accounted for line by line in
[`deploy/rbac.yaml`](../deploy/rbac.yaml) and in
[Design → Authentication and RBAC](DESIGN.md#authentication-and-rbac); `update` and
`delete` are both guarded in code by the tool's own `app.kubernetes.io/managed-by` label,
so an object it did not create is never written to and never removed.

## Check it came up

It is ready only once a manager session exists, so a green `/readyz` means it can actually
raise something:

```bash
kubectl -n agentic-preview rollout status deploy/agentic-preview
kubectl -n agentic-preview port-forward svc/agentic-preview 8080:80
curl -sS localhost:8080/readyz
```

## Running it where deny-all NetworkPolicies apply

The manifests assume the namespaces involved have no default-deny egress. If yours do, see
[Design → Running it where deny-all NetworkPolicies apply](DESIGN.md#running-it-where-deny-all-networkpolicies-apply)
for the four flows to open.
