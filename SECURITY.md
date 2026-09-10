# Security policy

## Reporting a vulnerability

Report it privately through GitHub, not in a public issue:

**[Report a vulnerability](https://github.com/Alchemy86/agentic-preview/security/advisories/new)**
— Security → Advisories → Report a vulnerability, on this repository.

That opens a draft advisory visible only to you and the maintainer. It carries the
discussion, a private fork to develop the fix in, and the CVE request if one is warranted.

Please include what an attacker can reach, what they end up able to do, and the smallest
sequence that shows it. A working reproduction against a real cluster is the most useful
thing you can send; a description of the reachable path is enough to start.

Expect an acknowledgement within a week. If you have had no reply after two weeks, the
notification was probably lost — open a public issue saying only that you are waiting on a
private report, with no detail in it.

## Supported versions

The latest tagged release. This project is pre-1.0 and there are no maintenance branches:
a fix goes onto `main` and into the next tag.

## What is already known, and is not a vulnerability

These are documented properties of the design, not oversights. Reporting them tells us
nothing we have not written down, and
[Honest limits](docs/LIMITS.md) is the authority on all of them.

- **The HTTP API does not authenticate its callers.** ClusterIP, no Ingress, no API key.
  Anything that can reach the service can raise a preview and build a workload. It is
  meant to sit where only your pipeline can reach it.
- **`ALLOWED_NAMESPACES` is the only fence on the forward target.** The forwarding end of a
  preview is a plain `net.Dial` from this pod; no Kubernetes permission is consulted for
  it, so no Role can bound it. That environment variable is required, has no default, and
  is checked in both directions.
- **`delete` on Deployments in an allowed namespace is `delete` on any Deployment in that
  namespace.** RBAC cannot be narrowed by label. What narrows it is the code: every delete
  lists by the tool's own `managed-by` label, re-checks that label on the object, and
  deletes by name. Both that guard and the matching one on `update` have tests.
- **A `SIGKILL` leaves intercepts in manager state and requests carrying those specific
  header values hang.** Measured, documented, and cleared by the next `POST` for that
  service.

A way to get *past* one of those boundaries is a vulnerability, and we want to hear about
it. So is anything that lets a caller reach a namespace outside the allow-list, write to or
delete an object the tool did not create, or read the contents of a ConfigMap or Secret —
the service references live ones and never reads them.

## Scope

This repository: the Go service, `deploy/`, and the published image at
`ghcr.io/alchemy86/agentic-preview`.

Not this repository: [Telepresence](https://github.com/telepresenceio/telepresence), whose
traffic-manager and traffic-agents do the interception — report those to that project — and
your own cluster's configuration.
