package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage/catalog"
	"github.com/sopranoworks/shoka/internal/storage/deletedlog"
	"github.com/sopranoworks/shoka/internal/storage/filelock"
	"github.com/sopranoworks/shoka/internal/storage/index"
	"github.com/sopranoworks/shoka/internal/storage/nsregistry"
	"github.com/sopranoworks/shoka/internal/storage/wal"
	"github.com/sopranoworks/shoka/internal/storage/walworker"
	"github.com/sopranoworks/shoka/internal/utils"
)

// FSGitStorage implements StorageService with the file system as the ground
// truth and git as a background audit log (the 2026-05-30 storage redesign).
//
// Reads are pure os.ReadFile — no lock, no git. Writes take a per-file lock only
// across the check-and-write critical section, append to a write-ahead log, and
// return immediately; the git commit happens later in a background worker pool.
// The old global s.mu is gone.
type FSGitStorage struct {
	baseDir string

	locks         *filelock.Manager
	wal           *wal.Log
	pool          *walworker.Pool
	maxWALEntries int

	stateMu sync.RWMutex
	states  map[string]ProjectState

	// catalogs holds one open per-project catalog (the 2026-05-30 catalog
	// directive) keyed by "<namespace>/<project>". Catalogs are opened/rebuilt at
	// startup, created on CreateProject, and opened lazily otherwise. The bbolt
	// handles are concurrency-safe; catMu guards only the map.
	catMu    sync.Mutex
	catalogs map[string]*catalog.Catalog

	// indexes holds one open per-project derivative index (the 2026-06-04 I1
	// directive) keyed by "<namespace>/<project>", at the sibling path
	// <base>/<ns>/<project>.index.db. Like the catalog it is a disposable
	// derivative of the truth, opened lazily and rebuilt by StartIndexSweep when
	// missing/stale/corrupt. The bbolt handles are concurrency-safe; idxMu guards
	// only the map. I1 keeps this store warm but no query reads it yet (the fast
	// path is I2/I3).
	idxMu   sync.Mutex
	indexes map[string]*index.Index

	// deletedLogs holds one open per-project deleted-file log (the 2026-06-18
	// deleted-log directive) keyed by "<namespace>/<project>", at the sibling path
	// <base>/<ns>/<project>.deleted.db. It is a SEPARATE store (not a bucket in the
	// index — keeps the index's present-paths-only invariant), a disposable
	// derivative of git: the commit-land hook keeps it live and a bounded two-trigger
	// repair rebuilds it when absent. dlMu guards only the map; the bbolt handles are
	// concurrency-safe. dlUpdateFailed counts best-effort hook failures (the I1
	// idxUpdateFailed precedent): a failure never fails the commit; repair is the net.
	dlMu           sync.Mutex
	deletedLogs    map[string]*deletedlog.Store
	dlUpdateFailed atomic.Int64

	// Deleted-log tunables (from Options.DeletedLog / config storage.deleted_log).
	// There is deliberately NO interval: repair is lazy on two triggers (log absent;
	// revival-hash-gone), never a background sweep. repairDepth bounds the recent-
	// commit rebuild walk; maxEntries is the FIFO cap.
	deletedLogEnabled     bool
	deletedLogRepairDepth int
	deletedLogMaxEntries  int

	// fixLinksKicks carries post-move fix_links reconciliation requests (I3). A
	// successful Move does a non-blocking send here (so a move never blocks on a
	// full channel — it stays a pure rename); the StartIndexSweep goroutine drains
	// it via its select loop and runs fixLinks. There is no periodic backstop: a
	// dropped kick leaves a stale-but-recoverable link the tenets absorb.
	fixLinksKicks chan fixLinksKick

	// Index observability counters: best-effort incremental-update failures and
	// repair-sweep rebuilds. Update failures are surfaced via IndexCounters();
	// rebuilds are split by reason (stale marker vs recreated nil-handle) and
	// surfaced reason-labelled via IndexRebuildCounters(). IndexCounters() still
	// returns their sum as the rebuild total for callers that want the aggregate.
	idxUpdateFailedWrite  atomic.Int64
	idxUpdateFailedDelete atomic.Int64
	idxRebuildsStale      atomic.Int64
	idxRebuildsRecreated  atomic.Int64

	// Index-line class-B: index repair-sweep passes (the 2026-06-05 M2 directive).
	// A plain atomic incremented once per reconcile pass, read on scrape via
	// IndexSweepRuns(). Distinct from rebuilds: a pass that rebuilds nothing still
	// counts (the metric shows the worker is alive and how often it reconciles).
	idxSweepRuns atomic.Int64

	// I2 content-search fast-path outcome (the 2026-06-05 M2 directive). One add
	// per content-searching query at the engage/fallback decision in SearchFiles —
	// fastpath when the healthy index narrows reads, fallback when no query bigram
	// or an unhealthy/absent index means every file is read. One atomic per query,
	// never per file (the WalkDir loop is untouched). Filename-only searches never
	// reach the decision and are counted in neither. Read via SearchFastpathStats().
	searchFastpath atomic.Int64
	searchFallback atomic.Int64

	// I3 fix_links worker counters (the 2026-06-05 M2 directive). enqueued/dropped
	// split the post-move kick at enqueueFixLinks (a full cap-256 channel drops —
	// the dropped count is the key health signal that link repairs are being lost);
	// this one pair is incremented on the Move (request) path, one atomic per move.
	// The remaining four run on the sweep goroutine that drains the kick: rewrites
	// (successful if_match referrer rewrites), conflicts (VersionConflictError
	// back-offs), and lookupIndex/lookupTruthscan (referrers answered by the index
	// vs the discoverReferrers truth-scan). Read via FixLinks* Source methods.
	fixLinksEnqueued        atomic.Int64
	fixLinksDropped         atomic.Int64
	fixLinksRewrites        atomic.Int64
	fixLinksConflicts       atomic.Int64
	fixLinksLookupIndex     atomic.Int64
	fixLinksLookupTruthscan atomic.Int64

	// lost+found worker counters (the 2026-06-05 M3 directive). sweeps is
	// incremented once per sweepAllProjects pass (initial + each ticker tick),
	// distinct from actions (a pass that acts on nothing still counts) — the
	// sweep-pass analogue of idxSweepRuns. disposed/moved split the two action
	// arms in sweepProject (an untracked file matching shoka.disposable is deleted;
	// otherwise it is relocated to lost+found); the third arm (tracked → untouched)
	// is not an action and is not counted. There is deliberately NO "quarantined"
	// action: the sweep never quarantines — that is the D3 walworker's deposit,
	// already counted as shoka_wal_quarantined_total (M1). skippedCorrupted/
	// skippedDangerous split the healthy-only gate (sweepProject returns without
	// acting on a non-healthy project), labelled by the two non-healthy
	// ProjectStates. Read via LostFound* Source methods.
	lostFoundSweeps           atomic.Int64
	lostFoundDisposed         atomic.Int64
	lostFoundMoved            atomic.Int64
	lostFoundSkippedCorrupted atomic.Int64
	lostFoundSkippedDangerous atomic.Int64

	// Catalog observability counters, surfaced through the metrics Source (§10).
	catUpdateFailedWrite   atomic.Int64
	catUpdateFailedDelete  atomic.Int64
	catInvariantViolations atomic.Int64
	catRebuildMissing      atomic.Int64
	catRebuildCorrupt      atomic.Int64
	catRebuildSchema       atomic.Int64
	catRebuildUnreadable   atomic.Int64

	// lazyRescans counts D1 lazy-rescan-on-corrupted-hit invocations: a write that
	// hit checkWritable's case StateCorrupted and re-ran DetectDrift before deciding
	// (B-25). Observability for the self-extinguishing cost — a healthy project never
	// increments it; a genuinely-corrupted one re-pays per write attempt until
	// recovery clears it. Surfaced via LazyRescanCount().
	lazyRescans atomic.Int64

	// notify is the in-process notification center (internal/notify). It may be
	// nil; every call site uses the nil-safe receiver method, so storage never
	// guards it. It is a side-channel: a failure to publish (impossible in the
	// MVP) must never affect a storage operation's outcome.
	notify *notify.Center

	// scopeCleaner removes authorization grants that reference a deleted namespace/
	// project BY NAME (the B-28 cascade cleanup) from the userstore + oauthstore + invite
	// scopes, so re-creating the same name never resurrects old access. It is wired at
	// composition (cmd/shoka) over those sibling stores via SetScopeCleaner; storage
	// only knows the interface, keeping the go-git layer decoupled from the auth stores.
	// nil = no-op (e.g. tests with no user/oauth stores) — DeleteProject/DeleteNamespace
	// then perform only the on-disk removal.
	scopeCleaner ScopeCleaner

	// nsReg is the managed-namespace registry (B-28 ns/proj-management stage A): the
	// EXPLICIT set of namespaces Shoka manages + their managed project names, at
	// <base>/namespaces.db. It is "what should be" — the record that outlives a directory's
	// disappearance, which ListNamespaces returns (the managed set, NOT every subdir) and
	// which stage B's health check compares against on-disk/git reality. Storage-owned
	// (opened here, not injected like userstore/oauthstore) because ListNamespaces /
	// CreateNamespace / DeleteNamespace / Create-/DeleteProject all maintain it.
	nsReg *nsregistry.Registry

	// moveMu serializes project moves (B-28 project move). A move is a SPECIAL op for which
	// coarse locking is acceptable for safety, so only one move runs at a time — no
	// move-vs-move interleaving can occur. moving is the set of "<ns>/<project>" keys
	// currently being moved (both the source and target keys for the duration), which
	// checkWritable consults to refuse writes to a moving project with the retriable
	// ErrProjectMoving; reads take no lock and are not fenced (the dir rename is atomic and
	// handles are evicted first, so a racing read sees the old location or not-found).
	// moveMu (the op-mutex) serializes every SPECIAL op — move + ns/proj rename (B-28) — so
	// only one runs at a time. moving holds the "<ns>/<project>" keys currently being moved or
	// renamed; movingNs holds the namespace names currently being renamed (a whole-namespace
	// quiesce: a namespace rename relabels every project under it at once, so checkWritable
	// fences the namespace as a whole rather than enumerating-and-marking each project, which
	// would race a concurrent project create). Both are consulted by checkWritable to refuse
	// writes with the retriable ErrProjectMoving for the op's (short, serialized) duration.
	moveMu   sync.Mutex
	movingMu sync.Mutex
	moving   map[string]bool
	movingNs map[string]bool

	// identityDefaults is the configured single-user identity + agent fallback
	// used to author commits (the 2026-06-01 identity-config directive). The
	// per-request agent declaration arrives on the write's context; this is the
	// floor it resolves against. PROVISIONAL — see internal/identity (B-28).
	identityDefaults identity.Defaults

	changeHandler ChangeHandler
	logger        *slog.Logger

	// relocWG tracks the non-blocking leftover-relocation goroutine StartupInit
	// spawns (D4 / B-38.1). It is deliberately NOT awaited on the synchronous
	// startup gate — that would undo the D4 §4 property (listener-startup latency
	// must not include a whole-tree move). It is awaited only by Close (clean
	// shutdown: an object awaits its own in-flight goroutine before tearing down)
	// and by in-package tests, which use it to wait for the relocation deposit to
	// finish before t.TempDir() cleanup runs (B-42 — closes the teardown race
	// where RemoveAll caught a mid-deposit lost+found directory non-empty).
	relocWG sync.WaitGroup
}

