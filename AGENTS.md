# Project agent memory

`agentic-preview` is a single Go package at the repo root that raises header-routed
Telepresence previews. **Read [`docs/DESIGN.md`](docs/DESIGN.md) before changing anything**
— it holds the design fact the whole service is built on (the traffic-agent never dials
`target_host`; the dial happens in this process) and the mechanism of an intercepted
request. The measured numbers and failure modes are in
[`docs/LIMITS.md`](docs/LIMITS.md). This file does not repeat either.

## Sharp edges

- **No local Go toolchain is assumed.** Build and test in a container:
  `docker run --rm -v "$PWD":/src -w /src golang:1.27-alpine sh -c 'gofmt -l . && go vet ./... && go build ./... && go test ./...'`
  Note that `go build ./...` drops a ~30 MB binary in the repo root; it is gitignored.
- **`ALLOWED_NAMESPACES` and BOTH sets of Roles in `deploy/rbac.yaml` (attach and build)
  must name the same set.** The Roles bound interception and creation; nothing but that env
  var bounds the forward target. `preview_test.go` and `workload_test.go` are the executable
  form of that boundary — if you touch `PreviewRequest.validate` or `createWorkload`, those
  tests are the thing to satisfy. The chart generates all three from one `allowedNamespaces`
  list, which is the only place they cannot drift.
- **`charts/agentic-preview/` is a parameterisation of `deploy/`, not a second design.**
  Anything you change in `deploy/deployment.yaml`, `service.yaml`, `serviceaccount.yaml` or
  `rbac.yaml` has a twin under `charts/agentic-preview/templates/` and both must move
  together — nothing enforces it. Do not add a value the manifests do not already prove,
  and never add `replicaCount` or an Ingress; the comments on those two say why.
  Beware: **`helm lint` reports a template `fail` as INFO and still exits 0**, so
  `helm template` is the check that means anything. CI's `chart` job runs both plus the
  assertion that rendering without `allowedNamespaces` still fails.
- **The chart repository is the `gh-pages` branch**, published only by the `chart` job in
  `release.yml` on a `v*` tag, after the image job and stamped from the same tag. Never
  publish it by hand. `charts/artifacthub-repo.yml` has to land beside `index.yaml` there
  because Artifact Hub reads it over HTTP, not from `main`; that file's own header explains
  what is deliberately left unset in it and why.
- **`kube.go`'s `kubeAPI` interface is the inventory the RBAC is written from.** It is the
  whole Kubernetes surface this service uses. Adding a method to it means adding a verb to
  `deploy/rbac.yaml`, with the reason spelled out there — do both or neither.
- **The tool knows nothing about how an image came to exist.** No tag conventions, no
  registry assumptions, no parsing of the work id, no notion of a pull request. Callers hand
  it an image reference and it runs that reference verbatim; teardown is explicit. If a
  change wants to infer something from a tag or a merge, that is the line.
  [`docs/NON-GOALS.md`](docs/NON-GOALS.md) is the authority.
- **README.md is the front page, not the manual.** It keeps the pitch, the feature map,
  install, the API table, and the short forms of the non-goals and the limits — everything
  else lives as a page under `docs/`, indexed by [`docs/README.md`](docs/README.md). Detail
  belongs on the subpage with a link from the front page, never appended to README.md; when
  you add a page, add its row to that index. Every relative link and heading anchor across
  README and `docs/` is expected to resolve.
- **Previews are built by COPYING the live Deployment** (`buildPreviewDeployment`), never
  from a template. Three earlier attempts failed three ways by inventing what could be
  copied. The header comment on that function is the record; read it before changing what
  the preview carries.
- **`deploy/` ships four deliberate placeholders**, catalogued in the header comment of
  `deploy/kustomization.yaml`. `shop` and `checkout-api` are a fictional example service
  used consistently across the repo, not a default. Keep it that way; nothing in this repo
  should name a real cluster, namespace, registry or hostname.
- **`brand/` is generated, not drawn.** `python3 brand/make.py` reproduces all three SVGs
  byte for byte via [Glyphsmith](https://github.com/Alchemy86/Glyphsmith). Never hand-edit
  the SVGs. Both PNGs are derived from them — the README's "The mark" section has the
  `magick` commands. `agentic-preview-social.png` is GitHub's social preview card
  (1280×640, solid background, set by hand in Settings; there is no API for it).
- **CI runs exactly the local one-liner above**, in
  [`.github/workflows/ci.yml`](.github/workflows/ci.yml), against the Go version `go.mod`
  declares rather than the newest release. If the two ever diverge, the workflow is the
  bug. A `v*` tag additionally publishes the multi-arch image to
  `ghcr.io/alchemy86/agentic-preview`; `.dockerignore` is an allowlist, so the build
  context is go.mod, go.sum and `*.go` and nothing else.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
