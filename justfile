set shell := ["bash", "-euo", "pipefail", "-c"]

version := env("DOORSTEP_VERSION", "dev")
image_name := env("DOORSTEP_IMAGE", "doorstep")
govulncheck_version := "v1.10.0"
deadcode_version := "v0.53.0"
kubernetes_version := "1.35.0"
buildinfo_package := "github.com/sharpintelligence/doorstep/internal/buildinfo"
ldflags := "-s -w -X " + buildinfo_package + ".version=" + version

default:
    @just --list

fmt:
    golangci-lint fmt

fmt-check:
    golangci-lint fmt --diff

vet:
    go vet ./...

lint:
    golangci-lint run

tidy-check:
    go mod tidy -diff

test *args:
    go test -race -shuffle=on -count=1 {{args}} ./...

build:
    CGO_ENABLED=0 go build -trimpath -ldflags {{quote(ldflags)}} -o bin/doorstep ./cmd/doorstep

image:
    docker build --build-arg VERSION={{version}} --build-arg COMMIT=$(git rev-parse HEAD) --build-arg COMMIT_TIME=$(git log -1 --format=%cI) -t {{image_name}}:{{version}} .

vuln:
    go run golang.org/x/vuln/cmd/govulncheck@{{govulncheck_version}} ./...

deadcode:
    #!/usr/bin/env bash
    set -euo pipefail
    unused=$(go run golang.org/x/tools/cmd/deadcode@{{deadcode_version}} -test ./...)
    if [ -n "$unused" ]; then
        echo "$unused"
        exit 1
    fi

lint-actions:
    actionlint

lint-manifests:
    for dir in deploy/kustomize/base deploy/kustomize/examples/*/; do echo "$dir"; kustomize build "$dir" | kubeconform -strict -summary -kubernetes-version {{kubernetes_version}} -; done

changelog *args:
    git-cliff {{args}}

run *args:
    go run ./cmd/doorstep serve {{args}}

clean:
    rm -rf bin coverage.out

check: fmt-check vet lint lint-actions tidy-check test
