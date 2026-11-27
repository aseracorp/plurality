package ai

// eco_fold_replay_cap.go — the END cap on the fold's prefix replay.
//
// THE OWNER'S RULING (2026-11-27): "Prefix stays, you may cap the replay at the
// END to fit the context window. Best of both worlds: keep caching and make it
// fit the window."
//
// WHY THIS EXISTS. The prefix replay refuses when the prefix does not fit the
// window: prefix + schemas + brief + safety + output floor <= window, yes/no,
// never a squeeze. The fold runs BECAUSE the conversation is large, so on a real
// conversation past the compaction trigger the replay is unbuildable and the
// fold falls to the machine render. Measured on the live deployment: stage 1
// produced 2 LLM answers, stages 2 and 3 produced 0, and every escalation died
// on "the cached prefix does not fit this fold call" (a 262144 window, a 232689
// prefix and 28395 of schemas).
//
// WHY CAPPING THE END KEEPS THE CACHE. KV prompt caching matches the LONGEST
// COMMON PREFIX of the previous request's token stream. So the cap is a
// constraint on where bytes may change, not on how much history ships:
//
//	[ system rows ][ head of the history, byte-identical ][ cut ][ brief ]
//
// Everything at or before the cut is the chat request's own rows, byte for byte
// — so the cached prefix is reused up to the cut, and only the brief is new.
// Capping at the START (keeping the newest rows) would move the head on every
// fold and pay a full re-prefill every time: the exact defect this subsystem was
// built to remove.
//
// WHY THE NEWEST ROWS GO FIRST. The fold compresses the OLDEST rows — the brief
// says "the N oldest messages above are about to be EVICTED". The tail is the
// least needed input for that job and the most expensive to send. Dropping it
// keeps the cache and gives the model the range it is being asked to record.
//
// THE FLOOR. The newest USER row is never dropped: it is the user's ask, the
// anchor every budgeting pass needs, and the same floor the storage layer's fold
// enforces (`lastUserIdx` in db.CompactConversationDurable). If the prefix down
// to that floor cannot fit, this cap stops and the caller's refusal stands: the
// machine-render shape, which carries no prefix, is then the only LLM path.
//
// PAIR SAFETY IS STRUCTURAL, NOT A REPAIR PASS. A cut is taken only at a group
// boundary, where a group is an assistant row carrying tool_calls plus the tool
// rows that answer it, kept together. So the kept rows can never contain an
// orphaned tool result or a dangling tool_call, and no repair function exists to
// be forgotten: see TestFoldReplayEndCapIsPairSafe.

import ()

// replayGroup is one indivisible unit of the replayed history: a row the cap may
// drop on its own, or an assistant row with tool_calls plus every tool row that
// answers its calls.
type replayGroup struct {
	start  int // index into the history slice
	end    int // exclusive
	tokens int
	isUser bool
}

// groupReplayHistory segments the replayed history rows into drop-units.
//
// The chat builder emits an assistant row carrying tool_calls immediately before
// the tool rows that answer it, so matching by tool_call_id inside the run that
// follows is exact and needs no index into the DB. An assistant row whose tool
// rows are NOT contiguous (defensive: a future builder, or a corrupted slice)
// keeps its calls attached to whatever follows, and the run stops at the first
// row that is not one of its answers — the pair is then kept whole by the
// boundary rule, never split.
func groupReplayHistory(rows []StandardMessageReq) []replayGroup {
	groups := make([]replayGroup, 0, len(rows))
	for i := 0; i < len(rows); {
		g := replayGroup{start: i, end: i + 1, isUser: rows[i].Role == "user"}
		if calls := rows[i].ToolCalls; rows[i].Role == "assistant" && len(calls) > 0 {
			want := make(map[string]bool, len(calls))
			for _, tc := range calls {
				want[tc.ID] = true
			}
			j := i + 1
			for j < len(rows) && rows[j].Role == "tool" && want[rows[j].ToolCallID] {
				j++
			}
			g.end = j
		}
		g.tokens = renderedRowsTokens(rows[g.start:g.end])
		groups = append(groups, g)
		i = g.end
	}
	return groups
}

