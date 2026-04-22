package dispatch

import "strings"

type ResultKind string

const (
	ResultSuccess ResultKind = "success"
	ResultFork    ResultKind = "fork"
	ResultFail    ResultKind = "fail"
)

type ResultMessage struct {
	Kind    ResultKind
	Summary string
	Branch  string
	Rev     string
}

func FormatResult(msg ResultMessage) string {
	parts := []string{"dispatch-result", string(msg.Kind), msg.Summary}
	if msg.Branch != "" {
		parts = append(parts, msg.Branch)
	}
	if msg.Rev != "" {
		parts = append(parts, msg.Rev)
	}
	return strings.Join(parts, "|")
}

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
	switch msg.Kind {
	case ResultSuccess, ResultFork, ResultFail:
		return msg, true
	default:
		return ResultMessage{}, false
	}
}
