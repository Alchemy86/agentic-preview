# agentic-preview documentation

The [front page](../README.md) is the short version: what it does, how to install it, and
the boundaries. This is where the detail lives.

| Page | What is in it |
| :--- | :--- |
| [Design and measured behaviour](DESIGN.md) | The one design fact the whole service is built on, the mechanism of an intercepted request, the API in full, authentication and RBAC, cleanup, and every measured number |
| [Installing agentic-preview](INSTALL.md) | Both install paths in full — the traffic-manager prerequisite, digest pinning, the four placeholders in `deploy/`, and how to check it came up |
| [The `kubectl` plugin](PLUGIN.md) | Getting `kubectl agentic-preview` onto your PATH, how it reaches a ClusterIP-only Service without a tunnel, the one permission it needs, and how it picks a work id |
| [Configuration](CONFIGURATION.md) | Every environment variable, its default and what it is for |
| [A worked example, end to end](WORKED-EXAMPLE.md) | Six steps from pull request to teardown, and which two of them are this tool |
| [What it deliberately does not do](NON-GOALS.md) | Each boundary stated in full, and why it is where it is |
| [Honest limits](LIMITS.md) | The failure modes, what each one costs, and what clears it |
| [Building, testing and finding your way around](DEVELOPING.md) | The one-line build and test command, and what each source file holds |
| [The Helm chart](CHART.md) | How the chart relates to `deploy/`, how to check it, and how it is published |
| [The mark](BRAND.md) | Regenerating the SVGs and the PNGs derived from them |
| [Why Apache 2.0](LICENSING.md) | The licence, and the dependency that decided it |

Also in the repo root: [`CONTRIBUTING.md`](../CONTRIBUTING.md) for the three boundaries a
change has to respect, [`SECURITY.md`](../SECURITY.md) for reporting a vulnerability
privately, and [`CHANGELOG.md`](../CHANGELOG.md).
