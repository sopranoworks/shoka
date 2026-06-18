package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/adminapi"
	"github.com/sopranoworks/shoka/internal/auth"
	"github.com/sopranoworks/shoka/internal/authapi"
	"github.com/sopranoworks/shoka/internal/config"
	"github.com/sopranoworks/shoka/internal/drafts"
	"github.com/sopranoworks/shoka/internal/httplog"
	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/logging"
	"github.com/sopranoworks/shoka/internal/metrics"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/oauth"
	"github.com/sopranoworks/shoka/internal/reqtrace"
	"github.com/sopranoworks/shoka/internal/scopeclean"
	"github.com/sopranoworks/shoka/internal/serverurl"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/storage/filelock"
	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
	"github.com/sopranoworks/shoka/internal/storage/userstore"
	"github.com/sopranoworks/shoka/internal/storage/walworker"
	"github.com/sopranoworks/shoka/internal/tools"
	"github.com/sopranoworks/shoka/internal/ui"
	"github.com/sopranoworks/shoka/internal/webhooks"
	"github.com/sopranoworks/shoka/server"
	"golang.org/x/sync/errgroup"
)

// boolPtr returns a pointer to b, for filling pointer-valued option/config fields.
func boolPtr(b bool) *bool { return &b }

func main() {
	// Subcommand dispatch: `shoka project ...` / `shoka wal ...` / `shoka snapshot ...`
	// run the CLI; anything else (flags or nothing) runs the server.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "project", "wal", "snapshot":
			if err := runCLI(os.Args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
	}

	configPath := flag.String("config", "shoka.yaml", "Path to configuration file")
	configCheck := flag.Bool("config-check", false, "Load and validate the config (strict — unknown/misplaced keys are errors), print 'config OK' or the exact error, and EXIT without starting the server or binding any port (Apache configtest-style).")
	profileAddr := flag.String("profile-addr", "", "If set, serve net/http/pprof on this loopback address (e.g. localhost:9060). Empty disables profiling.")
	flag.Parse()

	// B-58 config dry-run: validate the config and exit, binding no port and starting
	// no server, so the operator can confirm a config (including a key's placement)
	// BEFORE a restart instead of discovering a silent miss by trial-connect.
	if *configCheck {
		if err := runConfigCheck(*configPath, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "config check failed:", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// B-66: resolve WHERE log output goes (the destination), defaulting to stderr.
	// slog.New already writes to any io.Writer; this selects which one from config
	// (stderr unchanged, or a bounded file). Fail loud on an unopenable destination
	// rather than silently falling back to stderr (B-58 fail-loud stance).
	logDest, err := openLogDestination(cfg.Server.Log)
	if err != nil {
		log.Fatalf("failed to open log destination: %v", err)
	}
	logger, err := logging.New(cfg.Server.Log.Level, cfg.Server.Log.Format, logDest.Writer)
	if err != nil {
		log.Fatalf("failed to initialize logger: %v", err)
	}
	slog.SetDefault(logger)
	// Closed last (registered before s.Close/stop) so every shutdown log line —
	// including "servers shut down gracefully" — still reaches the destination.
	defer logDest.Close()

	// Setup context with signal handling
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// B-66: drive at-least-daily rotation of a file destination (no-op for
	// stderr). Tied to ctx, so it stops on SIGINT/SIGTERM with the rest.
	logDest.StartDailyRotation(ctx, logger)

	// Optional pprof endpoint: default off, loopback-only. Block and mutex
	// profiling rates are enabled only when the flag is set, so production runs
	// pay nothing. (Used by cmd/stress for the storage write-path investigation.)
	if *profileAddr != "" {
		startProfileServer(ctx, *profileAddr, logger)
	}

	// In-process notification center: records recent storage activity for
	// future consumers (Web UI auto-refresh, drift-rescan triggers). Storage
	// publishes to it on successful writes/deletes/project-creates. It is always
	// constructed; no transport or subscriber API exists yet. The center is
	// reachable for future code via the storage that holds it (s).
	notifyCenter := notify.NewCenter(cfg.Notify.MaxEntries)

	storageOpts := storage.Options{
		FileLock: filelock.Config{
			MaxLeaseDuration: cfg.FileLock.MaxLeaseDuration.Std(),
			ReaperInterval:   cfg.FileLock.ReaperInterval.Std(),
		},
		WALMaxEntries: cfg.WAL.MaxEntries,
		WALWorker: walworker.Config{
			MinWorkers:     cfg.WALWorker.MinWorkers,
			MaxWorkers:     cfg.WALWorker.MaxWorkers,
			IdleTimeout:    cfg.WALWorker.IdleTimeout.Std(),
			ScanInterval:   cfg.WALWorker.ScanInterval.Std(),
			BackoffInitial: cfg.WALWorker.BackoffInitial.Std(),
			BackoffMax:     cfg.WALWorker.BackoffMax.Std(),
		},
		NotifyCenter: notifyCenter,
		// Single-user-mode commit identity (the 2026-06-01 identity-config
		// directive). PROVISIONAL — see internal/identity (backlog B-28).
		Identity: identity.Defaults{
			UserName:    cfg.Identity.User.Name,
			UserEmail:   cfg.Identity.User.Email,
			AgentName:   cfg.Identity.AgentDefault.Name,
			AgentWorker: cfg.Identity.AgentDefault.Worker,
		},
		// Deleted-file log (the 2026-06-18 deleted-log directive). No interval — the
		// repair is lazy (two triggers), never a sweep.
		DeletedLog: storage.DeletedLogOptions{
			Enabled:     boolPtr(cfg.Storage.DeletedLog.IsEnabled()),
			RepairDepth: cfg.Storage.DeletedLog.EffectiveRepairDepth(),
			MaxEntries:  cfg.Storage.DeletedLog.EffectiveMaxEntries(),
		},
	}
	s, err := storage.NewFSGitStorageWithOptions(cfg.Storage.BaseDir, storageOpts)
	if err != nil {
		log.Fatalf("failed to initialize storage: %v", err)
	}
	s.SetLogger(logger)
	defer s.Close()

	// Blocking startup gate (catalog directive §6): drain the WAL, then open or
	// rebuild every project's catalog and compute its drift state. This MUST
	// complete before either listener accepts a connection, so callers never
	// observe a project mid-initialisation.
	s.StartupInit(ctx)

	// Periodic background drift re-scan (the startup pass above already ran the
	// first one). Disabled when the interval is zero.
	if cfg.Storage.DriftScan.OnStartupEnabled() {
		s.StartDriftScan(ctx, cfg.Storage.DriftScan.Interval.Std())
	}

	// Lost+found worker (the 2026-06-02 directive): a periodic sweep that deletes
	// untracked files matching shoka.disposable and moves the rest to a
	// per-project lost+found area, restoring the tracked-only invariant. Runs
	// after StartupInit so catalogs/states are ready; disabled via config.
	if cfg.Storage.LostFound.IsEnabled() {
		s.StartLostFoundSweep(ctx, cfg.Storage.LostFound.Interval.Std())
	}

	// Index repair worker (the 2026-06-04 I1 directive): the fourth periodic sweep,
	// reconciling each project's derivative index.db with HEAD and rebuilding it
	// from working-tree bytes when stale/missing/corrupt. Runs after StartupInit
	// alongside the other sweeps; disabled via config. No query reads the index yet
	// (the fast path is I2/I3) — this only keeps the substrate warm.
	if cfg.Storage.Index.IsEnabled() {
		s.StartIndexSweep(ctx, cfg.Storage.Index.Interval.Std())
	}

	// Backup snapshot scheduler (B-70 phase 3): a periodic SnapshotScope→PruneSnapshots
	// cycle to the configured output dir. OFF by default; the scope string was
	// validated in config.Validate. StartSnapshotSweep is a no-op when disabled and
	// is tick-only (no immediate snapshot at boot). The same config also backs the
	// on-demand /api/snapshot admin endpoint below.
	backupScope, err := storage.ParseScope(cfg.Storage.Backup.Scope)
	if err != nil {
		log.Fatalf("invalid storage.backup.scope: %v", err)
	}
	backupSweepCfg := storage.SnapshotSweepConfig{
		Enabled:         cfg.Storage.Backup.IsEnabled(),
		Interval:        cfg.Storage.Backup.Interval.Std(),
		OutputDir:       cfg.Storage.Backup.OutputDir,
		Scope:           backupScope,
		RetentionCount:  cfg.Storage.Backup.EffectiveRetentionCount(),
		RetentionMaxAge: time.Duration(cfg.Storage.Backup.RetentionDays) * 24 * time.Hour,
	}
	s.StartSnapshotSweep(ctx, backupSweepCfg)

	notifier := webhooks.New(toWebhookConfigs(cfg.Webhooks))
	notifier.SetLogger(logger)
	s.SetChangeHandler(func(ev storage.ChangeEvent) {
		notifier.Emit(webhooks.Event{
			Event:      ev.Event,
			Namespace:  ev.Namespace,
			Project:    ev.Project,
			Path:       ev.Path,
			CommitHash: ev.CommitHash,
			Timestamp:  ev.Timestamp,
		})
	})

	dm, err := drafts.NewManager(cfg.Storage.BaseDir)
	if err != nil {
		log.Fatalf("failed to initialize draft manager: %v", err)
	}

	uim := ui.NewManager(s, dm, notifyCenter)

	// B-50: "is OAuth active" ≡ the OAuth MCP transport is configured (its listen
	// address is set). The former server.auth.oauth.enabled flag is gone. The
	// per-port listener wiring is phase 2; phase 1 only repoints the reads.
	oauthEnabled := cfg.Server.MCP.OAuth.Listen != ""
	// B-63 §0.1: the advertised client-registration posture is a config switch. Both
	// resolution code paths stay in the binary; only the advertised AS metadata differs.
	discoveryCfg := oauth.DiscoveryConfig{
		ExternalURL:      cfg.Server.MCP.OAuth.ExternalURL,
		RegistrationMode: oauth.RegistrationMode(cfg.Server.MCP.OAuth.RegistrationModeOrDefault()),
		Logger:           logger,
	}
	// authConfig carries ONLY the OAuth MCP transport's concerns — the RFC 9728
	// challenge composer and the token-enforcement closure, both set below when the
	// OAuth port is configured and consumed SOLELY by that port. server.auth's
	// static-bearer/origin policy is deliberately NOT here: it is the Web/non-MCP
	// routes' gate (webAuth, below) and the plain MCP port's bearer source (B-50
	// phase 2). Keeping them apart is what stops the OAuth closure from reaching a
	// Web route (B-50 phase 3 decoupling).
	var authConfig auth.Config
	var authServer *oauth.AuthServer
	var oauthStore *oauthstore.Store
	if oauthEnabled {
		// The 401 challenge advertises where to find the Protected Resource
		// Metadata (RFC 9728 §5.1), composed per-request so the proxy's
		// X-Forwarded-* headers can drive it when no external_url is configured.
		authConfig.ResourceMetadataURL = func(r *http.Request) string {
			base, err := serverurl.Base(discoveryCfg.ExternalURL, r)
			if err != nil {
				return ""
			}
			return serverurl.ProtectedResourceMetadataURL(base)
		}
		if cfg.Server.MCP.OAuth.ExternalURL == "" {
			logger.Warn("oauth transport configured without server.mcp.oauth.external_url; " +
				"relying on per-request X-Forwarded-* headers to compose the public URL — " +
				"set external_url in production")
		}

		// The authorization-server core (B-39 (b)): a go-git-free token store, the
		// CIMD verifier (trusted-domain allowlist from config), and the /authorize +
		// /token endpoints. Enabling oauth now ENFORCES tokens on the MCP path (it
		// supersedes the static-bearer switch); discovery alone is no longer the
		// only effect of the toggle.
		var oerr error
		oauthStore, oerr = oauthstore.Open(filepath.Join(cfg.Storage.BaseDir, "oauth.db"))
		if oerr != nil {
			log.Fatalf("failed to open oauth token store: %v", oerr)
		}
		defer func() { _ = oauthStore.Close() }()

		// OAuth dead-series cleaner (2026-06-15 authz foundation): a periodic sweep
		// deleting fully-dead token series (refresh expired + grace) — the OAuth store
		// has no other GC, so dead series otherwise accumulate forever. ON by default;
		// tick-only (no boot sweep), mirroring the storage sweep workers. Only started
		// here, inside the OAuth-enabled block, so it never runs without a store.
		oauthStore.StartCleaner(ctx, oauthstore.CleanerConfig{
			Enabled:  cfg.Storage.OAuthCleaner.IsEnabled(),
			Interval: cfg.Storage.OAuthCleaner.Interval.Std(),
			Grace:    cfg.Storage.OAuthCleaner.Grace.Std(),
			Logger:   logger,
		})

		// Wire the OAuth connection store into the Web UI manager so the
		// administrator-only OAUTH_LIST/OAUTH_REVOKE management requests can
		// enumerate and revoke connections (B-39 (c)). Admin authorization is enforced
		// by the stage-2 dispatch authzGate (OAUTH_* are admin-level); the former
		// config-admin seam was retired in stage 4.
		uim.SetOAuthStore(oauthStore)

		oc := cfg.Server.MCP.OAuth
		if len(oc.TrustedClientMetadataDomains) == 0 {
			logger.Warn("oauth enabled with an empty trusted_client_metadata_domains allowlist; " +
				"no client can connect until at least the legitimate connector domain is listed")
		}
		if oc.ConsentCredential == "" {
			logger.Warn("oauth enabled without server.auth.oauth.consent_credential; " +
				"all /authorize approvals will be denied until a consent credential is set")
		}
		verifier := oauth.NewVerifier(oc.TrustedClientMetadataDomains)
		authServer = oauth.NewAuthServer(oauthStore, verifier, oauth.AuthServerConfig{
			ExternalURL: cfg.Server.MCP.OAuth.ExternalURL,
			PrincipalAuth: oauth.ConsentCredentialAuth{
				Credential: oc.ConsentCredential,
				Principal: oauthstore.Principal{
					Name:  cfg.Identity.User.Name,
					Email: cfg.Identity.User.Email,
				},
			},
			AccessTTL:  oc.AccessTokenTTL.Std(),
			RefreshTTL: oc.RefreshTokenTTL.Std(),
			CodeTTL:    oc.AuthorizationCodeTTL.Std(),
			Logger:     logger,
		})

		// Token-to-self (B-46b §2.2): the admin-gated OAUTH_ISSUE_SELF action mints
		// a fresh access token for the current-mode operator and shows it once in the
		// Web UI so it can be pasted into the CLI client config. It wraps the same
		// NewSeries primitive /token uses — with the operator principal, the
		// configured TTLs, and the RFC 8707 resource derived per-request exactly as
		// /authorize does — so a CLI token is indistinguishable from any other. All
		// oauth/serverurl/identity wiring stays here in main; the manager only
		// admin-gates and calls. The minted token is never logged on this path.
		uim.SetOAuthSelfIssuer(ui.OAuthSelfIssuerFunc(func(r *http.Request) (string, time.Time, error) {
			base, berr := serverurl.Base(cfg.Server.MCP.OAuth.ExternalURL, r)
			if berr != nil {
				return "", time.Time{}, berr
			}
			rec, nerr := oauthStore.NewSeries(
				"shoka-cli",
				oauthstore.Principal{Name: cfg.Identity.User.Name, Email: cfg.Identity.User.Email},
				serverurl.ResourceURL(base),
				"*", // the operator's self-issued CLI token is all-access, like any DCR token
				time.Now(),
				oc.AccessTokenTTL.Std(),
				oc.RefreshTokenTTL.Std(),
			)
			if nerr != nil {
				return "", time.Time{}, nerr
			}
			return rec.AccessToken, rec.AccessExpiry, nil
		}))

		// Token enforcement: a valid OAuth access token is required on the MCP path
		// and its bound principal is attached to the request (→ commit Committer). The
		// RejectReason (B-53 §2.4) is logging-only — it names WHY a token was rejected
		// (the store already distinguishes not-found from expired); the allow/deny
		// decision is the bool alone, so the wire behaviour is unchanged.
		authConfig.ValidateToken = func(token string) (auth.Principal, auth.RejectReason, bool) {
			if token == "" {
				return auth.Principal{}, auth.ReasonMissingBearer, false
			}
			rec, lerr := oauthStore.Lookup(token, time.Now())
			if lerr != nil {
				reason := auth.ReasonInvalidToken
				if errors.Is(lerr, oauthstore.ErrExpired) {
					reason = auth.ReasonExpired
				}
				return auth.Principal{}, reason, false
			}
			if rec.Principal.Name == "" {
				// Token validated but carries no resolvable principal (defensive).
				return auth.Principal{}, auth.ReasonPrincipalUnresolved, false
			}
			// ClientID is carried for diagnostic logging only (B-52 §2.4 — "which
			// client got bound to the session"); the commit identity uses Name/Email.
			// Scope is the token's authorization grant for the tools/call authz gate;
			// an empty Scope on a pre-field token is read as "*" (all-access).
			scope := rec.Scope
			if scope == "" {
				scope = "*"
			}
			return auth.Principal{Name: rec.Principal.Name, Email: rec.Principal.Email, ClientID: rec.ClientID, Scope: scope}, "", true
		}
	}
	// The Web/non-MCP routes (/drafts/, /ws/ui, /api/) get their OWN authenticator
	// carrying ONLY server.auth's static-bearer tokens + WS origin policy — never
	// the OAuth ValidateToken closure (B-50 phase 3). An OAuth access token is an
	// MCP-client credential; at HEAD it gated /api/ (the OAuth-wrapped admin route)
	// whenever the OAuth transport was configured, which locked the browser Web-UI
	// recovery dialog out — the latent bug this decoupling fixes. This is the
	// existing server.auth gate the Web routes were always built on, NOT a decision
	// about the Web UI's own auth model (static-bearer vs B-28 user-auth vs open),
	// which stays a separate, deferred question.
	webAuth := auth.New(webAuthConfig(cfg.Server.Auth))
	dm.SetOriginChecker(webAuth.OriginAllowed)
	uim.SetOriginChecker(webAuth.OriginAllowed)

	// Optional Prometheus metrics endpoint: default off, loopback-only (mirrors
	// the pprof endpoint's defaults). Started here — after the UI manager (the
	// notify-drop source) and the oauth store exist — so the collector bridge can
	// see beyond storage. uim is the notify-drop extra; the oauth store joins as a
	// second extra (M3). oauthStore is nil when OAuth is disabled, and a typed-nil
	// *oauthstore.Store boxed into an `any` would NOT be caught by the collector's
	// untyped-nil guard (the interface carries a type, so it is non-nil) — it would
	// wire a non-nil OAuthSource over a nil pointer and panic in Collect. So we drop
	// the nil BEFORE boxing: append the store only when non-nil, leaving the
	// collector's extras loop unchanged. OAuth disabled → no OAuth families.
	if cfg.Metrics.Addr != "" {
		extras := []any{uim}
		if oauthStore != nil {
			extras = append(extras, oauthStore)
		}
		startMetricsServer(ctx, cfg.Metrics.Addr, s, logger, extras...)
	}

	mcpServer := setupMCPServer(ctx, cfg, s, logger, notifyCenter)

	// WebUI multi-user login (B-28 stage 1): a server-level user/session store
	// (go-git-free bbolt sibling of oauth.db) backing the /auth/* login surface. It
	// is INDEPENDENT of the OAuth MCP transport — login works whether or not OAuth is
	// enabled — and strictly separate from the MCP token surface (B-50): a session is
	// an opaque cookie, never an OAuth token, and the OAuth ValidateToken closure is
	// never consulted on the Web path.
	totpKey, terr := userstore.ResolveTOTPKey(cfg.Server.Auth.Users.TOTPEncryptionKey, filepath.Join(cfg.Storage.BaseDir, "userstore.key"))
	if terr != nil {
		log.Fatalf("failed to resolve user-store TOTP key: %v", terr)
	}
	userStore, uerr := userstore.Open(filepath.Join(cfg.Storage.BaseDir, "users.db"), totpKey)
	if uerr != nil {
		log.Fatalf("failed to open user store: %v", uerr)
	}
	defer func() { _ = userStore.Close() }()

	// The super-user-only user-management ops (B-28 stage 3) ride /ws/ui, gated at
	// admin level by the stage-2 dispatch gate. Wiring the store enables them.
	uim.SetUserStore(userStore)

	// Cascade cleanup (B-28 ns/proj management part 1): when a namespace/project is
	// deleted, every grant referencing it by name is purged from user + invite + token
	// scopes, so re-creating the same name never resurrects old access. Wired over the
	// user store and the (optional, possibly-nil) OAuth store; storage drives it from
	// inside DeleteProject/DeleteNamespace via the ScopeCleaner interface.
	s.SetScopeCleaner(scopeclean.New(userStore, oauthStore))

	// WebAuthn engine: built only when a canonical rp_id is configured (the
	// per-deployment "passkeys on" choice). Empty rp_id ⇒ nil ⇒ passkeys disabled
	// while the password+TOTP floor still works (incl. a bare internal-IP deployment
	// that cannot host a WebAuthn RP ID). rp_id/origins live ONLY in config.
	var webAuthn *webauthn.WebAuthn
	if cfg.Server.Auth.WebAuthn.Enabled() {
		webAuthn, err = webauthn.New(&webauthn.Config{
			RPID:          cfg.Server.Auth.WebAuthn.RPID,
			RPDisplayName: cfg.Server.Auth.WebAuthn.RPDisplayName,
			RPOrigins:     cfg.Server.Auth.WebAuthn.RPOrigins,
		})
		if err != nil {
			log.Fatalf("failed to configure WebAuthn: %v", err)
		}
	}
	authHandler := authapi.New(authapi.Config{
		Users:              userStore,
		WebAuthn:           webAuthn,
		RPDisplayName:      cfg.Server.Auth.WebAuthn.RPDisplayName,
		SessionTTL:         cfg.Server.Auth.Users.SessionTTL.Std(),
		AllowFirstRunAdmin: cfg.Server.Auth.Users.FirstRunAdminAllowed(),
		Logger:             logger,
	})

	webHandler, err := setupWebHandler(s, dm, uim, webAuth, authHandler, backupSweepCfg)
	if err != nil {
		log.Fatalf("failed to setup web handler: %v", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	// Web Server. reqtrace is the outermost layer (B-53): the Web listener is NOT
	// wrapped by httplog, so without this its routes (/api, /ws/ui, /drafts, /) had
	// no entry-to-exit trace at all. The surface label is a fixed category, never the
	// listen address.
	dumpHTTP := cfg.Server.Debug.DumpHTTP
	tracedWeb := tracedHandler(logger, "web", dumpHTTP, webHandler)
	g.Go(func() error {
		return runServer(ctx, "Web", cfg.Server.HTTP, tracedWeb, logger)
	})

	// MCP transports (B-50 phase 2): open up to two MCP listeners over ONE shared
	// mcpServer, selected by config PRESENCE (Validate guarantees at least one is
	// set). Each opened port gets its OWN StreamableHTTPHandler instance, so a
	// Mcp-Session-Id minted on one port is unknown on the other (per-port session
	// maps — defense-in-depth; the OAuth middleware re-validates every request
	// regardless). Stateful mode (the SDK default) is required: it validates the
	// Mcp-Session-Id header and returns 404 for an unknown/stale id, which is how a
	// client recovers after a restart. The handler is path-agnostic, so the
	// documented /mcp endpoint is just one path it serves (see
	// docs/contracts/mcp-v1.md § Transport). Init stays SINGLE — one storage/
	// catalog/index, one notifyCenter, one mcpServer, one subscription-manager +
	// reaper — shared by both legs; duplicating any of them is the only way to
	// introduce a cross-transport race. No MCP TLS: Shoka terminates none; both
	// ports sit behind an external TLS-terminating reverse proxy.
	newMCPHandler := func() http.Handler {
		return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			return mcpServer
		}, &mcp.StreamableHTTPOptions{Logger: logger})
	}

	// Plain (internal) transport: static-bearer iff bearer_auth (validated against
	// server.auth.tokens), else unauthenticated loopback use. Never OAuth — no
	// discovery/AS surface, no token enforcement closure.
	if cfg.Server.MCP.Plain.Listen != "" {
		var plainAuth *auth.Authenticator
		if cfg.Server.MCP.Plain.BearerAuth {
			plainAuth = auth.New(auth.Config{Enabled: true, Tokens: cfg.Server.Auth.Tokens})
		} else {
			plainAuth = auth.New(auth.Config{}) // disabled → allows all (loopback use)
		}
		plainSettings := config.ServerSettings{
			Listen:      cfg.Server.MCP.Plain.Listen,
			ExternalURL: cfg.Server.MCP.Plain.ExternalURL,
		}
		// reqtrace outermost (B-53): one correlation id per request, raw-inbound entry
		// record, and response record (status+reason+route) — shared by httplog/auth's
		// lines via the context id.
		plainHandler := tracedHandler(logger, "mcp-plain", dumpHTTP,
			httplog.Middleware(logger)(plainAuth.Middleware(reqtrace.Route("mcp-dispatch", newMCPHandler()))))
		g.Go(func() error {
			return runServer(ctx, "MCP-plain", plainSettings, plainHandler, logger)
		})
	}

	// OAuth (external) transport: PURE OAuth — the OAuth-enforcing authenticator
	// (the ValidateToken closure over oauthStore.Lookup) behind a per-listener mux
	// that also mounts the discovery documents + the /authorize + /token endpoints
	// unauthenticated (they must be reachable before a token exists). It never
	// accepts a static bearer. Opened iff oauth.listen is set — which is exactly
	// oauthEnabled, so authServer and the authConfig closures below are non-nil.
	if cfg.Server.MCP.OAuth.Listen != "" {
		oauthAuth := auth.New(auth.Config{
			ResourceMetadataURL: authConfig.ResourceMetadataURL,
			ValidateToken:       authConfig.ValidateToken,
			Logger:              logger,
		})
		oauthSettings := config.ServerSettings{
			Listen:      cfg.Server.MCP.OAuth.Listen,
			ExternalURL: cfg.Server.MCP.OAuth.ExternalURL,
		}
		// reqtrace outermost (B-53): correlates the discovery / /authorize / /token /
		// MCP lines under one per-request id and adds the entry + response records — so
		// the live token-bearing initialize that 401s on path=/ is traceable end to end.
		oauthHandler := tracedHandler(logger, "mcp-oauth", dumpHTTP,
			httplog.Middleware(logger)(oauthListenerHandler(discoveryCfg, authServer, newMCPHandler(), oauthAuth)))
		g.Go(func() error {
			return runServer(ctx, "MCP-oauth", oauthSettings, oauthHandler, logger)
		})
	}

	// Operator-facing startup posture (B-50 phase 4): one line per opened surface
	// stating PRESENCE + AUTH POSTURE as categories, so the operator confirms the
	// deployment topology at a glance — important now that auth is split across
	// ports (an externally-reachable plain port left unauthenticated should be
	// visible here). Confidentiality is LOAD-BEARING: these lines carry ONLY
	// categories — never a listen address, port, host, domain, external_url, token,
	// or consent credential. (runServer logs the bind address separately; the lines
	// here are deliberately address-free and add no new address exposure.)
	for _, p := range describeStartupPostures(cfg) {
		if p.Policy != "" {
			logger.Info("startup transport posture", "surface", p.Surface, "auth", p.Auth, "policy", p.Policy)
		} else {
			logger.Info("startup transport posture", "surface", p.Surface, "auth", p.Auth)
		}
	}

	// B-57: make the verbatim HTTP-dump switch (server.debug.dump_http, B-56) state
	// VISIBLE at startup, so the operator confirms at a glance whether the dump is ON
	// without a trial connect — the gap that left B-56 "tests green, live silent" was
	// that the switch was undiscoverable and its state invisible. A bool only; never an
	// address or secret. When enabled=true the live log emits `http request dump` /
	// `http response dump` per request on all three surfaces, correlated by request_id.
	logger.Info("startup http dump", "enabled", dumpHTTP)

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}

	// Drain any in-flight webhook deliveries before exiting.
	notifier.Wait()

	logger.Info("servers shut down gracefully")
}

// openLogDestination maps the log config onto a logging.Destination: stderr by
// default (unchanged), or a bounded lumberjack-backed file. The mapping lives
// here (cmd/shoka already depends on both config and logging) so internal/logging
// stays decoupled from internal/config. Validation has already rejected an
// unknown output or a file output with no path (config.Validate), so this only
// builds the writer; it does not re-validate.
func openLogDestination(c config.LogConfig) (*logging.Destination, error) {
	switch c.Output {
	case "", "stderr":
		return logging.Stderr(), nil
	case "file":
		rotateDaily := true // default: rotate at least daily even with no size pressure
		if c.File.RotateDaily != nil {
			rotateDaily = *c.File.RotateDaily
		}
		return logging.OpenFile(logging.FileConfig{
			Path:        c.File.Path,
			MaxSizeMB:   c.File.MaxSizeMB,
			MaxBackups:  c.File.MaxBackups,
			MaxAgeDays:  c.File.MaxAgeDays,
			Compress:    c.File.Compress,
			RotateDaily: rotateDaily,
		})
	default:
		return nil, fmt.Errorf("invalid log output %q (want stderr|file)", c.Output)
	}
}

func toWebhookConfigs(in []config.WebhookConfig) []webhooks.Config {
	out := make([]webhooks.Config, 0, len(in))
	for _, w := range in {
		out = append(out, webhooks.Config{
			Name:   w.Name,
			URL:    w.URL,
			Events: w.Events,
			Secret: w.Secret,
		})
	}
	return out
}

func setupMCPServer(ctx context.Context, cfg *config.Config, s *storage.FSGitStorage, logger *slog.Logger, notifyCenter *notify.Center) *mcp.Server {
	mcpServer := mcp.NewServer(
		&mcp.Implementation{
			Name:    "shoka",
			Version: "0.1.0",
		},
		&mcp.ServerOptions{Logger: logger},
	)

	// Core-first tools/list ordering (B-49 fix-1): the SDK emits tools/list sorted
	// alphabetically by name with no ordering hook; this receiving middleware
	// reorders the response so the core read/write tools appear first. It touches
	// only the listing order — registration and tools/call dispatch are unaffected.
	mcpServer.AddReceivingMiddleware(tools.CoreFirstToolsMiddleware())

	// Authorization choke point (2026-06-15 authz foundation): one receiving
	// middleware through which EVERY tools/call flows, so authz lives in a single
	// place rather than scattering across handlers as tools grow. It reads the
	// principal (already on ctx from the auth middleware) and the call's namespace/
	// project, and applies the scope grant. *-pass today (every token is Scope "*"),
	// with a dormant-but-tested else-branch that enforces a future pre-issued scoped
	// token automatically. It no-ops for non-tools/call methods.
	mcpServer.AddReceivingMiddleware(tools.AuthzMiddleware())

	// Scoped MCP change notifications (B-45b): the subscribe/unsubscribe tools let
	// an MCP client (e.g. an automation watcher) receive notifications/message for
	// external file changes under a subscribed namespace/project/path pattern,
	// reusing notifyCenter and its built-in sender-exclusion. Registered alongside
	// the file tools; the reaper drops subscriptions for sessions that disconnect.
	subMgr := tools.NewSubscriptionManager(notifyCenter, s, logger)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "get_server_info",
		Description: "Get information about the server's public URL and configuration",
	}, tools.LoggedTool(logger, "get_server_info", tools.GetServerInfoHandler(cfg, s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "create_project",
		Description: "Create a new project with Git initialization",
	}, tools.LoggedTool(logger, "create_project", tools.CreateProjectHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "list_projects",
		Description: "List all projects in a namespace",
	}, tools.LoggedTool(logger, "list_projects", tools.ListProjectsHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a file from a project",
	}, tools.LoggedTool(logger, "read_file", tools.ReadFileHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "write_file",
		Description: "Create a new file OR overwrite an existing one, as one atomic Git commit (the prior content stays recoverable via get_history / read_file_at_version). Writing to an existing path replaces it; there is no separate delete needed. if_match is an OPTIONAL optimistic-concurrency guard: omit it and the overwrite always proceeds; pass the file's current etag (from read_file) and the write is rejected with a conflict if the file has changed since. Fails if the project does not exist.",
	}, tools.LoggedTool(logger, "write_file", tools.WriteFileHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "delete_file",
		Description: "Delete a file from a project with atomic Git commit",
	}, tools.LoggedTool(logger, "delete_file", tools.DeleteFileHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "append_to_file",
		Description: "Insert text into a file without resending the whole file: append at end (default) or insert before/after a unique anchor string. Server-side splice on the file's faithful bytes under the per-file lock; same atomic Git commit and if_match etag as write_file. For large append-mostly files (backlog, journal) this sends only the changed span",
	}, tools.LoggedTool(logger, "append_to_file", tools.AppendToFileHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "patch_file",
		Description: "Replace a single unique occurrence of old_string with new_string (str_replace-style; the server never guesses — zero or multiple matches are an error). Server-side splice on the file's faithful bytes under the per-file lock; same atomic Git commit and if_match etag as write_file. Sends only the changed span, not the whole file",
	}, tools.LoggedTool(logger, "patch_file", tools.PatchFileHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "move_file",
		Description: "Rename or move a file within a project as a single atomic Git commit: a pure, history-preserving rename. It does not rewrite Markdown links that point at the file (links_rewritten is always 0)",
	}, tools.LoggedTool(logger, "move_file", tools.MoveFileHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "list_files",
		Description: "List files in a project path; each response includes a modified_at map giving every entry's working-tree modification time (RFC3339 nanosecond UTC)",
	}, tools.LoggedTool(logger, "list_files", tools.ListFilesHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "get_history",
		Description: "Get Git commit history for a project or file",
	}, tools.LoggedTool(logger, "get_history", tools.GetHistoryHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "read_file_at_version",
		Description: "Read a file at a specific Git commit hash",
	}, tools.LoggedTool(logger, "read_file_at_version", tools.ReadFileAtVersionHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "get_diff",
		Description: "Diff two explicit versions of a SINGLE file (Shoka commits one file per commit), returning a structured per-hunk diff. Pass from_hash and to_hash as commit hashes obtained from get_history. The result carries status (modified/added/deleted) and per-line ops (equal/add/delete) with line numbers; if the diff is omitted the 'suppressed' field says why (binary/too_large/timeout) rather than returning an empty diff.",
	}, tools.LoggedTool(logger, "get_diff", tools.GetDiffHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "read_summary",
		Description: "Get a context-efficient summary of a Markdown file (frontmatter, first heading, short excerpt, size, etag, modified_at) without its full body",
	}, tools.LoggedTool(logger, "read_summary", tools.ReadSummaryHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "list_files_since",
		Description: "List files changed after a given RFC3339 timestamp or commit hash, with each file's change kind (added/modified/deleted)",
	}, tools.LoggedTool(logger, "list_files_since", tools.ListFilesSinceHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "search_files",
		Description: "Search a project's files by filename, content, or both (case-insensitive substring), returning matches with context snippets",
	}, tools.LoggedTool(logger, "search_files", tools.SearchFilesHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "list_deleted",
		Description: "List a project's currently-deleted files (path, the commit that deleted it, and when), a cheap read of the deleted-file log. Admin-only. Pair with revive_file to restore one.",
	}, tools.LoggedTool(logger, "list_deleted", tools.ListDeletedHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "revive_file",
		Description: "Restore a deleted file by re-creating its last content as a NEW commit (forward-only; history is preserved). Pass the path from list_deleted; from_commit optionally overrides the deletion commit. Admin-only. Errors clearly if git no longer has the deletion (cap eviction or external history rewrite).",
	}, tools.LoggedTool(logger, "revive_file", tools.ReviveFileHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "recover_project",
		Description: "Recover a project stuck in a 'corrupted' (uncommitted working-tree drift) state by re-syncing its write-path baseline to the ACTUAL on-disk git HEAD and clearing a FALSE corrupted flag. Non-destructive: it neither commits nor discards working-tree content. Use it when a project went corrupted after an external git HEAD move (a host 'git reset', an out-of-band 'git add' landing/revert) even though the working tree is clean — writes are re-enabled. A project with GENUINE uncommitted drift stays corrupted (the response says so); resolve that from the Web UI recover action (accept-working-tree to adopt, accept-head to discard).",
	}, tools.LoggedTool(logger, "recover_project", tools.RecoverProjectHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "delete_project",
		Description: "Permanently delete an ENTIRE project — its working tree, git history, and derivative state — distinct from delete_file (which removes one file). DESTRUCTIVE and irreversible. Requires admin on the target namespace.",
	}, tools.LoggedTool(logger, "delete_project", tools.DeleteProjectHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "create_namespace",
		Description: "Create an explicit, empty namespace that is listed even with zero projects and survives deleting its last project. Requires a super-user.",
	}, tools.LoggedTool(logger, "create_namespace", tools.CreateNamespaceHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "delete_namespace",
		Description: "Permanently delete an ENTIRE namespace and EVERY project under it. DESTRUCTIVE and irreversible. Requires a super-user.",
	}, tools.LoggedTool(logger, "delete_namespace", tools.DeleteNamespaceHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "namespace_health",
		Description: "Report managed-namespace health: for every namespace you administer (a super-user sees all), each managed project's state (healthy/corrupted/dangerous/missing), plus orphaned catalog/index DBs and untracked-foreign dirs (flagged adoptable). Read-only diagnostic — nothing is changed.",
	}, tools.LoggedTool(logger, "namespace_health", tools.NamespaceHealthHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "namespace_recover",
		Description: "Run one explicit, non-destructive-by-default recovery action on the managed set: action 'drop_missing' (remove a registry record whose on-disk target is confirmed gone — for corrupted projects use recover_project instead), 'clean_orphaned' (remove a stray catalog/index DB with no project dir), or 'adopt' (bring a valid untracked namespace/project under management). Project-level actions need admin on the namespace; whole-namespace actions (omit project_name) need a super-user.",
	}, tools.LoggedTool(logger, "namespace_recover", tools.NamespaceRecoverHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "move_project",
		Description: "Move an ENTIRE project from one namespace to another, preserving its name and full git history. The target namespace must already exist; refuses if a project of that name is already there (no overwrite). Distinct from delete — nothing is destroyed. Requires a super-user.",
	}, tools.LoggedTool(logger, "move_project", tools.MoveProjectHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "rename_project",
		Description: "Rename a project WITHIN its namespace, preserving its full git history and derivative state. Refuses if a project of the new name already exists in the namespace (no overwrite). Distinct from move (which changes the namespace) and delete (which destroys). Requires admin on the namespace.",
	}, tools.LoggedTool(logger, "rename_project", tools.RenameProjectHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "rename_namespace",
		Description: "Rename (relabel) an ENTIRE namespace, carrying every project under it with their full git history, and re-home every grant that referenced the old name. Allowed even when the namespace has projects (it is a relabel, not a delete). Refuses if the new name already exists, and the 'default' namespace cannot be renamed. Requires a super-user.",
	}, tools.LoggedTool(logger, "rename_namespace", tools.RenameNamespaceHandler(s)))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "subscribe",
		Description: "Subscribe to scoped file-change notifications. Pass a pattern '<namespace>/<project>/<path>' where namespace and project are required and literal (no wildcards) and the path part is a prefix (e.g. 'directives/') or a single-segment glob (e.g. 'directives/2026-*'); recursive '**' is not supported. The session then receives a notifications/message for each external file.write/file.move/file.delete under a matching pattern — never its own writes. Additive: call again to watch more patterns (Redis SUBSCRIBE semantics). Requires the client to issue logging/setLevel to receive the messages",
	}, tools.LoggedTool(logger, "subscribe", subMgr.SubscribeHandler()))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "unsubscribe",
		Description: "Remove a change-notification pattern previously registered with subscribe; omit the pattern to clear ALL of this session's subscriptions (Redis UNSUBSCRIBE semantics)",
	}, tools.LoggedTool(logger, "unsubscribe", subMgr.UnsubscribeHandler()))

	// Wire the reaper to the live session set and start it (drops subscriptions for
	// disconnected sessions; the SDK exposes no tool-session close hook).
	subMgr.SetServer(mcpServer)
	subMgr.StartReaper(ctx)

	return mcpServer
}

