# Litopys core image: one static binary, `litopys serve | worker | migrate | verify | ...`.
# Build: docker build -t <registry>/litopys:<commit> .
# Only the public modules go in: core and internal. core/go.mod reaches internal by its replace line, so the build
# runs without go.work (GOWORK=off) — the same Dockerfile serves the private repo and the public snapshot (#179),
# where enterprise/ and importers/ do not exist.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY internal ./internal
COPY core ./core
WORKDIR /src/core
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOWORK=off go build -trimpath -buildvcs=false -o /out/litopys ./cmd/litopys

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/litopys /litopys
USER nonroot:nonroot
EXPOSE 8766
HEALTHCHECK NONE
ENTRYPOINT ["/litopys"]
CMD ["serve"]
