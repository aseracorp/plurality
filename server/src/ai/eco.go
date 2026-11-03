// renderMessagesForSummary produces a plain-text dump of the given message
// slice suitable for feeding to the summarisation model, bounded to
// maxSummaryInputChars characters. Walks every content part of every
// message and includes inline text — pasted snippets, attached file
// contents that travelled inline, and tool results. Skips image_url parts
// and parts whose Text is a storage path rather than real content
// (post-ExtractBlobsFromMessage, document parts like pdf/docx/xlsx/pptx
// have part.Text rewritten to a URL path, so we surface only the filename
// in that case).
//
// The budget preserves BOTH ends of the compacted range: the OLDEST
// messages (where the original task, goals and early decisions live) are
// always included first, so a huge compaction can never silently erase the
// very context the summary is meant to carry forward; the REMAINING budget
// is then spent on the NEWEST messages (the recent state the live model
// just lost). This is the fix for the "eco compaction wipes context"
// symptom: the previous newest-only walk meant a large compacted range fed
// the summariser ONLY its tail — the original task and early decisions were
// dropped before the summary model ever saw them, so the resulting
// checkpoint discarded the conversation's original goal. A banner notes
// when the middle was omitted. Never returns a context-busting blob to the
// summary model.
func renderMessagesForSummary(messages []utils.Message) string {
	var b strings.Builder
	// 1) Always keep the OLDEST messages first (the head of the compacted
	//    range: the original task, goals, decisions). The earliest context is
	//    the part the live tail will never re-show, so it is the most
	//    important thing a checkpoint must preserve.
	headBudget := maxSummaryInputChars
	var head []utils.Message
	for _, m := range messages {
		if db.IsCheckpointMessage(m) {
			continue
		}
		if int64(headBudget) <= 0 {
			break
		}
		mBytes := m.Content.TotalContentChars()
		if mBytes > perMsgBudget {
			mBytes = perMsgBudget
		}
		if mBytes > headBudget {
			continue
		}
		headBudget -= mBytes
		head = append(head, m)
	}

	// 2) Spend whatever budget remains on the NEWEST messages (the recent
	//    state the live model just lost), skipping anything already taken by
	//    the head so no message is counted twice.
	tailBudget := headBudget
	var tail []utils.Message
	for i := len(messages) - 1; i >= 0 && int64(tailBudget) > 0; i-- {
		m := messages[i]
		if db.IsCheckpointMessage(m) {
			continue
		}
		if messageInSlice(m, head) {
			continue
		}
		mBytes := m.Content.TotalContentChars()
		if mBytes > perMsgBudget {
			mBytes = perMsgBudget
		}
		if mBytes > tailBudget {
			continue
		}
		tailBudget -= mBytes
		tail = append(tail, m)
	}

	// Combined output, chronological (head then reversed tail).
	keptCount := len(head) + len(tail)
	if keptCount < len(messages) {
		b.WriteString("[NOTE: the middle of the compacted range was omitted to fit the summary input budget — the original task/goals (oldest) and the recent state (newest) are preserved.\n\n")
	}
	for _, m := range head {
		writeSummaryMessage(&b, m)
	}
	for i := len(tail) - 1; i >= 0; i-- {
		writeSummaryMessage(&b, tail[i])
	}
	return b.String()
}

// messageInSlice reports whether m is already among the head messages kept
// from the oldest end (identity match on role/tool_call_id/Content), so the
// newest-end pass never double-counts it.
func messageInSlice(m utils.Message, slice []utils.Message) bool {
	for i := range slice {
		if slice[i].Content == m.Content && slice[i].Role == m.Role && slice[i].ToolCallID == m.ToolCallID {
			return true
		}
	}
	return false
}

// writeSummaryMessage renders a single message into the summary builder in
// the canonical eco-summary shape (USER/ASSISTANT/TOOL_RESULT blocks).
func writeSummaryMessage(b *strings.Builder, m utils.Message) {
	switch m.Role {
	case "user":
		b.WriteString("USER:\n")
		writeContentPartsBudget(b, m.ContentParts())
		b.WriteString("\n")
	case "assistant":
		b.WriteString("ASSISTANT:\n")
		writeContentPartsBudget(b, m.ContentParts())
		for _, tc := range m.ToolCalls {
			b.WriteString("[tool_call ")
			b.WriteString(tc.Function.Name)
			b.WriteString("] args=")
			b.WriteString(tc.Function.Arguments)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	case "tool":
		b.WriteString("TOOL_RESULT ")
		b.WriteString(m.Name)
		b.WriteString(":\n")
		writeContentPartsBudget(b, m.ContentParts())
		b.WriteString("\n")
	}
}