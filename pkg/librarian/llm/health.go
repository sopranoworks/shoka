package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
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
	// env key is auth_failed — say which variable, never its value. (For the
	// ollama debug loop the placeholder ANTHROPIC_API_KEY=ollama is non-empty, so
	// this passes through to the real call.)
	if env := apiKeyEnvVar(cfg.Provider); env != "" && os.Getenv(env) == "" {
		return HealthResult{Kind: HealthAuthFailed, Detail: env + " is empty or unset"}
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

// apiKeyEnvVar names the environment variable each provider's SDK reads its key
// from, or "" for an unknown provider. We only ever check this variable's
// PRESENCE to phrase a message — its value is never read into config or logged.
func apiKeyEnvVar(provider string) string {
	switch provider {
	case ProviderAnthropic:
		return "ANTHROPIC_API_KEY"
	case ProviderOpenAI:
		return "OPENAI_API_KEY"
	default:
		return ""
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
