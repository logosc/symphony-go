package runner

// This file is the foundation for proposal 0005 §4.4 — centralized
// event classification for agent runners. It introduces:
//
//  - AgentEvent: a provider-agnostic decoded event (kind, text,
//    terminal status, raw type/method for diagnostics).
//  - ProtocolStats: counters per run that downstream callers (doctor,
//    diagnostics artifact, dashboards) can consume without scanning
//    raw event bytes.
//
// Provider-specific parsers live alongside this file (one per
// transport): see `parser_codex_exec.go`. Subsequent PRs will add
// `parser_codex_appserver.go` and `parser_claude.go` and migrate the
// existing runners to use these parsers instead of inline decoding.
// Until then, this layer is exercised purely by parser-only tests
// driven by fixtures under `internal/runner/testdata/`.
//
// Centralizing event decoding makes shape drift cheap to cover: when
// a provider adds a wrapper field (as codex did with `item.{type,text}`
// nested under `item.completed` — see commit 6456809), the fix lands
// in one place, with one fixture, regardless of how many runners
// share the parser.

// AgentKind classifies a decoded agent event.
type AgentKind string

const (
	// AgentKindAssistantMessage is text the agent intends to surface
	// to the operator (plan body, PR body, code-reviewer rationale).
	AgentKindAssistantMessage AgentKind = "agent_message"
	// AgentKindTerminal marks the end of a turn. The Terminal field
	// distinguishes completion from failure/interruption.
	AgentKindTerminal AgentKind = "terminal"
	// AgentKindToolEvent is any tool-call activity (not surfaced as
	// final text but useful for diagnostics + protocol stats).
	AgentKindToolEvent AgentKind = "tool_event"
	// AgentKindUnknown is a frame whose shape we recognize as JSON
	// but whose semantics we don't classify. Counted but not actioned.
	AgentKindUnknown AgentKind = "unknown"
)

// AgentEvent is one decoded frame from a provider's event stream.
// Fields not relevant to a given Kind are zero-valued. Provider-
// specific parsers populate this and feed a slice into downstream
// classification logic.
type AgentEvent struct {
	// Kind is the high-level classification.
	Kind AgentKind
	// Text is the user-visible content for AgentKindAssistantMessage.
	// Empty for other kinds.
	Text string
	// Terminal is set when Kind == AgentKindTerminal. One of:
	// "completed", "failed", "interrupted".
	Terminal string
	// RawType is the original protocol type/event name (e.g.
	// "item.completed", "turn.failed", "assistant"). Useful in slog
	// when ProtocolStats counters don't carry enough info.
	RawType string
	// RawMethod is the JSON-RPC method name when the frame came from
	// a request/notification (codex app-server). Empty for other
	// transports.
	RawMethod string
}

// ProtocolStats are counters collected over one runner invocation.
// Goals:
//   - Make event-shape drift cheap to detect (a non-zero
//     MalformedFrames or UnknownFrames count is a smoke signal).
//   - Let `symphony-go doctor` and the per-run diagnostics artifact
//     summarize protocol behavior without re-scanning raw events.
//   - Distinguish "agent did real work" (AssistantMessages > 0,
//     terminal == completed) from "agent crashed silently" (no
//     terminal, no assistant) without inspecting Text.
type ProtocolStats struct {
	// FramesTotal counts every non-empty line/frame the runner saw,
	// including frames that failed JSON unmarshaling.
	FramesTotal int
	// MalformedFrames is the count of non-empty lines/frames that
	// failed JSON unmarshaling. > 0 indicates protocol drift or a
	// truncated stream.
	MalformedFrames int
	// AssistantMessages is the count of agent_message events with
	// non-empty text. The orchestrator's plan body / PR body comes
	// from these.
	AssistantMessages int
	// TerminalCount is the number of terminal frames seen. Should be
	// exactly 1 on a healthy run; 0 means the subprocess died before
	// declaring done; >1 is a protocol bug.
	TerminalCount int
	// UnknownFrames counts frames we parsed as JSON but couldn't
	// classify. Useful for catching schema drift before it bites a
	// production run.
	UnknownFrames int
	// LastTerminal records the value of the last AgentKindTerminal
	// event's Terminal field. "completed" on success, "failed" or
	// "interrupted" otherwise. Empty when no terminal was observed.
	LastTerminal string
}

// ParseSummary aggregates the products of one parser pass: the
// classified event slice, accumulated counters, and the final
// user-visible text picked by the parser's text-extraction policy.
type ParseSummary struct {
	Events    []AgentEvent
	Stats     ProtocolStats
	FinalText string
}
