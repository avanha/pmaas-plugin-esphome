#!/bin/sh
# Regenerates pb/ from proto/. Needs only Go and a network connection: the tools are built into a temporary
# directory at pinned versions, so nothing has to be installed.
#
# To move to a newer ESPHome API, replace proto/api.proto and proto/api_options.proto with the ones from a newer
# tag of https://github.com/esphome/aioesphomeapi (aioesphomeapi/api.proto), update proto/SOURCE, and run this.
set -eu

cd "$(dirname "$0")"

tools=$(mktemp -d)
trap 'rm -rf "$tools"' EXIT

GOBIN="$tools" GOWORK=off go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
GOBIN="$tools" GOWORK=off go install github.com/bufbuild/buf/cmd/buf@v1.74.0

rm -f pb/*.pb.go
PATH="$tools:$PATH" buf generate
