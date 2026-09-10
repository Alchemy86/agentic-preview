# Contributing

Issues and pull requests are welcome. This is a small, opinionated tool and the
things below are the ones that will decide whether a change lands.

## Read the design notes first

[`docs/DESIGN.md`](docs/DESIGN.md) holds the fact the whole service is built on — the
Telepresence traffic-agent never dials `target_host`; the dial happens in this process —
and the measured behaviour behind every claim in the README. Most surprising code in this
repo is explained there.

[`AGENTS.md`](AGENTS.md) lists the sharp edges: the places where a reasonable-looking
change breaks something that is not obvious from the file you are editing.

## Build and test

No local Go toolchain needed:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.27-alpine \
  sh -c 'gofmt -l . && go vet ./... && go build ./... && go test ./...'
```

That is exactly what [CI](.github/workflows/ci.yml) runs on every push and pull request,
against the Go version `go.mod` declares. If the two disagree, CI is the bug.

`go build ./...` leaves a ~30 MB binary in the repo root. It is gitignored.

The container image builds for `linux/amd64` and `linux/arm64`:

```bash
docker buildx build --platform linux/amd64,linux/arm64 .
```

## Three boundaries that are not negotiable

**`ALLOWED_NAMESPACES` and both sets of Roles in `deploy/rbac.yaml` must name the same
set.** The Roles bound interception and creation. Nothing but that environment variable
bounds the forward target. `preview_test.go` and `workload_test.go` are the executable
form of that boundary — a change to `PreviewRequest.validate` or `createWorkload` has to
satisfy them.

**`kube.go`'s `kubeAPI` interface is the inventory the RBAC is written from.** It is the
entire Kubernetes surface this service uses. A new method on it means a new verb in
`deploy/rbac.yaml`, with the reason written beside it. Both or neither.

**The tool knows nothing about how an image came to exist.** No tag conventions, no
registry assumptions, no parsing of the work id, no notion of a pull request. A caller
hands it an image reference and it runs that reference verbatim; teardown is explicit. A
change that infers something from a tag or a merge is over the line — the README's
"What it deliberately does not do" is the authority on where that line is.

## Two more worth knowing

Previews are built by **copying the live Deployment** (`buildPreviewDeployment`), never
from a template. Three earlier attempts failed three different ways by inventing what
could be copied. The header comment on that function is the record.

`deploy/` ships four deliberate placeholders, catalogued in the header comment of
`deploy/kustomization.yaml`. `shop` and `checkout-api` are a fictional example service
used consistently across the repo, not a default. Nothing in this repo should name a real
cluster, namespace, registry or hostname, and that includes anything you add.

`brand/` is generated, not drawn. `python3 brand/make.py` reproduces both SVGs byte for
byte. Never hand-edit the SVGs.

## Pull requests

One change per pull request. Say what broke or what was missing, not just what you
changed. If the change alters behaviour anyone could observe, the README or `docs/DESIGN.md`
changes with it in the same commit.

CI has to be green. A test that is weakened to make a change pass is not a passing test.

## Licence

Contributions are accepted under the [Apache License 2.0](LICENSE), the licence this
project is released under.
