package tools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/config"
)

// WALInfoProvider exposes the write-ahead log status surfaced by get_server_info
// (§8.6) so non-Prometheus operators can see it over MCP. The storage layer
// implements it.
type WALInfoProvider interface {
	WALPending() int
	WALWriteDisabled() bool
	WALOldestEntryAge() time.Duration
}

type GetServerInfoInput struct {
}

type GetServerInfoOutput struct {
	ExternalURL    string `json:"external_url"`
	HTTPListen     string `json:"http_listen"`
	MCPListen      string `json:"mcp_listen"`
	StorageBaseDir string `json:"storage_base_dir"`
	// WAL status (storage redesign §8.6). These mirror the metrics endpoint so
	// operators without Prometheus can observe write-path health over MCP.
	WALPending            int     `json:"wal_pending"`
	WALWriteDisabled      bool    `json:"wal_write_disabled"`
	WALOldestEntryAgeSecs float64 `json:"wal_oldest_entry_age_seconds"`
}

func GetServerInfoHandler(cfg *config.Config, wal WALInfoProvider) func(context.Context, *mcp.CallToolRequest, GetServerInfoInput) (*mcp.CallToolResult, GetServerInfoOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input GetServerInfoInput) (*mcp.CallToolResult, GetServerInfoOutput, error) {
		return nil, GetServerInfoOutput{
			ExternalURL:           cfg.Server.HTTP.ExternalURL,
			HTTPListen:            cfg.Server.HTTP.Listen,
			MCPListen:             cfg.Server.MCP.Plain.Listen,
			StorageBaseDir:        cfg.Storage.BaseDir,
			WALPending:            wal.WALPending(),
			WALWriteDisabled:      wal.WALWriteDisabled(),
			WALOldestEntryAgeSecs: wal.WALOldestEntryAge().Seconds(),
		}, nil
	}
}
