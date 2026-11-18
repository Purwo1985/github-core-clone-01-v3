# github-core-clone-01-v3

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Purwo1985/github-core-clone-01-v3/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Purwo1985/github-core-clone-01-v3/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in Go, standard library only — no git binary, no outside service. The server is a single statically linked binary (`forge`, built from the Go sources in this repository) launched by a thin Node entrypoint (`main.ts`), which reads the port the binary binds, registers it with the trial environment, and shuts it down when the trial ends. The hot paths are allocation-free by construction: a raw TCP HTTP/1.1 server (no `net/http` handler tree), one `Write` per response, hand-built JSON with cached strings on the records, an in-memory object arena with a pack cache, and pre-rendered ls-refs. It speaks git smart HTTP (upload-pack protocol v2 and receive-pack v0, fast-forward only), REST v3 (repositories, branches, commits, contents, labels, issues, pull requests with squash merge), GraphQL (the documents the GitHub CLI and github-mcp-server send, plus introspection), and MCP (JSON-RPC 2.0 Streamable HTTP, modern `server/discover` and legacy `initialize`) — all over one in-memory store with a deterministic clock, so a run replays exactly.

## Run it

```sh
MAKEFUN_ENV_URL=http://localhost:8080 node --experimental-strip-types main.ts
```

Requires Node 22 and the committed `forge` binary (Linux amd64, statically linked). `MAKEFUN_ENV_URL` is the trial environment the entry registers with; the entry binds an ephemeral port, registers it, answers `/healthz`, and exits when the environment reports the trial is done. `MAKEFUN_SHOP_TOKEN` is the bearer <redacted> clients must present (empty means dev mode, any token is accepted); `MAKEFUN_TRIAL_NOW` sets the deterministic clock.

## Build the binary

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o forge .
```

Requires Go 1.25. The committed binary is the build output of the Go sources in this repository.

## Layout

- `main.ts` — lifecycle: start `forge`, read its port, register, poll `/done`, exit.
- `main.go` — binary entrypoint: derive identities, build the store, listen on an ephemeral port.
- `http.go` — raw TCP HTTP/1.1 server, request parsing, reply helpers.
- `store.go` — object arena, repositories, reachability, packs, merges.
- `gitobj.go` — SHA-1 objects, pkt-lines, pack read/write, deltas.
- `ident.go` — the deterministic scenario identities (viewer and stranger).
- `rest.go` — REST v3 handlers and dispatch.
- `gitproto.go` — git smart HTTP (v2 ls-refs/fetch, v0 upload/receive-pack).
- `gql.go` — hand-written GraphQL parser and executor.
- `mcp.go` — MCP tools over JSON-RPC 2.0.
