# The `kubectl` plugin

`kubectl agentic-preview` is [one shell script](../hack/kubectl-agentic_preview) over
`kubectl`. It raises, lists and drops previews from a terminal without a port-forward, a
tunnel, an extra pod or a URL to know.

`kubectl agentic-preview --help` is the command reference and this page does not repeat
it. What is here is how it gets onto your PATH, how it reaches the cluster, and what it
needs permission to do.

## Installing it

kubectl finds a plugin by filename: any executable on PATH called `kubectl-agentic_preview`
becomes the command `kubectl agentic-preview`. The underscore is what kubectl turns into
the space — it is not a typo, and renaming it to a dash breaks the command.

```bash
curl -fsSLo ~/.local/bin/kubectl-agentic_preview https://raw.githubusercontent.com/Alchemy86/agentic-preview/main/hack/kubectl-agentic_preview
chmod +x ~/.local/bin/kubectl-agentic_preview
```

From a clone, symlink it instead and it tracks the branch you are on:

```bash
ln -s "$PWD/hack/kubectl-agentic_preview" ~/.local/bin/kubectl-agentic_preview
```

Either way, `kubectl plugin list` should now show it, and `kubectl agentic-preview --help`
should print. If kubectl says `unknown command`, the directory is not on your PATH.

## How it reaches the service

The agentic-preview Service is ClusterIP-only and deliberately has no Ingress. The plugin
does not tunnel to it. It asks the API server to proxy:

```
GET /api/v1/namespaces/<service-ns>/services/<service>:80/proxy/previews
```

which is `kubectl get --raw`, `kubectl create --raw` and `kubectl delete --raw` against
that path and nothing more. Three things follow from that, and they are the reason it is
built this way:

- **It has no credentials of its own.** It inherits your kubeconfig, your current context,
  your auth plugin and your proxy settings, because it is your `kubectl` making the call.
  Switch context and the plugin follows.
- **It adds no dependency.** Not a Go client, not `jq`, not `curl` — just the `kubectl`
  that is already running it.
- **It can do nothing the API cannot.** Every call has an equivalent in
  [`examples/`](../examples/). The plugin assembles the JSON body and the path; it decides
  nothing else.

### Where it looks

Defaults match what `deploy/` and the chart install: a Service called `agentic-preview` in
a namespace called `agentic-preview`. Two ways to point it elsewhere, per-call or for good:

| Flag | Environment variable | What it names |
| :--- | :--- | :--- |
| `--service-namespace` | `AGENTIC_PREVIEW_NAMESPACE` | The namespace agentic-preview itself runs in |
| `--service` | `AGENTIC_PREVIEW_SERVICE` | Its Service name |

Note that `-n` / `--namespace` is *not* one of these: it is the namespace of the workload
you are previewing, which is the namespace a user actually thinks about. It is required on
`up`, because there is no safe default for it — it has to be one of the service's
`ALLOWED_NAMESPACES`, and those are different in every installation.

## Permissions it needs

Proxying to a Service is its own subresource. A user who can otherwise read the cluster
may still get `forbidden` here:

```yaml
rules:
  - apiGroups: [""]
    resources: ["services/proxy"]
    verbs: ["get", "create", "delete"]
```

in the namespace agentic-preview runs in. Nothing else — the plugin never touches the
namespaces being previewed, and never reads a Deployment, a Pod or a Secret. agentic-preview
does all of that itself, with [its own service account](DESIGN.md#authentication-and-rbac).

That also means the plugin is not an authorization boundary and was never meant to be.
Anything you can do through it, you can do with `kubectl get --raw` by hand; anyone who can
reach the Service at all can raise an intercept on any workload in `ALLOWED_NAMESPACES`.
The service [does not authenticate its callers](NON-GOALS.md) — where you put it is the
control.

## Where the work id comes from

`-w` is the header value and the key everything is grouped under. With no `-w`, the plugin
takes it from the image **tag**, which is usually the branch or PR name, and says so on
stderr before it uses it.

It reads the tag only where there genuinely is one — after the last colon of the last path
element, so `registry.example.com:5000/checkout-api` is not mistaken for a tag of `5000`.
Three cases are refused rather than guessed at, because a wrong guess here silently routes
a header nobody expects:

- a **digest-pinned** image (`checkout-api@sha256:…`) — a digest makes a useless header,
- an **untagged** image — there is nothing to read,
- the tag **`latest`** — every caller's `latest` would collide on one header.

In all three, pass `-w`. This is the plugin's own convenience and the API has no part in
it: `workId` is required there, and is never parsed by the service.

## Staying in step

The plugin covers the whole API surface — seven endpoints across `up`, `list`, `down`,
`status`, `schedules` and `override`. A new endpoint is a new subcommand, or a decided and
stated reason not to add one; see [`AGENTS.md`](../AGENTS.md).

`schedules` is read-only because the endpoint is: a [schedule](SCHEDULES.md) is declared in
the service's config, not raised from a command line, because it diverts all of a workload's
traffic — or a whole DNS name — rather than one header's worth. There is deliberately no
`up --global` for the same reason.

`override` forces a schedule that is ALREADY DECLARED open or closed now, and cannot create
one. It is here rather than on a surface of its own precisely because of the paragraph above
this one: where you put the Service is the control, and a second door would be a second
place to have to put it.
