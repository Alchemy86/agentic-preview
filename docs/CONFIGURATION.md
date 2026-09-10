# Configuration

Everything comes from the environment, so the Deployment manifest is the single place it is
set. With Helm, the values that map onto these are in
[the chart's README](../charts/agentic-preview/README.md).

| Variable | Default | What it is |
| :--- | :--- | :--- |
| `MANAGER_ADDR` | **required** | The traffic-manager's gRPC address. No default: the namespace Telepresence was installed into varies, and a wrong guess fails as an unhelpful dial timeout. |
| `ALLOWED_NAMESPACES` | **required** | Comma-separated. The boundary, in both directions. No default. |
| `HEADER_NAME` | `x-preview` | The single header every preview is routed on. Its *value* is the work id. |
| `LISTEN_ADDR` | `:8080` | Where the HTTP API listens. |
| `CLIENT_NAME` | `agentic-preview` | The client name the manager records. The orphan sweep only ever evicts sessions bearing this name. |
| `STATE_FILE` | `/var/lib/agentic-preview/session` | Best-effort record of the current session id, departed on startup. |
| `TOKEN_FILE` | the projected SA token path | The bearer token presented to the manager. |
| `REMAIN_INTERVAL` | `20s` | How often `Remain` holds the session open. |
| `RECONNECT_BACKOFF` | `5s` | Pause before rebuilding a dead session. |
| `AGENT_RECONCILE_INTERVAL` | `10s` | How often the agent-pod set is re-reconciled without a new snapshot. |
| `PREVIEW_LIFETIME` | `24h` | How long a preview lives untouched before it is swept. Any contact with a work id extends every preview in it. `off`, `never` or `0` disables expiry — do that when something else is responsible for cleaning up. |
| `PREVIEW_REAP_INTERVAL` | `1m` | How often expired previews are swept. |
| `PREVIEW_READY_TIMEOUT` | `120s` | How long a create waits for the preview's pods before failing the `POST` with the reason. `0` skips the wait. |

Two of these carry more weight than the rest, and each has its own note:

- **`ALLOWED_NAMESPACES`** is the only fence on the forward target. It is required, with no
  default, because an empty list read as "everything" is the wrong failure. See
  [Limits](LIMITS.md#allowed_namespaces-is-the-only-fence-on-the-forward-target).
- **`PREVIEW_LIFETIME`** is a safety net, not a policy. If something else is responsible
  for cleaning previews up, turn it off. See
  [Limits](LIMITS.md#there-is-a-timer-against-forgotten-previews-and-you-may-well-want-it-off).

`HEADER_NAME` is deliberately service-level and not a request field: two services of one
work id behind different header names would destroy the one guarantee a work id exists for.