// Options carries the storage-redesign tunables (§12). Zero values take the
// component packages' defaults.
type Options struct {
	FileLock      filelock.Config
	WALMaxEntries int
	WALWorker     walworker.Config
	// NotifyCenter is the in-process notification center events are published
	// to on successful mutations. It may be nil; storage tolerates that (the
	// hooks become no-ops).
	NotifyCenter *notify.Center

	// Identity is the configured single-user identity + agent fallback used to
	// author commits. Zero-valued fields fall back to safe built-in defaults so
	// storage constructed without identity (e.g. in tests) still produces an
	// intentional author. PROVISIONAL — see internal/identity (B-28).
	Identity identity.Defaults

	// DeletedLog carries the deleted-file log tunables (config storage.deleted_log).
	// The zero value is a sensible default-on store (see deletedLogDefaults).
	DeletedLog DeletedLogOptions
}

// DeletedLogOptions configures the per-project deleted-file log. Enabled is a
// pointer so the zero value (nil) means "default on" — storage built without
// config (e.g. tests) gets a live deleted-log; the config layer passes an explicit
// value. RepairDepth bounds the recent-commit repair walk; MaxEntries is the FIFO
// cap. There is no interval — repair is lazy (two triggers), never a sweep.
type DeletedLogOptions struct {
	Enabled     *bool
	RepairDepth int
	MaxEntries  int
}

