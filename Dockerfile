# ---- build ----
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
ARG TARGETOS TARGETARCH
# stamped into the binary so -version reports the released tag
ARG VERSION=dev
# CGO_ENABLED=0 is what makes the binary static, and therefore scratch-compatible
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /staybusy .

# A temp dir for the scratch image. scratch has none, and the disk dimension and
# -report both default to the temp dir under the non-root user, which cannot
# create it at runtime. Docker forces a copied directory to 0755 at the
# destination, so it is the --chown below, not the mode here, that makes it
# writable.
FROM --platform=$BUILDPLATFORM alpine:3.20 AS tmp
RUN mkdir -p /tmp && touch /tmp/.keep

# ---- runtime ----
# scratch: a static binary needs no libc, no shell and no runtime, so the image
# holds one executable and a CA bundle and has almost no attack surface.
FROM scratch
LABEL org.opencontainers.image.title="staybusy" \
      org.opencontainers.image.description="Put a controlled load on a machine" \
      org.opencontainers.image.licenses="MIT"
# the -net dimension speaks HTTPS, which needs a CA bundle
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /staybusy /staybusy
# Owned by nobody: Docker creates a copied directory 0755 at the destination, so
# the owner bit is what makes it writable for the non-root container user.
COPY --from=tmp --chown=65534:65534 /tmp /tmp
# nobody:nogroup - reading /proc/meminfo and /proc/stat needs no privilege
USER 65534:65534
ENTRYPOINT ["/staybusy"]
CMD ["-cpu", "25"]
