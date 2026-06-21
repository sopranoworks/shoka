package main

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/term"

	"github.com/sopranoworks/shoka/internal/config"
	"github.com/sopranoworks/shoka/internal/storage/userstore"
)

// runUserReset performs the operator-local account recovery (B-28 password recovery
// case 2): reset a user's password and/or clear their TOTP by opening the userstore DB
// DIRECTLY — no serving, no listener, NO authentication bypass. This is the MySQL
// `--init-file` model (act at startup, then the caller exits WITHOUT serving), NOT
// `--skip-grant-tables` (which would open an auth-disabled window — this opens none:
// nothing is served, authenticated or otherwise).
//
// Authorization is structural: "can run the server binary against its data dir" = the
// host service operator (the same trust boundary MySQL/GitLab/Grafana rely on). Because
// the userstore is bbolt (single-writer), this CANNOT run while the server is serving —
// userstore.Open times out on the held lock and we tell the operator to stop the server
// first. The new password is read from pwIn — the controlling TTY (no echo) when it is a
// terminal, else a line reader (pipes/tests) — and NEVER from argv (no password flag
// exists), so it cannot leak via shell history, ps, or the process argv.
func runUserReset(cfg *config.Config, resetEmail, clearEmail string, pwIn *os.File, logger *slog.Logger) error {
	totpKey, err := userstore.ResolveTOTPKey(
		cfg.Server.Auth.Users.TOTPEncryptionKey,
		filepath.Join(cfg.Storage.BaseDir, "userstore.key"),
	)
	if err != nil {
		return fmt.Errorf("resolve userstore key: %w", err)
	}
	store, err := userstore.Open(filepath.Join(cfg.Storage.BaseDir, "users.db"), totpKey)
	if err != nil {
		// bbolt holds a single-writer lock; a running server owns it. Surface a clear
		// operator instruction rather than the raw timeout.
		if errors.Is(err, bolt.ErrTimeout) {
			return errors.New("the user store is locked (the server is running) — stop the server first, then re-run the reset")
		}
		return fmt.Errorf("open user store: %w", err)
	}
	defer store.Close()

	if clearEmail != "" {
		if err := store.ClearTOTP(clearEmail); err != nil {
			return fmt.Errorf("clear two-factor for the account: %w", err)
		}
		logger.Info("operator-local 2FA clear (startup mode)", "email", clearEmail)
		fmt.Fprintf(os.Stderr, "Cleared two-factor (TOTP) for %s; their sessions were dropped.\n", clearEmail)
	}

	if resetEmail != "" {
		pw, err := readNewPassword(pwIn)
		if err != nil {
			return fmt.Errorf("read new password: %w", err)
		}
		if err := userstore.ValidatePassword(pw); err != nil {
			return err // the policy message, verbatim
		}
		hash, err := userstore.HashPassword(pw)
		if err != nil {
			return fmt.Errorf("hash password: %w", err)
		}
		if err := store.SetUserPassword(resetEmail, hash); err != nil {
			return fmt.Errorf("reset password for the account: %w", err)
		}
		// Audit — WHO/WHAT only; the password is NEVER logged.
		logger.Info("operator-local password reset (startup mode)", "email", resetEmail)
		fmt.Fprintf(os.Stderr,
			"Reset the password for %s. Their sessions were dropped; they must sign in again.\n", resetEmail)
	}
	return nil
}

// readNewPassword reads (and confirms) a new password from f — the controlling TTY with
// echo suppressed when f is a terminal, otherwise two newline-delimited lines from the
// reader (for `printf '%s\n%s\n' pw pw | shoka --reset-password ...` and for tests). It
// reads from stdin only; the password never appears on the command line.
func readNewPassword(f *os.File) (string, error) {
	fd := int(f.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "New password: ")
		b1, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(os.Stderr, "Confirm new password: ")
		b2, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(b1) != string(b2) {
			return "", errors.New("passwords do not match")
		}
		return string(b1), nil
	}
	r := bufio.NewReader(f)
	p1, err := readLine(r)
	if err != nil {
		return "", err
	}
	p2, err := readLine(r)
	if err != nil {
		return "", err
	}
	if p1 != p2 {
		return "", errors.New("passwords do not match")
	}
	return p1, nil
}

func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}
