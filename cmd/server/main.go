package main

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/shoka/mcp-server/internal/translation"
	"github.com/shoka/mcp-server/internal/ui"
	"github.com/shoka/mcp-server/server"
)

func main() {
	storageBaseDir := os.Getenv("STORAGE_BASE_DIR")
	if storageBaseDir == "" {
		storageBaseDir = "./data"
	}

	s, err := storage.NewFSGitStorage(storageBaseDir)
	if err != nil {
		log.Fatalf("failed to initialize storage: %v", err)
	}

	dm, err := drafts.NewManager(storageBaseDir)
	if err != nil {
		log.Fatalf("failed to initialize draft manager: %v", err)
	}

	uim := ui.NewManager(s, dm)

	draftsPort := os.Getenv("DRAFTS_PORT")
	if draftsPort == "" {
		draftsPort = "8080"
	}

	go func() {
		log.Printf("Starting drafts server on :%s...", draftsPort)
		mux := http.NewServeMux()
		mux.Handle("/drafts/", dm)
		mux.Handle("/ws/ui", uim)

		// Serve static files from embedded FS
		distFS, err := fs.Sub(server.DistFS, "dist")
		if err != nil {
			log.Fatalf("failed to get sub fs: %v", err)
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

		if err := http.ListenAndServe(":"+draftsPort, mux); err != nil {
			log.Printf("drafts server error: %v", err)
		}
	}()


	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	var ts translation.TranslationService
	if projectID != "" {
		var err error
		ts, err = translation.NewGoogleTranslationService(context.Background(), projectID)
		if err != nil {
			log.Printf("Warning: failed to initialize Google Translation service: %v. Translation tool will not be available.", err)
		} else {
			defer ts.Close()
		}
	}

	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "shoka",
			Version: "0.1.0",
		},
		nil,
	)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_project",
		Description: "Create a new project with Git initialization",
	}, tools.CreateProjectHandler(s))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_projects",
		Description: "List all projects in a namespace",
	}, tools.ListProjectsHandler(s))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a file from a project",
	}, tools.ReadFileHandler(s))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "write_file",
		Description: "Write a file to a project with atomic Git commit",
	}, tools.WriteFileHandler(s))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "delete_file",
		Description: "Delete a file from a project with atomic Git commit",
	}, tools.DeleteFileHandler(s))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_files",
		Description: "List files in a project path",
	}, tools.ListFilesHandler(s))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_history",
		Description: "Get Git commit history for a project or file",
	}, tools.GetHistoryHandler(s))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_file_at_version",
		Description: "Read a file at a specific Git commit hash",
	}, tools.ReadFileAtVersionHandler(s))

	if ts != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "translate_file",
			Description: "Translate a Markdown file to a target language (Japanese to English by default)",
		}, tools.TranslateFileHandler(s, ts))
	}

	log.Println("Starting Shoka MCP server...")
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
