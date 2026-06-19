package userstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// newHandleBytes returns 32 random bytes for an opaque WebAuthn user handle.
func newHandleBytes() ([]byte, error) {
	h := make([]byte, 32)
	if _, err := rand.Read(h); err != nil {
		return nil, fmt.Errorf("userstore: generate handle: %w", err)
	}
	return h, nil
}

// ErrInvalidInvite is returned by RedeemInvite when the code is unknown, already
// used, or expired.
var ErrInvalidInvite = errors.New("userstore: invalid or expired invite")

// --- user administration (B-28 stage 3) -------------------------------------

// UserInfo is the no-secret view of a user for the management screen.
type UserInfo struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Scope       string `json:"scope"`
	Disabled    bool   `json:"disabled"`
}

// ListUsers returns every user as a no-secret UserInfo (never the password hash,
// TOTP secret, or credentials), sorted by email for a stable admin list.
func (s *Store) ListUsers() ([]UserInfo, error) {
	var out []UserInfo
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(usersBucket)).ForEach(func(_, v []byte) error {
			var rec UserRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("userstore: decode user: %w", err)
			}
			out = append(out, UserInfo{Email: rec.Email, DisplayName: rec.DisplayName, Scope: rec.Scope, Disabled: rec.Disabled})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

// UpdateUserScope sets a user's authorization scope (the admin-only permission
// edit). Returns ErrNotFound if the user does not exist.
func (s *Store) UpdateUserScope(email, scope string) error {
	key := normalizeEmail(email)
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(usersBucket))
		v := b.Get([]byte(key))
		if v == nil {
			return ErrNotFound
		}
		var rec UserRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("userstore: decode user: %w", err)
		}
		rec.Scope = scope
		rec.UpdatedAt = time.Now()
		val, err := json.Marshal(&rec)
		if err != nil {
			return fmt.Errorf("userstore: encode user: %w", err)
		}
		return b.Put([]byte(key), val)
	})
}

// RemoveUser deletes a user and revokes any sessions bound to that email (so a
// removed user is logged out everywhere). Returns ErrNotFound if unknown. The caller
// MUST refuse a self-removal before calling this (the management UI omits self; the
// server self-guard lives at the call site, which knows the acting principal).
func (s *Store) RemoveUser(email string) error {
	key := normalizeEmail(email)
	return s.db.Update(func(tx *bolt.Tx) error {
		ub := tx.Bucket([]byte(usersBucket))
		if ub.Get([]byte(key)) == nil {
			return ErrNotFound
		}
		if err := ub.Delete([]byte(key)); err != nil {
			return err
		}
		return dropSessionsTx(tx, key)
	})
}

// SetUserDisabled sets a user's Disabled flag (the admin-only enable/disable). When
// disabling, it ALSO drops that user's live sessions in the same transaction — a
// disabled user is locked out immediately (the OAuth/MCP token revocation is a
// cross-store call wired at the handler layer, since oauthstore is separate). Returns
// ErrNotFound if the user does not exist. The caller MUST refuse a self-disable before
// calling this (the server self-guard lives at the call site, which knows the acting
// principal).
func (s *Store) SetUserDisabled(email string, disabled bool) error {
	key := normalizeEmail(email)
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(usersBucket))
		v := b.Get([]byte(key))
		if v == nil {
			return ErrNotFound
		}
		var rec UserRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("userstore: decode user: %w", err)
		}
		rec.Disabled = disabled
		rec.UpdatedAt = time.Now()
		val, err := json.Marshal(&rec)
		if err != nil {
			return fmt.Errorf("userstore: encode user: %w", err)
		}
		if err := b.Put([]byte(key), val); err != nil {
			return err
		}
		if disabled {
			return dropSessionsTx(tx, key)
		}
		return nil
	})
}