// deletedLogDefaults resolves unset deleted-log tunables to the directive's
// defaults (enabled true, repair_depth 50, max_entries 1000). A nil Enabled or a
// non-positive RepairDepth/MaxEntries means "unset"; the config layer applies its
// own defaults too, so this only guards storage built without config (e.g. tests).
func deletedLogDefaults(o DeletedLogOptions) (enabled bool, repairDepth, maxEntries int) {
	enabled = o.Enabled == nil || *o.Enabled
	repairDepth = o.RepairDepth
	if repairDepth <= 0 {
		repairDepth = 50
	}
	maxEntries = o.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return enabled, repairDepth, maxEntries
}

// ChangeEvent describes a successful mutation, delivered to a registered handler
// after the background git commit lands.
type ChangeEvent struct {
	Event      string // file_written | file_deleted | project_created | project_deleted | project_moved
	Namespace  string
	Project    string
	Path       string
	CommitHash string
	Timestamp  time.Time
	// OldNamespace/OldProject are set only for project_moved (B-28 project move): the
	// source the project moved FROM (Namespace/Project hold the new location).
	OldNamespace string
	OldProject   string
}

// ChangeHandler receives ChangeEvents after a successful mutation. It must not
// block (e.g. it should fan out to webhooks asynchronously).
type ChangeHandler func(ChangeEvent)

// Typed errors returned by the write path. Callers (and the MCP tool layer in
// §8) distinguish them to produce the structured {reason} responses.
var (
	// ErrProjectDangerous means the project's .git is unreadable/absent.
	ErrProjectDangerous = errors.New("project is in dangerous state: git repository is unreadable")
	// ErrProjectNotFound means the mutation targeted a project that has no git
	// repository — it was never created (CreateProject git-inits). Without this the
	// write path silently half-creates a project (the B-37 phantom); the guard in
	// checkWritable refuses such ops before any side-effect.
	ErrProjectNotFound = errors.New("project not found: no git repository at the target path")
	// ErrProjectCorrupted means the working tree drifted from git HEAD outside
	// the redesign's write path (hand-edit, git pull, another tool).
	ErrProjectCorrupted = errors.New("project is in corrupted state: working tree has uncommitted drift")
	// ErrWriteDisabled means the WAL has backed up past its threshold.
	ErrWriteDisabled = errors.New("writes are disabled: write-ahead log is full")
	// ErrProjectMoving means the project is being moved or renamed right now — or its
	// namespace is being renamed (B-28 move + ns/proj rename). It is transient and RETRIABLE:
	// the op is a brief, serialized special op, after which the project is writable again at
	// its new location/name.
	ErrProjectMoving = errors.New("project is being moved or renamed; retry shortly")
)

// VersionConflictError is returned by writes/deletes when the caller's expected
// value (if_match) does not match the file's current etag. Current is the
// current etag (sha256 of the file's content right now).
type VersionConflictError struct {
	Expected string
	Current  string
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("etag conflict: expected %q but current etag is %q", e.Expected, e.Current)
}

// SetChangeHandler registers a handler invoked after successful writes, deletes,
// and project creation.
func (s *FSGitStorage) SetChangeHandler(h ChangeHandler) { s.changeHandler = h }

// SetLogger attaches a structured logger.
func (s *FSGitStorage) SetLogger(l *slog.Logger) { s.logger = l }

func (s *FSGitStorage) log() *slog.Logger {
	if s.logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.logger
}

func (s *FSGitStorage) emit(ev ChangeEvent) {
	s.log().Info("git change committed",
		"event", ev.Event,
		"namespace", ev.Namespace,
		"project", ev.Project,
		"path", ev.Path,
		"commit", ev.CommitHash,
	)
	if s.changeHandler != nil {
		s.changeHandler(ev)
	}
}

// NewFSGitStorage creates storage with default tunables.
func NewFSGitStorage(baseDir string) (*FSGitStorage, error) {
	return NewFSGitStorageWithOptions(baseDir, Options{})
}

// NewFSGitStorageWithOptions creates storage and starts the lock reaper and the
// background commit worker pool. Any entries already in the WAL (from a previous
// run) are drained on startup.
func NewFSGitStorageWithOptions(baseDir string, opts Options) (*FSGitStorage, error) {
	absBaseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for base directory: %w", err)
	}
	if err := os.MkdirAll(absBaseDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	w, err := wal.Open(absBaseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open WAL: %w", err)
	}

	// Managed-namespace registry (stage A): a storage-owned sibling bbolt store at
	// <base>/namespaces.db. Opened here so ListNamespaces/Create/Delete can consult the
	// MANAGED set from the moment storage exists; the one-time rescue-adopt + the always-
	// managed `default` are seeded in StartupInit (which captures registry-emptiness first).
	nsReg, err := nsregistry.Open(filepath.Join(absBaseDir, "namespaces.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to open namespace registry: %w", err)
	}

	maxWAL := opts.WALMaxEntries
	if maxWAL <= 0 {
		maxWAL = 1000
	}

	s := &FSGitStorage{
		baseDir:          absBaseDir,
		locks:            filelock.NewManager(opts.FileLock),
		wal:              w,
		maxWALEntries:    maxWAL,
		states:           make(map[string]ProjectState),
		catalogs:         make(map[string]*catalog.Catalog),
		indexes:          make(map[string]*index.Index),
		deletedLogs:      make(map[string]*deletedlog.Store),
		fixLinksKicks:    make(chan fixLinksKick, fixLinksKickBuffer),
		notify:           opts.NotifyCenter,
		nsReg:            nsReg,
		moving:           make(map[string]bool),
		movingNs:         make(map[string]bool),
		identityDefaults: withIdentityFallback(opts.Identity),
	}
	s.deletedLogEnabled, s.deletedLogRepairDepth, s.deletedLogMaxEntries = deletedLogDefaults(opts.DeletedLog)
	s.pool = walworker.NewPool(w, s.commitEntry, s.quarantineEntry, opts.WALWorker)
	return s, nil
}

