// B-56/B-59 verbatim HTTP dump. When the operator enables server.debug.dump_http, the
// outermost reqtrace Middleware emits EVERY request and EVERY response on the three
// listeners VERBATIM — method, path, all headers, full body, status — correlated by
// the B-53 request_id, as a guaranteed PAIR per request_id with NO exception (boring,
// secret-bearing, unparseable, error, SSE — all of it).
//
// B-59 REMOVED the secret redaction entirely. This is a deliberate LOCAL debug switch,
// default OFF, on an unshipped product: the redaction (masking token/code/verifier/
// consent values and substituting an Authorization fingerprint) is exactly what kept
// defeating "dump everything" — it dropped the /token request dump, the single most
// important request in the OAuth debug. When the switch is ON the dump is now RAW and
// COMPLETE: tokens, authorization codes, code_verifier/code_challenge, the consent
// credential, and the Authorization header value are dumped verbatim like every other
// byte. The operator owns enabling the switch and the resulting log. (Default OFF is
// unchanged: normal operation emits no dump.)
package reqtrace

import (
	"bytes"
	"net/http"
	"sort"
	"strings"
)

// headersVerbatim renders ALL headers, one canonical "Key: value" per line (sorted for
// stable output), with NO redaction — the Authorization header value included, like
// every other header. B-59: no key is special-cased.
func headersVerbatim(h http.Header) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		for _, v := range h[k] {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// bodyForDump returns the captured body bytes as a string, verbatim (B-59: no redaction).
func bodyForDump(buf *bytes.Buffer) string {
	if buf == nil {
		return ""
	}
	return buf.String()
}