// webAuthConfig returns the auth.Config for the Web/non-MCP routes (/drafts/,
// /ws/ui, /api/): server.auth's static-bearer tokens + WS allowed-origins ONLY. It
// deliberately carries NEITHER the OAuth ValidateToken closure nor the RFC 9728
// ResourceMetadataURL — an OAuth access token is an MCP-client credential and must
// never gate a browser route. This returns the gate the Web routes were always
// built on (server.auth); it is NOT a decision about the Web UI's own auth model
// (static-bearer vs B-28 user-auth vs open), which remains a separate, deferred
// question (B-50 phase 3 only removes the OAuth coupling).
func webAuthConfig(a config.AuthConfig) auth.Config {
	return auth.Config{
		Enabled:        a.Enabled,
		Tokens:         a.Tokens,
		AllowedOrigins: a.AllowedOrigins,
	}
}

// startupPosture describes one network surface for the operator-facing startup
// log (B-50 phase 4): which surface opened and its auth posture, expressed as
// CATEGORIES only. Confidentiality is load-bearing — a startupPosture never carries
// a listen address, port, host, domain, external_url, token, or secret; Surface and
// Auth (and the optional web Policy) are fixed enumerated strings, not config values.
// runConfigCheck implements the B-58 dry-run (`shoka --config-check`): it loads and
// validates the config under STRICT decoding and reports the result WITHOUT starting
// the server or binding any port (Apache `apachectl configtest`-style). config.Load
// binds nothing, so this is pure config validation. It returns the exact load error
// (an unknown/misplaced key named with its line, a bad value, or a failed Validate)
// on any problem, or nil after printing `config OK` plus a terse summary. The summary
// reuses describeStartupPostures + the dump flag, so it names ONLY categories and
// booleans — never a listen address, host, token, or other secret.
func runConfigCheck(configPath string, out io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "config OK")
	for _, p := range describeStartupPostures(cfg) {
		if p.Policy != "" {
			fmt.Fprintf(out, "  transport: %s (auth=%s, %s)\n", p.Surface, p.Auth, p.Policy)
		} else {
			fmt.Fprintf(out, "  transport: %s (auth=%s)\n", p.Surface, p.Auth)
		}
	}
	dumpState := "disabled"
	if cfg.Server.Debug.DumpHTTP {
		dumpState = "enabled"
	}
	fmt.Fprintf(out, "  http dump: %s\n", dumpState)
	// Report WHERE log output goes, without opening it (config-check stays
	// side-effect-free: it binds no port and opens no destination — B-58).
	switch cfg.Server.Log.Output {
	case "", "stderr":
		fmt.Fprintln(out, "  log output: stderr")
	case "file":
		fmt.Fprintf(out, "  log output: file %s (bounded)\n", cfg.Server.Log.File.Path)
	}
	return nil
}

