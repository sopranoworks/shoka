package clientconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// withTempConfigHome points os.UserConfigDir at a temp dir for the test by setting
// $XDG_CONFIG_HOME (honoured by os.UserConfigDir on Linux) and $HOME (the macOS
// fallback base). Returns the base the config tree should hang under.
func withTempConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// On darwin os.UserConfigDir uses $HOME/Library/Application Support; point HOME
	// at the same temp root so the test is correct on either platform.
	home := t.TempDir()
	t.Setenv("HOME", home)
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	return base
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withTempConfigHome(t)
	in := &Config{
		Endpoint:         "https://example.invalid/mcp",
		Token:            "secret-token-value",
		DefaultNamespace: "ns",
		DefaultProject:   "proj",
	}
	if err := Save("prod", in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load("prod")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *out != *in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", *out, *in)
	}
}

func TestSaveUsesRestrictivePerms(t *testing.T) {
	withTempConfigHome(t)
	if err := Save("prod", &Config{Endpoint: "https://example.invalid/mcp", Token: "t"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, _ := Path("prod")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := fi.Mode().Perm(); got != filePerm {
		t.Fatalf("config file perm = %o, want %o", got, filePerm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat config dir: %v", err)
	}
	if got := di.Mode().Perm(); got != dirPerm {
		t.Fatalf("config dir perm = %o, want %o", got, dirPerm)
	}
}

func TestLoadMissingIsNotExist(t *testing.T) {
	withTempConfigHome(t)
	_, err := Load("prod")
	if err == nil {
		t.Fatal("Load of a missing config returned no error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load error = %v, want wrapped os.ErrNotExist", err)
	}
}

func TestEnvironmentsAreIsolated(t *testing.T) {
	withTempConfigHome(t)
	if err := Save("a", &Config{Endpoint: "https://a.invalid/mcp", Token: "ta"}); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if err := Save("b", &Config{Endpoint: "https://b.invalid/mcp", Token: "tb"}); err != nil {
		t.Fatalf("Save b: %v", err)
	}
	a, err := Load("a")
	if err != nil {
		t.Fatalf("Load a: %v", err)
	}
	b, err := Load("b")
	if err != nil {
		t.Fatalf("Load b: %v", err)
	}
	if a.Token == b.Token || a.Endpoint == b.Endpoint {
		t.Fatalf("environments not isolated: a=%+v b=%+v", *a, *b)
	}
}

func TestEmptyEnvironmentDefaults(t *testing.T) {
	withTempConfigHome(t)
	if err := Save("", &Config{Endpoint: "https://example.invalid/mcp", Token: "t"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	p1, _ := Path("")
	p2, _ := Path(DefaultEnvironment)
	if p1 != p2 {
		t.Fatalf("empty env path %q != default env path %q", p1, p2)
	}
	if _, err := Load(""); err != nil {
		t.Fatalf("Load default: %v", err)
	}
}

func TestInvalidEnvironmentRejected(t *testing.T) {
	withTempConfigHome(t)
	for _, bad := range []string{"a/b", "..", ".", `a\b`} {
		if _, err := Path(bad); err == nil {
			t.Errorf("Path(%q) = no error, want rejection", bad)
		}
		if err := Save(bad, &Config{Token: "t"}); err == nil {
			t.Errorf("Save(%q) = no error, want rejection", bad)
		}
	}
}
