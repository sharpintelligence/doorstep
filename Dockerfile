FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=
ARG COMMIT_TIME=
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
    -ldflags "-s -w \
    -X github.com/sharpintelligence/doorstep/internal/buildinfo.version=$VERSION \
    -X github.com/sharpintelligence/doorstep/internal/buildinfo.commit=$COMMIT \
    -X github.com/sharpintelligence/doorstep/internal/buildinfo.commitTime=$COMMIT_TIME" \
    -o /out/doorstep ./cmd/doorstep

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /out/doorstep /usr/local/bin/doorstep
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/doorstep"]
CMD ["serve"]
