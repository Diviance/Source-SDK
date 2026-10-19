# MangaVault Source Extension SDK

This independent Go module owns the versioned protobuf contract and the
`hashicorp/go-plugin` gRPC adapters shared by MangaVault and generated source
extension binaries.

An extension implements `sourceextsdk.SourceExtension` and calls:

```go
sourceextsdk.Serve(implementation, sourceextsdk.ServeOptions{})
```

Regenerate protobuf bindings with:

```sh
go generate ./...
```

## Anti-bot HTTP support

The `antibot` package provides a direct-first HTTP client with fallback to a
FlareSolverr-compatible `POST /v1` endpoint. Extension workers discover the
endpoint through `MANGAVAULT_ANTIBOT_URL`; each extension must also provide an
explicit upstream host allowlist when constructing the client. The solver is
only contacted after a recognizable challenge response. Returned cookies and
the browser User-Agent are then reused by the ordinary HTTP client. Set
`Options.StateFile` to persist that identity across worker restarts.

`NewFromEnvironment` reads `MANGAVAULT_ANTIBOT_TIMEOUT_SECONDS` (1–300 seconds)
when `Options.SolveTimeout` is unset, with a 60-second default. The solver receives
that value as its `maxTimeout`; the HTTP request deadline allows five additional
seconds to receive the response. A shorter solver HTTP client timeout is raised
on a copy of that client. Explicit caller deadlines and cancellation still take
precedence.

Use `Client.Do` for documents, `Client.DoDirect` for requests that must never
invoke the solver, and `Client.DoAsset` for binary assets. `DoAsset` detects an
asset challenge, refreshes the identity through the configured bootstrap URL,
and retries the binary request directly so solver JSON is never mistaken for
asset bytes.

The module path is local during development. Change it to the final private
Forgejo/Gitea module path before publishing the first SDK tag, and update the
workspace replacements in the backend and extension modules at the same time.

## Structured manga metadata (v0.1.7)

`SManga.publisher` is an optional publisher string. `SManga.characters` and `SManga.series` are repeated names; normalize source labels such as Parody or Parodies to Series, keep each name as a separate value, and omit these metadata lines from the synopsis. The added fields use tags 12–14 and remain compatible with protocol version 1 and older workers/hosts. Unavailable publisher values are omitted; unavailable lists are empty. SDK consumers should publish/use tag `v0.1.7` for these fields.
