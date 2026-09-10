<p align="center">
  <img src="../brand/agentic-preview-logo.svg" width="760"
       alt="agentic-preview — the wordmark closed by a phosphor full stop, over a row of six equal lanes with only the second one lit: the same row of services as always, one of them diverted by a header" />
</p>

---

# agentic-preview — design and measured behaviour

`agentic-preview` raises header-routed telepresence previews from an HTTP call. No
telepresence CLI, no connector daemon, no TUN device, no root, no laptop, no human. It
runs as an ordinary Deployment and a pipeline drives it.

It takes over **only the intercept half** of a preview. Something else — your CI pipeline,
a Helm chart, a script — must already have deployed the preview build of the service and
given it a `Service` to be reached on. agentic-preview's job starts there: it tells the
traffic-manager to divert requests carrying one header to that Service, and holds the
tunnel that carries them.

> **About the numbers in this document.** Every figure quoted here — the 0.31s fall-through,
> the 18s rollout window, the hang after 15s and 90s, the 24h TTL — was measured on a real
> Kubernetes cluster running telepresence, not estimated and not read off a datasheet. The
> examples are written around a fictional `checkout-api` in a namespace called `shop`
> because the cluster it was measured on is not yours; the behaviour is not fictional.

---

## The one design fact that shapes everything

`InterceptSpec.target_host` reads like an address the traffic-agent connects to. **It is
not. The agent never dials it.**

For each intercepted request the agent opens a tunnel back to the *client session* and
sends the destination down it as part of the connection ID; the dial happens at the
client's end, with a plain `net.Dial` (`pkg/tunnel/dialer.go`, `DefaultDialer`). So the
destination of a preview is not a value you hand the manager — it is *a process holding a
session and answering dial requests*. Put that process in the cluster and `net.Dial`
reaches any ClusterIP.

That is why this is a long-lived service and not a one-shot API call, and it is why
`target_host` must be a **literal IP**: the agent parses it with `iputil.ParseAddr`
(`cmd/traffic/cmd/agent/fwd/tcp.go`) and fails the intercept on a name. agentic-preview
resolves the preview Service itself before creating the intercept.

**One tunnel per agent pod per session, not per preview.** Because each dial request
carries its own destination, a single dial loop already forwards each request to whichever
preview matched. Two work ids previewing one workload therefore share one node-agent Job
and one tunnel — measured. Opening a second `WatchDial` for the same agent and session
would only fight the first for the same slot.

---

## The API

A work id is the primary key, and it is the header **value**.

A change spans repositories — the API, the worker, a shared client library — as separate
PRs, and all of them must be reachable behind *one* header or the feature cannot be tested
end to end. So the caller supplies a work id, whatever id your issue tracker gives one
piece of work, never a PR number, and services join that id incrementally as each repo's
PR builds.

The unit is a **service, never a repository**: one namespace commonly holds a dozen
services, and a PR touching only checkout must preview checkout and nothing else. The
caller states the service; agentic-preview never infers a service set from a repo.

```
POST   /previews                                  add one service to a work id
GET    /previews                                  every work id and what it spans
GET    /previews/{workId}                         one work id's service set
DELETE /previews/{workId}/{namespace}/{workload}  that repo's PR merged
DELETE /previews/{workId}                         the whole change is done
GET    /healthz                                   liveness
GET    /readyz                                    ready only once a manager session exists
```

```bash
curl -XPOST http://agentic-preview.agentic-preview.svc.cluster.local/previews \
  -H 'content-type: application/json' \
  -d '{"workId":"4821","workload":"checkout-api","namespace":"shop",
       "previewService":"checkout-api-preview-pr4821"}'
```

`previewService` accepts `name` or `name.namespace`, and defaults to the workload's
namespace. `previewPort` defaults to 80; `port` (the port identifier on the intercepted
workload) defaults to 80. POSTing the same work id **adds** to its set; POSTing the same
work id *and* service refreshes that one entry. It never replaces the set.

**The header name is service-level configuration, not a request field** (`HEADER_NAME`,
default `x-preview`). That is deliberate: if two services under one work id could be given
different header names, the one thing a work id exists to guarantee — that a single header
reaches all of them — would be lost.