type startupPosture struct {
	Surface string // "mcp-plain", "mcp-oauth", "web"
	Auth    string // "unauthenticated" | "static-bearer" | "oauth-protected" | "oauth-free"
	Policy  string // web-only sub-category: "open" | "static-bearer"; "" when n/a
}

// describeStartupPostures returns the presence+posture summary of the opened
// surfaces, derived purely from the loaded config: an MCP port appears only when its
// listen is set (matching the listener wiring above), and the Web UI always appears.
// It maps config flags to fixed category strings — by construction it cannot emit an
// address or secret, which is what the phase-4 confidentiality rule requires of every
// startup log line. The neither-MCP-port case is a config-load fatal (phase 1) and is
// not re-checked here.
func describeStartupPostures(cfg *config.Config) []startupPosture {
	var out []startupPosture
	if cfg.Server.MCP.Plain.Listen != "" {
		authPosture := "unauthenticated"
		if cfg.Server.MCP.Plain.BearerAuth {
			authPosture = "static-bearer"
		}
		out = append(out, startupPosture{Surface: "mcp-plain", Auth: authPosture})
	}
	if cfg.Server.MCP.OAuth.Listen != "" {
		// B-63 §0.1: surface the advertised client-registration posture (cimd|dcr) as a
		// category, so the operator confirms which signal the AS metadata advertises
		// before a live claude.ai connect. A fixed category — never an address or secret.
		out = append(out, startupPosture{Surface: "mcp-oauth", Auth: "oauth-protected", Policy: cfg.Server.MCP.OAuth.RegistrationModeOrDefault() + "-registration"})
	}
	webPolicy := "open"
	if cfg.Server.Auth.Enabled {
		webPolicy = "static-bearer"
	}
	out = append(out, startupPosture{Surface: "web", Auth: "oauth-free", Policy: webPolicy})
	return out
}