// dropSessionsTx deletes every session bound to the (already-normalized) email key,
// in the given transaction. Keys are collected during the scan and deleted after it,
// never mutating the bucket mid-iteration (the DeleteDeadSeries discipline).
func dropSessionsTx(tx *bolt.Tx, emailKey string) error {
	sb := tx.Bucket([]byte(sessionsBucket))
	var dead [][]byte
	err := sb.ForEach(func(k, v []byte) error {
		var sess SessionRecord
		if err := json.Unmarshal(v, &sess); err != nil {
			return nil // skip undecodable
		}
		if sess.Email == emailKey {
			dead = append(dead, append([]byte(nil), k...))
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, k := range dead {
		if derr := sb.Delete(k); derr != nil {
			return derr
		}
	}
	return nil
}

// RewriteScopes applies fn to every user's scope AND every pending invite's scope,
// rewriting any record whose scope fn changes — the cascade cleanup after a namespace/
// project delete (B-28 ns/proj management). fn is a pure scope→scope transform (the
// authz prune helpers); the store stays grammar-agnostic (it treats Scope as opaque). It
// returns the number of records changed (users + invites). One write transaction; keys
// are collected during each scan and written after it, never mutating mid-iteration
// (the RemoveUser / DeleteDeadSeries discipline).
func (s *Store) RewriteScopes(fn func(scope string) string) (int, error) {
	changed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		// Users.
		ub := tx.Bucket([]byte(usersBucket))
		type userKV struct {
			k   []byte
			rec UserRecord
		}
		var userUpd []userKV
		if err := ub.ForEach(func(k, v []byte) error {
			var rec UserRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("userstore: decode user: %w", err)
			}
			if ns := fn(rec.Scope); ns != rec.Scope {
				rec.Scope = ns
				rec.UpdatedAt = time.Now()
				userUpd = append(userUpd, userKV{append([]byte(nil), k...), rec})
			}
			return nil
		}); err != nil {
			return err
		}
		for _, u := range userUpd {
			val, err := json.Marshal(&u.rec)
			if err != nil {
				return fmt.Errorf("userstore: encode user: %w", err)
			}
			if err := ub.Put(u.k, val); err != nil {
				return err
			}
			changed++
		}

		// Pending invites.
		ib := tx.Bucket([]byte(invitesBucket))
		type inviteKV struct {
			k   []byte
			rec InviteRecord
		}
		var invUpd []inviteKV
		if err := ib.ForEach(func(k, v []byte) error {
			var rec InviteRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("userstore: decode invite: %w", err)
			}
			if ns := fn(rec.Scope); ns != rec.Scope {
				rec.Scope = ns
				invUpd = append(invUpd, inviteKV{append([]byte(nil), k...), rec})
			}
			return nil
		}); err != nil {
			return err
		}
		for _, u := range invUpd {
			val, err := json.Marshal(&u.rec)
			if err != nil {
				return fmt.Errorf("userstore: encode invite: %w", err)
			}
			if err := ib.Put(u.k, val); err != nil {
				return err
			}
			changed++
		}
		return nil
	})
	return changed, err
}

// --- invites (B-28 stage 3) --------------------------------------------------

