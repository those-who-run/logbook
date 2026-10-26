# The service's image: built in Go's image, run from distroless's static image as
# its non-root user. image.yml builds and smoke-tests it on every PR; a release
# builds it again and image-publish.yml pushes that build. Both base images are
# pinned by the digest of their multi-platform index (the tag beside it is for
# people); Renovate bumps the tag and the digest together.
#
# A static binary (CGO_ENABLED=0) needs no C library, so the runtime image has
# none. A component that needs cgo (logbook, for DuckDB: paddock ADR 0007) sets
# CGO_ENABLED=1 in the build stage and runs from
# gcr.io/distroless/cc-debian13:nonroot instead, which carries glibc; its go.yml
# caller passes `cgo: true`.

FROM docker.io/library/golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
# The modules first, so a change to the code alone reuses this layer.
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# The version the binary reports (`-version`, GET /healthz): `dev` unless the build
# passes one, as `docker build --build-arg VERSION=X.Y.Z .` does. image.yml in
# those-who-run/.github passes VERSION=X.Y.Z when it builds on a release tag vX.Y.Z,
# so a released image reports its version, as its org.opencontainers.image.version
# label does; a pull request's image build passes none, and reports `dev`.
ARG VERSION=dev
# -trimpath keeps the build machine's paths out of the binary.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/service ./cmd/logbook

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/service /service
# distroless's nonroot user (65532), by number so a runtime can check it isn't root.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/service"]
