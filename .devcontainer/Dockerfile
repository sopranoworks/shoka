# The base image must provide Go >= the `go` directive in go.mod (currently go 1.26.2).
# The `:1-bookworm` tag tracks the latest Go 1.x; Go's automatic toolchain management
# fetches the exact patch (e.g. 1.26.2) at build time if the image lags.
FROM mcr.microsoft.com/devcontainers/go:1-bookworm

# Install additional OS packages if needed
# RUN apt-get update && export DEBIAN_FRONTEND=noninteractive \
#     && apt-get -y install --no-install-recommends <your-package-list-here>

EXPOSE 8080 8081
