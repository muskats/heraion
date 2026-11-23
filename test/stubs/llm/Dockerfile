# syntax=docker/dockerfile:1
# The LLM stub of the kind smoke test. Build from the repo root:
#   docker build -f test/stubs/llm/Dockerfile -t heraion-llm-stub:kind .
ARG GO_VERSION=1.26.11

FROM golang:${GO_VERSION}@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/llm-stub ./test/stubs/llm

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/llm-stub /usr/local/bin/llm-stub
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/llm-stub"]
