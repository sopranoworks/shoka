package authapi

import (
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
)

func TestCeremonyCache_SingleUseAndUnknown(t *testing.T) {
	c := newCeremonyCache()
	c.put("id1", "a@x.com", webauthn.SessionData{Challenge: "ch"})

	got, ok := c.take("id1")
	if !ok || got.email != "a@x.com" || got.data.Challenge != "ch" {
		t.Fatalf("take id1 = %+v ok=%v", got, ok)
	}
	// Single-use: a second take of the same id fails.
	if _, ok := c.take("id1"); ok {
		t.Fatal("ceremony must be single-use")
	}
	// Unknown id fails.
	if _, ok := c.take("nope"); ok {
		t.Fatal("unknown ceremony must not resolve")
	}
}
