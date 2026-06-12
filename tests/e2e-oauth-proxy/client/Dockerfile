# Strict OAuth + PKCE + MCP test-client image for the B-61 e2e harness.
# Build context is the REPO ROOT (the client is a package in this module). The
# client trusts the local test CA via its own RootCAs pool (CA_CERT), not the
# system store, so it connects to https://shoka.test exactly as an external client.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/client ./tests/e2e-oauth-proxy/client

FROM debian:bookworm-slim
COPY tests/e2e-oauth-proxy/certs/ca.crt /etc/tls/ca.crt
COPY --from=build /out/client /usr/local/bin/client
ENTRYPOINT ["/usr/local/bin/client"]
