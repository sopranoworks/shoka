package drafts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetDraftPath(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "shoka-drafts-test-*")
	assert.NoError(t, err)
	defer os.RemoveAll(tempDir)

	manager, err := NewManager(tempDir)
	assert.NoError(t, err)

	tests := []struct {
		name        string
		namespace   string
		projectName string
		path        string
		wantPrefix  string
		wantErr     bool
	}{
		{
			name:        "Valid path",
			namespace:   "ns1",
			projectName: "proj1",
			path:        "docs/readme.md",
			wantPrefix:  filepath.Join(tempDir, "ns1", "proj1", ".drafts"),
			wantErr:     false,
		},
		{
			name:        "Valid path in root",
			namespace:   "ns1",
			projectName: "proj1",
			path:        "test.md",
			wantPrefix:  filepath.Join(tempDir, "ns1", "proj1", ".drafts"),
			wantErr:     false,
		},
		{
			name:        "Invalid namespace",
			namespace:   "../invalid",
			projectName: "proj1",
			path:        "test.md",
			wantErr:     true,
		},
		{
			name:        "Invalid project name",
			namespace:   "ns1",
			projectName: "proj/1",
			path:        "test.md",
			wantErr:     true,
		},
		{
			name:        "Path traversal attempt",
			namespace:   "ns1",
			projectName: "proj1",
			path:        "../outside.md",
			wantErr:     true,
		},
		{
			name:        "Path traversal attempt with absolute path",
			namespace:   "ns1",
			projectName: "proj1",
			path:        "/etc/passwd",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := manager.GetDraftPath(tt.namespace, tt.projectName, tt.path)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.True(t, strings.HasPrefix(got, tt.wantPrefix), "Path %s should have prefix %s", got, tt.wantPrefix)
				assert.True(t, strings.HasSuffix(got, tt.path), "Path %s should have suffix %s", got, tt.path)
			}
		})
	}
}
