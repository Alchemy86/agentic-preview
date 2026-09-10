# Why Apache 2.0

**Apache License 2.0** — see [`LICENSE`](../LICENSE) and [`NOTICE`](../NOTICE).

That choice is not arbitrary. agentic-preview embeds the Telepresence Go client: `rpc/v2`
for the manager and agent gRPC stubs, and `pkg/tunnel` for the dial loop that is the whole
forwarder. Telepresence is licensed **Apache 2.0**, whose terms for redistributing a work
that includes it are to keep the licence and attribution notices intact, state any changes,
and pass the licence along. Taking the same licence satisfies all of that with no
compatibility question to argue about, and carries the same explicit patent grant the
dependency already relies on. agentic-preview links those packages unmodified; the `NOTICE`
file records the attribution.

The rest of the dependency graph is permissive too — Apache-2.0, MIT and BSD-3-Clause
across gRPC, protobuf, the `k8s.io` client libraries, `quic-go`, `uuid` and the
`golang.org/x` packages. Nothing copyleft, and nothing that constrains the choice above.
