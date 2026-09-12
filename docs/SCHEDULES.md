# Scheduled, headerless intercepts

Everything else in this service is **header-keyed**: a preview exists, one header value
reaches it, and everything without that header reaches the live pod unaware. This page is
the one mode that is not. A **schedule** gives one workload a recurring window, and while
that window is open *every* request to it is diverted somewhere else — no header, nothing
to call, nobody awake.

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
    "name": "offhours",
    "workload": "checkout-api", "namespace": "shop", "port": "8443",
    "target": "auth-stub.shop:8443",
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

A scheduled intercept also appears in `GET /previews` under the schedule's name, marked
`"global": true` and with **no expiry** — its life is its window and nothing else, so
`PREVIEW_LIFETIME` does not apply to it and a `DELETE` of its name is refused.

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
