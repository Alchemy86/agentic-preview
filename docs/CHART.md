# The Helm chart

The chart in [`charts/agentic-preview/`](../charts/agentic-preview/) parameterises the
manifests in `deploy/` and adds nothing to them. Anything that changes in
`deploy/deployment.yaml`, `service.yaml`, `serviceaccount.yaml` or `rbac.yaml` has a twin
under `charts/agentic-preview/templates/`, and both have to move together.

The full table of values, with the reasoning behind each, is in
[the chart's own README](../charts/agentic-preview/README.md). For installing it, see
[Installing agentic-preview](INSTALL.md#with-helm).

## Checking it

Check it the way CI does:

```bash
helm lint charts/agentic-preview --strict -f charts/example-values.yaml
helm template agentic-preview charts/agentic-preview -f charts/example-values.yaml
```

`helm template` is the check that means anything: **`helm lint` reports a template `fail`
as INFO and still exits 0.**

CI runs both on every push, plus one more: that `helm template` with **no**
`allowedNamespaces` still fails. That is the one guarantee the chart makes, so it is
asserted rather than assumed. The chart generates all three of the things that have to
agree — one attach Role and one build Role with a RoleBinding each per namespace, plus
`ALLOWED_NAMESPACES` on the container — from that one list, which is the only place they
cannot drift.

**Never a ClusterRole, and the chart offers no way to ask for one.** Every Role it renders
is namespaced to a name on that list.

## Publishing

Publishing is the `chart` job in [`release.yml`](../.github/workflows/release.yml), and it
runs *after* the image job on the same `v*` tag. It stamps the chart version, the
`appVersion` and the `artifacthub.io/images` annotation from that tag, packages, and pushes
the `.tgz` and a regenerated `index.yaml` to the `gh-pages` branch — which GitHub Pages
serves as the chart repository, and which [Artifact Hub](https://artifacthub.io) polls for
changes. Nothing is published by hand, so the chart cannot advertise an image the registry
does not have.

[`charts/artifacthub-repo.yml`](../charts/artifacthub-repo.yml) is copied to that branch
beside `index.yaml`, because Artifact Hub reads it over HTTP rather than from the default
branch. Both of its fields are optional and both are left unset; the file explains what
each one buys and what has to happen before it can be filled in.
