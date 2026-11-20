# syntax=docker/dockerfile:1
# Static binary on distroless, non-root. go-git is pure Go, so no git binary.

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/heraion ./cmd/heraion

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/heraion /usr/local/bin/heraion
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/heraion"]
CMD ["--help"]
