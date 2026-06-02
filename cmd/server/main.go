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
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/storage/filelock"
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

	// Optional Prometheus metrics endpoint: default off, loopback-only (mirrors
	// the pprof endpoint's defaults).
	if cfg.Metrics.Addr != "" {
		startMetricsServer(ctx, cfg.Metrics.Addr, s, logger)
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

	authenticator := auth.New(auth.Config{
		Enabled:        cfg.Server.Auth.Enabled,
		Tokens:         cfg.Server.Auth.Tokens,
		AllowedOrigins: cfg.Server.Auth.AllowedOrigins,
	})
	dm.SetOriginChecker(authenticator.OriginAllowed)
	uim.SetOriginChecker(authenticator.OriginAllowed)

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

	mcpServer := setupMCPServer(cfg, s, ts, logger)
	// MCP is served over the Streamable HTTP transport (single endpoint on the
	// dedicated MCP listener; see docs/contracts/mcp-v1.md § Transport). The
	// handler is path-agnostic, so the documented endpoint is the MCP listener's
	// /mcp path. Stateful mode (the SDK default) is required: it validates the
	// Mcp-Session-Id header and returns 404 for an unknown/stale session id,
	// which is how a client recovers after a server restart.
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{Logger: logger})

	webHandler, err := setupWebHandler(s, dm, uim, authenticator)
	if err != nil {
		log.Fatalf("failed to setup web handler: %v", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	// Web Server
	g.Go(func() error {
		return runServer(ctx, "Web", cfg.Server.HTTP, webHandler, logger)
	})

	// MCP Server
	g.Go(func() error {
		return runServer(ctx, "MCP", cfg.Server.MCP, httplog.Middleware(logger)(authenticator.Middleware(mcpHandler)), logger)
	})

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

func setupMCPServer(cfg *config.Config, s *storage.FSGitStorage, ts translation.TranslationService, logger *slog.Logger) *mcp.Server {
	mcpServer := mcp.NewServer(
		&mcp.Implementation{
			Name:    "shoka",
			Version: "0.1.0",
		},
		&mcp.ServerOptions{Logger: logger},
	)

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
		Description: "Get a context-efficient summary of a Markdown file (frontmatter, first heading, short excerpt, size, version) without its full body",
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

	return mcpServer
}

func setupWebHandler(s *storage.FSGitStorage, dm *drafts.Manager, uim *ui.Manager, authenticator *auth.Authenticator) (http.Handler, error) {
	mux := http.NewServeMux()
	// WebSocket endpoints accept the ?token= query fallback (browsers cannot set
	// an Authorization header on a WS handshake). The MCP/SSE endpoint uses the
	// header-only Middleware (see runServer wiring) so tokens never ride in URLs.
	mux.Handle("/drafts/", authenticator.MiddlewareAllowQueryToken(dm))
	mux.Handle("/ws/ui", authenticator.MiddlewareAllowQueryToken(uim))

	// Admin API (project state/rescan/recover) — shared by the `shoka project`
	// CLI and the Web UI recovery dialog. Header-bearer auth (no ?token=).
	mux.Handle("/api/", authenticator.Middleware(adminapi.New(s)))

	// Serve static files from embedded FS
	distFS, err := fs.Sub(server.DistFS, "dist")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(distFS))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
	})

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
// metrics.addr config is non-empty. The server shuts down when ctx is done.
func startMetricsServer(ctx context.Context, addr string, src metrics.Source, logger *slog.Logger) {
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
	mux.Handle("/metrics", metrics.Handler(src))
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
