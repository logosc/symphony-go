package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// codexExecFrame mirrors the JSON shape emitted by `codex exec --json`.
// Two text-bearing layouts coexist for `item.completed`:
//
//	flat:    {"type":"item.completed","item_type":"agent_message","text":"..."}
//	nested:  {"type":"item.completed","item":{"type":"agent_message","text":"..."}}
//
// The parser reads both. Decoding both shapes onto one struct keeps
// the parser's hot path free of per-frame branch decisions; flatness
// preference (top-level wins when both are present) lives in
// classifyCodexExec below.
type codexExecFrame struct {
	Type     string `json:"type"`
	ItemType string `json:"item_type"`
	Text     string `json:"text"`
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

// ParseCodexExec decodes a `codex exec --json` event stream from body
// (newline-delimited JSON, one frame per line) into a ParseSummary.
//
// Text-extraction policy (matches the existing inline decoder in
// codex.go for behavior parity, ahead of the runner migration in a
// follow-up PR):
//
//  1. If any item.completed/agent_message events appeared with
//     non-empty Text, FinalText is the concatenation of those texts.
//  2. Otherwise, fall back to the concatenation of any non-empty
//     `text` fields seen in unclassified frames.
//  3. Otherwise (no JSON parsed at all), FinalText is the raw body.
//     This is the "agent emitted plain text on stdout" escape hatch.
//
// Malformed JSON lines are counted in Stats.MalformedFrames and
// otherwise ignored (mirroring runtime behavior — we don't want one
// stray non-JSON line from a CLI version mismatch to fail an
// otherwise-good plan).
func ParseCodexExec(body []byte) ParseSummary {
	var (
		out       ParseSummary
		textParts []string
		fallback  []string
		anyJSON   bool
	)

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		out.Stats.FramesTotal++

		var f codexExecFrame
		if err := json.Unmarshal(line, &f); err != nil {
			out.Stats.MalformedFrames++
			continue
		}
		anyJSON = true

		ev := classifyCodexExec(f)
		out.Events = append(out.Events, ev)

		switch ev.Kind {
		case AgentKindTerminal:
			out.Stats.TerminalCount++
			out.Stats.LastTerminal = ev.Terminal
		case AgentKindAssistantMessage:
			if ev.Text != "" {
				textParts = append(textParts, ev.Text)
				out.Stats.AssistantMessages++
			}
		case AgentKindToolEvent, AgentKindUnknown:
			if ev.Text != "" {
				fallback = append(fallback, ev.Text)
			}
			if ev.Kind == AgentKindUnknown {
				out.Stats.UnknownFrames++
			}
		}
	}

	switch {
	case len(textParts) > 0:
		out.FinalText = strings.Join(textParts, "")
	case len(fallback) > 0:
		out.FinalText = strings.Join(fallback, "")
	case !anyJSON:
		out.FinalText = string(body)
	}

	return out
}

// classifyCodexExec maps one decoded codex frame to an AgentEvent.
// Flat fields take precedence over nested fields when both populate
// the same slot (back-compat: older codex CLIs produced only the flat
// shape).
func classifyCodexExec(f codexExecFrame) AgentEvent {
	ev := AgentEvent{RawType: f.Type}

	switch f.Type {
	case "turn.completed":
		ev.Kind = AgentKindTerminal
		ev.Terminal = "completed"
		return ev
	case "turn.failed":
		ev.Kind = AgentKindTerminal
		ev.Terminal = "failed"
		return ev
	case "error":
		ev.Kind = AgentKindTerminal
		ev.Terminal = "failed"
		return ev
	case "item.completed":
		itemType := f.ItemType
		if itemType == "" {
			itemType = f.Item.Type
		}
		text := f.Text
		if text == "" {
			text = f.Item.Text
		}
		if itemType == "agent_message" && text != "" {
			ev.Kind = AgentKindAssistantMessage
			ev.Text = text
			return ev
		}
		ev.Kind = AgentKindToolEvent
		ev.Text = text
		return ev
	default:
		ev.Kind = AgentKindUnknown
		ev.Text = f.Text
		return ev
	}
}
