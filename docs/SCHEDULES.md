# Scheduled, headerless intercepts

Everything else in this service is **header-keyed**: a preview exists, one header value
reaches it, and everything without that header reaches the live pod unaware. This page is
the one mode that is not. A **schedule** gives one target a recurring window, and while
that window is open *every* request to it is diverted somewhere else — no header, nothing
to call, nobody awake.

There are two **kinds**, and every entry says which it is in `type:`:

| `type:` | what it diverts | how |
| :--- | :--- | :--- |
| `intercept` | one Kubernetes workload | a global, headerless telepresence intercept |
| [`dns`](#dns-redirect-mode) | one DNS name | one `hosts` line in a CoreDNS-style ConfigMap |

The name, the windows and the time zone mean the same thing in both, and there is one
implementation of them — a DNS schedule is not a second scheduler, it is a second target
for the one that already exists, with the same reconcile loop, the same drift check and the
same alarms behind it.

It is off unless you declare it. With no `SCHEDULE_FILE` the controller returns on its first
line and no workload behaves differently in any way.

> **Read [Two modes, one workload](#two-modes-one-workload-and-they-do-not-compose) before
> you declare one.** It is the constraint that decides whether this mode is usable for a
> given service at all, and it is not obvious from the outside.

---

## What it is for

The case it was built for: a dependency that is switched off outside working hours, taking
a service down with it. A stand-in can answer for that service while it is gone, but nothing
in the estate should have to know about the swap — no connection string edited, no
deployment changed, no code aware of it. A global intercept does exactly that, at the pod
port, for the hours you name.

## Declaring one

With Helm, a list under `schedules` in your values:

```yaml
allowedNamespaces:
  - shop

scheduleDefaultLocation: Europe/London

schedules:
  - name: offhours            # the id it is reported under, and reserved as a work id
    type: intercept           # required, and never inferred from the fields below
    workload: checkout-api
    namespace: shop
    port: "8443"              # port identifier on the workload; default 80
    # service: checkout-tls   # only when the workload has two Services on that port
    targetService: auth-stub  # or auth-stub.previews
    targetPort: 8443
    windows:
      - days: [Mon, Tue, Wed, Thu]
        start: "18:32"
        end:   "07:21"
      - days: [Fri]
        start: "18:32"
        duration: 60h49m
```

With the plain manifests, the same YAML goes in `deploy/schedules.yaml`, which ships with
that example filled in and its own header explaining the three edits that switch it on.

`type` is **required on every entry** and is never inferred from which fields are filled in.
An entry that says `intercept` and carries a `hostname`, or says `dns` and carries a
`workload`, is refused at startup rather than quietly doing half of what it says — whichever
half won silently would be the one its author was not thinking about.

Either way the file is read **once at startup and never reloaded**: a window that opens at
18:32 has to have been proved parseable at 09:00, not discovered to be unparseable at 18:32.
Anything wrong in it — a namespace off the allow-list, a day that is not a day, two
schedules on one workload — is fatal at boot, where somebody notices, rather than a window
that silently never opens. The chart's Deployment carries a checksum of the ConfigMap, so
editing a schedule rolls the pod for you, and the new schedule is re-validated as it starts.

### Windows

`days` are the days a window **opens** on, never the days it covers.

| | |
| :--- | :--- |
| `days` | `Mon`…`Sun` in any case, long or short, plus the aliases `weekdays`, `weekends`, `daily` |
| `start` | opening wall-clock time, `HH:MM` or `HH:MM:SS` |
| `end` | closing time. At or before `start` means the **next day**, so `18:32` → `07:21` is the overnight it reads as. `00:00` → `24:00` is the whole day |
| `duration` | how long it stays open, as a Go duration. Exactly one of `end` and `duration` |

`duration` exists because `end` cannot say everything. An overnight reads naturally as an end
time; a span from Friday evening straight through to Monday morning is not expressible as one
at all — it is *one* window of `60h49m` from `[Fri] 18:32`, not three windows. Several windows
per schedule are allowed and they may overlap; the intercept is up if any of them covers the
moment.

Times are read in `location` if the schedule names one, otherwise `scheduleDefaultLocation`,
otherwise UTC. Say which one you mean: a window written in local time and read in UTC is
silently an hour wrong for half the year, which is the worst way for this to fail. The zone
database is compiled into the binary, so any IANA name works on the distroless image.

### The same fence, in both directions

`ALLOWED_NAMESPACES` bounds a schedule exactly as it bounds a preview — both the namespace
it intercepts in and the namespace of the `targetService` it diverts traffic to, because that
target is a plain `net.Dial` to a ClusterIP that no Kubernetes permission is consulted for.
The Helm chart refuses to render a schedule outside the list, and the service refuses to
start on one.

## Reading it back

```
kubectl agentic-preview schedules
```

or `GET /schedules`, which is read-only. A window is declared in config and cannot be raised
over the API: a global intercept takes every request to a workload, so it is a thing to review
in a repository, not a thing anything that can reach the Service may ask for.

```json
{
  "checkInterval": "30s",
  "file": "/etc/agentic-preview/schedules.yaml",
  "schedules": [{
    "name": "offhours", "kind": "intercept",
    "workload": "checkout-api", "namespace": "shop", "port": "8443",
    "target": "auth-stub.shop:8443",
    "open": true, "up": true,
    "since": "2026-09-12T18:32:00Z", "nextChange": "2026-09-13T07:21:00Z",
    "raises": 1, "reRaises": 0
  }, {
    "name": "db-offhours", "kind": "dns",
    "hostname": "dev-db.example.internal", "redirectTo": "10.42.0.9",
    "target": "kube-system/coredns-custom[host.override]",
    "open": true, "up": true,
    "since": "2026-09-12T18:32:00Z", "nextChange": "2026-09-13T07:21:00Z",
    "raises": 1, "reRaises": 0
  }]
}
```

**`problem` is the field to watch, and empty is the only good value while `open` is true.**
A climbing `reRaises` is the other one: it counts the times the health check below found the
intercept dead and put it back, so a number that grows is something killing intercepts
underneath the controller.

A DNS entry reports no `workload`, `port` or intercept of its own — it has none, and an
empty one would send somebody looking for it. What both kinds report is the same four
things: the target, `open`/`up`, `since`/`nextChange`, and `problem`.

A scheduled intercept also appears in `GET /previews` under the schedule's name, marked
`"global": true` and with **no expiry** — its life is its window and nothing else, so
`PREVIEW_LIFETIME` does not apply to it and a `DELETE` of its name is refused.

---

## DNS-redirect mode

`type: dns`. Everything above diverts a **workload**. This kind diverts a **name**.

### What it is for

An intercept needs a workload: the traffic-agent is a sidecar on a pod, so there has to be a
pod. The case that breaks is a dependency that is not a workload at all — a managed database
reached over Private Link, an appliance, anything with a DNS name, an address, and no
Deployment, Service or pod anywhere in the cluster to attach to. There is exactly one
interception point that reaches it, the resolver, and this is the mode that takes it.

While the window is open one line is written into a CoreDNS `hosts` block:

```
10.42.0.9 dev-db.example.internal # agentic-preview:db-offhours
```

and when it closes, exactly that line is removed. The trailing comment is the whole
ownership model. It is a Corefile comment, so CoreDNS ignores it, and it is what makes the
line unambiguously *this* schedule's: every operation is keyed on it, which is what lets a
schedule share a hosts block with lines nobody here wrote and with other schedules' lines,
and take its own back out without reading or rewriting any of theirs.

> **It changes that name for the whole cluster.** Not for one workload, not for one header.
> Everything that resolves it gets the redirect, which is the point and also the risk.

### Declaring one

```yaml
schedules:
  - name: db-offhours
    type: dns
    hostname: dev-db.example.internal   # the name overridden while the window is open
    redirectTo: 10.42.0.9               # where it points, as a literal IP
    windows:
      - days: [Mon, Tue, Wed, Thu]
        start: "18:32"
        end:   "07:21"
```

`windows`, `location` and `defaultLocation` are the same fields, read the same way, in the
same zone. Nothing about them is different here.

**`redirectTo` is a literal IP address, not a Service name.** Resolving a name would put a
Kubernetes lookup — and a way for it to fail, at 18:32, with nobody watching — on the path of
the one operation that must not be fragile. A literal address is proved parseable at boot,
which is the posture everything else in this file already takes. To point at a Service, write
its ClusterIP down: a ClusterIP is stable for the life of the Service, and a Service
recreated with a new one is a change worth having to make deliberately.

### Where it writes, and the permission it needs

Three settings name the one ConfigMap key it may touch, and **there is no default for them**:

| | |
| :--- | :--- |
| `SCHEDULE_DNS_CONFIGMAP_NAMESPACE` | chart: `scheduleDNSConfigMap.namespace` |
| `SCHEDULE_DNS_CONFIGMAP_NAME` | chart: `scheduleDNSConfigMap.name` |
| `SCHEDULE_DNS_CONFIGMAP_KEY` | chart: `scheduleDNSConfigMap.key`, default `host.override` |

In a cluster that is usually `kube-system/coredns-custom`. Defaulting to it would make "I
declared a window" and "I edited the cluster's resolver" the same act, so a `type: dns`
schedule declared without them is **fatal at boot**, and the chart refuses to render.

**The permission is a separate grant, and neither `deploy/` nor the chart creates it.** That
ConfigMap is outside `ALLOWED_NAMESPACES` — the fence the rest of this service draws is about
intercepting workloads and has nothing to say about the resolver — so a Role granting `get`
and `update`, with `resourceNames` pinning it to the one ConfigMap, is yours to apply
deliberately. `deploy/rbac.yaml` carries it commented out, verbatim.

Without it **nothing crash-loops**: the schedule alarms, `problem` names the ConfigMap it may
not write, and every other schedule carries on. A permission gap is a live-environment fact,
not a reason to take the process down with it. The same is true of a ConfigMap that is not
there — this mode never creates one.

### The drift check is the reason this is not two CronJobs

A CronJob writes at 18:32 and is never heard from again. Anything that rewrites the ConfigMap
in between — a flux reconcile of the real manifest, an operator reasserting its copy, a
colleague with `kubectl` — silently puts the name back, and the window is open in name only.

Here the line is re-read every `SCHEDULE_CHECK_INTERVAL` while the window is open, and:

| what it finds | what it does |
| :--- | :--- |
| the line, exactly as it wrote it | nothing, and **no write** — a controller that wrote every tick would reload the cluster's resolver every tick |
| the line gone | logs `ALARM … is gone from`, re-applies it, counts a `reRaise` |
| the line present but pointing elsewhere | logs `ALARM … reads "…"`, corrects it, counts a `reRaise` |
| the ConfigMap unreadable or unwritable | alarms with a `problem` naming the ConfigMap and what permission is missing |

`reRaises` means the same thing it means for an intercept: a number that climbs is something
fighting the controller, and winning between ticks.

A restart re-derives everything from the ConfigMap rather than from memory, which also covers
the ugly case: a pod killed mid-window leaves a redirect live that nothing has declared, and
the replacement **sweeps it** on its first tick if the window has since closed.

That sweep is not a one-shot. A replacement pod coming up into a briefly unavailable API
server is both when the sweep matters most and when it is most likely to fail, so until one
succeeds every tick of a shut window tries again, and the redirect it has not been able to
rule out shows as a `problem` on `GET /schedules` in the meantime — not as a single log line.
The same applies after any failed write: a schedule that could not be opened, re-applied or
closed no longer knows what the ConfigMap holds, so its next shut window sweeps rather than
assumes.

### Reading a DNS entry back

`kubectl agentic-preview schedules` lists both kinds together; a DNS entry is `"kind": "dns"`
with `hostname`, `redirectTo` and the ConfigMap it writes into as its `target`. See
[Reading it back](#reading-it-back) above for the shape.

### Measured

Against CoreDNS 1.11.3, with `forward` written **before** `hosts` in the Corefile:

```
hosts {
    10.42.0.9 dev-sql.example.internal # agentic-preview:sql-offhours
    10.1.2.3 someone-elses.example.internal
    fallthrough
}
```

| query | answer |
| :--- | :--- |
| the tagged name | `10.42.0.9` — the trailing comment is ignored, and `hosts` wins over `forward` regardless of the order they are written in |
| the untagged neighbour | `10.1.2.3` — unaffected |
| anything else | forwarded upstream, because of `fallthrough` |

CoreDNS orders its plugin chain from its compile-time `plugins.cfg`, not from the order the
Corefile happens to list them in, which is why the first row holds however the file is
written. Drop the `fallthrough` and it would answer NXDOMAIN for every name in the zone the
block does not list — which is why a block written by this service always carries one.

---

## Forcing a window, on demand

A window is a prediction about when something is needed, and predictions are wrong: the
dependency goes down at 14:00 for an unplanned reason, or it is up at 22:00 and the diversion
is in the way of somebody testing against the real thing. The override is the manual lever
for exactly that — the same on-demand lever a preview has, rather than waiting for whatever
normally raises it.

```
kubectl agentic-preview override db-offhours open --for 2h --reason "dev SQL is down"
kubectl agentic-preview override db-offhours auto
```

or `POST /schedules/{name}/override` with `{"state":"open"|"closed"|"auto","duration":"2h"}`.
`auto` — also `DELETE` on the same path — hands the schedule back to its windows.

Four things about it:

- **It is a layer on top of the reconcile loop, never a replacement.** It changes one thing:
  the answer to "should this be open now". Everything else still runs — a forced-open
  intercept that dies is re-raised, a forced-open redirect that drifts is re-applied and
  counted, exactly as a scheduled one is.
- **It arrives on the same surface as raising a preview** — this service's API, through the
  API server's service proxy, guarded by the cluster's own RBAC. There is deliberately no
  second, laxer door: the property that a global intercept cannot be flipped by anything
  merely able to reach the Service is a property of *that* surface.
- **It cannot create a schedule, only force a declared one.** A window declared over HTTP
  would be a global intercept anybody could conjure on a workload nobody asked about.
- **It lives in memory and does not survive a restart.** The schedule is the durable,
  reviewable thing and it lives in a file in a repository; an override is an intervention
  somebody is present for. Without `--for` it lasts until it is cleared, and `GET /schedules`
  reports it as `override` for as long as it does.

The change lands on the next check interval, not instantly; the response says which.

---

## Two modes, one workload, and they do not compose

This is the part that is not obvious, and the reason there is a refusal rather than a
precedence rule.

Telepresence's traffic-agent decides HTTP mode or raw TCP **per workload port**, not per
intercept: the listener switches to HTTP as soon as *any* intercept on that port carries a
header or path filter. In HTTP mode the matcher has exactly two tiers — header, then path —
and an intercept with neither qualifies for neither. It is not a low-priority default tier;
it falls through to the real application and is **inert**. In raw TCP mode the global
intercept is the only thing the listener serves, and a header-keyed intercept raised beside it
is equally inert.

Either way nothing errors, nothing logs, and the manager reports the losing intercept
`ACTIVE`. So:

- **A window opening on a workload that already carries a header-keyed preview is refused**,
  loudly, in the log and in `problem`, and retried every tick with the alarm repeated every
  five minutes for as long as it lasts. The window does not open.
- **A `POST /previews` for a workload holding a live scheduled intercept is refused** with
  `409` and a message that names the schedule.

Both refusals name the other side and what to do about it. The practical consequence is a
design rule: **do not schedule a workload people raise previews against.** Nothing else is
affected — a preview of any other workload, and two work ids on one workload, behave exactly
as they always did.

## What a global intercept costs when it dies

A header-keyed intercept orphaned by a forced kill costs one header value
([Limits](LIMITS.md#a-forced-kill-leaves-headers-hanging-and-it-does-not-fail-open)). A global
one has no header to be narrow about: it is the whole workload, and there is nobody awake at
03:00 to notice. That is the risk this mode adds, and the controller is built against it.

`SCHEDULE_CHECK_INTERVAL` (30s) is both how promptly a window opens or closes **and** how
quickly a dead intercept is noticed. Every tick, for every open window, the controller asks
the manager directly whether the intercept it created still exists and what state it is in:

| what it finds | what it does |
| :--- | :--- |
| `NotFound` — gone from manager state | logs `ALARM … has vanished`, raises it again |
| `NO_AGENT`, `AGENT_ERROR`, `REMOVED`, `NO_PORTS`, `BAD_ARGS`, … | logs `ALARM … is <state>`, removes and raises again |
| `ACTIVE` with no node-agent tunnel established, for over 90s | logs the alarm and says plainly that requests are being held rather than answered |
| `ACTIVE` with a tunnel to *some* of the workload's agent pods, for over 90s | logs the alarm with both counts — the replicas without one are holding their share of the traffic |
| `ACTIVE` | nothing |

It deliberately does not rely on the two safety nets that already exist: `PREVIEW_LIFETIME`
is hours, and the conflict-driven orphan sweep only fires when something tries to raise the
same intercept again.

### Measured

On a Kubernetes cluster running telepresence 2.31.1 in node-agent mode, with a window open
and all traffic to the workload diverted:

| | recovery | what traffic did meanwhile |
| :--- | :--- | :--- |
| `kill -9` of the process, pod surviving | **1.6s** — the recorded session is departed on startup and the first tick re-raises | one failed request; no hang |
| pod force-deleted, `--grace-period=0` | **11s** — one check interval | fell through to the real pod; no hang |
| the node-agent serving the intercept force-deleted | **5s** to the `NO_AGENT` alarm, **9s** to traffic restored | fell through to the real pod |

None of the three hung, and none needed the expiry sweep. Note what the first two share: a
graceful stop is still much better than either, and `terminationGracePeriodSeconds` is what
buys it.
