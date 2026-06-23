package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

// HealthKind classifies whether the librarian's LLM config is actually usable.
type HealthKind string

const (
	HealthReady         HealthKind = "ready"           // a minimal call succeeded
	HealthModelNotFound HealthKind = "model_not_found" // the model name is wrong (the common typo)
	HealthAuthFailed    HealthKind = "auth_failed"     // missing/invalid env API key
	HealthUnreachable   HealthKind = "unreachable"     // endpoint down / network / 5xx
	HealthMisconfigured HealthKind = "misconfigured"   // provider/model unset or other bad request
)

// HealthResult is the outcome of a one-call health-check. Detail is a short,
// secret-free message (it NEVER contains the API key).
type HealthResult struct {
	Kind   HealthKind
	Detail string
}

// CheckHealth makes ONE minimal LLM call (no tools) to verify the config end to
// end: provider + model + the env API key + endpoint reachability. It catches
// the common failures — a mistyped model, a missing/invalid key, an unreachable
// endpoint — that otherwise only surface at first real use. It never returns the
// key or any secret.
func CheckHealth(ctx context.Context, cfg LLMConfig) HealthResult {
	if !cfg.IsConfigured() {
		return HealthResult{Kind: HealthMisconfigured, Detail: "provider and model are required"}
	}
	// The two basic key states are knowable without a round-trip, so name them
	// plainly instead of letting them fall through to a vaguer bucket. A missing
	// env key is auth_failed — say which variable(s), never the value. The key is
	// "missing" only when EVERY accepted variable is empty (gemini reads either
	// GEMINI_API_KEY or GOOGLE_API_KEY). (For the ollama debug loop the placeholder
	// ANTHROPIC_API_KEY=ollama is non-empty, so this passes through to the call.)
	if envs := apiKeyEnvVars(cfg.Provider); len(envs) > 0 && allEnvEmpty(envs) {
		return HealthResult{Kind: HealthAuthFailed, Detail: missingKeyDetail(envs)}
	}
	client, err := NewClient(cfg)
	if err != nil {
		return HealthResult{Kind: HealthMisconfigured, Detail: clip(err.Error())}
	}
	_, err = client.CreateMessage(ctx, CreateMessageParams{
		Messages: []Message{{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "ping"}}}},
	})
	if err != nil {
		return classifyError(err)
	}
	return HealthResult{Kind: HealthReady}
}

// apiKeyEnvVars names the environment variable(s) each provider's SDK reads its
// key from, primary first, or nil for an unknown provider. Gemini's genai SDK
// reads either GEMINI_API_KEY or GOOGLE_API_KEY (GOOGLE_API_KEY wins if both are
// set), so the key is present when EITHER is set. We only ever check these
// variables' PRESENCE to phrase a message — a value is never read or logged.
func apiKeyEnvVars(provider string) []string {
	switch provider {
	case ProviderAnthropic:
		return []string{"ANTHROPIC_API_KEY"}
	case ProviderOpenAI:
		return []string{"OPENAI_API_KEY"}
	case ProviderGemini:
		return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}
	default:
		return nil
	}
}

// allEnvEmpty reports whether every named environment variable is empty/unset.
func allEnvEmpty(envs []string) bool {
	for _, e := range envs {
		if os.Getenv(e) != "" {
			return false
		}
	}
	return true
}

// missingKeyDetail names the accepted env var(s) — never a value — for the
// auth_failed detail. With more than one accepted variable the primary is named
// with the alternative(s) in parentheses.
func missingKeyDetail(envs []string) string {
	switch len(envs) {
	case 0:
		return ""
	case 1:
		return envs[0] + " is empty or unset"
	default:
		return envs[0] + " (or " + strings.Join(envs[1:], ", ") + ") is empty or unset"
	}
}

