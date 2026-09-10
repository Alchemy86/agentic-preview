# agentic-preview: raises header-routed previews via the telepresence
# traffic-manager's gRPC API. Static binary, no CLI, distroless-nonroot.
FROM golang:1.27-alpine AS build

RUN apk add --no-cache git

WORKDIR /src

# Dependencies first, so a source-only change does not refetch the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/agentic-preview .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agentic-preview /agentic-preview
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/agentic-preview"]
