package llm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// CapturedExchange is one HTTP request/response pair captured by a
// CapturingClient. Bodies are the raw bytes read from the wire.
type CapturedExchange struct {
	Method      string
	URL         string
	RequestBody []byte
	StatusCode  int
	ResponseBody []byte
}

// CapturingClient wraps an OpenAI-compatible llm.Client and records the raw
// HTTP request/response body for every API call. Intended for integration test
// diagnostics — not for production use.
type CapturingClient struct {
	inner *openaiClient

	mu        sync.Mutex
	Exchanges []CapturedExchange
}

// NewCapturingClient builds an OpenAI-compatible Client that captures raw HTTP
// traffic. It uses the same conversion logic as the standard openaiClient.
func NewCapturingClient(cfg LLMConfig) *CapturingClient {
	cc := &CapturingClient{}
	var opts []option.RequestOption
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	opts = append(opts, option.WithMiddleware(cc.middleware))
	cc.inner = &openaiClient{
		client:    openai.NewClient(opts...),
		model:     cfg.Model,
		maxTokens: defaultMaxTokens,
	}
	return cc
}

func (cc *CapturingClient) middleware(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	resp, err := next(req)
	if err != nil {
		cc.mu.Lock()
		cc.Exchanges = append(cc.Exchanges, CapturedExchange{
			Method:      req.Method,
			URL:         req.URL.String(),
			RequestBody: reqBody,
		})
		cc.mu.Unlock()
		return resp, err
	}

	var respBody []byte
	if resp.Body != nil {
		respBody, _ = io.ReadAll(resp.Body)
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}

	cc.mu.Lock()
	cc.Exchanges = append(cc.Exchanges, CapturedExchange{
		Method:       req.Method,
		URL:          req.URL.String(),
		RequestBody:  reqBody,
		StatusCode:   resp.StatusCode,
		ResponseBody: respBody,
	})
	cc.mu.Unlock()

	return resp, nil
}

// CreateMessage delegates to the underlying openaiClient.
func (cc *CapturingClient) CreateMessage(ctx context.Context, params CreateMessageParams) (Message, error) {
	return cc.inner.CreateMessage(ctx, params)
}
