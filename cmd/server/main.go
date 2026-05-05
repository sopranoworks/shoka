package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
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

	log.Println("Starting Shoka MCP server...")
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
