#!/bin/sh
set -eu

protoc_gen_go="$(go tool -n protoc-gen-go)"
protoc_gen_go_grpc="$(go tool -n protoc-gen-go-grpc)"

protoc -I proto \
  --plugin="protoc-gen-go=$protoc_gen_go" \
  --plugin="protoc-gen-go-grpc=$protoc_gen_go_grpc" \
  --go_out=. \
  --go_opt=module=gitea.diviance.club/Diviance/Source-SDK/Source-SDK \
  --go-grpc_out=. \
  --go-grpc_opt=module=gitea.diviance.club/Diviance/Source-SDK/Source-SDK \
  proto/sourceext/v1/sourceext.proto
