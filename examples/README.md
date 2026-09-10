# Examples

Four things you actually do with agentic-preview, as `curl`. They are plain shell on
purpose: the API is small enough that a wrapper would only hide it, and a pipeline step
is going to be a `curl` anyway.

All four read `AGENTIC_PREVIEW_URL` and fall back to the in-cluster address the
manifests in `deploy/` create:

```bash
export AGENTIC_PREVIEW_URL=http://agentic-preview.agentic-preview.svc.cluster.local
```

Running them from outside the cluster? The Service is deliberately ClusterIP-only, so
port-forward to it first:

```bash
kubectl -n agentic-preview port-forward svc/agentic-preview 8080:80
export AGENTIC_PREVIEW_URL=http://localhost:8080
```

| Script | What it does |
| :--- | :--- |
| `raise-preview.sh` | Add one service to a work id — the PR built, route its header |
| `list-previews.sh` | Every work id and the services it spans, plus session health |
| `drop-service.sh` | Remove one service from a work id — that repo's PR merged |
| `drop-work.sh` | Remove a whole work id — the change is done |

The examples use a fictional `checkout-api` in a namespace called `shop`. Substitute
your own; the namespace has to be in the service's `ALLOWED_NAMESPACES`, and so does the
namespace of the Service you forward to.