---

## What it does that a one-shot script cannot

- **Survives a target-pod rollout.** The manager reconciles node-agent Jobs against the
  workload's live pod set for as long as an intercept claim exists (1s debounce, 30s
  resync — `cmd/traffic/cmd/manager/state/nodeagent_watch.go`, present since v2.30.0), so
  a roll reaps the old Job and creates one for the new pod by itself. The intercept is
  manager state keyed by name and session and survives untouched. agentic-preview consumes
  `WatchAgentPods` and rebuilds its tunnel when the agent pod's name, IP or randomised API
  port changes. Measured: a forced pod replacement moved the Job across nodes and ports and
  routing came back on its own.
- **Reconnects.** Each pass of the session loop builds a whole session and reconciles every
  registered preview onto it, so a manager restart or an expired session is the same code
  path as the first connection rather than a special case.
- **Many previews per process.** One session, one tunnel per agent pod, any number of
  previews.
- **Sweeps its own orphans** — see below.
- **Token-first.** Bearer ServiceAccount token on every manager call (re-read from disk each
  time, because a projected token is rotated in place), `GetSessionCredential` for a
  verified `WatchDial`, and both Roles enforcing mode reviews.

---

## Authentication and RBAC

The manager reads one gRPC metadata header, `authorization: bearer <token>`, and resolves
it with a Kubernetes TokenReview.

The cluster this was measured on ran `AUTHENTICATION_MODE=permissive` — the telepresence
default — where an **unauthenticated caller is let through** without a Principal and
authorization is skipped entirely. So under permissive the Roles below are never consulted.
They are declared anyway: the identity should be honest whether or not anything is
checking, and a later move to `enforcing` then costs agentic-preview nothing.

| Review | Grant | Namespace |
| :--- | :--- | :--- |
| `ConnectReview` | `create` `connections.telepresence.io` | the traffic-manager's own |
| `AttachmentReview` | `create`, `get` `attachments.telepresence.io` | each intercept namespace |

**`ALLOWED_NAMESPACES` IS the boundary, and it bounds BOTH directions** — the namespace a
preview is intercepted in, and the namespace it is forwarded to. Both are checked, and the
refusal says which of the two it means, because they are different request fields
(`namespace` against `previewService`/`previewNamespace`) and a vague message sends a
caller to change the wrong one. `ALLOWED_NAMESPACES` is required and has no default — an
empty list read as "everything" is the wrong failure.

**The two directions are not bounded by the same thing, which is the part worth
understanding.** The attachment Roles bound interception only: intercepting a workload is
an operation the manager authorizes. Forwarding is not — the far end of the tunnel is a
plain `net.Dial` from this pod to a ClusterIP, and no Kubernetes permission is consulted
for it. So the forward target is bounded by the in-process check and nothing else, in
enforcing mode exactly as in permissive. Keep the Roles and `ALLOWED_NAMESPACES` in step:
the Roles are the only thing standing between the list and interception, and the list is
the only thing standing between a caller and forwarding anywhere in the cluster.

The attachment Role is left unnamed (no `resourceNames`) because the manager also performs
an unnamed namespace-wide attachments review (`auth.NamespaceReview`), which a grant scoped
with `resourceNames` never matches. Adding `resourceNames: [checkout-api, ...]` tightens it
to a fixed workload list at the cost of failing that review.

The Service is ClusterIP with no Ingress: anything that can reach it can raise an intercept
on any workload in `ALLOWED_NAMESPACES`, and point it at any Service in those same
namespaces.

### `GetSessionCredential` may not exist on your manager

agentic-preview calls it and degrades cleanly. The manager measured against was
`ghcr.io/telepresenceio/tel2:2.31.1` — the released tag — and that RPC landed ~50 commits
*after* it on `release/v2`, so the call returns `Unimplemented` (observed, not inferred).
Agent calls are then unverified, which permissive agents accept. Under an enforcing agent a
verified credential would be required, and the code path is already there for when the
manager is new enough to mint one.

---

## Cleanup, and the one thing to know

**A stop and a kill are not the same, and the difference is measured.**

