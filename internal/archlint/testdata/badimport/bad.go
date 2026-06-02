//go:build ignore

// Package badimport is a DELIBERATELY-BAD fixture for the archlint self-test
// (TestScannerDetectsViolation). It imports go-git from outside the storage
// submodule — exactly what Anchor 2 forbids. It lives under testdata/, which the
// go tool ignores, so it never compiles and never breaks the build; the
// self-test scans it explicitly to prove the detector fires. Do NOT "fix" this
// import — its presence here is the test fixture.
package badimport

import (
	_ "github.com/go-git/go-git/v5"
)
