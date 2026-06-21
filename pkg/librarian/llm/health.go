package llm

import (
	"context"
	"errors"
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
	client, err := NewClient(cfg)
	if err != nil {
		return HealthResult{Kind: HealthMisconfigured, Detail: err.Error()}
	}
	_, err = client.CreateMessage(ctx, CreateMessageParams{
		Messages: []Message{{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "ping"}}}},
	})
	if err != nil {
		return classifyError(err)
	}
	return HealthResult{Kind: HealthReady}
}

// classifyError maps an SDK / transport error to a HealthResult. It reads the
// SDK error's status code + message (never calling .Error(), which dereferences
// the request/response), so it is safe on synthetic errors in tests too.
func classifyError(err error) HealthResult {
	var aerr *anthropic.Error
	if errors.As(err, &aerr) {
		return HealthResult{Kind: classifyStatus(aerr.StatusCode, aerr.RawJSON()), Detail: statusDetail(aerr.StatusCode)}
	}
	var oerr *openai.Error
	if errors.As(err, &oerr) {
		return HealthResult{Kind: classifyStatus(oerr.StatusCode, oerr.Message), Detail: shortDetail(oerr.Message)}
	}
	// No HTTP status reached us: a dial/timeout/DNS failure.
	return HealthResult{Kind: HealthUnreachable, Detail: "could not reach the LLM endpoint"}
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

func statusDetail(code int) string {
	switch {
	case code == 401 || code == 403:
		return "the API key (environment variable) is missing or invalid"
	case code == 404:
		return "the model was not found — check the model name"
	case code >= 500:
		return "the LLM endpoint returned a server error"
	default:
		return ""
	}
}

// shortDetail trims a provider message to a single concise line (it may name the
// model, which is not a secret; it never contains the key).
func shortDetail(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexAny(msg, "\n\r"); i >= 0 {
		msg = msg[:i]
	}
	const max = 160
	if len(msg) > max {
		msg = msg[:max] + "…"
	}
	return msg
}
