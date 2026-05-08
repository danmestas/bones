package hub

// rpcNameFromEventType maps a tasks.EventType-style string (the
// String() output of the closed iota set) to the dotted RPC name
// hub.log uses. Pulled out so the mapping stays one source of truth
// and the hub-side projector (which subscribes to `tasks.events.>`
// and writes hub.log entries) doesn't bake the names inline.
//
// The function is robust against an unrecognized input — returns
// the input prefixed with `tasks.` so an unknown event type still
// produces a parseable hub.log entry rather than dropping the line.
//
// # Why this is the only "middleware" surface
//
// #322's brief assumed the hub binary owned RPC handlers it could
// wrap. Bones doesn't work that way: CLI verbs talk to JetStream
// substrate directly, so the hub never sees a CLI invocation. The
// only thing the hub can genuinely observe is the post-mutation
// event flow on `tasks.events.>`, which the projector subscribes
// to. Read-only CLI verbs (tasks.list, tasks.show, etc.) and hook
// firings (emitted by `bones tasks prime --hook=session-start`, an
// external CLI process) are NOT visible to the hub and therefore
// have no logging surface here.
//
// Earlier drafts of this file carried LogRPC / LogRPCResult /
// LogHook helpers and a readOnlyRPCs allowlist for level demotion.
// Those were unwired (no production callers) and have been removed
// to keep the package honest about what it can observe. A follow-up
// issue will track making hook firings observable via a JetStream
// `hub.hooks.fired` marker that the projector can subscribe to.
func rpcNameFromEventType(eventTypeName string) string {
	switch eventTypeName {
	case "created":
		return "tasks.create"
	case "claimed":
		return "tasks.claim"
	case "unclaimed":
		return "tasks.unclaim"
	case "updated":
		return "tasks.update"
	case "linked":
		return "tasks.link"
	case "slot_changed":
		return "tasks.slot"
	case "closed":
		return "tasks.close"
	default:
		return "tasks." + eventTypeName
	}
}