// classifyError maps an SDK / transport error to a HealthResult. On EVERY
// non-ready outcome it populates Detail with the underlying error — for the SDK
// errors, the structured status/type/body fields (read directly, never via
// .Error(), which dereferences the nil Request/Response of a synthetic test
// error); for any other error, its err.Error() text. The api key is NEVER
// included: it lives only in the Authorization header, which none of these fields
// expose. result() guarantees Detail is never empty on a non-ready kind.
func classifyError(err error) HealthResult {
	var aerr *anthropic.Error
	if errors.As(err, &aerr) {
		kind := classifyStatus(aerr.StatusCode, aerr.RawJSON())
		// A body-level auth/permission error reads as auth_failed even if the
		// status line didn't say so (a 401/403 already maps to auth_failed).
		if t := aerr.Type(); t == "authentication_error" || t == "permission_error" {
			kind = HealthAuthFailed
		}
		return result(kind, detailFromAnthropic(aerr))
	}
	var oerr *openai.Error
	if errors.As(err, &oerr) {
		kind := classifyStatus(oerr.StatusCode, oerr.Message)
		if oerr.Code == "invalid_api_key" || oerr.Type == "authentication_error" {
			kind = HealthAuthFailed
		}
		return result(kind, detailFromOpenAI(oerr))
	}
	var gerr genai.APIError
	if errors.As(err, &gerr) {
		kind := classifyStatus(gerr.Code, gerr.Message)
		// Gemini reports a rejected key two ways the status code alone misses: a
		// 403/401 with status PERMISSION_DENIED/UNAUTHENTICATED, and a 400
		// INVALID_ARGUMENT whose message is "API key not valid". Force auth_failed.
		if geminiAuthError(&gerr) {
			kind = HealthAuthFailed
		}
		return result(kind, detailFromGemini(&gerr))
	}
	// No HTTP status reached us: a dial/timeout/DNS or a local SDK error (e.g. a
	// missing key surfaced without a round-trip). Surface its raw text verbatim.
	return result(HealthUnreachable, clip(err.Error()))
}

// result builds a HealthResult, guaranteeing a non-empty Detail for any non-ready
// outcome. The misconfigured fallback in particular must never log detail="".
func result(kind HealthKind, detail string) HealthResult {
	if kind != HealthReady && strings.TrimSpace(detail) == "" {
		detail = "the LLM call failed (no further detail available)"
	}
	return HealthResult{Kind: kind, Detail: detail}
}

// classifyStatus maps an HTTP status (and the response message, for the 400
// model-vs-other ambiguity) to a HealthKind. Exposed to tests as the pure core
// of the classifier.
func classifyStatus(code int, msg string) HealthKind {
	switch {
	case code == 401 || code == 403:
		return HealthAuthFailed
	case code == 404:
		return HealthModelNotFound
	case code == 400:
		if strings.Contains(strings.ToLower(msg), "model") {
			return HealthModelNotFound
		}
		return HealthMisconfigured
	case code == 429:
		// Rate-limited, but auth + model are valid — the config works.
		return HealthReady
	case code == 0 || code >= 500:
		return HealthUnreachable
	default:
		return HealthMisconfigured
	}
}

// detailFromAnthropic assembles a non-empty, secret-free detail from an Anthropic
// SDK error's structured fields (status, body error-type, raw body). It reads no
// Request/Response, so it is safe on the synthetic errors built in tests, and it
// never contains the api key (which is only ever in the Authorization header).
func detailFromAnthropic(e *anthropic.Error) string {
	return clip(joinNonEmpty(statusToken(e.StatusCode), string(e.Type()), e.RawJSON()))
}

// detailFromOpenAI assembles the equivalent from an OpenAI SDK error's structured
// fields (status, error type, message). Same safety guarantees as above.
func detailFromOpenAI(e *openai.Error) string {
	return clip(joinNonEmpty(statusToken(e.StatusCode), e.Type, e.Message))
}

// detailFromGemini assembles a non-empty, secret-free detail from a genai
// APIError's structured fields (HTTP code, status string, message — all from the
// response body). It is panic-safe (the value Error() reads no Request/Response)
// and never contains the api key, which the genai SDK sends only in the
// x-goog-api-key request header — never in the response body these fields come
// from.
func detailFromGemini(e *genai.APIError) string {
	return clip(joinNonEmpty(statusToken(e.Code), e.Status, e.Message))
}

// geminiAuthError reports whether a genai APIError indicates an auth/permission
// failure. A 401/403 already maps to auth_failed by status, but Gemini also
// returns a 403/401 with status PERMISSION_DENIED/UNAUTHENTICATED and a 400
// INVALID_ARGUMENT whose message is "API key not valid" for a rejected key.
func geminiAuthError(e *genai.APIError) bool {
	switch e.Status {
	case "UNAUTHENTICATED", "PERMISSION_DENIED":
		return true
	}
	msg := strings.ToLower(e.Message)
	return strings.Contains(msg, "api key not valid") || strings.Contains(msg, "api_key_invalid")
}

func statusToken(code int) string {
	if code == 0 {
		return ""
	}
	return fmt.Sprintf("HTTP %d", code)
}

// joinNonEmpty joins the non-blank parts with ": ", dropping empties so a sparse
// synthetic error (e.g. only a status code) still yields a tidy single line.
func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, strings.TrimSpace(p))
		}
	}
	return strings.Join(kept, ": ")
}

// clip reduces a message to a single concise line and caps its length, so a large
// response body never floods the startup log or the WebUI status card.
func clip(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexAny(msg, "\n\r"); i >= 0 {
		msg = msg[:i]
	}
	const max = 200
	if len(msg) > max {
		msg = msg[:max] + "…"
	}
	return msg
}
