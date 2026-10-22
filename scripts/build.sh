#!/usr/bin/env sh
# Binaries auto-update from UpdateRepo in updater.go; set LANFORGE_UPDATE_REPO=owner/name to override it.
set -eu
cd -- "$(dirname -- "$0")/.."
(cd ui && npm ci && npm run build)
go test -buildvcs=false ./...
mkdir -p dist
flags="-s -w${LANFORGE_UPDATE_REPO:+ -X main.UpdateRepo=$LANFORGE_UPDATE_REPO}"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags "$flags" -o dist/lanforge-linux-amd64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags "$flags" -o dist/lanforge-windows-amd64.exe .
# Upload both binaries and SHA256SUMS.txt to a GitHub release tagged vX.Y.Z (matching Version in model.go).
(cd dist && sha256sum lanforge-linux-amd64 lanforge-windows-amd64.exe > SHA256SUMS.txt)
