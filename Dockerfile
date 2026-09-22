# ---- build ----
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
ARG TARGETOS TARGETARCH
# CGO_ENABLED=0 is what makes the binary static, and therefore scratch-compatible
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /staybusy .

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
# nobody:nogroup - reading /proc/meminfo and /proc/stat needs no privilege
USER 65534:65534
ENTRYPOINT ["/staybusy"]
CMD ["-cpu", "25"]
