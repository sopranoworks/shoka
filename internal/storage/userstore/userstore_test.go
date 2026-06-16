package userstore

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pquerna/otp/totp"
	bolt "go.etcd.io/bbolt"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "users.db"), testKey())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPassword_HashVerifyRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword("correct horse battery staple", h)
	if err != nil || !ok {
		t.Fatalf("verify good password: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrong", h)
	if err != nil {
		t.Fatalf("verify wrong password err=%v", err)
	}
	if ok {
		t.Fatal("wrong password verified true")
	}
	if _, err := VerifyPassword("x", "not-a-phc-string"); err != ErrMalformedHash {
		t.Fatalf("malformed hash: want ErrMalformedHash, got %v", err)
	}
}

func TestCreateFirstAdmin_GetUser(t *testing.T) {
	s := openTestStore(t)
	empty, err := s.IsEmpty()
	if err != nil || !empty {
		t.Fatalf("fresh store IsEmpty: empty=%v err=%v", empty, err)
	}
	ph, _ := HashPassword("pw")
	if err := s.CreateFirstAdmin(&UserRecord{Email: "Op@Example.com", DisplayName: "Op", PasswordHash: ph}); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	empty, _ = s.IsEmpty()
	if empty {
		t.Fatal("store still empty after first admin")
	}
	// Email normalized to lower-case for the key and the stored identity.
	u, err := s.GetUser("op@example.com")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Email != "op@example.com" {
		t.Fatalf("email not normalized: %q", u.Email)
	}
	if u.Scope != AdminScope {
		t.Fatalf("first admin scope = %q, want %q", u.Scope, AdminScope)
	}
	if !u.IsAdmin() {
		t.Fatal("first admin IsAdmin()=false")
	}
	if len(u.Handle) != 32 {
		t.Fatalf("user handle len = %d, want 32", len(u.Handle))
	}
}

func TestCreateFirstAdmin_ConcurrentOnlyOneWins(t *testing.T) {
	s := openTestStore(t)
	ph, _ := HashPassword("pw")
	const n = 16
	var wg sync.WaitGroup
	results := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = s.CreateFirstAdmin(&UserRecord{
				Email:        "u" + string(rune('a'+i)) + "@x.com",
				DisplayName:  "U",
				PasswordHash: ph,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var wins, refused int
	for _, e := range results {
		switch e {
		case nil:
			wins++
		case ErrUsersExist:
			refused++
		default:
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one first-admin must win, got %d", wins)
	}
	if refused != n-1 {
		t.Fatalf("the other %d registrants must be refused, got %d", n-1, refused)
	}
	if got := countUsers(t, s); got != 1 {
		t.Fatalf("exactly one user must persist, got %d", got)
	}
}

// countUsers counts rows in the users bucket (same-package access to s.db).
func countUsers(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	if err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(usersBucket)).ForEach(func(_, _ []byte) error {
			n++
			return nil
		})
	}); err != nil {
		t.Fatalf("countUsers: %v", err)
	}
	return n
}