func setupWebHandler(s *storage.FSGitStorage, dm *drafts.Manager, uim *ui.Manager, authenticator *auth.Authenticator, authHandler *authapi.Handler, backupSweepCfg storage.SnapshotSweepConfig) (http.Handler, error) {
	mux := http.NewServeMux()
	// WebSocket endpoints accept the ?token= query fallback (browsers cannot set
	// an Authorization header on a WS handshake). The MCP/SSE endpoint uses the
	// header-only Middleware (see runServer wiring) so tokens never ride in URLs.
	// reqtrace.Route tags the routing stage (B-53 §2.5) INSIDE the auth layer, so a
	// request 401'd before the handler shows route="unrouted" while a served request
	// names its handler — both under the shared request_id.
	mux.Handle("/drafts/", authenticator.MiddlewareAllowQueryToken(reqtrace.Route("web-drafts", dm)))
	// /ws/ui additionally requires a valid WebUI login session ONCE a user exists
	// (B-28 stage 1): authHandler.RequireSession passes through while the user store
	// is empty (no-lockout) and 401s an un-authenticated connection afterward, so the
	// WebUI is used under the logged-in user's principal (attached by the outer
	// authHandler.Middleware that wraps this whole mux).
	mux.Handle("/ws/ui", authenticator.MiddlewareAllowQueryToken(authHandler.RequireSession(reqtrace.Route("web-ws-ui", uim))))

	// Multi-user login surface (B-28 stage 1): /auth/status|register|login|logout and
	// the WebAuthn passkey ceremonies. No bearer — it mints/clears the session cookie
	// itself, on the Web surface, never touching the MCP token path.
	mux.Handle("/auth/", reqtrace.Route("web-auth", authHandler))

	// Admin API (project state/rescan/recover) — shared by the `shoka project`
	// CLI and the Web UI recovery dialog. Header-bearer auth (no ?token=).
	mux.Handle("/api/", authenticator.Middleware(reqtrace.Route("web-api", adminapi.New(s, backupSweepCfg))))

	// Serve static files from embedded FS
	distFS, err := fs.Sub(server.DistFS, "dist")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(distFS))

	mux.Handle("/", reqtrace.Route("web-static", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			fileServer.ServeHTTP(w, r)
			return
		}

		// Check if file exists in embedded FS
		_, err := distFS.Open(path)
		if err != nil {
			// If not found, serve index.html for SPA routing
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})))

	// Wrap the whole web mux so the WebUI session principal (B-28 stage 1) is
	// resolved from the session cookie and attached to the request context for every
	// route — /ws/ui then reads it (writes authored as the logged-in user; the
	// RequireSession gate checks it). Attaching never blocks; only RequireSession
	// gates. The principal is the SAME auth.Principal the MCP path uses, but it is
	// sourced from the user store, never the OAuth token closure (B-50 separation).
	return authHandler.Middleware(mux), nil
}

