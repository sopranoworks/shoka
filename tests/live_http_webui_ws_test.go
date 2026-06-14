package tests

// B-31 regression guard: the Web UI shows its project list (and ALL read data)
// only over the /ws/ui WebSocket — there is no REST read API. The B-53 request-
// tracing middleware (reqtrace.statusRecorder) wraps every Web-listener request as
// the outermost layer; when that recorder failed to forward http.Hijacker, every
// /ws/ui (and /drafts/) WebSocket upgrade returned HTTP 500 "response does not
// implement http.Hijacker", so GET_PROJECTS never ran and the UI rendered "0
// projects". This test drives the FULL server (the real reqtrace -> auth -> ui.Manager
// stack, exec'd by the live harness — not a hand-built handler), so it catches ANY
// future writer wrapper on the Web listener that drops Hijack, not just this one.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/storage"
)

// wsProjectInfo mirrors the GET_PROJECTS response element (internal/ui.ProjectInfo)
// on the wire; the test decodes it structurally rather than importing the type.
type wsProjectInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	State     string `json:"state"`
}

type wsFrame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func TestLiveWebUI_WSUpgradeAndGetProjects(t *testing.T) {
	baseDir := t.TempDir()

	// Seed one real (git-backed) project BEFORE the server starts, the same shape a
	// real Shoka project has on disk — so startup catalogs it instead of relocating
	// it as a repo-less leftover. Close the seeding storage so the server reopens the
	// dir without a filelock/WAL conflict.
	seed, err := storage.NewFSGitStorage(baseDir)
	if err != nil {
		t.Fatalf("seed storage: %v", err)
	}
	if err := seed.CreateProject("acme", "widgets"); err != nil {
		t.Fatalf("seed CreateProject: %v", err)
	}
	seed.WaitForWAL(10 * time.Second)
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	var httpPort int
	var logPath string
	cleanup := startLiveServer(t, func() liveLaunch {
		httpPort = freePort(t)
		mcpPort := freePort(t)
		cfgPath := writeLiveConfig(t, baseDir, httpPort, mcpPort, "info", false, "")
		logPath = filepath.Join(t.TempDir(), "server.log")
		return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, mcpPort}}
	})
	defer cleanup()
	t.Logf("server log: %s", logPath)

	// The guard: the /ws/ui WebSocket upgrade must SUCCEED (HTTP 101). Before the
	// reqtrace.statusRecorder Hijack fix this Dial fails with "bad handshake" (the
	// handler 500s because the wrapped writer is not an http.Hijacker).
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/ws/ui", httpPort)
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("/ws/ui upgrade failed: %v (http status=%d). The Web listener's "+
			"response-writer wrapper must forward http.Hijacker so WebSocket upgrades work. "+
			"server log: %s", err, status, logPath)
	}
	defer conn.Close()

	// Over the open socket, request the project list and assert the seeded project
	// comes back non-empty — the exact path the Web UI's useProjectsQuery exercises.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"GET_PROJECTS","payload":{}}`)); err != nil {
		t.Fatalf("write GET_PROJECTS: %v", err)
	}

	projects := readProjects(t, conn)
	if len(projects) == 0 {
		t.Fatalf("GET_PROJECTS returned an empty project list though a project was seeded; "+
			"server log: %s", logPath)
	}
	found := false
	for _, p := range projects {
		if p.Namespace == "acme" && p.Name == "widgets" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("seeded project acme/widgets not in GET_PROJECTS response %+v; server log: %s",
			projects, logPath)
	}
}

// readProjects reads frames until the GET_PROJECTS response arrives (skipping any
// unsolicited NOTIFY push), then decodes its payload.
func readProjects(t *testing.T, conn *websocket.Conn) []wsProjectInfo {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read GET_PROJECTS response: %v", err)
		}
		var f wsFrame
		if err := json.Unmarshal(msg, &f); err != nil {
			t.Fatalf("decode frame %q: %v", string(msg), err)
		}
		if f.Type == "NOTIFY" {
			continue
		}
		if f.Type == "ERROR" {
			t.Fatalf("GET_PROJECTS returned an ERROR frame: %s", string(f.Payload))
		}
		var projects []wsProjectInfo
		if err := json.Unmarshal(f.Payload, &projects); err != nil {
			t.Fatalf("decode GET_PROJECTS payload %q: %v", string(f.Payload), err)
		}
		return projects
	}
}