// withIdentityFallback fills empty identity defaults with safe built-ins, so
// storage built without an Identity (e.g. in tests, or an old config) still
// produces an intentional, non-environmental commit author.
func withIdentityFallback(d identity.Defaults) identity.Defaults {
	if d.UserName == "" {
		d.UserName = "Shoka Operator"
	}
	if d.UserEmail == "" {
		d.UserEmail = "operator@shoka.local"
	}
	if d.AgentName == "" {
		d.AgentName = "shoka-agent"
	}
	return d
}

// Close stops the worker pool and lock reaper. WAL files on disk are preserved.
func (s *FSGitStorage) Close() error {
	// Await the non-blocking leftover-relocation goroutine (B-42) before tearing
	// anything down: an object's Close should not return while a goroutine it
	// spawned is still depositing into lost+found. This is the shutdown side of
	// relocWG — production gains a clean shutdown (it rides cmd/shoka/main.go's
	// existing `defer s.Close()`); the startup gate is unaffected.
	s.relocWG.Wait()
	if s.pool != nil {
		_ = s.pool.Shutdown(30 * time.Second)
	}
	if s.locks != nil {
		s.locks.Stop()
	}
	if s.wal != nil {
		_ = s.wal.Close()
	}
	s.catMu.Lock()
	for key, c := range s.catalogs {
		if c != nil {
			if err := c.Close(); err != nil {
				s.log().Warn("catalog close failed", "project", key, "err", err)
			}
		}
		delete(s.catalogs, key)
	}
	s.catMu.Unlock()
	s.idxMu.Lock()
	for key, ix := range s.indexes {
		if ix != nil {
			if err := ix.Close(); err != nil {
				s.log().Warn("index close failed", "project", key, "err", err)
			}
		}
		delete(s.indexes, key)
	}
	s.idxMu.Unlock()
	s.dlMu.Lock()
	for key, dl := range s.deletedLogs {
		if dl != nil {
			if err := dl.Close(); err != nil {
				s.log().Warn("deleted-log close failed", "project", key, "err", err)
			}
		}
		delete(s.deletedLogs, key)
	}
	s.dlMu.Unlock()
	if s.nsReg != nil {
		if err := s.nsReg.Close(); err != nil {
			s.log().Warn("namespace registry close failed", "err", err)
		}
	}
	return nil
}

func projectKey(namespace, projectName string) string { return namespace + "/" + projectName }

func (s *FSGitStorage) getProjectPath(namespace, projectName string) (string, error) {
	if namespace == "" {
		namespace = "default"
	}
	if !utils.IsValidName(namespace) {
		return "", fmt.Errorf("invalid namespace: %s", namespace)
	}
	if !utils.IsValidName(projectName) {
		return "", fmt.Errorf("invalid project name: %s", projectName)
	}
	return filepath.Join(s.baseDir, namespace, projectName), nil
}

// relWithin validates path traversal and returns the slash-relative path.
func relWithin(projectPath, path string) (string, string, error) {
	fullPath := filepath.Join(projectPath, path)
	rel, err := filepath.Rel(projectPath, fullPath)
	if filepath.IsAbs(path) || err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", "", fmt.Errorf("invalid file path: %s", path)
	}
	return fullPath, filepath.ToSlash(rel), nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CreateProject initializes a new project directory and a Git repository within
// it. It is the legacy non-ctx entry point (used widely by tests and any caller
// without a sender to declare); it delegates to CreateProjectCtx with a
// background context, so the resulting project.create event dispatches to every
// subscriber. The write-path handlers (web/MCP) use CreateProjectCtx to carry a
// sender so the creator is not notified of its own project.create.
func (s *FSGitStorage) CreateProject(namespace, projectName string) error {
	return s.CreateProjectCtx(context.Background(), namespace, projectName)
}

// CreateProjectCtx is CreateProject carrying a context so a sender identity (the
// originating /ws/ui connection or MCP session) flows to the project.create
// notification, excluding the originator from its own event (2026-06-01
// sender-exclusion directive). A context with no sender (the CreateProject
// wrapper) dispatches to all subscribers, preserving prior behaviour.
func (s *FSGitStorage) CreateProjectCtx(ctx context.Context, namespace, projectName string) error {
	// Normalize the namespace-omitted entry point to the default namespace up front, so the
	// path, catalog, managed registry, and notification all agree on "default" (the registry
	// key must be non-empty; getProjectPath already normalizes the path the same way).
	if namespace == "" {
		namespace = DefaultNamespace
	}
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		return fmt.Errorf("failed to create project directory: %w", err)
	}
	_, err = git.PlainInit(projectPath, false)
	if err != nil {
		if err == git.ErrRepositoryAlreadyExists {
			// Re-creating an existing project is not a user-visible mutation, but the
			// managed record must still be consistent (B-28 stage A): ensure the project
			// (and its parent namespace) are registered as managed even on this path.
			return s.registerManagedProject(namespace, projectName)
		}
		return fmt.Errorf("failed to initialize git repository: %w", err)
	}
	s.setState(namespace, projectName, StateHealthy)
	// Catalog (§5.5): the new project gets a catalog DB. catalogFor opens it if a
	// stale DB somehow exists, else creates it. Failure here is a hard failure for
	// create_project — the project directory and git repo are left for the
	// operator to clean up (consistent with "surface the inconsistent state").
	if _, cerr := s.catalogFor(namespace, projectName); cerr != nil {
		s.log().Error("catalog create failed for new project",
			"namespace", namespace, "project", projectName, "err", cerr)
		return fmt.Errorf("create catalog: %w", cerr)
	}
	// Managed registry (B-28 stage A): record the project under its namespace, auto-
	// registering the parent namespace if absent (decision 5 — the safety-net path; the
	// primary route is explicit CreateNamespace via the management UI). Like the catalog
	// above this is part of bringing the project under management, so a failure fails the
	// create (the project would otherwise exist on disk but not in the managed set).
	if rerr := s.registerManagedProject(namespace, projectName); rerr != nil {
		return rerr
	}
	// Notification center: a genuine new project was created. The early
	// ErrRepositoryAlreadyExists return above is intentionally NOT published —
	// re-creating an existing project is not a user-visible mutation. The
	// ctx-borne sender excludes the creator from its own project.create.
	s.notify.NotifyFrom(notify.SenderFrom(ctx), "project.create", namespace+"/"+projectName, "")
	s.emit(ChangeEvent{
		Event:     "project_created",
		Namespace: namespace,
		Project:   projectName,
		Timestamp: time.Now(),
	})
	return nil
}

