# Static build of the drop-measurement harness.
#
# CGO_ENABLED=0 yields a fully static binary, so it runs on a distroless-static
# base with no libc. The build stage inherits the builder's architecture, so on
# an Apple Silicon (arm64) kind host this produces an arm64 image the kind nodes
# can run directly (built + kind-loaded by scripts/build-app-images.sh).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/harness ./cmd/harness

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/harness /usr/local/bin/harness
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/harness"]
