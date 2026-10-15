# syntax=docker/dockerfile:1

# The build stage runs natively and cross-compiles, so a multi-arch image
# doesn't build Go under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
# modernc.org/sqlite is pure Go, so the binary is static.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/release-digest ./cmd/release-digest \
 && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/release-digest /release-digest
COPY deploy/release-digest.yaml /config/release-digest.yaml
# A named volume copies this directory's owner, so the nonroot user can write
# the store. A bind-mounted /data must be writable by uid 65532 itself.
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV RELEASE_DIGEST_CONFIG=/config/release-digest.yaml
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/release-digest", "healthcheck"]
ENTRYPOINT ["/release-digest"]
CMD ["run"]
