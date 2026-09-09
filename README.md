# hearsay

Hearsay is an open-source context platform for teams using coding agents. The
decisions about a piece of work are scattered across chat threads, meetings,
issue comments and PR reviews, so handing that context to an agent means either
pointing it at one source or hand-writing a long prompt. Hearsay ingests team
communication and artifacts, distills them into layered knowledge that keeps its
provenance, and serves scoped, permission-filtered context bundles to agents and
humans through a small read and assert API. It is the thing an agent asks
instead: chat apps, CLIs and trackers become interfaces to it rather than the
place context lives.

Status: early. The design is written and the scaffold is in place; the layers
are being built.

- [docs/design.md](docs/design.md) — what Hearsay is and how it works.
- [docs/adr/](docs/adr/) — the implementation decisions and why they were made.
- [CONTRIBUTING.md](CONTRIBUTING.md) — how to build, test and run it.

## Quick start

```sh
go build ./...
go test ./...
go run ./cmd/hearsay help
```

The four services — connectors, distiller, assertion worker and API — are
subcommands of one binary. They are stubs today: they start, log and shut down
cleanly, and do no work yet.

## License

[MIT](LICENSE).
