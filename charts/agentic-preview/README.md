# agentic-preview

Header-routed preview environments in Kubernetes, raised by your pipeline rather than
your laptop.

Your pipeline POSTs a work id, a workload, a namespace and an image. agentic-preview
copies the **live** Deployment, swaps in your image, puts a Service in front of the copy,
and asks the Telepresence traffic-manager to divert requests carrying one header to it.
Everything without the header reaches the live pod, unchanged and unaware.

One header value reaches every service raised under the same work id, so a change
spanning three repositories is one header, not three URLs.

- **Source, API reference and design notes:** <https://github.com/Alchemy86/agentic-preview>
- **Image:** `ghcr.io/alchemy86/agentic-preview` (public, `linux/amd64` and `linux/arm64`)
- **Licence:** Apache-2.0

## Prerequisite

A Telepresence traffic-manager already running in the cluster, **v2.30.0 or later**. The
node-agent reconciler that makes a preview survive a target-pod rollout landed in
v2.30.0; earlier managers will raise an intercept but lose it on the first roll. This
chart does not install Telepresence — see
[the Telepresence install docs](https://www.telepresence.io/docs/install/manager).

## Install

```bash
helm repo add agentic-preview https://alchemy86.github.io/agentic-preview
helm repo update

helm install agentic-preview agentic-preview/agentic-preview \
  --namespace agentic-preview --create-namespace \
  --set 'allowedNamespaces={shop,warehouse}'
```

Replace `shop` and `warehouse` with the namespaces holding the workloads you want to
preview. They are a fictional example used consistently across this project, not a
default worth keeping.

There is no default for `allowedNamespaces` and the chart **refuses to render** without
it, with a message explaining why. That is deliberate — see below.

If your traffic-manager is not in the `telepresence` namespace, say where it is:

```bash
  --set trafficManager.namespace=YOUR-MANAGER-NAMESPACE
```

Find it with `kubectl get deploy -A | grep traffic-manager`.

## `allowedNamespaces` is the safety boundary, in both directions

A preview has three ends, and they are bounded by three different things:

| End | Bounded by |
| :--- | :--- |
| Intercepting a workload | the **attach** Role, reviewed by the traffic-manager |
| Building the preview | the **build** Role, enforced by Kubernetes itself |
| Forwarding to a Service | **nothing but `ALLOWED_NAMESPACES`** |

Forwarding is a plain dial from the pod to a ClusterIP. No Kubernetes permission is
consulted for it, so no Role can bound it — that env var is the only thing that does.
This is why the value has no default, why an empty list is not read as "everything", and
why the chart stops at render time rather than letting the container crash-loop.

The chart generates **one attach Role and one build Role, with a RoleBinding each, per
entry in the list**, and passes the same list to the service. All three come off the one
value, so they cannot drift apart. **There is never a ClusterRole**: cluster-wide create
and delete on Deployments is the whole cluster, and nothing here needs to see outside the
namespaces it previews in. The chart offers no way to collapse them into one.

In the listed namespaces and nowhere else: `get`, `list`, `create`, `update` and `delete`
on Deployments and Services, and `list` on Pods. Nothing on ConfigMaps or Secrets — the
preview *references* the live ones, it never reads their contents. `update` and `delete`
are guarded in code by the tool's own `app.kubernetes.io/managed-by` label, so an object
it did not create is never written to and never removed.

Every verb is accounted for line by line in the comments of the rendered Roles:

```bash
helm template agentic-preview agentic-preview/agentic-preview \
  --set 'allowedNamespaces={shop}' | less
```

## Values

| Value | Default | What it is |
| :--- | :--- | :--- |
| `allowedNamespaces` | **required** | The namespaces a preview may be intercepted in, built in, and forwarded to. A list. No default. |
| `trafficManager.namespace` | `telepresence` | Where the traffic-manager runs. The ConnectReview Role is created here, and the manager address is derived from it. |
| `trafficManager.address` | derived | Override the manager's gRPC address for an unusual installation. Does **not** move the ConnectReview Role. |
| `image.repository` | `ghcr.io/alchemy86/agentic-preview` | |
| `image.tag` | the chart's `appVersion` | Left empty, the chart and the image are always the same release. |
| `image.digest` | `""` | Set it to pin the bytes. Wins over `tag`. |
| `image.pullPolicy` | `IfNotPresent` | |
| `image.pullSecrets` | `[]` | Only needed for a private copy of the image. The published one is public. |
| `preview.headerName` | `x-preview` | The single header every preview is routed on. Its *value* is the work id. |
| `preview.lifetime` | `24h` | How long a preview lives untouched before it is swept. `"off"` disables expiry — **quote it**, or YAML reads it as a boolean. |
| `preview.readyTimeout` | `120s` | How long a create waits for the preview's pods before failing the POST with the reason. `0` skips the wait. |
| `service.type` | `ClusterIP` | |
| `service.port` | `80` | |
| `serviceAccount.create` | `true` | |
| `serviceAccount.name` | the release full name | |
| `resources` | 25m/64Mi, 500m/256Mi | |
| `podAnnotations`, `podLabels`, `nodeSelector`, `tolerations`, `affinity` | empty | Passed through unchanged. |
| `terminationGracePeriodSeconds` | `60` | Long enough for shutdown to remove every intercept and depart the session. |

`values.yaml` carries the reasoning behind each one; this table is the summary.

### There is no `replicaCount`

Exactly one replica is a **correctness requirement**, not a preference. agentic-preview
holds one traffic-manager client session and the tunnels that serve it; two replicas
would each arrive as their own session and each try to raise the same intercepts, and the
second would collide on identical header filters rather than share the work. The
Deployment is hard-coded to one replica with a `Recreate` strategy, so the chart cannot be
asked to get it wrong.

### There is no Ingress

Anything that can reach this service can raise an intercept on any workload in
`allowedNamespaces` and point it at any Service in those namespaces. It is a ClusterIP
for callers inside the cluster, on purpose.

## `preview.lifetime` is a safety net, not a teardown policy

Nothing removes a preview because a pull request merged or closed — work carries on
against a preview after the merge, and this service has no notion of a pull request in
any case. Teardown is explicit: a preview goes when somebody `DELETE`s it.

The lifetime exists so forgotten previews do not accumulate when nobody is minding the
cluster, which is why it defaults to `24h` rather than off. Anything that touches a work
id puts every preview in that id back to a full lifetime, and `GET /previews` reports each
deadline *before* it arrives. If something else is responsible for cleaning previews up,
set `preview.lifetime: "off"` — a timer that removes a preview while somebody is still
testing against it is worse than a forgotten pod.

## Upgrading and uninstalling

```bash
helm upgrade agentic-preview agentic-preview/agentic-preview -f your-values.yaml
helm uninstall agentic-preview --namespace agentic-preview
```

`helm uninstall` removes the service and its Roles. It does **not** remove previews that
are still up — those are objects in your own namespaces that the service created. Drop
them first, or find the strays afterwards by the label the tool stamps on everything it
makes:

```bash
kubectl get deploy,svc -A -l app.kubernetes.io/managed-by=agentic-preview
```

## Without Helm

`deploy/` in the repository holds the same objects as plain manifests with four
placeholders and `kubectl apply -k deploy/`. Nothing about the service assumes it was
installed by Helm.
