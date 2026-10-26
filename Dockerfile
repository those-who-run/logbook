# The service's image: built in Go's image, run from distroless's cc image as its
# non-root user. image.yml builds and smoke-tests it on every PR; a release builds it
# again and image-publish.yml pushes that build. Both base images are pinned by the
# digest of their multi-platform index (the tag beside it is for people); Renovate
# bumps the tag and the digest together.
#
# logbook embeds DuckDB (paddock ADR 0007), which needs cgo: the build stage sets
# CGO_ENABLED=1, links DuckDB's static library with Go's image's C and C++
# toolchain, and the runtime image is distroless's cc image, which carries glibc
# and libstdc++ where the template's static image has no C library. Its go.yml
# caller passes `cgo: true`. chassis and every other Go component stay cgo-free.

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
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/service ./cmd/logbook \
    && mkdir /out/data

FROM gcr.io/distroless/cc-debian13:nonroot@sha256:3ee00291cf386ec52a552e6a858d555debd74776eb4bfa53fa3df2cd6677515e
COPY --from=build /out/service /service
# The store's data directory (paddock#38), owned by the user that writes it; a
# volume is mounted over it.
COPY --from=build --chown=65532:65532 /out/data /data
ENV TWR_DATA_DIR=/data
# distroless's nonroot user (65532), by number so a runtime can check it isn't root.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/service"]
# `logbook run`; arguments given to `docker run` replace it, as `-version` does.
CMD ["run"]
