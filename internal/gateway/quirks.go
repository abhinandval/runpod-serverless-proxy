package gateway

import "encoding/json"

// demoteSystemMessages rewrites Anthropic messages whose role is "system" into
// user messages wrapped in <system-reminder> tags.
//
// Claude Code puts such messages in the middle of the conversation, next to
// the top-level `system` field. llama.cpp hands them to the model's chat
// template as they are, and templates such as Qwen's refuse a system message
// that is not first ("System message must be at the beginning"). They are kept
// in place rather than moved into `system`, because changing the start of the
// prompt on every turn would defeat the engine's prompt cache.
func demoteSystemMessages(fields map[string]json.RawMessage) {
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(fields["messages"], &msgs) != nil {
		return
	}
	changed := false
	for _, m := range msgs {
		var role string
		if json.Unmarshal(m["role"], &role) != nil || role != "system" {
			continue
		}
		changed = true
		m["role"] = json.RawMessage(`"user"`)

		var text string
		if json.Unmarshal(m["content"], &text) == nil {
			m["content"], _ = json.Marshal(wrapReminder(text))
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(m["content"], &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			var kind, t string
			if json.Unmarshal(b["type"], &kind) == nil && kind == "text" && json.Unmarshal(b["text"], &t) == nil {
				b["text"], _ = json.Marshal(wrapReminder(t))
			}
			delete(b, "cache_control") // meaningless to the engine on a rewritten block
		}
		m["content"], _ = json.Marshal(blocks)
	}
	if changed {
		fields["messages"], _ = json.Marshal(msgs)
	}
}

func wrapReminder(s string) string {
	return "<system-reminder>\n" + s + "\n</system-reminder>"
}
