package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/adminapi"
	"github.com/shoka/mcp-server/internal/auth"
	"github.com/shoka/mcp-server/internal/config"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/httplog"
	"github.com/shoka/mcp-server/internal/identity"
	"github.com/shoka/mcp-server/internal/logging"
	"github.com/shoka/mcp-server/internal/metrics"
	"github.com/shoka/mcp-server/internal/notify"
	"github.com/shoka/mcp-server/internal/oauth"
	"github.com/shoka/mcp-server/internal/reqtrace"
	"github.com/shoka/mcp-server/internal/serverurl"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/storage/filelock"
	"github.com/shoka/mcp-server/internal/storage/oauthstore"
	"github.com/shoka/mcp-server/internal/storage/walworker"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/shoka/mcp-server/internal/translation"
	"github.com/shoka/mcp-server/internal/ui"
	"github.com/shoka/mcp-server/internal/webhooks"
	"github.com/shoka/mcp-server/server"
	"golang.org/x/sync/errgroup"
)

func main() {
	// Subcommand dispatch: `shoka project ...` / `shoka wal ...` run the CLI;
	// anything else (flags or nothing) runs the server.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "project", "wal":
			if err := runCLI(os.Args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
	}

	configPath := flag.String("config", "shoka.yaml", "Path to configuration file")
	profileAddr := flag.String("profile-addr", "", "If set, serve net/http/pprof on this loopback address (e.g. localhost:9060). Empty disables profiling.")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	logger, err := logging.New(cfg.Server.Log.Level, cfg.Server.Log.Format, os.Stderr)
	if err != nil {
		log.Fatalf("failed to initialize logger: %v", err)
	}
	slog.SetDefault(logger)

	// Setup context with signal handling
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	discoveryCfg := oauth.DiscoveryConfig{ExternalURL: cfg.Server.MCP.OAuth.ExternalURL, Logger: logger}
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

		// Wire the OAuth connection store into the Web UI manager so the
		// administrator-only OAUTH_LIST/OAUTH_REVOKE management requests can
		// enumerate and revoke connections (B-39 (c)). The admin authorizer stays
		// the single-user default (sole user = admin) until the B-28 Web-auth leg
		// supplies a real role check via uim.SetAdminAuthorizer.
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
			return auth.Principal{Name: rec.Principal.Name, Email: rec.Principal.Email, ClientID: rec.ClientID}, "", true
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

	var ts translation.TranslationService
	if cfg.Services.GoogleCloud.ProjectID != "" {
		var err error
		ts, err = translation.NewGoogleTranslationService(context.Background(), cfg.Services.GoogleCloud.ProjectID)
		if err != nil {
			logger.Warn("google translation unavailable; translate_file disabled", "error", err)
		} else {
			defer ts.Close()
		}
	}

	mcpServer := setupMCPServer(ctx, cfg, s, ts, logger, notifyCenter)

	webHandler, err := setupWebHandler(s, dm, uim, webAuth)
	if err != nil {
		log.Fatalf("failed to setup web handler: %v", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	// Web Server. reqtrace is the outermost layer (B-53): the Web listener is NOT
	// wrapped by httplog, so without this its routes (/api, /ws/ui, /drafts, /) had
	// no entry-to-exit trace at all. The surface label is a fixed category, never the
	// listen address.
	tracedWeb := reqtrace.Middleware(logger, "web")(webHandler)
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
		plainHandler := reqtrace.Middleware(logger, "mcp-plain")(
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
		oauthHandler := reqtrace.Middleware(logger, "mcp-oauth")(
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

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}

	// Drain any in-flight webhook deliveries before exiting.
	notifier.Wait()

	logger.Info("servers shut down gracefully")
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

func setupMCPServer(ctx context.Context, cfg *config.Config, s *storage.FSGitStorage, ts translation.TranslationService, logger *slog.Logger, notifyCenter *notify.Center) *mcp.Server {
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
		Description: "Write a file to a project with atomic Git commit",
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

	if ts != nil {
		mcp.AddTool(mcpServer, &mcp.Tool{
			Name:        "translate_file",
			Description: "Translate a Markdown file to a target language (Japanese to English by default)",
		}, tools.LoggedTool(logger, "translate_file", tools.TranslateFileHandler(s, ts)))
	}

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
		out = append(out, startupPosture{Surface: "mcp-oauth", Auth: "oauth-protected"})
	}
	webPolicy := "open"
	if cfg.Server.Auth.Enabled {
		webPolicy = "static-bearer"
	}
	out = append(out, startupPosture{Surface: "web", Auth: "oauth-free", Policy: webPolicy})
	return out
}

func setupWebHandler(s *storage.FSGitStorage, dm *drafts.Manager, uim *ui.Manager, authenticator *auth.Authenticator) (http.Handler, error) {
	mux := http.NewServeMux()
	// WebSocket endpoints accept the ?token= query fallback (browsers cannot set
	// an Authorization header on a WS handshake). The MCP/SSE endpoint uses the
	// header-only Middleware (see runServer wiring) so tokens never ride in URLs.
	// reqtrace.Route tags the routing stage (B-53 §2.5) INSIDE the auth layer, so a
	// request 401'd before the handler shows route="unrouted" while a served request
	// names its handler — both under the shared request_id.
	mux.Handle("/drafts/", authenticator.MiddlewareAllowQueryToken(reqtrace.Route("web-drafts", dm)))
	mux.Handle("/ws/ui", authenticator.MiddlewareAllowQueryToken(reqtrace.Route("web-ws-ui", uim)))

	// Admin API (project state/rescan/recover) — shared by the `shoka project`
	// CLI and the Web UI recovery dialog. Header-bearer auth (no ?token=).
	mux.Handle("/api/", authenticator.Middleware(reqtrace.Route("web-api", adminapi.New(s))))

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

	return mux, nil
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
