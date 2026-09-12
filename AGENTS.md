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
- **One manager session per allowed namespace, never one for the process.** A client session
  is bound to the namespace it arrives in and the manager answers `WatchAgentPods` for that
  namespace alone, so a single session silently strands every other allowed namespace: its
  intercepts go `ACTIVE`, no tunnel is ever opened, and the traffic is HELD. `Run` therefore
  supervises a loop per namespace and the tunnel pool is fed by all of them — which is why a
  `WatchAgentPods` snapshot replaces only its own namespace's entries (`replaceNamespace`)
  and a session ending closes only its own tunnels (`closeNamespace`). It is also why
  `reconcile` claims a pod key in `establishing` *before* dialling: an agent has ONE dial
  slot, a second `WatchDial` displaces the first, and concurrent passes over a shared pool
  otherwise leave an agent holding a watcher the losing pass cancels — reported up, traffic
  held. `agents_test.go` and `session_test.go` are the executable form; the measured failure
  is in [`docs/LIMITS.md`](docs/LIMITS.md).
- **`kube.go`'s `kubeAPI` interface is the inventory the RBAC is written from.** It is the
  whole Kubernetes surface this service uses. Adding a method to it means adding a verb to
  `deploy/rbac.yaml`, with the reason spelled out there — do both or neither.
- **The tool knows nothing about how an image came to exist.** No tag conventions, no
  registry assumptions, no parsing of the work id, no notion of a pull request. Callers hand
  it an image reference and it runs that reference verbatim; teardown is explicit. If a
  change wants to infer something from a tag or a merge, that is the line.
  [`docs/NON-GOALS.md`](docs/NON-GOALS.md) is the authority.
- **`hack/kubectl-agentic_preview` is a client of the HTTP API and must stay in step with
  it.** It is the `kubectl agentic-preview` plugin (kubectl maps the underscore to a
  space), a shell wrapper over `kubectl … --raw` against the API server's service proxy —
  no Go client, no credentials of its own. It currently covers all seven endpoints. Adding
  an endpoint to `api.go` means adding a subcommand or deciding in the open not to; the
  same goes for a new field on `PreviewRequest` and a flag on `up`.
  [`docs/PLUGIN.md`](docs/PLUGIN.md) is the page, and `--help` is the command reference —
  do not duplicate one into the other.
- **Scheduled intercepts are the one mode that is not header-keyed, and `schedule.go`'s header
  comment is the record of why.** `HeaderFilters` populated is a header-matched intercept;
  absent is a *global* one, raw TCP, whole workload. The two DO NOT compose on one workload -
  the traffic-agent's mode switch is per port, so whichever loses is created, reported
  `ACTIVE` and matches nothing. `registry.conflictingMode` is the refusal that exists for
  that, refused in both directions, and `schedule_test.go` is its executable form. The mode
  is off unless `SCHEDULE_FILE` is set, and it must stay that way: nothing without a declared
  window may behave differently. [`docs/SCHEDULES.md`](docs/SCHEDULES.md) is the page and
  carries the measured recovery numbers.
- **A schedule is one of TWO kinds and says which in `type:`; the windows are shared, the
  targets are not.** `type: intercept` is the above; `type: dns` writes one tagged `hosts`
  line into a ConfigMap (`dns.go`) for a dependency with no workload to attach to. The kind
  is never inferred from which fields are present - `scheduleKindOf` refuses a mismatch, and
  the chart template refuses the same one at render time, so the two must move together.
  Where the DNS kind may write is `SCHEDULE_DNS_CONFIGMAP_*` with NO default, and the RBAC
  for it is commented out in `deploy/rbac.yaml` on purpose: that ConfigMap is outside
  `ALLOWED_NAMESPACES`, which bounds interception and forwarding and says nothing about the
  resolver. A missing permission must stay an alarm, never a crash - `dns_test.go` is the
  executable form of that and of the coexistence and drift behaviour.
- **`POST /schedules/{name}/override` forces a declared window open or closed and must never
  become a way to declare one.** It changes exactly one thing, `desiredOpen`; everything else
  in the loop still runs, which is why a forced-open target is still health-checked, still
  drift-checked and still re-raised. It lives on the same surface as `POST /previews`
  deliberately - a second endpoint elsewhere would undo the property the schedules page
  claims - and in memory deliberately, so a restart returns to what the file says.
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
