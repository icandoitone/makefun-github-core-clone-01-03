# Forge v3 — Static Go Core

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/icandoitone/makefun-github-core-clone-01-03/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/icandoitone/makefun-github-core-clone-01-03/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single process: git smart HTTP
(upload-pack protocol v2 and v0, receive-pack v0), the REST v3 API,
GraphQL, and MCP (JSON-RPC 2.0, revision 2026-07-28), all backed by one
in-memory store.

## Layout

- `main.ts` — Node 22 launcher (stdlib only). Starts the `forge` binary,
  reads the port from its ready line, registers it with the trial
  environment at `$MAKEFUN_ENV_URL`, then long-polls `/done` until the
  trial finishes.
- `go/` — the Go core (stdlib only, no Cgo). HTTP/1.1 is hand-rolled over
  raw TCP sockets for speed and a small binary. Everything is kept in
  memory; nothing is written to disk at runtime.
- `forge` — the compiled, statically linked, stripped linux/amd64 binary.

## Running

```sh
MAKEFUN_SHOP_TOKEN=<token> node main.ts
```

Without `MAKEFUN_ENV_URL` the launcher just starts the core and waits.
The core binds an ephemeral port on 127.0.0.1 and prints `ready <port>`.

To rebuild the core:

```sh
cd go
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../forge .
```

## Design notes

- Deterministic: sequential counters for ids and numbers, fixed
  timestamps, fixed response ordering — the same request sequence always
  yields the same bytes.
- Fast paths cache serialized responses (issue JSON, repo JSON, pack
  bytes per tip, advertisements) and invalidate only on mutation.
- Packs are emitted with full objects only; incoming packs are decoded
  with full objects and both offset and reference deltas.
- The bearer token from `MAKEFUN_SHOP_TOKEN` is required on every
  request except the `/healthz` probe. The token owner's login is learned
  from the first path-addressed repository access (bind-on-first-use).
