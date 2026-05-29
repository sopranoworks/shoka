package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/auth"
	"github.com/shoka/mcp-server/internal/config"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/httplog"
	"github.com/shoka/mcp-server/internal/logging"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/shoka/mcp-server/internal/translation"
	"github.com/shoka/mcp-server/internal/ui"
	"github.com/shoka/mcp-server/internal/webhooks"
	"github.com/shoka/mcp-server/server"
	"golang.org/x/sync/errgroup"
)

func main() {
	configPath := flag.String("config", "shoka.yaml", "Path to configuration file")
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

	s, err := storage.NewFSGitStorage(cfg.Storage.BaseDir)
	if err != nil {
		log.Fatalf("failed to initialize storage: %v", err)
	}
	s.SetLogger(logger)

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

	uim := ui.NewManager(s, dm)

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

	webHandler, err := setupWebHandler(dm, uim, authenticator)
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

func setupMCPServer(cfg *config.Config, s storage.StorageService, ts translation.TranslationService, logger *slog.Logger) *mcp.Server {
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
	}, tools.LoggedTool(logger, "get_server_info", tools.GetServerInfoHandler(cfg)))

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
		Description: "List files in a project path",
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

func setupWebHandler(dm *drafts.Manager, uim *ui.Manager, authenticator *auth.Authenticator) (http.Handler, error) {
	mux := http.NewServeMux()
	// WebSocket endpoints accept the ?token= query fallback (browsers cannot set
	// an Authorization header on a WS handshake). The MCP/SSE endpoint uses the
	// header-only Middleware (see runServer wiring) so tokens never ride in URLs.
	mux.Handle("/drafts/", authenticator.MiddlewareAllowQueryToken(dm))
	mux.Handle("/ws/ui", authenticator.MiddlewareAllowQueryToken(uim))

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
