# syntax=docker/dockerfile:1
# Static binary on distroless, non-root. go-git is pure Go, so no git binary.
# GO_VERSION equals the toolchain line in go.mod (checked by scripts/check-version.sh).
# Bump both, and the digest, together.
ARG GO_VERSION=1.26.11

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/heraion ./cmd/heraion

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/heraion /usr/local/bin/heraion
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/heraion"]
CMD ["--help"]
