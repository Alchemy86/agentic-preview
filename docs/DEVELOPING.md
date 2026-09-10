# Building, testing and finding your way around

## Repository layout

`agentic-preview` is a single Go package at the repo root.

| Path | What it holds |
| :--- | :--- |
| `main.go` | HTTP server, signal handling, shutdown ordering |
| `config.go` | Environment configuration; `ALLOWED_NAMESPACES` is the boundary |
| `preview.go` | Work ids, the service set under each, request validation, name resolution, expiry |
| `workload.go` | Building the preview from the live Deployment, the isolation check, label-scoped delete |
| `kube.go` | The entire Kubernetes surface this service uses, as one interface — the RBAC grants exactly this |
| `session.go` | Manager session: arrive, remain, credential, reconnect, raise/remove, orphan sweep |
| `agents.go` | One tunnel per node-agent pod, rebuilt as the agent pod set changes |
| `api.go` | The HTTP API |
| `deploy/` | Deployment, Service, ServiceAccount, RBAC, kustomization — four placeholders |
| `charts/` | The Helm chart, a realistic values file, and the Artifact Hub repository metadata |
| `examples/` | Raise (built or your own), list, drop one, drop all — runnable `curl` |
| `docs/` | Design notes, the measured behaviour behind every claim, install and limits |
| `brand/` | The mark, the social preview card, and the generator that draws them |
| `.github/workflows/` | CI on every push and pull request; the image *and the chart* published on every `v*` tag |
| `CONTRIBUTING.md` | How to build it, and the three boundaries a change has to respect |
| `SECURITY.md` | How to report a vulnerability privately, and what is already known |

## Building and testing

No local Go toolchain needed:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.27-alpine \
  sh -c 'gofmt -l . && go vet ./... && go build ./... && go test ./...'
```

[CI](../.github/workflows/ci.yml) runs those same four checks on every push and pull
request, against the Go version `go.mod` declares rather than whatever is newest — a Go
release cannot turn this repo red without a commit that says so. The badge on the front
page is that workflow.

Note that `go build ./...` drops a ~30 MB binary in the repo root; it is gitignored.

The image:

```bash
docker buildx build --platform linux/amd64,linux/arm64 .
```

`.dockerignore` is an allowlist, so the build context is `go.mod`, `go.sum` and `*.go` and
nothing else.

## The two test files that are a boundary

`preview_test.go` and `workload_test.go` are the executable form of the
`ALLOWED_NAMESPACES` boundary and of the label-scoped delete guard. If you touch
`PreviewRequest.validate` or `createWorkload`, those tests are the thing to satisfy.

[`CONTRIBUTING.md`](../CONTRIBUTING.md) has the rest, including the three boundaries a
change has to respect.
