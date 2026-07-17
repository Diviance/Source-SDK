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
the browser User-Agent are then reused by the ordinary HTTP client.

The module path is local during development. Change it to the final private
Forgejo/Gitea module path before publishing the first SDK tag, and update the
workspace replacements in the backend and extension modules at the same time.