// replayCap is the outcome of capReplayRowsToEnd.
type replayCap struct {
	// historyRows is the number of history rows (excluding system rows) kept.
	keptRows int
	// droppedRows counts the history rows removed from the END.
	droppedRows int
	// keptTokens is the rendered size of the kept rows, system rows included.
	keptTokens int
	// droppedTokens is the rendered size of the rows the cap removed.
	droppedTokens int
	// capped is true when the cap removed anything: the replay ships less history
	// than the chat request, and the fold's log says so.
	capped bool
	// atFloor is true when the cap stopped at the newest user row while still
	// over budget: the prefix is as short as this cap may make it.
	atFloor bool
}

// capReplayRowsToEnd trims the END of the replayed history until the rows that
// remain fit `roomTokens`, dropping whole tool pairs, newest group first, and
// never crossing the newest user row.
//
// `headRows` rows of system content stay untouched and are counted in the room:
// they are the head of the prompt, and moving them is a new cache entry.
func capReplayRowsToEnd(rows []StandardMessageReq, headRows int, roomTokens int) replayCap {
	history := rows[headRows:]
	groups := groupReplayHistory(history)

	// Floor boundary: the group holding the newest user row. Everything from the
	// start of the history up to and including that group is kept, whatever the
	// budget says.
	floorGroups := len(groups)
	for i := len(groups) - 1; i >= 0; i-- {
		if groups[i].isUser {
			floorGroups = i + 1
			break
		}
	}

	total := 0
	for _, g := range groups {
		total += g.tokens
	}
	headTokens := renderedRowsTokens(rows[:headRows])

	c := replayCap{keptRows: len(history), keptTokens: headTokens + total}
	if total <= roomTokens {
		return c
	}

	// Drop newest groups first, down to (but never past) the floor.
	keep := len(groups)
	remaining := total
	for keep > floorGroups && remaining > roomTokens {
		keep--
		remaining -= groups[keep].tokens
	}
	c.keptRows = 0
	if keep > 0 {
		c.keptRows = groups[keep-1].end
	}
	c.keptTokens = headTokens + remaining
	c.droppedRows = len(history) - c.keptRows
	c.droppedTokens = total - remaining
	c.capped = c.droppedRows > 0
	c.atFloor = remaining > roomTokens
	return c
}

// replayRowsAtSafeBoundary walks a requested row count (counting history rows,
// after `headRows` system rows) back to the largest group boundary at or below
// it. Exported for tests, and the reason the cap has no repair pass: a cut here
// can never split an assistant/tool pair.
func replayRowsAtSafeBoundary(rows []StandardMessageReq, headRows, wantHistoryRows int) int {
	groups := groupReplayHistory(rows[headRows:])
	last := 0
	for _, g := range groups {
		if g.end > wantHistoryRows {
			break
		}
		last = g.end
	}
	return last
}

// hasOrphanedToolRows reports whether a request's messages contain a tool result
// with no owning tool_call, or an assistant tool_call with no answer. The fold's
// cap is tested with it; it is a probe, NOT a repair — nothing in the request
// path calls it, and no repairConversationToolCallPairs exists.
func hasOrphanedToolRows(rows []StandardMessageReq) (danglingCalls int, orphans int) {
	pending := make(map[string]bool)
	for _, r := range rows {
		for _, tc := range r.ToolCalls {
			pending[tc.ID] = true
		}
		if r.Role == "tool" && r.ToolCallID != "" {
			if !pending[r.ToolCallID] {
				orphans++
			}
			delete(pending, r.ToolCallID)
		}
	}
	for _, v := range pending {
		if v {
			danglingCalls++
		}
	}
	return danglingCalls, orphans
}
