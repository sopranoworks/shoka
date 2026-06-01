package notify

import "context"

// Sender identity flows from a write entry point to the dispatch decision on the
// same context.Context that already carries the identity declarations
// (internal/identity's WithAgent/WithUser). It is an independent context key, so
// it composes cleanly with those: a write may declare both who authored it and
// which connection originated it.
//
// The storage write paths (Write/Delete/CreateProjectCtx) read the sender with
// SenderFrom and pass it to Center.NotifyFrom, so the originating subscriber is
// excluded from the resulting event. A write whose context carries no sender
// (the legacy non-ctx wrappers, background reconciliation) yields "" and
// dispatches to everyone — unchanged behaviour.
type senderCtxKey struct{}

// WithSender attaches a sender identifier to ctx. For a /ws/ui write the
// identifier is the originating connection's id; for an MCP write it is a
// session-derived id that cannot collide with a /ws/ui connection (so the write
// reaches every /ws/ui subscriber). Pass "" — or simply omit the call — to
// dispatch to all subscribers.
func WithSender(ctx context.Context, sender string) context.Context {
	return context.WithValue(ctx, senderCtxKey{}, sender)
}

// SenderFrom returns the sender identifier carried on ctx, or "" if none. "" is
// the unidentified sender: NotifyFrom dispatches it to every subscriber.
func SenderFrom(ctx context.Context) string {
	if s, ok := ctx.Value(senderCtxKey{}).(string); ok {
		return s
	}
	return ""
}
