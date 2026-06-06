// Package skillcache implements the apt-model skill distribution substrate for
// shoka-cli (B-15c): a local on-disk cache synced from a remote skills repo by
// the `git` BINARY (os/exec), plus skill discovery, a deterministic per-skill
// content hash (the drift signal), and a recursive copy into a runtime
// convention directory.
//
// Two hard rules shape this package:
//
//   - NO go-git. internal/archlint scans the whole module and cmd/shoka-cli (and
//     anything it imports outside internal/storage) may not import go-git
//     (Anchor 2). All git access here is a shell-out to the `git` binary.
//   - Skills are NOT Shoka data. This package never touches the MCP data path
//     (write_file/read_file) or the ingest allowlist (B-37/phantom); it is git +
//     filesystem only.
//
// The cache lives at os.UserCacheDir()/shoka/skills. A "skill" is a top-level
// directory in the cache (or in a convention dir) that contains a SKILL.md file;
// other top-level entries (e.g. a repo README.md) are not skills.
package skillcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// skillMarker is the file whose presence makes a directory a skill.
const skillMarker = "SKILL.md"

// DefaultCacheDir returns the on-disk skills cache path,
// os.UserCacheDir()/shoka/skills. It honours $XDG_CACHE_HOME on Linux and
// resolves under ~/Library/Caches on macOS (whatever os.UserCacheDir reports).
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "shoka", "skills"), nil
}

// SyncResult reports what a Sync changed in the cache, by skill name.
type SyncResult struct {
	Added     []string // present after, absent before
	Updated   []string // present in both, content hash changed
	Unchanged []string // present in both, content hash identical
	Removed   []string // absent after, present before
}

// Sync clones (first run) or fetches+hard-resets (subsequent runs) the skills
// repository at repo into cacheDir, using the git binary. ref is an optional
// branch or tag to sync; when empty, the remote's default branch (HEAD) is used.
// It returns a per-skill diff of the cache before vs after.
//
// This is the ONE network operation in the skill line. repo is required (there
// is no baked-in default — the caller passes --repo).
func Sync(cacheDir, repo, ref string) (SyncResult, error) {
	if strings.TrimSpace(repo) == "" {
		return SyncResult{}, fmt.Errorf("a remote skills repository is required (pass --repo)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return SyncResult{}, fmt.Errorf("the git executable was not found in PATH (skill update shells out to git): %w", err)
	}

	before, err := snapshotHashes(cacheDir)
	if err != nil {
		return SyncResult{}, err
	}

	gitDir := filepath.Join(cacheDir, ".git")
	if _, statErr := os.Stat(gitDir); statErr != nil {
		// First sync: clone into the cache. git clone needs the target to be
		// absent or empty; create only the parent at 0700.
		if err := os.MkdirAll(filepath.Dir(cacheDir), 0o700); err != nil {
			return SyncResult{}, fmt.Errorf("create cache parent: %w", err)
		}
		if err := runGit("", "clone", repo, cacheDir); err != nil {
			return SyncResult{}, err
		}
	} else {
		// Subsequent sync: re-point origin (the repo may differ run to run, since
		// it is not persisted) and update in place.
		if err := runGit(cacheDir, "remote", "set-url", "origin", repo); err != nil {
			return SyncResult{}, err
		}
	}

	target := strings.TrimSpace(ref)
	if target == "" {
		target = "HEAD" // the remote's default branch
	}
	if err := runGit(cacheDir, "fetch", "origin", target); err != nil {
		return SyncResult{}, err
	}
	if err := runGit(cacheDir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return SyncResult{}, err
	}
	if err := runGit(cacheDir, "clean", "-fd"); err != nil {
		return SyncResult{}, err
	}

	after, err := snapshotHashes(cacheDir)
	if err != nil {
		return SyncResult{}, err
	}
	return diffHashes(before, after), nil
}

// List returns the names of the skills under dir (top-level directories that
// contain a SKILL.md), sorted. A missing dir yields an empty list, no error.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if isSkillDir(filepath.Join(dir, e.Name())) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Has reports whether name is a skill present under dir.
func Has(dir, name string) bool {
	return isSkillDir(filepath.Join(dir, name))
}

// isSkillDir reports whether path is a directory containing a SKILL.md.
func isSkillDir(path string) bool {
	fi, err := os.Stat(filepath.Join(path, skillMarker))
	return err == nil && !fi.IsDir()
}

// DirHash returns a deterministic content hash of the skill directory at path:
// SHA-256 over the sorted list of (relative-path, file-content-hash) for every
// regular file under path. Hashing the whole directory — not just SKILL.md —
// means an added, removed, or renamed supporting file is detected as drift.
func DirHash(path string) (string, error) {
	type entry struct{ rel, sum string }
	var entries []entry
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks/devices: not part of a skill's content
		}
		rel, rerr := filepath.Rel(path, p)
		if rerr != nil {
			return rerr
		}
		sum, ferr := fileHash(p)
		if ferr != nil {
			return ferr
		}
		entries = append(entries, entry{rel: filepath.ToSlash(rel), sum: sum})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		// NUL/newline framing so distinct (rel, sum) lists cannot collide.
		fmt.Fprintf(h, "%s\x00%s\n", e.rel, e.sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CopySkill copies the skill directory at src into dstParent as dstParent/<name>,
// where name is src's base name. Any existing destination skill directory is
// removed first so the copy is a clean replacement (no stale files survive an
// upgrade). It copies regular files only, preserving their permission bits.
func CopySkill(src, dstParent string) (string, error) {
	name := filepath.Base(src)
	dst := filepath.Join(dstParent, name)
	if err := os.RemoveAll(dst); err != nil {
		return "", fmt.Errorf("clear destination %s: %w", dst, err)
	}
	if err := os.MkdirAll(dstParent, 0o755); err != nil {
		return "", fmt.Errorf("create destination parent %s: %w", dstParent, err)
	}
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		if !d.Type().IsRegular() {
			return nil // skip non-regular files
		}
		return copyFile(p, target)
	})
	if err != nil {
		return "", err
	}
	return dst, nil
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// snapshotHashes returns a map of skill name -> DirHash for every skill in dir.
// A missing dir yields an empty map (the pre-clone state).
func snapshotHashes(dir string) (map[string]string, error) {
	names, err := List(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		h, herr := DirHash(filepath.Join(dir, n))
		if herr != nil {
			return nil, herr
		}
		out[n] = h
	}
	return out, nil
}

func diffHashes(before, after map[string]string) SyncResult {
	var res SyncResult
	for name, h := range after {
		old, ok := before[name]
		switch {
		case !ok:
			res.Added = append(res.Added, name)
		case old != h:
			res.Updated = append(res.Updated, name)
		default:
			res.Unchanged = append(res.Unchanged, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			res.Removed = append(res.Removed, name)
		}
	}
	sort.Strings(res.Added)
	sort.Strings(res.Updated)
	sort.Strings(res.Unchanged)
	sort.Strings(res.Removed)
	return res
}

// runGit runs the git binary with args, in dir (cwd when dir == ""). On failure
// it surfaces git's stderr. Only args[0] (the subcommand) is named in the error
// prefix; git may echo the operator-supplied repo in its stderr, which is the
// caller's own input, not a baked-in deployment detail.
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("git %s: %w: %s", args[0], err, msg)
		}
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}
