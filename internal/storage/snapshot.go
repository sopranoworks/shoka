package storage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// snapshotWALDrain bounds the best-effort WaitForWAL before a snapshot. A
// timeout is not fatal: the current HEAD is always a consistent immutable commit
// even if a write is still un-drained, and a slightly-stale-but-consistent
// backup beats none.
const snapshotWALDrain = 30 * time.Second

// snapshotModTime is the fixed modification time stamped on every archive entry.
// Git tracks no mtime, so a fixed value makes the archive content-determined
// (reproducible) — stabler for retention/dedup and tests.
var snapshotModTime = time.Unix(0, 0).UTC()

// SnapshotProject archives the project's immutable HEAD tree to w as gzip+tar.
// It mirrors the lock-free independent-handle model of ReadFileAtVersion /
// DiffVersions: an independent PlainOpen, immutable tree/blob reads, and NO lock
// across the heavy archive. The only pre-archive step is a bounded best-effort
// WaitForWAL so HEAD reflects the latest committed writes. Entries are emitted in
// the tree's git-sorted order with fixed mtime/uid/gid for reproducibility; non-
// file entries (directories, submodules) are skipped, symlinks become tar symlink
// headers. ctx cancellation is honoured between entries.
func (s *FSGitStorage) SnapshotProject(ctx context.Context, namespace, projectName string, w io.Writer) error {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return err
	}

	// Best-effort drain so HEAD is current. Not a lock; a timeout is not fatal —
	// the resolved HEAD is consistent regardless.
	s.WaitForWAL(snapshotWALDrain)

	// Independent handle, no lock — same as the history reads.
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	// Resolve HEAD's tree. A commit-less project (no HEAD) is not an error: it
	// yields a valid empty archive (friendlier for a whole-store fan-out).
	headRef, headErr := r.Head()
	if headErr == nil {
		commit, err := r.CommitObject(headRef.Hash())
		if err != nil {
			return fmt.Errorf("failed to get HEAD commit: %w", err)
		}
		tree, err := commit.Tree()
		if err != nil {
			return fmt.Errorf("failed to get HEAD tree: %w", err)
		}
		if err := archiveTree(ctx, tree, tw); err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("failed to finalise tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("failed to finalise gzip: %w", err)
	}
	return nil
}

// archiveTree walks tree recursively (immutable-object reads only, no lock) and
// streams every file entry into tw.
func archiveTree(ctx context.Context, tree *object.Tree, tw *tar.Writer) error {
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		name, entry, err := walker.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to walk tree: %w", err)
		}

		// Skip directories and submodules/gitlinks — only regular/executable
		// files and symlinks are archived.
		if !entry.Mode.IsFile() {
			continue
		}

		file, err := tree.TreeEntryFile(&entry)
		if err != nil {
			return fmt.Errorf("failed to resolve blob for %q: %w", name, err)
		}

		if err := archiveFile(name, entry.Mode, file, tw); err != nil {
			return err
		}
	}
}

// archiveFile writes one file (or symlink) entry into tw.
func archiveFile(name string, mode filemode.FileMode, file *object.File, tw *tar.Writer) error {
	osMode, err := mode.ToOSFileMode()
	if err != nil {
		return fmt.Errorf("failed to map mode for %q: %w", name, err)
	}

	hdr := &tar.Header{
		Name:    name,
		Mode:    int64(osMode.Perm()),
		ModTime: snapshotModTime,
		Format:  tar.FormatPAX,
	}

	if mode == filemode.Symlink {
		// A symlink blob's content is the link target.
		target, err := file.Contents()
		if err != nil {
			return fmt.Errorf("failed to read symlink target %q: %w", name, err)
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = target
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write symlink header %q: %w", name, err)
		}
		return nil
	}

	hdr.Typeflag = tar.TypeReg
	hdr.Size = file.Size
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("failed to write header %q: %w", name, err)
	}

	rc, err := file.Reader()
	if err != nil {
		return fmt.Errorf("failed to read blob %q: %w", name, err)
	}
	defer rc.Close()

	if _, err := io.Copy(tw, rc); err != nil {
		return fmt.Errorf("failed to stream %q: %w", name, err)
	}
	return nil
}