On **SIGTERM** — every rollout, scale-down, drain and eviction — agentic-preview removes each
intercept and departs its session before exiting. Measured: every header falls straight
through to the live pod in under 0.31s, and zero node-agent Jobs are left behind.

On a **forced kill** the intercepts stay in manager state with nobody holding their tunnel,
and **requests carrying that header hang — they do not fail open.** It is tempting to read
the agent's fail-open branch as covering a dead client. It does not. The agent's
`CreateClientStream` retries "no dial watcher" on a constant 20ms backoff with **no max
elapsed time** (`cmd/traffic/cmd/agent/server.go:176-193`), so `errClientStream` — the
branch that fails open to the app container at `fwd/http.go:288-293` — is never reached
when there is no client at all. Fail-open covers a *broken* stream, not an *absent* one.
Measured twice: no response after 15s and after 90s, while unmarked traffic served normally
in 1.5s.

So an orphaned intercept is harmless to everyone *except* the header it answers, which
becomes a black hole until something clears it. Nothing else is affected — no wrong answers
to anybody, no other traffic touched.

**The sweep that clears it needs no saved state.** The obvious approach does not work:
`WatchIntercepts` is filtered to the calling session's own intercepts and rejects an empty
session id outright (`InvalidArgument`, "a session id is required"), and `ArriveAsClient`
always mints a fresh UUID, so a restarted agentic-preview can neither enumerate nor re-enter its
predecessor's session. What does work is the manager's own conflict error, which names the
blocking session:

```
conflict with intercept 3f2a91c0-1111-2222-3333-444455556666:checkout-api-4821 on port 80
created by client "agentic-preview": header filters overlap
```

On that error agentic-preview departs the named session and retries once — releasing every
intercept that session held. It fires only when the blocking client's name is agentic-preview's
own and the session is not the current one, so it can never evict a developer's intercept;
`Depart` requiring the same Principal is the second gate. Measured: one re-POST cleared
three orphans and restored routing.

There is also a best-effort session id recorded to `STATE_FILE`, departed on startup. It is
on an `emptyDir`, so it survives a container restart but not a pod replacement — the
conflict-driven sweep above is the one that matters.

`CLIENT_CONNECTION_TTL` on the measured cluster was **24h** (verified on the traffic-manager
deployment; the GC loop expires client sessions on that TTL every 5s, agent sessions after
70s). It is deliberately left alone: shortening it would make an orphan self-heal sooner,
but it is the same setting a developer's own telepresence session depends on.

### One caveat worth knowing

During a target-pod rollout there is a window — 18s in the measured run — between the old
node-agent Job dying and the tunnel being re-established to the new one. Requests carrying
a preview header in that window hang rather than falling back to live, for the same reason
as above. Unmarked traffic is unaffected throughout.

---

## Operating notes

- **Exactly one replica, `strategy: Recreate`.** Two replicas would each arrive as their
  own session and each try to raise the same intercepts, colliding on identical header
  filters rather than sharing the work; a rolling update would briefly do the same.
- agentic-preview needs no Kubernetes API access beyond its own projected token. It resolves
  preview Services by **DNS**, which is why `previewPort` is a request field: it cannot read
  the Service to discover the port. Because DNS will resolve a Service in any namespace, the
  forward target is checked against `ALLOWED_NAMESPACES` before it is resolved, and both
  halves of the address must be plain DNS labels so an FQDN cannot be smuggled in as the
  namespace.
- Build: `docker build .` — a static binary on distroless-nonroot. The telepresence
  dependency is pinned to an exact commit in `go.mod` and fetched from the module proxy; no
  local telepresence tree is needed.

### Running it where deny-all NetworkPolicies apply

The manifests in `deploy/` assume the namespaces involved have no default-deny egress. If
yours do, agentic-preview needs, and none of it is in the manifests because the shape depends
on your policies:

- egress to the traffic-manager on 8081;
- egress to the node-agent pods in the traffic-manager's namespace on their **randomised
  per-Job API ports** — this is the awkward one, and it wants deciding before it is
  attempted;
- egress to every preview Service it forwards to;
- matching ingress on each of those.