// WriteFile writes content with no optimistic-concurrency check.
func (s *FSGitStorage) WriteFile(namespace, projectName, path, content string) error {
	_, err := s.write(context.Background(), "", namespace, projectName, path, content, nil)
	return err
}

// WriteFileVersioned writes with optimistic locking. A non-empty expectedVersion
// must equal the file's current etag (sha256 of its content) or a
// *VersionConflictError is returned. Returns the new etag.
func (s *FSGitStorage) WriteFileVersioned(namespace, projectName, path, content, expectedVersion string) (string, error) {
	return s.write(context.Background(), "", namespace, projectName, path, content, ifMatchPtr(expectedVersion))
}

// Write is the redesign's write entry point (used by the §8 tool layer). ifMatch
// nil skips the check; non-nil requires the current etag to equal *ifMatch.
func (s *FSGitStorage) Write(ctx context.Context, sessionID, namespace, projectName, path, content string, ifMatch *string) (string, error) {
	return s.write(ctx, sessionID, namespace, projectName, path, content, ifMatch)
}

func ifMatchPtr(expected string) *string {
	if expected == "" {
		return nil
	}
	return &expected
}

func (s *FSGitStorage) write(ctx context.Context, sessionID, namespace, projectName, path, content string, ifMatch *string) (string, error) {
	// A plain whole-file write is the identity transform: the new bytes are the
	// caller's content, regardless of the file's current bytes.
	return s.writeTransformed(ctx, sessionID, namespace, projectName, path, ifMatch,
		func(_ []byte) ([]byte, error) { return []byte(content), nil })
}

// writeTransformed is the shared read-modify-write critical section behind write()
// (whole-file replace) and the B-36 partial-edit ops AppendToFile / PatchFile.
// Under the per-file lock it reads the file's current faithful bytes, enforces
// if_match against their etag, hands them to transform to compute the new bytes
// (transform may return a typed error — e.g. *MatchError — to abort with no
// write), writes the result atomically, appends ONE ordinary "write" WAL entry
// (so the background worker makes one faithful commit), updates the catalog, and
// publishes the file.write NOTIFY. transform always sees the SERVER's bytes, so
// for a partial edit the only LLM-mediated bytes are whatever it splices in. No
// new lock, WAL op, commit branch, or NOTIFY kind: append/patch are, to every
// observer, just file writes.
func (s *FSGitStorage) writeTransformed(ctx context.Context, sessionID, namespace, projectName, path string, ifMatch *string, transform func(current []byte) ([]byte, error)) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	fullPath, rel, err := relWithin(projectPath, path)
	if err != nil {
		return "", err
	}
	if err := s.checkWritable(namespace, projectName); err != nil {
		return "", err
	}

	var newEtag string
	lockErr := s.locks.WithLock(ctx, sessionID, fullPath, func() error {
		current, _ := os.ReadFile(fullPath) // empty if absent
		currentEtag := sha256Hex(current)
		if ifMatch != nil && *ifMatch != currentEtag {
			return &VersionConflictError{Expected: *ifMatch, Current: currentEtag}
		}
		newContent, terr := transform(current)
		if terr != nil {
			return terr
		}
		// Parent-create + atomic write run under the directory-scoped lock (B-48):
		// it serialises this MkdirAll+create against a concurrent empty-dir reaper
		// on the same parent, closing the MkdirAll→CreateTemp window so the reaper
		// can never delete the directory between its creation and the file landing
		// in it. The dir-lock is INNER to the per-file lock already held here
		// (file-outer, dir-inner — the deadlock-free order; see WithDirLock).
		if werr := s.locks.WithDirLock(ctx, sessionID, filepath.Dir(fullPath), func() error {
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
				return fmt.Errorf("failed to create directories for file: %w", err)
			}
			if err := atomicWriteFile(fullPath, newContent); err != nil {
				return fmt.Errorf("failed to write file: %w", err)
			}
			return nil
		}); werr != nil {
			return werr
		}
		newEtag = sha256Hex(newContent)
		id := identity.Resolve(ctx, s.identityDefaults)
		if _, err := s.wal.Append(wal.Entry{
			Namespace:    namespace,
			Project:      projectName,
			Path:         rel,
			Op:           "write",
			Content:      newContent,
			UserName:     id.UserName,
			UserEmail:    id.UserEmail,
			AgentName:    id.AgentName,
			WorkerID:     id.WorkerID,
			AuthorIsUser: id.AuthorIsUser,
		}); err != nil {
			return fmt.Errorf("failed to append to WAL: %w", err)
		}
		// Catalog update (working tree → WAL → catalog, §5.1). Best-effort: it
		// never fails the write, since the working tree and WAL already succeeded.
		s.catalogPut(namespace, projectName, rel, newEtag, len(newContent), fullPath)
		// Index update (I1): strictly after the authoritative catalog op, equally
		// best-effort, under the same lock. A failure leaves the index stale for
		// the repair sweep; it never fails the write.
		s.indexPut(namespace, projectName, rel, newContent, newEtag)
		return nil
	})
	if lockErr != nil {
		return "", lockErr
	}
	s.pool.Notify()
	// Notification center: publish only on success, after the write is durable
	// (atomic file write + WAL append committed under the lock). rel is the
	// cleaned within-project path, matching the WAL record. The ctx-borne sender
	// (the originating /ws/ui connection or MCP session) is passed so the center
	// does not echo this write back to its originator (2026-06-01 directive).
	s.notify.NotifyFrom(notify.SenderFrom(ctx), "file.write", namespace+"/"+projectName, rel)
	return newEtag, nil
}

