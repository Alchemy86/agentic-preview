# Project agent memory

`agentic-preview` is a single Go package at the repo root that raises header-routed
Telepresence previews. **Read [`docs/DESIGN.md`](docs/DESIGN.md) before changing anything**
— it holds the design fact the whole service is built on (the traffic-agent never dials
`target_host`; the dial happens in this process) and the measured behaviour behind every
claim in the README. This file does not repeat it.

## Sharp edges

- **No local Go toolchain is assumed.** Build and test in a container:
  `docker run --rm -v "$PWD":/src -w /src golang:1.27-alpine sh -c 'gofmt -l . && go vet ./... && go build ./... && go test ./...'`
  Note that `go build ./...` drops a ~30 MB binary in the repo root; it is gitignored.
- **`ALLOWED_NAMESPACES` and BOTH sets of Roles in `deploy/rbac.yaml` (attach and build)
  must name the same set.** The Roles bound interception and creation; nothing but that env
  var bounds the forward target. `preview_test.go` and `workload_test.go` are the executable
  form of that boundary — if you touch `PreviewRequest.validate` or `createWorkload`, those
  tests are the thing to satisfy.
- **`kube.go`'s `kubeAPI` interface is the inventory the RBAC is written from.** It is the
  whole Kubernetes surface this service uses. Adding a method to it means adding a verb to
  `deploy/rbac.yaml`, with the reason spelled out there — do both or neither.
- **The tool knows nothing about how an image came to exist.** No tag conventions, no
  registry assumptions, no parsing of the work id, no notion of a pull request. Callers hand
  it an image reference and it runs that reference verbatim; teardown is explicit. If a
  change wants to infer something from a tag or a merge, that is the line. The README's
  "What it deliberately does not do" is the authority.
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
