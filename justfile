set shell := ["bash", "-euo", "pipefail", "-c"]

version := env("DOORSTEP_VERSION", "dev")

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
    CGO_ENABLED=0 go build -trimpath -ldflags {{quote("-s -w -X github.com/sharpintelligence/doorstep/internal/buildinfo.version=" + version)}} -o bin/doorstep ./cmd/doorstep

run *args:
    go run ./cmd/doorstep serve {{args}}

clean:
    rm -rf bin coverage.out

check: fmt-check vet lint tidy-check test
