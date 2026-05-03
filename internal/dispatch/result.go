package dispatch

import "strings"

type ResultKind string

const (
	ResultSuccess ResultKind = "success"
	ResultFork    ResultKind = "fork"
	ResultFail    ResultKind = "fail"
)

// ResultMessage is the dispatch payload posted on the task thread when
// a swarm slot closes. Kind + Summary are agent intent; Branch + Rev
// describe a fork outcome. SubstrateError and SubstrateFault are
// substrate-level signals — populated when bones (not the agent)
// observed a failure on the close path, so a downstream orchestrator
// can distinguish "agent didn't accomplish X" from "the substrate
// blocked the agent from accomplishing X" (#159).
//
// SubstrateError is a free-text message (e.g. "swarm commit failed:
// nats: no responders"). SubstrateFault is a short category tag
// (e.g. "commit-failed", "hub-unreachable") for orchestrator-side
// branching without parsing free text.
type ResultMessage struct {
	Kind           ResultKind
	Summary        string
	Branch         string
	Rev            string
	SubstrateError string
	SubstrateFault string
}

// FormatResult serializes msg as a pipe-delimited dispatch payload.
// The wire format is positional — index 0 is the literal
// "dispatch-result" tag, 1 is kind, 2 is summary, 3 is branch, 4 is
// rev, 5 is substrate_error, 6 is substrate_fault. To preserve
// position when an early field is empty but a later one is set,
// empty-string placeholders fill the gaps. Trailing empty fields
// are elided so format output for messages without substrate data
// is unchanged from the pre-#159 schema.
func FormatResult(msg ResultMessage) string {
	parts := []string{"dispatch-result", string(msg.Kind), msg.Summary}

	optional := []string{
		msg.Branch,
		msg.Rev,
		msg.SubstrateError,
		msg.SubstrateFault,
	}
	highest := -1
	for i, v := range optional {
		if v != "" {
			highest = i
		}
	}
	for i := 0; i <= highest; i++ {
		parts = append(parts, optional[i])
	}
	return strings.Join(parts, "|")
}

// ParseResult deserializes a pipe-delimited dispatch payload. Missing
// trailing fields are treated as empty strings so consumers reading
// the older 3/4/5-field format continue to work. Unknown Kind values
// are rejected.
func ParseResult(body string) (ResultMessage, bool) {
	parts := strings.Split(body, "|")
	if len(parts) < 3 || parts[0] != "dispatch-result" {
		return ResultMessage{}, false
	}
	msg := ResultMessage{
		Kind:    ResultKind(parts[1]),
		Summary: parts[2],
	}
	if len(parts) > 3 {
		msg.Branch = parts[3]
	}
	if len(parts) > 4 {
		msg.Rev = parts[4]
	}
	if len(parts) > 5 {
		msg.SubstrateError = parts[5]
	}
	if len(parts) > 6 {
		msg.SubstrateFault = parts[6]
	}
	switch msg.Kind {
	case ResultSuccess, ResultFork, ResultFail:
		return msg, true
	default:
		return ResultMessage{}, false
	}
}
