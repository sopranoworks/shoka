package llm

import (
	"errors"
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
