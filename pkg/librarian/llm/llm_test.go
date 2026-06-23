package llm

import (
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
)

func TestNewClient_ProviderSwitch(t *testing.T) {
	if c, err := NewClient(LLMConfig{Provider: ProviderAnthropic, Model: "claude-x"}); err != nil || c == nil {
		t.Errorf("anthropic: got (%v, %v), want a client", c, err)
	}
	if c, err := NewClient(LLMConfig{Provider: ProviderOpenAI, Model: "gpt-x"}); err != nil || c == nil {
		t.Errorf("openai: got (%v, %v), want a client", c, err)
	}
	// Both accept a base-URL override (ollama debug) without error.
	if _, err := NewClient(LLMConfig{Provider: ProviderOpenAI, Model: "m", BaseURL: "http://localhost:11434/v1"}); err != nil {
		t.Errorf("openai with base_url: unexpected error %v", err)
	}
	// Unknown provider is a clear error, not a silent fallback.
	if _, err := NewClient(LLMConfig{Provider: "gemini", Model: "m"}); err == nil {
		t.Errorf("unknown provider must be an error")
	}
	if _, err := NewClient(LLMConfig{Provider: "", Model: "m"}); err == nil {
		t.Errorf("empty provider must be an error")
	}
}

func TestLLMConfig_IsConfigured(t *testing.T) {
	cases := []struct {
		cfg  LLMConfig
		want bool
	}{
		{LLMConfig{Provider: "anthropic", Model: "claude-x"}, true},
		{LLMConfig{Provider: "openai", Model: "gpt-x"}, true},
		{LLMConfig{Provider: "anthropic"}, false}, // no model
		{LLMConfig{Model: "claude-x"}, false},     // no provider
		{LLMConfig{}, false},
	}
	for _, c := range cases {
		if got := c.cfg.IsConfigured(); got != c.want {
			t.Errorf("IsConfigured(%+v) = %v, want %v", c.cfg, got, c.want)
		}
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want HealthKind
	}{
		{401, "", HealthAuthFailed},
		{403, "", HealthAuthFailed},
		{404, "model not found", HealthModelNotFound},
		{400, `the model "gpt-x" does not exist`, HealthModelNotFound},
		{400, "some other bad request", HealthMisconfigured},
		{429, "rate limited", HealthReady},
		{500, "server error", HealthUnreachable},
		{503, "", HealthUnreachable},
		{0, "", HealthUnreachable},
		{418, "", HealthMisconfigured},
	}
	for _, c := range cases {
		if got := classifyStatus(c.code, c.msg); got != c.want {
			t.Errorf("classifyStatus(%d, %q) = %v, want %v", c.code, c.msg, got, c.want)
		}
	}
}

func TestClassifyError(t *testing.T) {
	// An OpenAI 404 naming the model ⇒ model_not_found.
	oerr := &openai.Error{StatusCode: 404, Message: `the model "gpt-x" does not exist`}
	if got := classifyError(oerr); got.Kind != HealthModelNotFound {
		t.Errorf("openai 404 = %v, want model_not_found", got.Kind)
	}
	// An Anthropic 401 ⇒ auth_failed.
	aerr := &anthropic.Error{StatusCode: 401}
	if got := classifyError(aerr); got.Kind != HealthAuthFailed {
		t.Errorf("anthropic 401 = %v, want auth_failed", got.Kind)
	}
	// A plain (non-SDK) error ⇒ unreachable (dial/timeout/DNS).
	if got := classifyError(errors.New("dial tcp: connection refused")); got.Kind != HealthUnreachable {
		t.Errorf("network error = %v, want unreachable", got.Kind)
	}
}

func TestCheckHealth_Misconfigured(t *testing.T) {
	// No provider/model ⇒ misconfigured, with NO network call.
	if got := CheckHealth(t.Context(), LLMConfig{}); got.Kind != HealthMisconfigured {
		t.Errorf("empty config = %v, want misconfigured", got.Kind)
	}
}

// The diagnostic field exists to carry the WHY: every non-ready classification —
// and ESPECIALLY the misconfigured fallback and the local/no-HTTP-status case —
// must yield a non-empty Detail that reflects the underlying error, never "".
func TestClassifyError_DetailAlwaysPopulated(t *testing.T) {
	// The misconfigured fallback (a status that maps to no specific kind) must
	// carry a non-empty detail naming the status — no more `misconfigured detail=""`.
	if got := classifyError(&anthropic.Error{StatusCode: 418}); got.Kind != HealthMisconfigured {
		t.Fatalf("anthropic 418 kind = %v, want misconfigured", got.Kind)
	} else if got.Detail == "" || !strings.Contains(got.Detail, "418") {
		t.Errorf("misconfigured fallback detail = %q, want non-empty containing the status", got.Detail)
	}
	// A 400 that isn't about the model is also the fallback bucket; still non-empty.
	if got := classifyError(&anthropic.Error{StatusCode: 400}); got.Kind != HealthMisconfigured || got.Detail == "" {
		t.Errorf("anthropic 400 = %+v, want misconfigured with non-empty detail", got)
	}
	// The local / no-HTTP-status case: Detail is exactly the underlying err.Error().
	if got := classifyError(errors.New("dial tcp: connection refused")); got.Detail != "dial tcp: connection refused" {
		t.Errorf("local error detail = %q, want it to equal err.Error()", got.Detail)
	}
	// An OpenAI message-bearing error surfaces that message in the detail.
	oerr := &openai.Error{StatusCode: 400, Message: "unsupported parameter"}
	if got := classifyError(oerr); got.Detail == "" || !strings.Contains(got.Detail, "unsupported parameter") {
		t.Errorf("openai detail = %q, want it to include the message", got.Detail)
	}
}

// A body-level authentication error reads as auth_failed even when the status
// mapping alone would not (defence in depth for a key rejected without a clean 401).
func TestClassifyError_AuthErrorType(t *testing.T) {
	var ae anthropic.Error
	if err := ae.UnmarshalJSON([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)); err != nil {
		t.Fatalf("unmarshal synthetic anthropic error: %v", err)
	}
	if ae.Type() != "authentication_error" {
		t.Skipf("SDK did not surface the body error-type (got %q); override path untestable here", ae.Type())
	}
	if got := classifyError(&ae); got.Kind != HealthAuthFailed {
		t.Errorf("body authentication_error = %v, want auth_failed", got.Kind)
	}
}

// A missing env key is named plainly as auth_failed with NO network call, and the
// detail names only the variable — never any value.
func TestCheckHealth_MissingKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "") // force empty regardless of the host env
	got := CheckHealth(t.Context(), LLMConfig{Provider: ProviderAnthropic, Model: "claude-x"})
	if got.Kind != HealthAuthFailed {
		t.Fatalf("missing key kind = %v, want auth_failed", got.Kind)
	}
	if got.Detail != "ANTHROPIC_API_KEY is empty or unset" {
		t.Errorf("missing key detail = %q, want the env-var-name message", got.Detail)
	}
}
