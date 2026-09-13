# Rolling Star

LSP Gateway

rolling-star is a Language Intelligence Gateway: to your editor or agent it behaves as a single Language Server, and to a pool of real language servers it behaves as their LSP client, routing each document to the right one by path/glob and `languageId`. See `docs/init.md` for the full motivation (in short: editor/agent LSP configs route by file extension only, so you cannot run `actions-languageserver` on `.github/workflows/**/*.yml` and `yaml-language-server` on every other `.yml`/`.yaml` file at the same time — rolling-star makes that configuration possible).

## Build

```sh
go build -o rolling-star ./cmd/rolling-star
```

Requires Go 1.27+.

## Configure

Write a YAML config listing the downstream servers and the selectors that route documents to them. `rolling-star.example.yaml` is a complete, working example that reproduces the motivating case from `docs/init.md` §2:

```yaml
servers:
  - name: actions
    command: actions-languageserver
    args: ["--stdio"]
    selectors:
      - language: yaml
        pattern: ".github/workflows/**/*.{yml,yaml}"

  - name: yaml
    command: yaml-language-server
    args: ["--stdio"]
    initializationOptions:
      yaml:
        validate: true
    selectors:
      - language: yaml
        pattern: "**/*.{yml,yaml}"
```

Key points (see the comments in `rolling-star.example.yaml` and `internal/config` for the rest):

- **Selectors are matched in config order, first match wins.** List more specific servers (like `actions` above) before more general fallbacks (like `yaml`) — that's what lets `.github/workflows/**/*.yml` be claimed by `actions` even though `yaml`'s `**/*.{yml,yaml}` would also match it.
- **`pattern` matches the path relative to the workspace root**, not the document URI and not an absolute filesystem path.
- Either `language` or `pattern` (or both) must be set per selector; an empty one matches anything.
- `initializationOptions` is passed through verbatim as that server's LSP `initializationOptions`.

## Run

Point your editor or agent's LSP client at the built binary instead of at the individual language servers:

```sh
rolling-star -c /path/to/rolling-star.yaml
```

rolling-star speaks LSP over stdio (standard `Content-Length`-framed JSON-RPC on stdin/stdout), so it drops into any LSP client configuration that would otherwise point at a single language server binary — e.g. Neovim's `lspconfig`, or a Claude Code / Copilot CLI LSP entry. Configure the client's file-extension routing to hand `.yml`/`.yaml` (or whatever extensions your `servers` selectors cover) to `rolling-star`, and it does the rest of the routing internally.

All logging goes to stderr; stdout carries only the LSP protocol traffic to your editor/agent.
