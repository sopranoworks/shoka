// Package archlint holds build-enforced architecture invariants for Shoka.
//
// Its sole current invariant (the 2026-06-01 gitwrap directive, Anchor 2): the
// go-git library may be imported ONLY from within the internal/storage submodule.
// All git access is confined there behind a business-intent API; no other code —
// production or test — may touch git directly. The check is a plain test that
// fires under `go test ./...` (one of the mandatory gates), so it cannot be
// bypassed by `--no-verify` of a git hook. See archlint_test.go.
package archlint
