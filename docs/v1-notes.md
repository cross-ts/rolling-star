# v1 implementation notes

This records the simplifications and deferred-work seams chosen while building the v1 core (see `.claude/plans/glistening-meandering-mist.md` for the plan they came from), and *why* each one was made, so picking up deferred work starts from decisions rather than code archaeology. `docs/init.md` carries the overall concept and non-goals; this file is about the specific choices made inside the v1 scope.

## Selector routing: first-match-wins, config order

`internal/router.Router.Route` does a linear scan and returns the first rule whose `language`/`pattern` both match. `internal/config` flattens `servers[].selectors` into that rule list in file order, so config order is routing order.

**Why:** the plan's example (`actions-languageserver` on `.github/workflows/**/*.{yml,yaml}`, `yaml-language-server` on everything else `.yml`/`.yaml`) needs the fallback server's selector to be "any yaml file not already claimed by a more specific rule." Doublestar globs cannot express that negation ("not under `.github/workflows`"). Ordering sidesteps the problem entirely: list `actions` first, `yaml` second, and `yaml`'s broad `**/*.{yml,yaml}` only ever gets reached for files `actions` didn't already claim. Selectors being mutually exclusive is a *recommendation* for config authors, not a requirement the router enforces.

## `textDocumentSync` forced to `change: 1` (Full)

`mergeCapabilities` (`internal/gateway/capabilities.go`) unconditionally overrides the merged `textDocumentSync` to `{"openClose":true,"change":1,"save":{"includeText":false}}`, regardless of what any downstream server advertised.

**Why:** a full-document `didChange` is always legal to send to a server that asked for `Incremental` (2) — Full is a superset, not a competing sync strategy. Forcing it lets rolling-star relay `didChange` notifications verbatim to every bound downstream without tracking incremental diffs per server, or worse, per (server, document) pair. This is a load-bearing simplification: it's what keeps `textDocument/didChange` forwarding in `routing.go` a one-line `Notify` instead of a synchronization layer.

## Workspace-scoped capabilities dropped; workspace requests get `-32601`

`mergeCapabilities` deletes `workspaceSymbolProvider`, `executeCommandProvider`, and `workspace` from the merged result after merging. Separately, `Session.route` (`internal/gateway/routing.go`) replies `-32601 MethodNotFound` to any request that has no document URI and isn't one of the explicitly-broadcast `workspace/*` notifications (`didChangeConfiguration`, `didChangeWatchedFiles`, `didChangeWorkspaceFolders`) or `$/setTrace`.

**Why:** these two decisions are one policy, not two. A workspace-scoped request like `workspace/symbol` has no single document to route to — there's no principled way to pick which downstream should answer it in v1 (no aggregation, no primary-server designation). Rather than silently answering from an arbitrary downstream, or answering incompletely, rolling-star tells the client the capability doesn't exist at all, by stripping it from what it advertises. `-32601` is then the *correct* response, not a fallback: a well-behaved client honoring the advertised capabilities will never send `workspace/symbol` in the first place, because rolling-star never claimed to support it.

## `$/cancelRequest` is dropped

`Session.route` drops `$/cancelRequest` unconditionally.

**Why:** cancellation needs to know which downstream request an upstream ID maps to, and the deliberate design choice (see `internal/jsonrpc.Conn.Call`) is to keep *no* separate ID-remap table — the remap is implicit in the closure that waits on the `Call` channel and replies with the original upstream ID (see "Request forwarding and ID remap" below). Without a table, there's nothing to look up `$/cancelRequest`'s `id` against, and forwarding it downstream with a fabricated/wrong ID would be worse than dropping it. This is a real, load-bearing limitation, not a convenience: v1 cannot cancel in-flight downstream requests.

## Request forwarding and the ID remap

Client → server (and, mirrored, server → client) request forwarding sends the downstream `conn.Call(...)` inline from the read loop — preserving send order relative to notifications — and only the wait on the returned channel is handed to a goroutine, which replies to the *other* side using the original request's ID, captured in the closure. `internal/jsonrpc.Conn.Call` allocates its own fresh ID for the outgoing leg. That pairing (closure-captured original ID + Call's fresh ID) is the entire "remap"; no separate ID map exists anywhere in the gateway. This is why `$/cancelRequest` (above) has nothing to act on.

## Unroutable documents: notifications dropped, requests answered `null`, no default server

