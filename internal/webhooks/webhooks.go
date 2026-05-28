// Package webhooks delivers best-effort, asynchronous HTTP notifications when
// Shoka writes occur. Delivery never blocks or fails the originating operation.
package webhooks

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Config describes one webhook subscription.
type Config struct {
	Name   string
	URL    string
	Events []string
	Secret string
}

// Event is the JSON payload POSTed to subscribed webhooks.
type Event struct {
	Event      string    `json:"event"`
	Namespace  string    `json:"namespace"`
	Project    string    `json:"project"`
	Path       string    `json:"path,omitempty"`
	CommitHash string    `json:"commit_hash,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

type hook struct {
	name   string
	url    string
	events map[string]bool
	secret string
}

// Notifier fans out events to configured webhooks.
type Notifier struct {
	hooks  []hook
	client *http.Client
	wg     sync.WaitGroup
	logger *slog.Logger
}

// New builds a Notifier from the given subscriptions.
func New(configs []Config) *Notifier {
	hooks := make([]hook, 0, len(configs))
	for _, c := range configs {
		ev := make(map[string]bool, len(c.Events))
		for _, e := range c.Events {
			ev[e] = true
		}
		hooks = append(hooks, hook{name: c.Name, url: c.URL, events: ev, secret: c.Secret})
	}
	return &Notifier{hooks: hooks, client: &http.Client{Timeout: 10 * time.Second}}
}

// SetLogger attaches a structured logger. A nil logger (default) discards output.
func (n *Notifier) SetLogger(l *slog.Logger) { n.logger = l }

func (n *Notifier) log() *slog.Logger {
	if n.logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return n.logger
}

// Emit asynchronously delivers ev to every hook subscribed to ev.Event. It
// returns immediately; delivery failures are logged, never propagated.
func (n *Notifier) Emit(ev Event) {
	if len(n.hooks) == 0 {
		return
	}
	body, err := json.Marshal(ev)
	if err != nil {
		n.log().Error("webhook: failed to marshal event", "error", err)
		return
	}
	for _, h := range n.hooks {
		if !h.events[ev.Event] {
			continue
		}
		n.wg.Add(1)
		go func(h hook) {
			defer n.wg.Done()
			n.deliver(h, body)
		}(h)
	}
}

// Wait blocks until all in-flight deliveries finish (for tests and graceful
// shutdown).
func (n *Notifier) Wait() {
	n.wg.Wait()
}

func (n *Notifier) deliver(h hook, body []byte) {
	const attempts = 2
	backoff := 200 * time.Millisecond
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequest(http.MethodPost, h.url, bytes.NewReader(body))
		if err != nil {
			n.log().Error("webhook build request failed", "webhook", h.name, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if h.secret != "" {
			mac := hmac.New(sha256.New, []byte(h.secret))
			mac.Write(body)
			req.Header.Set("X-Shoka-Signature", hex.EncodeToString(mac.Sum(nil)))
		}

		resp, err := n.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 400 {
				n.log().Info("webhook delivered", "webhook", h.name, "status", resp.StatusCode, "attempt", attempt)
				return
			}
			n.log().Warn("webhook delivery failed", "webhook", h.name, "status", resp.StatusCode, "attempt", attempt, "max_attempts", attempts)
		} else {
			n.log().Warn("webhook delivery error", "webhook", h.name, "error", err, "attempt", attempt, "max_attempts", attempts)
		}

		if attempt < attempts {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
}
