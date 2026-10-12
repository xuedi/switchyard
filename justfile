# switchyard, run `just` to list the recipes

import? 'local.just'

version := `sed -n 's/^const version = "\(.*\)"/\1/p' main.go`

default:
    @just --list

# gofmt, vet, the tests, shellcheck on the kit (shellcheck installed, or through docker)
check:
    test -z "$(gofmt -l .)"
    go vet ./...
    go test ./...
    just shellcheck

# shellcheck on the kit's scripts and the example hooks
shellcheck:
    #!/usr/bin/env bash
    set -euo pipefail
    files=(kit/*.sh kit/router/*.sh examples/*/hooks/*)
    if command -v shellcheck >/dev/null; then shellcheck -s bash "${files[@]}"
    else docker run --rm -v "$PWD:/src:ro" -w /src docker.io/koalaman/shellcheck:v0.11.0 -s bash "${files[@]}"; fi

# Run against a cluster: just run https://host:6443 path/to/token path/to/ca.crt namespace
run api token ca namespace:
    SWITCHYARD_API={{api}} SWITCHYARD_TOKEN={{token}} SWITCHYARD_CA={{ca}} SWITCHYARD_NAMESPACE={{namespace}} SWITCHYARD_LISTEN=127.0.0.1:8080 go run .

# The cluster side of an app, for kubectl apply -f -: just manifests path/to/app.env
manifests config:
    @go run . manifests --config {{config}}

# Static binary for this machine
build:
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o switchyard .

# Container image for this machine
image:
    docker build -t switchyard:{{version}} .
