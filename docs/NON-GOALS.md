# What it deliberately does not do

These are the boundaries, not gaps waiting to be filled. Most of them exist because the
alternative would only ever be right for one company's pipeline. The front page has the
short version; this is the full statement of each, because the reasoning is the part that
tells you whether the boundary is in the right place.

- **It does not trigger anything.** It receives calls. Nothing here watches a repository,
  a registry, a webhook or a queue. *Triggering is the adopter's job* — you already have
  something that knows when a build finished, and it knows far more about your process than
  this could.
- **It knows nothing about how an image came to exist, or what its tag means.** You give it
  an image reference and it runs that reference, verbatim. It does not complete a bare tag
  against the live container's registry, does not read an id or a branch or a commit out of
  a tag, and assumes no registry. A tool that guessed at tag conventions would be wrong
  everywhere but the one place it was written.
- **It has no notion of a pull request.** Not opened, not merged, not closed. A work id is
  an opaque string used as the header value and as a label — never parsed. Nothing tears a
  preview down because a branch merged; see
  [Limits → the lifetime timer](LIMITS.md#there-is-a-timer-against-forgotten-previews-and-you-may-well-want-it-off)
  for the only thing that removes a preview on its own.
- **It does not build or push images.** Your CI already does.
- **It does not manage DNS, ingress or certificates.** Traffic arrives through the ingress
  you already have; the header is the only thing that changes.
- **It does not accept a per-request header name.** `HEADER_NAME` is service-level
  configuration. Two services of one work id behind different header names would destroy
  the one guarantee a work id exists for.
- **It does not scale out.** Exactly one replica, `strategy: Recreate`. Two replicas would
  each arrive as their own session and collide raising identical header filters rather than
  sharing the work.
- **It does not authenticate its callers.** ClusterIP, no Ingress, no API key. Anything
  that can reach it can raise a preview — and now, build one. Put it where only your
  pipeline can reach it.
- **It does not touch the cluster outside the namespaces on its allow-list**, and holds no
  ClusterRole at all.
- **It does not install or manage Telepresence.**

The limits that are *not* boundaries — the failure modes, and what each one costs — are in
[Honest limits](LIMITS.md).
