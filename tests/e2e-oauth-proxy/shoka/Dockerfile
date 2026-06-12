# Shoka server image for the B-61 OAuth e2e harness.
#
# Build context is the REPO ROOT (so the whole module is available); built from
# source at HEAD, not a prebuilt binary. The runtime image installs the local test
# CA into the SYSTEM trust store because Shoka's CIMD fetch (internal/oauth/cimd.go)
# uses system roots (tlsConfig nil) — without it the outbound HTTPS fetch of the
# client's metadata document at https://client.test/... would fail TLS verification.
#
# Pin: go.mod declares `go 1.26.2`; the builder uses Go 1.26 and the toolchain
# fetches the exact patch if the image lags (same contract as .devcontainer).
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/shoka ./cmd/server

FROM debian:bookworm-slim

# Trust the harness's local CA for outbound CIMD fetches over TLS. Shoka's CIMD
# Verifier uses Go's system roots (internal/oauth/cimd.go, tlsConfig nil); on Debian
# Go reads /etc/ssl/certs/ca-certificates.crt first. We install ONLY the local test
# CA there (the harness's TLS surface is signed by it), so the fetch of
# https://client.test/cimd/client.json verifies — without needing the ca-certificates
# package (the harness builds offline; the public CA bundle is not needed).
COPY tests/e2e-oauth-proxy/certs/ca.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /out/shoka /usr/local/bin/shoka
COPY tests/e2e-oauth-proxy/shoka/shoka.yaml /etc/shoka/shoka.yaml
RUN mkdir -p /data

EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/shoka", "--config", "/etc/shoka/shoka.yaml"]