// ReadFile reads the content of a file from a project. No lock, no git access.
func (s *FSGitStorage) ReadFile(namespace, projectName, path string) (string, error) {
	content, _, err := s.ReadFileWithETag(namespace, projectName, path)
	return content, err
}

// ReadFileWithETag reads a file and returns its content and etag (sha256 of the
// content). It takes no lock and never touches git. Dangerous projects are
// refused.
func (s *FSGitStorage) ReadFileWithETag(namespace, projectName, path string) (string, string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", "", err
	}
	if s.State(namespace, projectName) == StateDangerous {
		return "", "", ErrProjectDangerous
	}
	fullPath, rel, err := relWithin(projectPath, path)
	if err != nil {
		return "", "", err
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Not found in the working tree. Consult the catalog (cold path only):
			// if the catalog claims this path exists, the core invariant is broken
			// (catalog membership must imply working-tree presence). Surface it as
			// a warning + metric + notify event, but still return not-found to the
			// caller (§5.4 / design-log §6.1). Never creates a catalog on a read.
			if c := s.catalogForRead(namespace, projectName); c != nil {
				if had, herr := c.HasFile(rel); herr == nil && had {
					s.log().Warn("catalog invariant violation: catalog has path but working tree does not",
						"namespace", namespace, "project", projectName, "path", rel)
					s.catInvariantViolations.Add(1)
					s.notify.Notify("catalog.invariant_violation", namespace+"/"+projectName, rel)
				}
			}
		}
		return "", "", fmt.Errorf("failed to read file: %w", err)
	}
	return string(content), sha256Hex(content), nil
}

// StatModTime returns the working-tree filesystem modification time of a single
// file (os.Stat().ModTime()) — the same inode mtime that ListFiles reports via
// fs.DirEntry.Info(). It takes no lock and never touches git, so it reflects the
// latest write immediately rather than waiting for the background commit. The
// missing-file and other os.Stat errors are returned to the caller, not masked.
func (s *FSGitStorage) StatModTime(namespace, projectName, path string) (time.Time, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return time.Time{}, err
	}
	fullPath, _, err := relWithin(projectPath, path)
	if err != nil {
		return time.Time{}, err
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to stat file: %w", err)
	}
	return info.ModTime(), nil
}

// ListProjects returns a list of project names within a namespace.
func (s *FSGitStorage) ListProjects(namespace string) ([]string, error) {
	if namespace == "" {
		namespace = "default"
	}
	if !utils.IsValidName(namespace) {
		return nil, fmt.Errorf("invalid namespace: %s", namespace)
	}
	namespacePath := filepath.Join(s.baseDir, namespace)
	entries, err := os.ReadDir(namespacePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read namespace directory: %w", err)
	}
	var projects []string
	for _, entry := range entries {
		// Share the single project-eligibility predicate with discoverProjects (B-31)
		// so the listing the UI/MCP uses can never diverge from discovery: a
		// dot-prefixed Shoka-internal dir (.shoka-lostfound, .shoka, .drafts, .git) or
		// a repo-less leftover is not a project and must not be listed.
		if classifyProjectEntry(namespacePath, entry) == entryProject {
			projects = append(projects, entry.Name())
		}
	}
	return projects, nil
}

// DeleteFile deletes a file with no optimistic-concurrency check.
func (s *FSGitStorage) DeleteFile(namespace, projectName, path string) error {
	_, err := s.deleteFile(context.Background(), "", namespace, projectName, path, nil)
	return err
}

// DeleteFileVersioned deletes with optimistic locking (see WriteFileVersioned).
func (s *FSGitStorage) DeleteFileVersioned(namespace, projectName, path, expectedVersion string) (string, error) {
	return s.deleteFile(context.Background(), "", namespace, projectName, path, ifMatchPtr(expectedVersion))
}

// Delete is the redesign's delete entry point (used by the §8 tool layer).
func (s *FSGitStorage) Delete(ctx context.Context, sessionID, namespace, projectName, path string, ifMatch *string) error {
	_, err := s.deleteFile(ctx, sessionID, namespace, projectName, path, ifMatch)
	return err
}

func (s *FSGitStorage) deleteFile(ctx context.Context, sessionID, namespace, projectName, path string, ifMatch *string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	fullPath, rel, err := relWithin(projectPath, path)
	if err != nil {
		return "", err
	}
	if err := s.checkWritable(namespace, projectName); err != nil {
		return "", err
	}

	lockErr := s.locks.WithLock(ctx, sessionID, fullPath, func() error {
		current, _ := os.ReadFile(fullPath)
		currentEtag := sha256Hex(current)
		if ifMatch != nil && *ifMatch != currentEtag {
			return &VersionConflictError{Expected: *ifMatch, Current: currentEtag}
		}
		// Delete ordering (§5.2 / design log §6.2): catalog → WAL → working tree.
		// Disown before destroying, so the catalog never claims a path the working
		// tree still lacks. The catalog delete is best-effort.
		s.catalogDelete(namespace, projectName, rel)
		// Index update (I1): mirror the catalog delete, best-effort, under the lock.
		s.indexDelete(namespace, projectName, rel)
		id := identity.Resolve(ctx, s.identityDefaults)
		if _, err := s.wal.Append(wal.Entry{
			Namespace:    namespace,
			Project:      projectName,
			Path:         rel,
			Op:           "delete",
			UserName:     id.UserName,
			UserEmail:    id.UserEmail,
			AgentName:    id.AgentName,
			WorkerID:     id.WorkerID,
			AuthorIsUser: id.AuthorIsUser,
		}); err != nil {
			return fmt.Errorf("failed to append to WAL: %w", err)
		}
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove file from disk: %w", err)
		}
		// One-level empty-parent reap (B-48): the delete may have emptied the
		// parent directory; reclaim it on the spot — one level only, rm semantics,
		// dir-locked. The dir-lock is INNER to the per-file lock held here
		// (file-outer, dir-inner). If the parent still holds other files the reap
		// is a no-op (ENOTEMPTY); if removing it empties the grandparent, that is
		// reaped only by a later operation or the sweep backstop (no chain ascent).
		s.reapEmptyDir(ctx, sessionID, projectPath, filepath.Dir(fullPath))
		return nil
	})
	if lockErr != nil {
		return "", lockErr
	}
	s.pool.Notify()
	// Notification center: publish only on success. rel is the cleaned
	// within-project path, matching the WAL record. The ctx-borne sender is
	// passed so the originator is not notified of its own delete (2026-06-01).
	s.notify.NotifyFrom(notify.SenderFrom(ctx), "file.delete", namespace+"/"+projectName, rel)
	return "", nil
}