func TestCreateUser_DuplicateRefused(t *testing.T) {
	s := openTestStore(t)
	ph, _ := HashPassword("pw")
	if err := s.CreateFirstAdmin(&UserRecord{Email: "a@x.com", PasswordHash: ph}); err != nil {
		t.Fatalf("first admin: %v", err)
	}
	if err := s.CreateUser(&UserRecord{Email: "b@x.com", PasswordHash: ph, Scope: "namespace:foo"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.CreateUser(&UserRecord{Email: "B@x.com", PasswordHash: ph}); err != ErrExists {
		t.Fatalf("duplicate email: want ErrExists, got %v", err)
	}
}

func TestTOTP_SealOpenAndVerifyWithinSkew(t *testing.T) {
	s := openTestStore(t)
	key, err := GenerateTOTP("Shoka", "op@example.com")
	if err != nil {
		t.Fatalf("GenerateTOTP: %v", err)
	}
	enc, err := s.SealTOTPSecret(key.Secret())
	if err != nil {
		t.Fatalf("SealTOTPSecret: %v", err)
	}
	if len(enc) == 0 {
		t.Fatal("sealed secret empty")
	}
	got, err := s.OpenTOTPSecret(enc)
	if err != nil || got != key.Secret() {
		t.Fatalf("seal/open round-trip: got=%q err=%v", got, err)
	}
	rec := &UserRecord{TOTPSecretEnc: enc}

	now := time.Now()
	code, _ := totp.GenerateCode(key.Secret(), now)
	if ok, err := s.VerifyTOTP(rec, code, now); err != nil || !ok {
		t.Fatalf("verify current code: ok=%v err=%v", ok, err)
	}
	// ±1 step accepted.
	prev, _ := totp.GenerateCode(key.Secret(), now.Add(-30*time.Second))
	if ok, _ := s.VerifyTOTP(rec, prev, now); !ok {
		t.Fatal("verify -1 step code: rejected, want accepted (skew 1)")
	}
	next, _ := totp.GenerateCode(key.Secret(), now.Add(30*time.Second))
	if ok, _ := s.VerifyTOTP(rec, next, now); !ok {
		t.Fatal("verify +1 step code: rejected, want accepted (skew 1)")
	}
	// Outside skew rejected.
	far, _ := totp.GenerateCode(key.Secret(), now.Add(5*time.Minute))
	if ok, _ := s.VerifyTOTP(rec, far, now); ok {
		t.Fatal("verify +10 step code: accepted, want rejected")
	}
	// Unenrolled user → false, no error.
	if ok, err := s.VerifyTOTP(&UserRecord{}, code, now); err != nil || ok {
		t.Fatalf("unenrolled verify: ok=%v err=%v", ok, err)
	}
}

func TestWebAuthnCredential_RoundTrips(t *testing.T) {
	s := openTestStore(t)
	ph, _ := HashPassword("pw")
	cred := webauthn.Credential{
		ID:        []byte{1, 2, 3, 4},
		PublicKey: []byte{9, 8, 7},
		Transport: nil,
	}
	cred.Authenticator.SignCount = 7
	if err := s.CreateFirstAdmin(&UserRecord{Email: "a@x.com", PasswordHash: ph, Credentials: []webauthn.Credential{cred}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	u, err := s.GetUser("a@x.com")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(u.WebAuthnCredentials()) != 1 {
		t.Fatalf("creds len = %d", len(u.WebAuthnCredentials()))
	}
	rt := u.WebAuthnCredentials()[0]
	if string(rt.ID) != string(cred.ID) || rt.Authenticator.SignCount != 7 {
		t.Fatalf("credential did not round-trip: %+v", rt)
	}
	if string(u.WebAuthnID()) != string(u.Handle) || u.WebAuthnName() != "a@x.com" {
		t.Fatal("webauthn.User interface mismatch")
	}
}

func TestUserRecord_OldRecordDecodesZeroValue(t *testing.T) {
	// A record written before the Credentials/TOTP fields existed (only the early
	// fields) must decode with zero values — the migration-free property.
	old := `{"email":"a@x.com","display_name":"A","password_hash":"$argon2id$x","scope":"*:admin"}`
	var rec UserRecord
	if err := json.Unmarshal([]byte(old), &rec); err != nil {
		t.Fatalf("decode old record: %v", err)
	}
	if rec.HasTOTP() {
		t.Fatal("old record should have no TOTP")
	}
	if len(rec.Credentials) != 0 {
		t.Fatal("old record should have no credentials")
	}
	if !rec.IsAdmin() {
		t.Fatal("old admin record IsAdmin()=false")
	}
}

func TestSessions_MintResolveLogoutExpire(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	sess, err := s.CreateSession("a@x.com", now, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.LookupSession(sess.ID, now)
	if err != nil || got.Email != "a@x.com" {
		t.Fatalf("LookupSession: got=%+v err=%v", got, err)
	}
	// Logout deletes it.
	if err := s.DeleteSession(sess.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.LookupSession(sess.ID, now); err != ErrNotFound {
		t.Fatalf("after logout: want ErrNotFound, got %v", err)
	}
	// Expired session rejected and swept on lookup.
	sess2, _ := s.CreateSession("a@x.com", now, time.Minute)
	if _, err := s.LookupSession(sess2.ID, now.Add(2*time.Minute)); err != ErrExpired {
		t.Fatalf("expired lookup: want ErrExpired, got %v", err)
	}
	if _, err := s.LookupSession(sess2.ID, now); err != ErrNotFound {
		t.Fatalf("expired session should be deleted on lookup, got %v", err)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	live, _ := s.CreateSession("a@x.com", now, time.Hour)
	_, _ = s.CreateSession("b@x.com", now, time.Minute)
	n, err := s.DeleteExpiredSessions(now.Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d, want 1", n)
	}
	if _, err := s.LookupSession(live.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("live session swept: %v", err)
	}
}
