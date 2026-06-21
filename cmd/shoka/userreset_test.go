package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/config"
	"github.com/sopranoworks/shoka/pkg/userstore"
)

// These cover the B-28 password-recovery case-2 startup-mode core (runUserReset): it
// opens the userstore DIRECTLY, resets/clears the named account, and returns — it never
// serves and binds no listener (structurally: the function contains no server code, like
// runConfigCheck). The "exits without serving / no --password flag" main()-level
// properties are covered by the subprocess test below.

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// resetTestStore makes a userstore under dir keyed by the SAME key runUserReset will
// resolve from cfgKey, seeds an admin with the given password, and CLOSES it (releasing
// the bbolt lock) so the reset can open it. Returns the resolved cfg.
func resetTestStore(t *testing.T, dir, cfgKey, email, password string) *config.Config {
	t.Helper()
	key, err := userstore.ResolveTOTPKey(cfgKey, filepath.Join(dir, "userstore.key"))
	if err != nil {
		t.Fatalf("resolve key: %v", err)
	}
	s, err := userstore.Open(filepath.Join(dir, "users.db"), key)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ph, _ := userstore.HashPassword(password)
	if err := s.CreateFirstAdmin(&userstore.UserRecord{Email: email, DisplayName: "Admin", PasswordHash: ph}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if _, err := s.CreateSession(email, time.Now(), time.Hour); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	_ = s.Close()

	cfg := &config.Config{}
	cfg.Storage.BaseDir = dir
	cfg.Server.Auth.Users.TOTPEncryptionKey = cfgKey
	return cfg
}

// feedPassword returns a non-terminal *os.File delivering the new password twice
// (new + confirm), as the non-TTY line-reader path consumes it — the value rides stdin,
// never argv.
func feedPassword(t *testing.T, pw string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	go func() {
		_, _ = io.WriteString(w, pw+"\n"+pw+"\n")
		_ = w.Close()
	}()
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func reopenStore(t *testing.T, cfg *config.Config) *userstore.Store {
	t.Helper()
	key, err := userstore.ResolveTOTPKey(cfg.Server.Auth.Users.TOTPEncryptionKey, filepath.Join(cfg.Storage.BaseDir, "userstore.key"))
	if err != nil {
		t.Fatalf("resolve key: %v", err)
	}
	s, err := userstore.Open(filepath.Join(cfg.Storage.BaseDir, "users.db"), key)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestRunUserReset_ResetsPasswordAndDropsSessions: the startup-mode reset re-hashes the
// password (new verifies, OLD does not) and drops the user's sessions.
func TestRunUserReset_ResetsPasswordAndDropsSessions(t *testing.T) {
	dir := t.TempDir()
	cfg := resetTestStore(t, dir, "k", "admin@example.com", "oldpassword1")

	if err := runUserReset(cfg, "admin@example.com", "", feedPassword(t, "newpassword1"), discardLogger()); err != nil {
		t.Fatalf("runUserReset: %v", err)
	}

	s := reopenStore(t, cfg)
	u, err := s.GetUser("admin@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if ok, _ := userstore.VerifyPassword("newpassword1", u.PasswordHash); !ok {
		t.Fatal("the new password must verify after the reset")
	}
	if ok, _ := userstore.VerifyPassword("oldpassword1", u.PasswordHash); ok {
		t.Fatal("the old password must NOT verify after the reset")
	}
	// Sessions for the user were dropped (forcing re-login).
	if _, err := s.LookupSession("any", time.Now()); err == nil {
		t.Fatal("expected no live session lookup to succeed")
	}
}

// TestRunUserReset_PolicyEnforced: a too-short new password is rejected and the stored
// hash is unchanged.
func TestRunUserReset_PolicyEnforced(t *testing.T) {
	dir := t.TempDir()
	cfg := resetTestStore(t, dir, "k", "admin@example.com", "oldpassword1")

	err := runUserReset(cfg, "admin@example.com", "", feedPassword(t, "short"), discardLogger())
	if err == nil {
		t.Fatal("a too-short new password must be rejected")
	}
	s := reopenStore(t, cfg)
	u, _ := s.GetUser("admin@example.com")
	if ok, _ := userstore.VerifyPassword("oldpassword1", u.PasswordHash); !ok {
		t.Fatal("the original password must still verify after a rejected reset")
	}
}

// TestRunUserReset_ClearTOTP: --clear-2fa removes an enrolled TOTP secret.
func TestRunUserReset_ClearTOTP(t *testing.T) {
	dir := t.TempDir()
	key, _ := userstore.ResolveTOTPKey("k", filepath.Join(dir, "userstore.key"))
	s, err := userstore.Open(filepath.Join(dir, "users.db"), key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	enc, _ := s.SealTOTPSecret("JBSWY3DPEHPK3PXP")
	ph, _ := userstore.HashPassword("oldpassword1")
	_ = s.CreateFirstAdmin(&userstore.UserRecord{Email: "admin@example.com", PasswordHash: ph, TOTPSecretEnc: enc})
	_ = s.Close()

	cfg := &config.Config{}
	cfg.Storage.BaseDir = dir
	cfg.Server.Auth.Users.TOTPEncryptionKey = "k"

	if err := runUserReset(cfg, "", "admin@example.com", nil, discardLogger()); err != nil {
		t.Fatalf("clear-2fa: %v", err)
	}
	s2 := reopenStore(t, cfg)
	u, _ := s2.GetUser("admin@example.com")
	if u.HasTOTP() {
		t.Fatal("TOTP must be cleared after --clear-2fa")
	}
}

// TestRunUserReset_LockedStore_StopServerFirst: the userstore is single-writer, so when
// it is already open (the server "running"), the reset fails with a clear "stop the
// server first" message rather than corrupting state or hanging.
// RED proof: removing the bolt.ErrTimeout handling surfaces the raw "timeout" error
// instead of this operator instruction.
func TestRunUserReset_LockedStore_StopServerFirst(t *testing.T) {
	dir := t.TempDir()
	cfg := resetTestStore(t, dir, "k", "admin@example.com", "oldpassword1")

	// Hold the lock (simulate the running server owning the DB).
	key, _ := userstore.ResolveTOTPKey("k", filepath.Join(dir, "userstore.key"))
	held, err := userstore.Open(filepath.Join(dir, "users.db"), key)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer held.Close()

	err = runUserReset(cfg, "admin@example.com", "", feedPassword(t, "newpassword1"), discardLogger())
	if err == nil {
		t.Fatal("a locked store must make the reset fail, not proceed")
	}
	if !strings.Contains(err.Error(), "stop the server first") {
		t.Fatalf("error should instruct stopping the server, got: %v", err)
	}
}
