# switchyard, run `just` to list the recipes

import? 'local.just'

version := `sed -n 's/^const version = "\(.*\)"/\1/p' main.go`

default:
    @just --list

# gofmt, vet, and the builds for every release platform
check:
    test -z "$(gofmt -l .)"
    go vet ./...
    for p in amd64 arm64; do CGO_ENABLED=0 GOARCH=$p go build -o /dev/null . ; done
    CGO_ENABLED=0 GOARCH=arm GOARM=7 go build -o /dev/null .

# Run it here: just run 127.0.0.1:8083 (just setup for the installer)
run listen="127.0.0.1:8083":
    go run . serve -listen {{listen}}

# The installer here; it prints its one-time URL
setup listen="127.0.0.1:8083":
    go run . setup -listen {{listen}}

# Static binary for this machine
build:
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o switchyard .

# The release binaries in dist/, for `gh release create v{{version}} dist/*`
release: check
    #!/usr/bin/env bash
    set -euo pipefail
    rm -rf dist && mkdir dist
    for p in amd64 arm64 arm; do
        name=switchyard-{{version}}-linux-$p
        [ $p = arm ] && name=switchyard-{{version}}-linux-armv7
        CGO_ENABLED=0 GOOS=linux GOARCH=$p GOARM=7 go build -trimpath -ldflags="-s -w" -o dist/$name .
    done
    (cd dist && sha256sum switchyard-* > SHA256SUMS)
    ls -l dist
