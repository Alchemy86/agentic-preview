# Honest limits

All of these are worth reading *before* you deploy it, not after. The first three were true
when it only did routing; the rest arrived with its ability to create workloads, which is
the part with consequences that outlive the request.

Every figure here was measured on a real Kubernetes cluster running Telepresence. The
code-level evidence behind the first two is in
[Design → Cleanup, and the one thing to know](DESIGN.md#cleanup-and-the-one-thing-to-know).

## A forced kill leaves headers hanging, and it does not fail open

On `SIGTERM` — every rollout, scale-down, drain and eviction — the service removes each
intercept and departs its session first; measured, every header falls straight through to
the live pod in under 0.31s with no node-agent Jobs left behind. On a `SIGKILL` it cannot.
The intercepts stay in manager state with nobody holding their tunnel, and **requests
carrying those headers hang.** The agent's fail-open branch covers a *broken* client
stream, not an *absent* one: with no dial watcher at all it retries on a constant 20ms
backoff with no maximum elapsed time and never reaches the fail-open path. Measured twice —
no response after 15s and after 90s, while unmarked traffic served normally in 1.5s.

The damage is narrow and it is worth being precise about: only the *specific header value*
is affected. No other traffic is touched, and nobody gets a wrong answer. The next `POST`
for that service clears it — the manager's conflict error names the dead session, and
agentic-preview departs it and retries once, releasing everything that session held.

**A SCHEDULED intercept is the exception to "the damage is narrow", and it is the reason
[scheduled intercepts](SCHEDULES.md) have a health check of their own.** A global intercept
carries no header filter, so "that specific header value" is all of the workload's traffic,
and a window is open precisely when nobody is watching. The controller therefore re-checks
its own intercept against the manager every `SCHEDULE_CHECK_INTERVAL` (30s) rather than
relying on either safety net above — measured recovery from a `kill -9` was 1.6s and from a
force-deleted pod 11s, neither of which hung. The numbers and the three cases are on
[that page](SCHEDULES.md#measured).

## There is a window during a target-pod rollout

When the workload you are previewing rolls, the manager reaps the old node-agent Job and
provisions one for the new pod, and agentic-preview rebuilds its tunnel to it. That recovers
on its own — but there is a gap first: **18s in the measured run.** Requests carrying a
preview header in that window hang rather than falling back to live, for the same reason as
above. Unmarked traffic is unaffected throughout.

## A multi-replica workload needs a tunnel per replica

Each replica of an intercepted workload gets its own node-agent Job, and each Job holds its
own dial stream. Traffic reaching a replica whose agent has no tunnel is **held**, not
failed over — the intercept on that pod is `ACTIVE`, so the agent diverts the connection and
waits for a client that is not listening.

This was measured on a two-replica workload with a pool keyed by workload rather than by
agent pod: **17 of 40 requests served, 23 hung**, against 20 of 20 at one replica — and
`/schedules` reported `open`, `up` and no problem throughout, because one tunnel existed.
With a tunnel per agent pod the same run is 40 of 40 at two replicas and 60 of 60 at three,
with no hung requests. `/readyz` lists every agent pod holding a tunnel, and each preview
reports `agentPods` alongside `agentPodsReported` — the count the manager gave for that
workload — so fewer tunnels than replicas is visible rather than silent, and a scheduled
intercept alarms on the difference.

The gap that remains is the provisioning lag, not the pool: a replica that arrives before
its Job does serves its own traffic until the manager provisions one (measured mid-rollout:
half the requests reached the live workload, none hung). That is the same window as the
rollout case above, and it fails open.

## A session only ever sees its own namespace's agent pods

A client session is bound to the namespace it arrives in, and the traffic-manager answers
`WatchAgentPods` for that namespace and no other. With one session for the whole process,
every namespace in `ALLOWED_NAMESPACES` but the one the session arrived in was a namespace
whose agent pods were never reported — so no tunnel was ever opened there, and its
intercepts **held** their traffic exactly as an un-tunnelled replica does above.

It was silent in the worst way. The intercept was created, went `ACTIVE` and stayed
`ACTIVE`; the manager provisioned the node-agent Jobs and the agents connected to it; and
`/schedules` reported `open` and `up`. Measured on a real cluster with two allowed
namespaces: the first namespace in the list was served and the second one's requests hung,
and **swapping the order moved which namespace hung** rather than fixing either — which is
what ruled out RBAC, the node-agent Jobs and the schedule, none of which know anything about
list order.

agentic-preview now holds one session per allowed namespace, each with its own
`WatchAgentPods` stream feeding the one tunnel pool, and each reconnecting independently.
`/readyz` reports a session per namespace and names any namespace under
`disconnectedNamespaces` that has none — a namespace listed there can be intercepted in and
never tunnelled to, which is the state this entry is about.

Readiness is deliberately *at least one* namespace connected rather than all of them:
`/readyz` is the readiness probe, and failing it would take the pod out of its Service, so
an all-or-nothing reading would let one namespace's manager trouble stop callers reaching
the namespaces that are working.

## `ALLOWED_NAMESPACES` is the only fence on the forward target

This is the one to understand properly. A preview has three ends, and they are *not*
bounded by the same thing:

- The end that **intercepts** is authorized by Kubernetes. The attach Roles in
  [`deploy/rbac.yaml`](../deploy/rbac.yaml) are what the traffic-manager reviews before it
  will let this service intercept a workload.
- The end that **builds** is authorized by Kubernetes too, and this one is enforced
  whatever the manager is doing. The build Roles in `deploy/rbac.yaml` are namespaced, so
  even if the in-process check were wrong, the API server would refuse a create outside the
  listed namespaces. The in-process check is still there, and states the same boundary at
  the point of use.
- The end that **forwards** is not authorized by anything. It is a plain `net.Dial` from
  this pod to a ClusterIP, and no Kubernetes permission is consulted for it. No Role can
  bound it, because Kubernetes is not in that path at all.

So the in-process check against `ALLOWED_NAMESPACES` is the *only* thing standing between a
caller and forwarding intercepted production traffic to any Service anywhere in the
cluster. It is checked in every direction and required with no default — an empty list read
as "everything" is the wrong failure — and the refusal names *which* namespace it means,
because they are different request fields and a vague message sends a caller to change the
wrong one. Keep both sets of Roles and the env var naming the same set.

## One more thing worth saying plainly about the build permissions

`delete` on Deployments in a namespace is `delete` on *any* Deployment in that namespace;
RBAC cannot be narrowed by label. What narrows it is the code: every delete first *lists*
by the tool's own `managed-by` label, then re-checks that label on each object before naming
it, and deletes by name — never `deletecollection`, never a selector the API server could
interpret more widely than intended. `update` is guarded the same way, so an object the tool
did not create is never written to either. Both guards have tests. The namespace boundary is
Kubernetes’; the "only its own objects" boundary is this code’s, and it is stated here
rather than assumed.

## A preview can be built and still not run, and the pod is what tells you

Creating a Deployment always succeeds; whether its pods start is a separate question
answered ten seconds later. So a create waits — `PREVIEW_READY_TIMEOUT`, 120s by default —
and if the pods have not come up it fails the `POST` with the reason Kubernetes gives, and
raises no intercept. That is deliberate: an intercept pointed at a Service with no endpoints
turns a broken build into a hanging header, which is a far worse way to find out. The three
failures this design exists to avoid — an image reference that does not resolve, no
credentials to pull it, a pod that dies instantly for want of configuration — all surface
here as `ErrImagePull`, `ImagePullBackOff` or a crash loop. **The objects are left in place
so you can look at them**; `DELETE /previews/{workId}` clears them.

## The preview's pod labels are copied from live, and that is a hazard the tool refuses rather than manages

A copied pod template carries every label the live pods carry, including whatever the live
Service selects on. A preview pod that still matches it joins the live EndpointSlice, and
unreviewed code serves live traffic to everybody, silently. So before creating anything,
agentic-preview checks the preview's pod labels against the selector of **every** Service in
the namespace and refuses if any of them would claim it. It overrides `app` and adds its own
unique label, which covers the ordinary case; a workload whose Service selects on something
else entirely gets a refusal naming the Service and the selector, and you either change what
that Service selects on or deploy the preview yourself with `previewService`. It will not
create a pod it cannot prove is isolated.

## There is a timer against forgotten previews, and you may well want it off

`PREVIEW_LIFETIME` sweeps a work id that nothing has touched for 24 hours — intercepts and
created objects together, by the same path a `DELETE` takes. Any contact with an id —
raising a service under it again, adding another — puts the whole id back to a full
lifetime, because a work id is one change and its services are used together.
`GET /previews` reports each preview's age and, when a lifetime is set, exactly when it
expires, so an expiry is visible *before* it happens and can be extended rather than
discovered afterwards.

24 hours is the default because it is the safe answer for somebody with nobody minding
their cluster. **If something else is responsible for cleaning previews up, turn it off** —
`PREVIEW_LIFETIME: off` — because a timer that removes a preview while somebody is still
testing against it is worse than a forgotten pod. Off is an explicit choice on purpose:
leave the configuration alone and you get the timer. With expiry off, the age in
`GET /previews` is what a person or a supervising process reviews instead.

## A restart puts previews out of the timer's reach, and the labels are the answer to that

The record of what is live is in memory, so a restarted process no longer knows about the
previews its predecessor raised — and will not expire them. It has not lost them, though:
everything it creates carries `app.kubernetes.io/managed-by=agentic-preview` and the work
id, so a stray is always findable and always removable —

```bash
kubectl get deploy,svc -A -l app.kubernetes.io/managed-by=agentic-preview
curl -XDELETE .../previews/1234    # goes by label, not by memory
```

— and re-POSTing the same work id adopts the existing objects and rolls them forward rather
than colliding with them. It is a real gap all the same: after a restart, cleanup is
somebody asking, not a timer. Previews are also **not** removed on `SIGTERM`, deliberately —
destroying every preview in the cluster on each rollout of this service would be its own
outage.

## If you expire preview images, exclude the ones in use

`GET /previews` reports the image each preview it built is running, precisely so an
image-retention policy can skip them. A preview that outlives its image keeps working until
its pod is replaced — a node drain, an eviction, a rollout — and then cannot pull, and the
failure looks like a fault in this service. It is not one. agentic-preview deliberately
knows nothing about image retention; it just tells you what is in use.

## `GetSessionCredential` may not exist on your manager

A smaller one. It landed after the v2.31.1 release tag. agentic-preview calls it, logs the
`Unimplemented`, and carries on with unverified agent calls, which permissive agents accept.
An enforcing agent would require it. The detail is in
[Design → `GetSessionCredential` may not exist on your manager](DESIGN.md#getsessioncredential-may-not-exist-on-your-manager).
