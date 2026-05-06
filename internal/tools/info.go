package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/config"
)

type GetServerInfoInput struct {
}

type GetServerInfoOutput struct {
	ExternalURL    string `json:"external_url"`
	HTTPListen     string `json:"http_listen"`
	MCPListen      string `json:"mcp_listen"`
	StorageBaseDir string `json:"storage_base_dir"`
}

func GetServerInfoHandler(cfg *config.Config) func(context.Context, *mcp.CallToolRequest, GetServerInfoInput) (*mcp.CallToolResult, GetServerInfoOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input GetServerInfoInput) (*mcp.CallToolResult, GetServerInfoOutput, error) {
		return nil, GetServerInfoOutput{
			ExternalURL:    cfg.Server.HTTP.ExternalURL,
			HTTPListen:     cfg.Server.HTTP.Listen,
			MCPListen:      cfg.Server.MCP.Listen,
			StorageBaseDir: cfg.Storage.BaseDir,
		}, nil
	}
}