// isLoopbackHost reports whether host is a loopback address or "localhost".
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// startProfileServer starts a dedicated http.Server exposing net/http/pprof on
// addr. The host is forced to loopback (a non-loopback host is rewritten to
// localhost with a WARN) so profiling is never reachable off-box. Block and
// mutex profiling are enabled here — and only here — so they cost nothing when
// the --profile-addr flag is absent. The server shuts down when ctx is done.
func startProfileServer(ctx context.Context, addr string, logger *slog.Logger) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		logger.Error("invalid --profile-addr; pprof endpoint not started", "addr", addr, "error", err)
		return
	}
	if !isLoopbackHost(host) {
		logger.Warn("--profile-addr is not a loopback host; forcing localhost", "given", host)
		host = "localhost"
		addr = net.JoinHostPort(host, port)
	}

	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{Addr: addr, Handler: mux}
	logger.Info("pprof profiling endpoint enabled", "addr", addr)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof server error", "error", err)
		}
	}()
}

// startMetricsServer starts a dedicated http.Server exposing /metrics in
// Prometheus format. Like the pprof endpoint, it is loopback-only (a non-loopback
// host is rewritten to localhost with a WARN) and is only started when the
// metrics.addr config is non-empty. The server shuts down when ctx is done. The
// variadic extras are optional bridge sources (e.g. the UI manager for the
// notify-drop counter) passed through to metrics.Handler; a nil extra is safe.
func startMetricsServer(ctx context.Context, addr string, src metrics.Source, logger *slog.Logger, extras ...any) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		logger.Error("invalid metrics.addr; metrics endpoint not started", "addr", addr, "error", err)
		return
	}
	if !isLoopbackHost(host) {
		logger.Warn("metrics.addr is not a loopback host; forcing localhost", "given", host)
		host = "localhost"
		addr = net.JoinHostPort(host, port)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler(src, extras...))
	srv := &http.Server{Addr: addr, Handler: mux}
	logger.Info("metrics endpoint enabled", "addr", addr)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server error", "error", err)
		}
	}()
}