There is no default/fallback server for a document that no selector matches (`internal/gateway/routing.go`, `handleDocumentMessage`/`handleDidOpen`). The binding is cached as `nil` so the routing decision isn't repeated on every message for that document. Given a `nil` binding:

- a **notification** (`didOpen`, `didChange`, ...) is dropped silently — there's nothing to notify and no side effect the client is waiting on.
- a **request** (`hover`, `definition`, ...) is answered with a `null` result, not `-32601` and not silence.

**Why no default server:** an approved v1 decision (see the plan) — a document nobody's selectors claim gets no language intelligence, rather than being guessed at by whichever server happens to be configured first.

**Why `null`, not `-32601`, not silence, for requests:** this was corrected mid-implementation after initially specifying silence (matching a literal read of "dropped silently" in the original plan text, which was only ever meant to describe the notification case). Silence is a real defect, not an acceptable consequence of "no default server": a JSON-RPC request always gets exactly one response, and a client blocked forever on `textDocument/hover` for one unloved file is user-visible breakage. `-32601` would also be wrong: the merged capabilities already told the client hover exists, and it does work for every routable document in the same session — replying MethodNotFound here would be a lie about the same method, not a statement about this document. `null` is the LSP-conventional "no answer for this document" shape for hover/definition/completion/etc., so that's the reply.

## The `serverHandlers` hook point

`Session.serverHandlers` (`map[string]ServerRequestHandler`, declared in `session.go`) is always empty in v1 — nothing ever registers into it. It exists purely as the one structural concession to deferred work: `Downstream.Handle` (`internal/gateway/serverhandler.go`) checks this map before forwarding a downstream request upstream, and if an entry is ever added, that request is answered locally by the handler instead of being forwarded — without changing anything else in the forwarding loop.

This is where the deferred v1-out-of-scope work plugs in later, per `docs/init.md` §3:
- `actions/readFile` (server → client custom request `actions-languageserver` uses to resolve local reusable workflows)
- a locally-answered `workspace/configuration` (see the known limitation below — this is also its fix)
- `initializationOptions` resolved via `gh` CLI at start time rather than read verbatim from config

None of that is implemented; only the hook point is.

## Known limitation: downstream `workspace/configuration` during its own `initialize` (bounded, not fixed)

`Session.handleInitialize` is fully synchronous: nothing else proceeds until every configured server has been started and answered `initialize`. If a downstream server issues a request (most plausibly `workspace/configuration`) *during* its own `initialize` handling — before it has sent its `initialize` response back to rolling-star — `downstreamHandler.Handle` still forwards that request upstream to the client. The client is necessarily still blocked waiting on rolling-star's own `initialize` response at that point (a spec-conformant client cannot send anything else until then), so the request cannot be answered until the client is unblocked, and the client can't unblock until rolling-star's `initialize` handling finishes — which itself is waiting on this downstream's `initialize` response. Left unbounded, this is a deadlock that takes the whole gateway down with it, not just the one misbehaving server.

**What actually happens as of v1:** `handleInitialize` bounds each downstream's `Initialize` call with `downstreamInitializeTimeout` (`lifecycle.go`, 45s — generous, since real language servers can be legitimately slow to start; not a config knob, add one only if someone hits the limit). A downstream that deadlocks this way times out and falls into the same "failed to initialize" path as any other broken server: it's logged and dropped, and the rest of the session comes up normally. So the failure mode is now a bounded degradation (one server missing for up to ~45s of startup latency) instead of an unbounded hang.

**Why this isn't the fix:** the correct fix is still answering such requests locally via `serverHandlers` instead of forwarding — the same deferred hook point described above — rather than adding special-casing to the core forwarding loop. The timeout only bounds the damage; it doesn't make the request work. Spec-conformant downstream servers do not send requests before their own `initialize` response, so this only bites against a non-conformant peer.

## `Downstream.Terminate` only on a timed-out exit

`Session`'s exit sequence (`exitAll` in `lifecycle.go`) sends `exit`, then selects on the process exiting on its own vs. a grace period elapsing. `Terminate()` is only called on the grace-period arm: a well-behaved downstream (the common case: it saw `exit` and quit promptly) needs nothing further once its own exit is observed, so there is no `Terminate()` call, and thus no `os.ErrProcessDone` to filter out of the logs on that path. Only a downstream that is still alive when the grace period elapses gets `Terminate()`'d, and any error from that call is a genuine `Warn`.
