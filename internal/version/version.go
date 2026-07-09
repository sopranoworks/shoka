// Package version is the single source of truth for the Shoka release version.
//
// Both binaries (cmd/shoka, the server, and cmd/shoka-cli) import this so a
// `--version` flag and the MCP get_server_info implementation report one
// consistent string. Bump Version here for a release; the operator tags the
// repository separately (the version string and the git tag are kept in step by
// convention, not wired together).
package version

// Version is the current Shoka release, in semver with an optional pre-release
// suffix (e.g. "1.0.0-rc3"). Exported so the server/CLI can print it.
const Version = "1.0.0-rc3"
