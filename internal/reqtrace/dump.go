// B-56 verbatim HTTP dump. When the operator enables server.debug.dump_http, the
// outermost reqtrace Middleware emits EVERY request and EVERY response on the three
// listeners VERBATIM — method, path, all headers, full body, status — correlated by
// the B-53 request_id. This ends the "which field do I log next" loop (B-51→B-55):
// no selected subset can miss the cause because nothing is selected. The ONLY
// processing applied is a fixed, minimal secret-redaction list; everything else is
// emitted unprocessed.
//
// Redacted (and ONLY these): token values (access/refresh/bearer), authorization
// codes, code_verifier/code_challenge values, the consent credential, and the
// Authorization header VALUE (replaced by presence + the B-54 tokenfp fingerprint).
// The redactions are byte-preserving: only the secret value substring is replaced by
// the marker, so every other byte of the body/headers is identical to the wire.
package reqtrace

import (
	"bytes"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/shoka/mcp-server/internal/tokenfp"
)

// redactedMarker replaces a redacted secret value in the dump. It is deliberately
// distinctive so a test can assert a secret was masked (marker present) and the raw
// value absent.
const redactedMarker = "«redacted»"

// secretBodyKeys are the form-field / JSON keys whose VALUES are secrets per the
// directive's fixed list. The dump masks only these values; every other field is
// verbatim. Authorization is handled separately (header → presence + fingerprint).
var secretBodyKeys = []string{
	"access_token", "refresh_token", "token",
	"code", "code_verifier", "code_challenge",
	"consent_credential",
}

// formValueRe / jsonValueRe per key mask the VALUE only, leaving the key and all
// surrounding bytes intact. The `=`/closing-quote delimiters keep `code` from
// matching `code_verifier` or `code_challenge`.
var (
	formValueRe = map[string]*regexp.Regexp{}
	jsonValueRe = map[string]*regexp.Regexp{}
)

func init() {
	for _, k := range secretBodyKeys {
		qk := regexp.QuoteMeta(k)
		// form / query: (start|&) key = value(up to & or end)
		formValueRe[k] = regexp.MustCompile(`(^|&)(` + qk + `=)([^&]*)`)
		// JSON: "key" : "value"
		jsonValueRe[k] = regexp.MustCompile(`("` + qk + `"\s*:\s*")[^"]*(")`)
	}
}

// redactBody masks the secret VALUES in body, applying BOTH the form-encoded and the
// JSON passes. A form body carries no `"key":` shape and a JSON body carries no
// `&key=` shape, so applying both is safe and avoids brittle content-type sniffing.
// Non-secret bytes are returned unchanged (verbatim).
func redactBody(body []byte) []byte {
	out := body
	for _, k := range secretBodyKeys {
		out = formValueRe[k].ReplaceAll(out, []byte(`${1}${2}`+redactedMarker))
		out = jsonValueRe[k].ReplaceAll(out, []byte(`${1}`+redactedMarker+`${2}`))
	}
	return out
}

// redactURL returns the request target (path + query) with the `token` query
// parameter VALUE masked — the WebSocket bearer fallback (auth.go tokenFromRequest)
// is a token value and must not appear in the dump. All other query params and the
// path are verbatim.
func redactURL(u *url.URL) string {
	target := u.RequestURI()
	if u.RawQuery == "" {
		return target
	}
	masked := formValueRe["token"].ReplaceAll([]byte(u.RawQuery), []byte(`${1}${2}`+redactedMarker))
	return u.EscapedPath() + "?" + string(masked)
}

// redactHeaders renders all headers verbatim, one canonical "Key: value" per line
// (sorted for stable output), EXCEPT the Authorization header, whose value is never
// emitted: it becomes "present; fingerprint=<tokenfp>" (or "present" for a
// non-Bearer scheme), correlating the same token across the dump and the B-54
// auth-stage line without exposing the value.
func redactHeaders(h http.Header) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if strings.EqualFold(k, "Authorization") {
			b.WriteString("Authorization: ")
			b.WriteString(redactAuthorization(h.Get(k)))
			b.WriteByte('\n')
			continue
		}
		for _, v := range h[k] {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// redactAuthorization turns an Authorization header value into a presence +
// fingerprint marker, never the value. A "Bearer <token>" value is fingerprinted on
// the post-"Bearer " token (matching internal/auth's bearerToken/tokenfp use) so the
// dump correlates with the auth-stage fingerprint.
func redactAuthorization(value string) string {
	if value == "" {
		return ""
	}
	if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
		return redactedMarker + " (present; fingerprint=" + tokenfp.Fingerprint(value[7:]) + ")"
	}
	return redactedMarker + " (present; fingerprint=" + tokenfp.Fingerprint(value) + ")"
}

// isEventStream reports whether the response is the long-lived MCP SSE stream, whose
// body must NOT be buffered (it would alter flush timing / grow unbounded). For those
// the dump records headers + status and notes the capture limit instead of the body.
func isEventStream(h http.Header) bool {
	ct := h.Get("Content-Type")
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/event-stream")
}

// bodyForDump returns the (redacted) captured body bytes as a string.
func bodyForDump(buf *bytes.Buffer) string {
	if buf == nil {
		return ""
	}
	return string(redactBody(buf.Bytes()))
}
