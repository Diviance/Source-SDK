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

The module path is local during development. Change it to the final private
Forgejo/Gitea module path before publishing the first SDK tag, and update the
workspace replacements in the backend and extension modules at the same time.
