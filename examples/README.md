# Examples

Five things you actually do with agentic-preview, as `curl`. They are plain shell on
purpose: the API is small enough that a wrapper would only hide it, and a pipeline step
is going to be a `curl` anyway.

All of them read `AGENTIC_PREVIEW_URL` and fall back to the in-cluster address the
manifests in `deploy/` create:

```bash
export AGENTIC_PREVIEW_URL=http://agentic-preview.agentic-preview.svc.cluster.local
```

Running them from outside the cluster? The Service is deliberately ClusterIP-only. You do
not need a tunnel for that — the API server will proxy to it:

```bash
kubectl get --raw "/api/v1/namespaces/agentic-preview/services/agentic-preview:80/proxy/previews"
```

`kubectl proxy` in one terminal turns that into an address these scripts can use:

```bash
kubectl proxy &   # serves the whole API on :8001
export AGENTIC_PREVIEW_URL=http://localhost:8001/api/v1/namespaces/agentic-preview/services/agentic-preview:80/proxy
```

For anything interactive, though, reach for
[`kubectl agentic-preview`](../hack/kubectl-agentic_preview) instead — same API, same
proxy, no URL to assemble. These scripts are here for the pipeline step, which is going to
be a `curl` from inside the cluster anyway.

| Script | What it does |
| :--- | :--- |
| `raise-preview.sh` | Build a preview of one service from an image, and route its header — the whole job in one call |
| `raise-preview-existing.sh` | Route to a preview Service you deployed yourself — the routing half only |
| `list-previews.sh` | Every work id, what it spans, what image each preview runs, when each expires |
| `drop-service.sh` | Remove one service from a work id — you are done with that repository's part |
| `drop-work.sh` | Remove a whole work id — you are done with the change |

The examples use a fictional `checkout-api` in a namespace called `shop`, and a made-up
work id of `1234`. Substitute your own; the namespace has to be in the service's
`ALLOWED_NAMESPACES`, and so does the namespace of any Service you forward to.

A work id is **any string you pick** — an issue number, a branch name, somebody's
initials. It is the header value and nothing is ever read out of it. The same goes for
the image reference: agentic-preview runs what you name, verbatim, and knows nothing
about how it came to exist or what its tag means.

Nothing tears a preview down on your behalf. A pull request merging is deliberately not
a trigger — you may well still be testing against the preview after it merges. Previews
go when `drop-service.sh` or `drop-work.sh` says so, or when nothing has touched the
work id for `PREVIEW_LIFETIME`, which is a safety net against forgotten previews rather
than a policy.
