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
- **`ALLOWED_NAMESPACES` and the attach Roles in `deploy/rbac.yaml` must name the same
  set.** The Roles bound interception; nothing but that env var bounds the forward target.
  `preview_test.go` is the executable form of that boundary — if you touch
  `PreviewRequest.validate`, those tests are the thing to satisfy.
- **`deploy/` ships four deliberate placeholders**, catalogued in the header comment of
  `deploy/kustomization.yaml`. `shop` and `checkout-api` are a fictional example service
  used consistently across the repo, not a default. Keep it that way; nothing in this repo
  should name a real cluster, namespace, registry or hostname.
- **`brand/` is generated, not drawn.** `python3 brand/make.py` reproduces both SVGs byte
  for byte via [Glyphsmith](https://github.com/Alchemy86/Glyphsmith). Never hand-edit the
  SVGs. The PNG is derived from the logo SVG — the README has the `magick` command.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
