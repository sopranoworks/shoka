package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
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

	// Setup context with signal handling
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := storage.NewFSGitStorage(cfg.Storage.BaseDir)
	if err != nil {
		log.Fatalf("failed to initialize storage: %v", err)
	}

	notifier := webhooks.New(toWebhookConfigs(cfg.Webhooks))
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
			log.Printf("Warning: failed to initialize Google Translation service: %v. Translation tool will not be available.", err)
		} else {
			defer ts.Close()
		}
	}

	mcpServer := setupMCPServer(cfg, s, ts)
	mcpHandler := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server {
		return mcpServer
	}, nil)

	webHandler, err := setupWebHandler(dm, uim, authenticator)
	if err != nil {
		log.Fatalf("failed to setup web handler: %v", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	// Web Server
	g.Go(func() error {
		return runServer(ctx, "Web", cfg.Server.HTTP, webHandler)
	})

	// MCP Server
	g.Go(func() error {
		return runServer(ctx, "MCP", cfg.Server.MCP, authenticator.Middleware(mcpHandler))
	})

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("Server error: %v", err)
		os.Exit(1)
	}

	// Drain any in-flight webhook deliveries before exiting.
	notifier.Wait()

	log.Println("Servers shut down gracefully")
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

func setupMCPServer(cfg *config.Config, s storage.StorageService, ts translation.TranslationService) *mcp.Server {
	mcpServer := mcp.NewServer(
		&mcp.Implementation{
			Name:    "shoka",
			Version: "0.1.0",
		},
		nil,
	)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "get_server_info",
		Description: "Get information about the server's public URL and configuration",
	}, tools.GetServerInfoHandler(cfg))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "create_project",
		Description: "Create a new project with Git initialization",
	}, tools.CreateProjectHandler(s))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "list_projects",
		Description: "List all projects in a namespace",
	}, tools.ListProjectsHandler(s))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a file from a project",
	}, tools.ReadFileHandler(s))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "write_file",
		Description: "Write a file to a project with atomic Git commit",
	}, tools.WriteFileHandler(s))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "delete_file",
		Description: "Delete a file from a project with atomic Git commit",
	}, tools.DeleteFileHandler(s))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "list_files",
		Description: "List files in a project path",
	}, tools.ListFilesHandler(s))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "get_history",
		Description: "Get Git commit history for a project or file",
	}, tools.GetHistoryHandler(s))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "read_file_at_version",
		Description: "Read a file at a specific Git commit hash",
	}, tools.ReadFileAtVersionHandler(s))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "read_summary",
		Description: "Get a context-efficient summary of a Markdown file (frontmatter, first heading, short excerpt, size, version) without its full body",
	}, tools.ReadSummaryHandler(s))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "list_files_since",
		Description: "List files changed after a given RFC3339 timestamp or commit hash, with each file's change kind (added/modified/deleted)",
	}, tools.ListFilesSinceHandler(s))

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "search_files",
		Description: "Search a project's files by filename, content, or both (case-insensitive substring), returning matches with context snippets",
	}, tools.SearchFilesHandler(s))

	if ts != nil {
		mcp.AddTool(mcpServer, &mcp.Tool{
			Name:        "translate_file",
			Description: "Translate a Markdown file to a target language (Japanese to English by default)",
		}, tools.TranslateFileHandler(s, ts))
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

func runServer(ctx context.Context, name string, settings config.ServerSettings, handler http.Handler) error {
	srv := &http.Server{
		Addr:    settings.Listen,
		Handler: handler,
	}

	errChan := make(chan error, 1)
	go func() {
		log.Printf("Starting %s server on %s...", name, settings.Listen)
		var err error
		if settings.TLS.Enabled {
			log.Printf("TLS enabled for %s", settings.Listen)
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
		log.Printf("Shutting down %s server...", name)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errChan:
		return err
	}
}
