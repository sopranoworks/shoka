package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/tools"
	"github.com/sopranoworks/shoka/pkg/librarian"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

var librarianE2ERan atomic.Bool

// TestLibrarianE2E_MCP exercises the complete MCP round-trip for
// ask_the_librarian: real HTTP transport, real tool handler, real
// librariansrc.Corpus over FSGitStorage, real LLM calls to LM Studio.
//
// This test uses an in-process MCP server (httptest) with the production
// AskTheLibrarianHandler wired in — the same handler the real Shoka server
// uses. The librariansrc.Corpus adapter, NOT fsCorpus, backs every search
// and read. The only difference from a full server is config loading /
// process lifecycle, which the existing live_http_* suite already covers.
//
// Skip: LM Studio not reachable, already ran in this process.
func TestLibrarianE2E_MCP(t *testing.T) {
	if !librarianE2ERan.CompareAndSwap(false, true) {
		t.Skip("librarian E2E already exercised once in this process")
	}

	baseURL := "http://localhost:1234/v1"
	if v := os.Getenv("LIBRARIAN_LMSTUDIO_BASE_URL"); v != "" {
		baseURL = v
	}
	model := "qwen3-1.7b"
	if v := os.Getenv("LIBRARIAN_LMSTUDIO_MODEL"); v != "" {
		model = v
	}

	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
	if i := strings.LastIndex(host, "/"); i > 0 {
		host = host[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("LM Studio not reachable at %s: %v", baseURL, err)
	}
	_ = conn.Close()

	// --- Storage + corpus ---
	s, err := storage.NewFSGitStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSGitStorage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.CreateProject("default", "e2e-lib"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := s.WriteFileVersioned("default", "e2e-lib", "target.md",
		"# Build Tool\n\nfuigo installation was added to README.md on July 3, 2026.\n"+
			"The setup procedure is now documented in the project README.\n", ""); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if _, err := s.WriteFileVersioned("default", "e2e-lib", "noise1.md",
		"# Overview\n\nThis project uses Go for the backend.\n", ""); err != nil {
		t.Fatalf("write noise1: %v", err)
	}
	if _, err := s.WriteFileVersioned("default", "e2e-lib", "noise2.md",
		"# Architecture\n\nThe storage layer uses filesystem isolation.\n", ""); err != nil {
		t.Fatalf("write noise2: %v", err)
	}
	for i, doc := range e2eMixedNoiseDocs {
		path := fmt.Sprintf("mixed/doc%02d.md", i+1)
		if _, err := s.WriteFileVersioned("default", "e2e-lib", path, doc, ""); err != nil {
			t.Fatalf("write mixed doc %d: %v", i+1, err)
		}
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL drain timeout")
	}

	// --- LLM + librarian ---
	t.Setenv("OPENAI_API_KEY", "lm-studio")
	client, err := llm.NewClient(llm.LLMConfig{
		Provider: llm.ProviderOpenAI,
		BaseURL:  baseURL,
		Model:    model,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	lib := librarian.New(client, 0)

	// --- In-process MCP server with production handler ---
	srv := mcp.NewServer(&mcp.Implementation{Name: "shoka-librarian-e2e", Version: "0.0.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ask_the_librarian"}, tools.AskTheLibrarianHandler(s, lib))

	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return srv }, nil)
	httpSrv := httptest.NewServer(h)
	defer httpSrv.Close()

	// --- MCP client ---
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	mcpCli := mcp.NewClient(&mcp.Implementation{Name: "librarian-e2e-client", Version: "0.0.0"}, nil)
	sess, err := mcpCli.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpSrv.URL}, nil)
	if err != nil {
		t.Fatalf("MCP Connect: %v", err)
	}
	defer func() { _ = sess.Close() }()

	// --- Call ask_the_librarian via MCP ---
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "ask_the_librarian",
		Arguments: map[string]any{
			"namespace":    "default",
			"project_name": "e2e-lib",
			"question":     "When was the build tool added to the documentation?",
		},
	})
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "connect:") {
			t.Skipf("LM Studio went away: %v", err)
		}
		t.Fatalf("CallTool ask_the_librarian: %v", err)
	}

	text := wireText(res)
	t.Logf("MCP response (IsError=%v): %s", res.IsError, text)
	t.Logf("GATE_RAW_LEAK=%v", strings.Contains(text, "<|"))

	if res.IsError {
		if strings.Contains(text, "connection refused") || strings.Contains(text, "connect:") {
			t.Skipf("LM Studio went away mid-call: %s", text)
		}
		t.Fatalf("E2E FAIL: ask_the_librarian returned error: %s", text)
	}

	// Parse structured output — the handler returns JSON with answer + sources.
	var output struct {
		Answer  string `json:"answer"`
		Sources []struct {
			Path string `json:"path"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Logf("Response is not JSON (raw: %q); checking as plain text", text)
		if strings.TrimSpace(text) == "" {
			t.Errorf("E2E FAIL: empty response from MCP")
		}
		return
	}

	if strings.TrimSpace(output.Answer) == "" {
		t.Errorf("E2E FAIL: empty answer in structured output")
	} else {
		t.Logf("Answer: %q", output.Answer)
	}

	if len(output.Sources) > 0 {
		for _, src := range output.Sources {
			t.Logf("Source: %s", src.Path)
		}
	}
}

var e2eMixedNoiseDocs = [...]string{
	// Legal/NDA style (long-form, structured prose with sections)
	`# Mutual Non-Disclosure Agreement

## Definitions

"Confidential Information" means all confidential information (however recorded,
preserved or disclosed) disclosed by a Party or its Representatives to the other
Party and that Party's Representatives including but not limited to: (a) the fact
that discussions and negotiations are taking place concerning the Purpose and the
status of those discussions, (b) the existence and terms of this agreement, (c) any
information that would be regarded as confidential by a reasonable business person
relating to the business, affairs, customers, clients, suppliers, plans, intentions,
or market opportunities of the disclosing party.

## Obligations of Receiving Party

The Receiving Party undertakes that it shall: (i) keep the Confidential Information
confidential; (ii) not disclose the Confidential Information to any other person
other than in accordance with this Agreement; (iii) not use the Confidential
Information for any purpose other than the Purpose; (iv) not copy or reduce to
writing any part of the Confidential Information except as may be reasonably
necessary for the Purpose.

## Duration

This Agreement shall remain in effect for a period of three (3) years from the
Effective Date. The obligations of confidentiality shall survive the termination
of this Agreement for a period of five (5) years.
`,

	// Technical specification (API documentation style)
	`# Storage API v2 Specification

## Overview

The Storage API provides a RESTful interface for managing project files within
isolated namespaces. Each namespace contains zero or more projects, and each
project maintains a full version history via Git-based storage.

## Endpoints

### POST /api/v2/files/write

Write a file to a project. The file content is committed atomically with a
generated commit message. If the file already exists, it is overwritten and
the previous version remains accessible via the history endpoint.

**Request Body:**
` + "```json\n" + `{
  "namespace": "string (required)",
  "project": "string (required)",
  "path": "string (required)",
  "content": "string (required, UTF-8 text)",
  "if_match": "string (optional, etag for optimistic concurrency)"
}
` + "```\n" + `
**Response:** 200 OK with etag of new version, or 409 Conflict if if_match
is provided and the file has changed since the given etag.

### GET /api/v2/files/read

Read a file from a project at its current version or at a specific historical
version identified by a Git commit hash. Returns the file content and metadata
including the current etag, last modified timestamp, and commit information.

## Error Handling

All endpoints return structured JSON error responses with a machine-readable
error code and human-readable message. Common error codes include:
NOT_FOUND, CONFLICT, INVALID_INPUT, INTERNAL_ERROR.
`,

	// Legislative/regulatory analysis (long-form)
	`# Regulatory Impact Assessment: Data Retention Directive

## Executive Summary

This assessment examines the proposed amendments to the Data Retention
Framework Directive (2024/XXX) as adopted by the regulatory committee on
15 March 2026. The proposed changes extend mandatory data retention periods
for telecommunications providers from 12 months to 24 months for metadata
and introduce new categories of data subject to retention requirements.

## Background

The original Directive was adopted in response to concerns about national
security and law enforcement access to communications data. Since its adoption,
several constitutional challenges have been brought in member state courts,
with varying outcomes. The European Court of Justice ruling in Case C-293/12
invalidated portions of the predecessor directive, citing disproportionate
interference with fundamental rights to privacy and data protection.

## Impact on Small and Medium Enterprises

Small telecommunications providers (fewer than 250 employees) face
disproportionate compliance costs under the proposed framework. Initial
estimates suggest implementation costs of EUR 150,000-500,000 per provider
for systems upgrades, staff training, and ongoing compliance monitoring.
The proposed exemption threshold of 100,000 subscribers would exclude
approximately 60% of affected providers from the most burdensome requirements.

## Recommendations

The committee recommends: (1) phased implementation over 36 months rather
than the proposed 18 months, (2) establishment of a technical assistance
fund for SME compliance, (3) annual review of retention categories to ensure
continued proportionality, (4) mandatory sunset clause after 5 years.
`,

	// Scientific research abstract (technical, dense prose)
	`# Efficient Approximate Nearest Neighbor Search in High-Dimensional Spaces

## Abstract

We present a novel approach to approximate nearest neighbor (ANN) search
in high-dimensional vector spaces that achieves sub-linear query time while
maintaining recall rates above 95% on standard benchmarks. Our method
combines locality-sensitive hashing with learned quantization functions
trained on the data distribution, eliminating the need for manual
hyperparameter tuning that plagues existing methods.

## Introduction

The problem of finding nearest neighbors in high-dimensional spaces is
fundamental to many applications in machine learning, information retrieval,
and database systems. As embedding dimensions grow (current language models
produce 768 to 4096-dimensional vectors), exact nearest neighbor search
becomes computationally infeasible due to the curse of dimensionality.

Existing approximate methods fall into three categories: tree-based methods
(KD-trees, ball trees), hash-based methods (locality-sensitive hashing),
and graph-based methods (HNSW, NSG). Each family trades off index
construction time, memory footprint, query latency, and recall differently.

## Method

Our approach, which we term Adaptive Quantized Hashing (AQH), proceeds
in three phases: (1) learn a non-linear projection from the ambient space
to a lower-dimensional representation using a lightweight neural network
trained on a sample of the dataset, (2) construct multiple hash tables
using data-dependent hash functions derived from the learned projection,
(3) at query time, probe hash buckets in order of estimated relevance
using a priority queue over bucket collision counts.

## Experimental Results

On the ANN-Benchmarks suite (SIFT-1M, GIST-1M, GloVe-200), AQH achieves
97.2% recall at 10-nearest-neighbors with average query latency of 0.3ms,
compared to 96.8% recall at 0.5ms for HNSW and 94.1% recall at 0.2ms
for FAISS-IVF with equivalent memory budgets.
`,

	// Project status report (operational documentation style)
	`# Migration Status Report: Legacy Auth to OAuth 2.0

## Report Date: 2026-06-15

## Current Status: Phase 2 of 4 — In Progress

### Phase 1: Token Format Migration (COMPLETED 2026-05-20)

All internal services have been updated to accept both legacy session
tokens and new JWT-format tokens. The dual-acceptance window ensures
zero downtime during the transition. Monitoring confirms that 100% of
new sessions now issue JWT tokens, while approximately 12% of active
sessions still hold legacy tokens that will expire naturally within
30 days.

### Phase 2: Authorization Server Deployment (IN PROGRESS)

The new OAuth 2.0 authorization server has been deployed to staging
and is undergoing load testing. Initial results show p99 latency of
45ms for token issuance (target: <100ms) and 12ms for token validation
(target: <20ms). Two issues remain open:

1. **ISSUE-4521**: Refresh token rotation fails intermittently under
   high concurrency (>10,000 concurrent refresh requests). Root cause
   identified as a race condition in the token family tracking logic.
   Fix deployed to staging, awaiting 48-hour soak test.

2. **ISSUE-4533**: PKCE code verifier validation rejects valid S256
   challenges when the code_verifier contains certain Unicode characters.
   Scope limited to non-ASCII verifiers, which are rare in practice
   but required by spec.

### Phase 3: Client SDK Updates (NOT STARTED — Target: 2026-07-01)

Updated SDKs for Python, Go, and TypeScript are in development. The
Go SDK is feature-complete and in review. Python and TypeScript SDKs
are at approximately 60% completion.

### Phase 4: Legacy Deprecation (NOT STARTED — Target: 2026-08-15)

After a 30-day overlap period following Phase 3 completion, legacy
token endpoints will begin returning deprecation warnings. Full
shutdown of legacy endpoints is planned for 2026-09-15.

## Risks

- Timeline risk if ISSUE-4521 soak test reveals additional failure modes
- Dependency on mobile team releasing app update with new SDK before Phase 4
`,
}