// checkWritable rejects writes when the project is corrupted/dangerous or the
// WAL is full.
func (s *FSGitStorage) checkWritable(namespace, projectName string) error {
	// A project being moved/renamed — or any project under a namespace being renamed — is
	// briefly fenced (B-28 move + ns/proj rename): refuse writes with a retriable error for
	// the op's (short, serialized) duration.
	if s.isMoving(namespace, projectName) || s.isMovingNs(namespace) {
		return ErrProjectMoving
	}
	switch s.State(namespace, projectName) {
	case StateDangerous:
		return ErrProjectDangerous
	case StateCorrupted:
		// B-25 (D1): the in-memory corrupted state may merely be stale. An operator
		// can reconcile drift out-of-band (commit the working tree by hand — exactly
		// how this project's own directives/reports are placed), after which the tree
		// matches the catalog but nothing has re-run DetectDrift, so honest writes
		// keep being refused with reason=corrupted until the next rescan/restart. Look
		// again before refusing: re-run drift detection, then proceed iff the project
		// is now genuinely healthy.
		//
		// This recomputes TRUTH, not success. If the bytes actually diverge from the
		// catalog (an operator content-edit behind Shoka's back), DetectDrift reports
		// Modified, the state stays corrupted, and the write is still (correctly)
		// refused — the operator's path then is recovery (RepairTrackedChanges), not a
		// write that papers over a real divergence.
		//
		// No deadlock: this branch holds no lock (State() RLocked and released before
		// the switch body), DetectDrift takes no file lock, and the per-file lock is
		// taken only after checkWritable returns; setState is stateMu-guarded. Cost is
		// self-extinguishing — paid only on the corrupted branch, and the first clean
		// rescan flips the project to healthy so subsequent writes hit case
		// StateHealthy and skip it.
		s.lazyRescans.Add(1)
		_, _ = s.DetectDrift(namespace, projectName)
		switch s.State(namespace, projectName) {
		case StateDangerous:
			return ErrProjectDangerous
		case StateHealthy:
			// Genuinely healthy now: fall through to the repo-less / WAL-full guards
			// below and allow the write.
		default:
			return ErrProjectCorrupted
		}
	}
	// A mutation may only touch a real, git-backed project. CreateProject git-inits,
	// so a legitimate project always has a repo; a repo-less path is "not a project".
	// Without this, a write to a never-created project is silently accepted and
	// half-creates one — a working-tree dir, a per-project .db, and an un-committable
	// WAL entry the worker loops on forever (the B-37 phantom). checkWritable is the
	// single gate every mutation (write/delete/append/patch/move) funnels through, and
	// it runs before any side-effect, so the half-project is never born. The state
	// switch above stays first, so a KNOWN dangerous/corrupted project keeps its own
	// reason; only a never-seen repo-less path reports not-found.
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return err
	}
	if !hasGitRepo(projectPath) {
		return ErrProjectNotFound
	}
	if s.wal.PendingCount() >= s.maxWALEntries {
		return ErrWriteDisabled
	}
	return nil
}

// atomicWriteFile writes data to a temp file in the same directory and renames
// it into place, so a reader never sees a partially-written file.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ListFiles returns a list of files in a project path (non-recursive).
// ListFiles returns the non-recursive listing of a project path: the entry
// names (directories carry a trailing "/") and a parallel map of each entry's
// working-tree modification time (os.Stat().ModTime()). The map is keyed by the
// same display name that appears in the returned slice. An entry that vanishes
// between the directory scan and its stat (fs.DirEntry.Info error) is omitted
// from both the slice and the map (§4.3 of the modified_at directive).
func (s *FSGitStorage) ListFiles(namespace, projectName, path string) ([]string, map[string]time.Time, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return nil, nil, err
	}
	if s.State(namespace, projectName) == StateDangerous {
		return nil, nil, ErrProjectDangerous
	}
	searchPath := filepath.Join(projectPath, path)
	relSearch, err := filepath.Rel(projectPath, searchPath)
	if err != nil || (relSearch != "." && (strings.HasPrefix(relSearch, "..") || strings.HasPrefix(relSearch, "/"))) {
		return nil, nil, fmt.Errorf("invalid search path: %s", path)
	}

	// Listing is sourced from the catalog — the set of files Shoka manages — so
	// working-tree noise (.DS_Store, .claude/, …) never appears (§5.3). Each
	// entry's modified_at, by contrast, is the live working-tree mtime: this
	// keeps it byte-identical with read_summary and the shipped 2026-05-30
	// modified-at contract, whose tests are non-negotiable. (This refines §5.3,
	// which would have sourced modified_at from the catalog.)
	cat, err := s.catalogFor(namespace, projectName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open catalog: %w", err)
	}
	catEntries, subdirs, err := cat.List(path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list catalog: %w", err)
	}

	files := []string{}
	modTimes := make(map[string]time.Time, len(catEntries)+len(subdirs))
	add := func(display, name string) {
		info, serr := os.Stat(filepath.Join(searchPath, name))
		if serr != nil {
			// Catalog claims the entry but the working tree lacks it (invariant
			// violation) or it vanished mid-scan: omit it rather than surface a
			// phantom. The read path is what flags invariant violations.
			return
		}
		files = append(files, display)
		modTimes[display] = info.ModTime()
	}
	for _, e := range catEntries {
		add(e.Name, e.Name)
	}
	for _, d := range subdirs {
		add(d+"/", d)
	}
	return files, modTimes, nil
}