// tracedHandler applies the outermost reqtrace layer (B-53) to a listener's
// handler, threading the B-56 server.debug.dump_http switch. All three listeners
// (web / mcp-plain / mcp-oauth) wrap through this ONE helper, so the HTTP-dump
// enable is single-sourced — and the B-57 live-path test drives a request through
// THIS function with a config-loaded flag, proving the switch actually reaches the
// dump. That closes the "tests green, live silent" gap: the B-56 unit test
// constructed the middleware with dumpHTTP=true directly, bypassing the
// config-load + wiring path where the live silence actually originated. Pure
// observability wiring — behaviour-identical to the prior inline reqtrace wraps.
func tracedHandler(logger *slog.Logger, surface string, dumpHTTP bool, inner http.Handler) http.Handler {
	return reqtrace.Middleware(logger, surface, dumpHTTP)(inner)
}

// oauthListenerHandler assembles the OAuth MCP listener's HTTP handler. The RFC
// 9728 / RFC 8414 discovery documents and the /authorize + /token endpoints are
// mounted WITHOUT auth (they must be reachable before a token exists); the
// OAuth-enforcing MCP handler is the "/" catch-all. The MCP handler is
// path-agnostic, so the catch-all serves it on /mcp and elsewhere. This handler
// is built ONLY for the OAuth port (B-50 phase 2) — the plain port wraps the bare
// MCP handler with its own static-bearer-or-none authenticator and has no
// discovery/AS surface.
func oauthListenerHandler(discoveryCfg oauth.DiscoveryConfig, authServer *oauth.AuthServer, mcpHandler http.Handler, authenticator *auth.Authenticator) http.Handler {
	mux := http.NewServeMux()
	oauth.RegisterDiscovery(mux, discoveryCfg) // RFC 9728 / RFC 8414, no auth
	if authServer != nil {
		authServer.RegisterEndpoints(mux) // /authorize + /token, no bearer (they mint one)
	}
	// Route tag INSIDE auth (B-53 §2.5): a token-bearing initialize that 401s here
	// shows route="unrouted" (blocked before routing) — the exact disambiguation the
	// live path=/ failure needs; a valid request shows route="mcp-dispatch".
	mux.Handle("/", authenticator.Middleware(reqtrace.Route("mcp-dispatch", mcpHandler)))
	return mux
}

func runServer(ctx context.Context, name string, settings config.ServerSettings, handler http.Handler, logger *slog.Logger) error {
	srv := &http.Server{
		Addr:    settings.Listen,
		Handler: handler,
	}

	errChan := make(chan error, 1)
	go func() {
		logger.Info("starting server", "name", name, "addr", settings.Listen)
		var err error
		if settings.TLS.Enabled {
			logger.Info("tls enabled", "addr", settings.Listen)
			err = srv.ListenAndServeTLS(settings.TLS.CertFile, settings.TLS.KeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down server", "name", name)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errChan:
		return err
	}
}