// InviteRecord is one outstanding invitation: a hash of the single-use code (the
// code itself is shown once at creation and never stored), the invited email, the
// scope the redeemed account will receive, an expiry, and a used flag.
type InviteRecord struct {
	CodeHash  string    `json:"code_hash"` // hex sha256 of the code = the bucket key
	Email     string    `json:"email"`
	Scope     string    `json:"scope"`
	Expiry    time.Time `json:"expiry"`
	Used      bool      `json:"used"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// InviteInfo is the no-secret listing view (carries the code hash as the handle the
// admin revokes by, never the code).
type InviteInfo struct {
	CodeHash  string    `json:"code_hash"`
	Email     string    `json:"email"`
	Scope     string    `json:"scope"`
	Expiry    time.Time `json:"expiry"`
	Used      bool      `json:"used"`
	CreatedAt time.Time `json:"created_at"`
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// CreateInvite mints a single-use, time-limited invite for email at scope, returning
// the plaintext code (shown ONCE to the admin to convey out-of-band) and the stored
// record. Only the code's hash is persisted.
func (s *Store) CreateInvite(email, scope, createdBy string, now time.Time, ttl time.Duration) (code string, rec InviteRecord, err error) {
	code, err = NewHandle()
	if err != nil {
		return "", InviteRecord{}, err
	}
	rec = InviteRecord{
		CodeHash:  hashCode(code),
		Email:     normalizeEmail(email),
		Scope:     scope,
		Expiry:    now.Add(ttl),
		CreatedBy: normalizeEmail(createdBy),
		CreatedAt: now,
	}
	val, merr := json.Marshal(&rec)
	if merr != nil {
		return "", InviteRecord{}, fmt.Errorf("userstore: encode invite: %w", merr)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(invitesBucket)).Put([]byte(rec.CodeHash), val)
	}); err != nil {
		return "", InviteRecord{}, err
	}
	return code, rec, nil
}

// ListInvites returns all invite records as no-secret InviteInfo, newest first.
func (s *Store) ListInvites() ([]InviteInfo, error) {
	var out []InviteInfo
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(invitesBucket)).ForEach(func(_, v []byte) error {
			var rec InviteRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("userstore: decode invite: %w", err)
			}
			out = append(out, InviteInfo{
				CodeHash: rec.CodeHash, Email: rec.Email, Scope: rec.Scope,
				Expiry: rec.Expiry, Used: rec.Used, CreatedAt: rec.CreatedAt,
			})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// InviteByCode resolves a plaintext code to its no-secret InviteInfo WITHOUT
// consuming it — for the redeem screen to show the invitee their fixed email/scope.
// Returns ErrInvalidInvite if unknown, used, or expired.
func (s *Store) InviteByCode(code string, now time.Time) (InviteInfo, error) {
	ch := hashCode(code)
	var rec InviteRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(invitesBucket)).Get([]byte(ch))
		if raw == nil {
			return ErrInvalidInvite
		}
		return json.Unmarshal(raw, &rec)
	})
	if err != nil {
		return InviteInfo{}, err
	}
	if rec.Used || now.After(rec.Expiry) {
		return InviteInfo{}, ErrInvalidInvite
	}
	return InviteInfo{
		CodeHash: rec.CodeHash, Email: rec.Email, Scope: rec.Scope,
		Expiry: rec.Expiry, Used: rec.Used, CreatedAt: rec.CreatedAt,
	}, nil
}

// RevokeInvite deletes an outstanding invite by its code hash. Idempotent.
func (s *Store) RevokeInvite(codeHash string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(invitesBucket)).Delete([]byte(codeHash))
	})
}

// RedeemInvite atomically consumes a valid invite and creates the invitee's account.
// In one transaction it: looks up the invite by hash(code); rejects unknown / used /
// expired (ErrInvalidInvite); fills rec.Email and rec.Scope FROM the invite (the
// invitee cannot choose either); refuses if the email is already registered
// (ErrExists); writes the user; marks the invite used. The caller supplies rec with
// the credentials it collected (password hash, optional sealed TOTP); WebAuthn
// credentials are added afterward via the authenticated enrolment path (as in
// first-run). Single-use is guaranteed by the in-tx used-flag flip.
func (s *Store) RedeemInvite(code string, now time.Time, rec *UserRecord) error {
	ch := hashCode(code)
	if len(rec.Handle) == 0 {
		h, err := newHandleBytes()
		if err != nil {
			return err
		}
		rec.Handle = h
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		ib := tx.Bucket([]byte(invitesBucket))
		raw := ib.Get([]byte(ch))
		if raw == nil {
			return ErrInvalidInvite
		}
		var inv InviteRecord
		if err := json.Unmarshal(raw, &inv); err != nil {
			return fmt.Errorf("userstore: decode invite: %w", err)
		}
		if inv.Used || now.After(inv.Expiry) {
			return ErrInvalidInvite
		}
		ub := tx.Bucket([]byte(usersBucket))
		rec.Email = inv.Email
		rec.Scope = inv.Scope
		if ub.Get([]byte(rec.Email)) != nil {
			return ErrExists
		}
		if rec.CreatedAt.IsZero() {
			rec.CreatedAt = now
		}
		rec.UpdatedAt = now
		uval, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("userstore: encode user: %w", err)
		}
		if err := ub.Put([]byte(rec.Email), uval); err != nil {
			return err
		}
		inv.Used = true
		ival, err := json.Marshal(&inv)
		if err != nil {
			return fmt.Errorf("userstore: encode invite: %w", err)
		}
		return ib.Put([]byte(ch), ival)
	})
}