// GetHistory returns the commit history for a specific file (git-backed).
func (s *FSGitStorage) GetHistory(namespace, projectName, path string, limit int) ([]CommitInfo, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return nil, err
	}
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	if _, err := r.Head(); err != nil {
		// No commits yet (e.g. a write is still pending in the WAL). Not an error.
		return []CommitInfo{}, nil
	}
	logOptions := &git.LogOptions{Order: git.LogOrderCommitterTime}
	if path != "" {
		fullPath := filepath.Join(projectPath, path)
		rel, err := filepath.Rel(projectPath, fullPath)
		if filepath.IsAbs(path) || err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
			return nil, fmt.Errorf("invalid file path: %s", path)
		}
		rel = filepath.ToSlash(rel)
		logOptions.FileName = &rel
	}
	cIter, err := r.Log(logOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to get git log: %w", err)
	}
	defer cIter.Close()

	var history []CommitInfo
	for {
		if limit > 0 && len(history) >= limit {
			break
		}
		c, err := cIter.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed to get next commit: %w", err)
		}
		history = append(history, CommitInfo{
			Hash:       c.Hash.String(),
			Author:     c.Author.Name,
			Date:       c.Author.When,
			Message:    c.Message,
			Committer:  c.Committer.Name,
			CommitDate: c.Committer.When,
		})
	}
	return history, nil
}

// ReadFileAtVersion reads the content of a file at a specific Git commit hash.
// This is the "past access" API; it is git-backed and isolated from the write
// path's lock manager.
func (s *FSGitStorage) ReadFileAtVersion(namespace, projectName, path, hash string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	_, rel, err := relWithin(projectPath, path)
	if err != nil {
		return "", err
	}
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return "", fmt.Errorf("failed to open git repository: %w", err)
	}
	commit, err := r.CommitObject(plumbing.NewHash(hash))
	if err != nil {
		return "", fmt.Errorf("failed to get commit object: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("failed to get tree: %w", err)
	}
	file, err := tree.File(rel)
	if err != nil {
		return "", fmt.Errorf("failed to get file from tree: %w", err)
	}
	content, err := file.Contents()
	if err != nil {
		return "", fmt.Errorf("failed to read file contents: %w", err)
	}
	return content, nil
}

// GetCurrentVersion returns the current etag (sha256 of the file's content), or
// "" if the file does not exist. It validates the path (rejecting traversal),
// takes no lock, and never touches git.
func (s *FSGitStorage) GetCurrentVersion(namespace, projectName, path string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	fullPath, _, err := relWithin(projectPath, path)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", nil // absent → no version (mirrors the previous "" semantics)
	}
	return sha256Hex(content), nil
}

// LazyRescanCount returns the number of D1 lazy rescans run at checkWritable's
// corrupted branch (B-25 observability). It is 0 for a project that never went
// corrupted; it increments once when a stale-corrupted clean project is unblocked,
// and once per write attempt for a genuinely-corrupted project until recovery.
func (s *FSGitStorage) LazyRescanCount() int64 { return s.lazyRescans.Load() }

// --- WAL / worker observability (for get_server_info §8.6 and metrics §11) ---

// WALPending returns the number of WAL entries awaiting a git commit.
func (s *FSGitStorage) WALPending() int { return s.wal.PendingCount() }

// WALPendingBytes returns the summed size of pending WAL entries.
func (s *FSGitStorage) WALPendingBytes() int64 { return s.wal.PendingBytes() }

// WALWriteDisabled reports whether the WAL has backed up past its threshold.
func (s *FSGitStorage) WALWriteDisabled() bool { return s.wal.PendingCount() >= s.maxWALEntries }

// WALMaxEntries returns the configured write-disabled threshold.
func (s *FSGitStorage) WALMaxEntries() int { return s.maxWALEntries }

// WALOldestEntryAge returns the age of the oldest pending WAL entry.
func (s *FSGitStorage) WALOldestEntryAge() time.Duration { return s.wal.OldestEntryAge() }

// WorkerStats returns the background commit pool's stats.
func (s *FSGitStorage) WorkerStats() walworker.Stats { return s.pool.Stats() }

// CommitStats returns the cumulative successful and failed background-commit
// counts, for the shoka_wal_commits_total metric.
func (s *FSGitStorage) CommitStats() (success, failure int64) {
	st := s.pool.Stats()
	return st.CommitsTotal, st.CommitsFailed
}

// QuarantineStats returns the cumulative count of WAL entries quarantined to
// lost+found (and removed from the WAL) and the count of quarantine attempts
// whose deposit failed, for the shoka_wal_quarantined_total /
// shoka_wal_quarantine_failed_total metrics. It reads the same pool stats as
// CommitStats; no walworker behaviour changes.
func (s *FSGitStorage) QuarantineStats() (quarantined, failed int64) {
	st := s.pool.Stats()
	return st.QuarantinedTotal, st.QuarantineFailed
}

// ProjectStates returns each tracked project ("namespace/project") mapped to its
// state string, for the shoka_project_state metric.
func (s *FSGitStorage) ProjectStates() map[string]string {
	src := s.AllStates()
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = string(v)
	}
	return out
}

// LockStats returns the file-lock manager's active leases and forced-release count.
func (s *FSGitStorage) LockStats() (activeLeases int, forcedReleases int64) {
	return len(s.locks.ActiveLeases()), s.locks.ForcedReleaseCount()
}

// WaitForWAL blocks until the WAL has drained (no pending entries) or timeout
// elapses. It returns true if the WAL drained. Intended for tests and for
// drift detection's "after the WAL has caught up" precondition.
func (s *FSGitStorage) WaitForWAL(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if s.wal.PendingCount() == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}
