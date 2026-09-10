# agentic-preview: raises header-routed previews via the telepresence
# traffic-manager's gRPC API. Static binary, no CLI, distroless-nonroot.
#
# --platform=$BUILDPLATFORM keeps the compile on the builder's own architecture
# and cross-compiles to $TARGETARCH instead of emulating it, so a multi-arch
# build costs a second `go build` rather than a QEMU run. Both args are
# supplied by buildx; a plain `docker build` defaults them to the host.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

RUN apk add --no-cache git

WORKDIR /src

# Dependencies first, so a source-only change does not refetch the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/agentic-preview .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agentic-preview /agentic-preview
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/agentic-preview"]
