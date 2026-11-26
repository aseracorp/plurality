package ai

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/docsupport"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

const kb = 1024

// attachmentInlineCapBytes bounds how much content ONE attachment part ships IN
// PLACE at its own row. Above it the row is a fetch placeholder from its very
// first turn.
//
// In-place content is prompt prefix: paid once, then cached. That is the whole
// point of the in-place design — the trailing row cost ~10K tokens EVERY turn
// (deployed: uncached 10693-12361 vs 149-1715 added by the turn), and the same
// bytes in place cost them once.
//
// The cap exists only for the pathological part, and it is deliberately
// GENEROUS. It does not try to bound the conversation: the bound on the total
// is compaction, which is the mechanism this design defers ageing to. A
// per-conversation byte budget in the renderer would need a term over the whole
// history, and any term over the whole history moves as the history grows — the
// age-window defect in a different shape.
//
// 100 KB is the floor of the pathological case. Under it, content is what the
// model was just handed and must be able to READ: #217 pinned exactly this
// (a single 40 KB report evicted by an aggregate budget left "a placeholder for
// the file it was just given"), and 40 KB of file is a normal prompt. A part
// over 100 KB is a bulk dump, and bulk is what retrieval tools exist for.
const attachmentInlineCapBytes = 100 * kb

// attachmentTailHeader opens the synthetic trailing row. Exported to tests
// through the same constant on purpose: the tests that assert the row exists
// (and the helper that strips it before comparing history) must key on the
// production string, not a copy that goes stale the moment the wording changes.
const attachmentTailHeader = "[Images attached for this turn"

// attachmentDelivery is the delivery rule for ONE attachment part: what its
// row ships in place, and whether its real content is fresh enough to ride in
// the trailing attachment row.
//
// The in-place decision (inline / placeholder) is a PURE function of stored
// state — part type, the message's role+name, the attachment's size, and the
// model's capabilities. It deliberately has NO "user messages since" term:
// that term grows every turn, so a rule that reads it flips a row from real
// content to placeholder in the MIDDLE of an already-sent prefix, and every
// byte from that row on diverges from the previous request. KV prompt caching
// matches the longest common PREFIX, so one mid-history flip discards the
// whole conversation's cached KV. This is the same defect class as the
// overflow-fold churn fixed in the fold-hysteresis commit: history rendering
// must be monotone, and the only safe place for per-turn change is the TAIL.
//
// The one age term that remains is showWindow, and it gates ONLY the trailing
// row (append-only tail bytes, expected to change) — never an in-place row.
type attachmentDelivery struct {
	// inline: ship the stored part verbatim in its own row.
	inline bool
	// placeholder: the in-row text that replaces the part. Empty means
	// "drop the part entirely" (images for a non-action model, which has no
	// tool with which to retrieve them).
	placeholder string
	// showWindow: for an IMAGE only — how many user messages may follow the
	// attachment's row before its pixels drop out of the trailing row. Text
	// attachments have no window at all (see attachmentDeliveryFor). 0 = never
	// shown.
	showWindow int
}

// attachmentPlaceholder is the stable in-row stand-in for an omitted
// attachment. It is a function of stored metadata only (ID, ext, size), so it
// renders identically on the first request that sees the attachment and on
// every later one.
func attachmentPlaceholder(att utils.AttachmentMeta) string {
	desc := att.Type
	if att.Ext != "" {
		desc = att.Ext
	}
	if docsupport.IsDocumentType(att.Type) {
		return fmt.Sprintf(
			"[Document omitted: %s (%s, %d bytes). Use the \"read_document\" function to extract and read its text content.]",
			att.ID, desc, att.Size,
		)
	}
	return fmt.Sprintf(
		"[Attachment omitted: %s (%s, %d bytes). Use the \"conversation_attachments\" function to retrieve it.]",
		att.ID, desc, att.Size,
	)
}

// attachmentDeliveryFor is the whole delivery decision for one attachment
// part, as a pure function of stored state.
func attachmentDeliveryFor(part utils.ContentPart, att utils.AttachmentMeta, isAction bool) attachmentDelivery {
	if part.Type == "image_url" {
		// Images are visible while they are FRESH: on the turn that uploaded
		// them (user role), on the turn that produced them (assistant rows and
		// tool results, including conversation_attachments recalls). The
		// window is one user message, and the bytes ride in the trailing row.
		d := attachmentDelivery{showWindow: 1}
		if !isAction {
			// No tools → no way to retrieve a stripped image later, and a
			// non-vision model cannot read it at all. Strip it; the trailing
			// row keeps it for the fresh turn (and
			// stripImagesForNonVisionModel removes it again if the selected
			// model truly cannot see images).
			return d
		}
		d.placeholder = attachmentPlaceholder(att)
		return d
	}

	// Text-based attachment. Documents are stored as a PATH, not as content
	// (the bytes live in the blob store), so there is nothing to append: the
	// read_document placeholder is the contract, showWindow stays 0.
	if docsupport.IsDocumentType(att.Type) {
		d := attachmentDelivery{showWindow: 0}
		if isAction {
			d.placeholder = attachmentPlaceholder(att)
		} else {
			d.inline = true // non-action model: keep the snippet/file inline
		}
		return d
	}

	// Text-based, non-document: the content ships IN PLACE, in the row that
	// produced it, and stays there until durable compaction folds that row.
	//
	// There is deliberately no freshness window here. A window is a term that
	// GROWS with the conversation, so it flips a row from real content to
	// placeholder in the MIDDLE of an already-sent prefix, and KV prompt
	// caching — which matches the longest common prefix — discards the whole
	// conversation's cached KV at that byte. Upstream (azukaar/Plurality)
	// ships content in place with exactly such a window (>50KB→1, >25KB→2,
	// else 3 user messages); this fork added a trailing attachment row to stop
	// the churn it caused, and paid for it with a ~10K-token re-prefill on
	// every turn of an agentic loop, because a row appended at the END of the
	// slice sits behind the append point. In-place + no age term is the fix:
	// the in-place view is a pure function of stored state, so the prefix only
	// ever extends, and the tail carries only what the newest turn adds.
	//
	// The size cap replaces the window: size is immutable, so the cap is a
	// stable in-place rule. Content above it becomes a retrieval the first
	// time it is seen, never mid-history.
	if !isAction {
		// No function calling → no tool to retrieve with. Ship it inline
		// forever, whatever the size.
		return attachmentDelivery{inline: true}
	}
	if att.Size <= attachmentInlineCapBytes {
		return attachmentDelivery{inline: true}
	}
	return attachmentDelivery{placeholder: attachmentPlaceholder(att)}
}

// appendTrailingAttachment copies an attachment's REAL content into the
// trailing row, labelled with its ID so the model can tie the bytes back to
// the placeholder in the history and re-fetch them later via
// conversation_attachments.
func appendTrailingAttachment(out []utils.ContentPart, part utils.ContentPart, att utils.AttachmentMeta, content string) []utils.ContentPart {
	desc := att.Ext
	if desc == "" {
		desc = att.Type
	}
	if att.Filename != "" {
		desc = att.Filename
	}
	if part.Type == "image_url" {
		out = append(out, utils.ContentPart{
			Type: "text",
			Text: fmt.Sprintf("[Attached image: %s (%s, %d bytes)]", att.ID, desc, att.Size),
		})
		if content == "" && part.ImageURL != nil {
			content = part.ImageURL.URL
		}
		// Internal URLs are re-inflated to data URIs by reInflateImageURLs on
		// the way out (see the bottom of PrepareMessagesForAI).
		out = append(out, utils.ContentPart{
			Type:     "image_url",
			ImageURL: &utils.ContentImageURL{URL: content},
		})
		return out
	}
	out = append(out, utils.ContentPart{
		Type: "text",
		Text: fmt.Sprintf("[Attached file: %s (%s, %d bytes)]:\n%s", att.ID, desc, att.Size, content),
	})
	return out
}

// PrepareMessagesForAI processes conversation messages before sending them to
// an AI provider. TEXT attachment content ships IN PLACE, in the row that
// stored it, and stays there for the life of the row: the row is real content,
// not a pointer to content that lives somewhere else in the request.
//
// Only content that cannot live in its own row moves: images (providers accept
// image parts in user rows, not tool rows) go to the synthetic trailing row
// while they are fresh.
//
// Why in place, and why there is no age term: KV prompt caching matches the
// longest common PREFIX, so the prefix must be byte-monotone. Upstream
// (azukaar/Plurality) ships content in place with a staleness window measured
// in user messages since the row; that term GROWS every turn, so it flips a
// history row from content to placeholder in the middle of an already-sent
// prefix and discards the whole conversation's cached KV. This fork answered
// that by relocating the content into a trailing row — which fixed the churn
// and created a worse one: a row appended at the END of the array sits behind
// the append point, so every turn re-prefills it. Deployed numbers: 149-1715
// tokens added per turn against an uncached cost pinned at 10693-12361, i.e.
// the row itself (32521 bytes / 3.0 = ~10.8K tokens) was re-sent every turn.
//
// Dropping the age term is what makes in-place safe. Size decides in-place
// delivery and size is immutable, so a row renders identically the turn it is
// written, the next turn, and a hundred turns later. The prefix only extends.
// Ageing out is compaction's job: durable folding removes the row from the
// LLM-facing view and the summary carries the attachment's ID, so the model can
// fetch the content back on demand — the placeholder's promise stays honoured
// because attachment IDs are stable (see utils.attachmentID) and the retrieval
// tools read the FULL history, folded rows included.
//
// Non-action models (no function calling) get no placeholder — they have no
// tool with which to retrieve anything: images are stripped, text-based
// attachments stay inline at any size.
//
// Returns the processed messages, whether the conversation has any attachments,
// and whether any of them are document attachments (PDF, etc.).
func PrepareMessagesForAI(messages []utils.Message, model utils.Model) ([]utils.Message, bool, bool) {
	index := utils.BuildAttachmentIndex(messages, storage.FileSizeFromURL)

	if len(index.Items) == 0 {
		utils.Debug("[Attachments] No attachments found in conversation")
		return messages, false, false
	}

	hasDocumentAttachments := false
	for _, item := range index.Items {
		if docsupport.IsDocumentType(item.Type) {
			hasDocumentAttachments = true
			break
		}
	}

	utils.Log("[Attachments] Found %d attachment(s) in conversation", len(index.Items))

	isAction := Models.IsActionModel(model.Name)

	// Age of each row, measured as the number of user messages that follow it.
	// Used ONLY for the trailing row's showWindow (see attachmentDelivery: the
	// in-place bytes must never read this).
	userMessagesSince := make([]int, len(messages))
	userCount := 0
	for i := len(messages) - 1; i >= 0; i-- {
		userMessagesSince[i] = userCount
		if messages[i].Role == "user" {
			userCount++
		}
	}

	// Build a lookup: (messageIndex, partIndex) -> AttachmentMeta
	type key struct{ msg, part int }
	attByPos := make(map[key]utils.AttachmentMeta)
	for _, att := range index.Items {
		attByPos[key{att.MessageIndex, att.PartIndex}] = att
	}

	removedCount := 0
	savedBytes := 0

	// Trailing row: real content of the attachments that are fresh enough.
	// Collected per attachment (not flattened) so the budget below can bill
	// and evict one attachment at a time.
	var tail []tailEntry

	// Deep copy messages and replace attachment parts with their placeholders.
	result := make([]utils.Message, len(messages))
	for i, msg := range messages {
		parts := msg.ContentParts()
		if parts == nil {
			result[i] = msg
			continue
		}

		isRecallRow := msg.Role == "tool" && msg.Name == "conversation_attachments"
		newParts := make([]utils.ContentPart, 0, len(parts))
		changed := false

		for partIdx, part := range parts {
			att, isAttachment := attByPos[key{i, partIdx}]
			if !isAttachment {
				// A conversation_attachments result is an ephemeral retrieval,
				// so BuildAttachmentIndex skips it (indexing it would duplicate
				// the original). Its images cannot ride in a tool row —
				// providers accept images in user rows only — so a FRESH recall
				// result's images move to the trailing row and the tool row
				// keeps a stable text pointer.
				if isRecallRow && part.Type == "image_url" {
					changed = true
					if userMessagesSince[i] < 1 {
						content := ""
						if part.ImageURL != nil {
							content = part.ImageURL.URL
						}
						tail = append(tail, tailEntry{
							parts: []utils.ContentPart{
								{Type: "text", Text: "[Retrieved image attached for this turn]"},
								{Type: "image_url", ImageURL: &utils.ContentImageURL{URL: content}},
							},
							size:   len(content),
							exempt: true,
						})
					}
					continue
				}
				newParts = append(newParts, part)
				continue
			}

			d := attachmentDeliveryFor(part, att, isAction)
			if d.inline {
				// The 3KB guard, and text parts for non-action models: the
				// stored bytes ship verbatim, in place, on every turn.
				newParts = append(newParts, part)
				continue
			}

			changed = true
			removedCount++
			savedBytes += att.Size

			if d.placeholder != "" {
				newParts = append(newParts, utils.ContentPart{Type: "text", Text: d.placeholder})
			}

			// Fresh enough → the real content rides the trailing row. The
			// window is age-relative, so this is the ONLY per-turn decision,
			// and it lands in append-only tail bytes.
			if userMessagesSince[i] < d.showWindow {
				tail = append(tail, tailEntry{
					parts: appendTrailingAttachment(nil, part, att, index.ContentMap[att.ID]),
					id:    att.ID,
					size:  att.Size,
				})
			}
		}

		if !changed {
			result[i] = msg
		} else if len(newParts) == 0 {
			// Every part of the row was dropped (a tool row carrying nothing
			// but stripped images): keep a stable pointer so the row stays a
			// coherent turn boundary.
			result[i] = utils.Message{
				Role:       msg.Role,
				Content:    utils.NewTextContent("Tool result content omitted (no images in this result)."),
				ToolCallID: msg.ToolCallID,
				Name:       msg.Name,
				Timestamp:  msg.Timestamp,
			}
		} else {
			result[i] = utils.Message{
				Role:             msg.Role,
				Content:          utils.NewPartsContent(newParts),
				ToolCalls:        msg.ToolCalls,
				ToolCallID:       msg.ToolCallID,
				Name:             msg.Name,
				Timestamp:        msg.Timestamp,
				TotalTokens:      msg.TotalTokens,
				PromptTokens:     msg.PromptTokens,
				CompletionTokens: msg.CompletionTokens,
				ResponseCost:     msg.ResponseCost,
				Model:            msg.Model,
			}
		}
	}

	// A conversation_attachments tool result stays IN PLACE, forever, like any
	// other attachment: it is the content the model asked for, and replacing it
	// with "call the tool again" once the turn passes is an age term on a
	// history row — the same mid-prefix rewrite that breaks KV reuse. Ageing is
	// compaction's job (durable folding removes the row and the summary carries
	// its attachment ID), not the per-request renderer's.
	//
	// The duplicate-data argument for stripping it still holds, but only for
	// BULK: a recall repeats bytes the original row already carries. That is
	// what the fetch tools' paging limits and the megacave row budget bound at
	// compaction time, which is the place where shrinking is free.

	// The single synthetic trailing row. Appended at the END, after the
	// history, so the prefix (system prompt + every history row) is untouched:
	// that is what keeps the KV prefix warm. Nothing is injected in the middle
	// of the history — the previous code injected a user row right after each
	// image-carrying tool result, which inserted bytes mid-history on every
	// turn (and re-shipped old tool images forever, since the injection had no
	// staleness gate at all).
	// Images only, by construction: a text attachment is either inline in its
	// own row (≤ attachmentInlineCapBytes) or a placeholder with showWindow 0,
	// so it never reaches `tail`. Images are here because providers accept image
	// parts in a user row and not in a tool row — this row is the only legal
	// place for them, and it is append-only tail bytes.
	trailing, trailingIDs := budgetAttachmentTail(tail, attachmentTailBudgetChars)
	if len(trailing) > 0 {
		head := attachmentTailHeader + ": " + strings.Join(trailingIDs, ", ") +
			"]. The history rows above show placeholders for them; they can be re-read later with the attachment tools."
		if len(trailingIDs) == 0 {
			head = attachmentTailHeader + "]. The content is here, and can be re-read later with the attachment tools."
		}
		parts := make([]utils.ContentPart, 0, len(trailing)+1)
		parts = append(parts, utils.ContentPart{Type: "text", Text: head})
		parts = append(parts, trailing...)
		result = append(result, utils.Message{Role: "user", Content: utils.NewPartsContent(parts)})
	}

	utils.Debug("[Attachments] Indexed and removed %d attachment(s) (%d KB saved)", removedCount, savedBytes/kb)

	// Re-inflate internal URLs back to data URIs so LLMs can see the images
	// (the trailing row's image parts are the only images in the request).
	return reInflateImageURLs(result), true, hasDocumentAttachments
}

// stripImagesForNonVisionModel replaces every image part in the outgoing
// message slice with a lightweight placeholder. A non-vision model cannot
// process images, and shipping base64 data URIs to it is pure context
// waste (a single screenshot can be 10-50k tokens). The caller already
// ran SelectModel, so a vision swap (if configured) already happened;
// reaching this helper means the selected model truly cannot see images.
func stripImagesForNonVisionModel(messages []utils.Message) []utils.Message {
	out := make([]utils.Message, 0, len(messages))
	for _, m := range messages {
		parts := m.ContentParts()
		changed := false
		newParts := make([]utils.ContentPart, 0, len(parts))
		for _, p := range parts {
			if p.Type == "image_url" {
				changed = true
				// Keep the turn boundary coherent: placeholder text.
				newParts = append(newParts, utils.ContentPart{
					Type: "text",
					Text: "[Image omitted: the selected model cannot see images]",
				})
				continue
			}
			newParts = append(newParts, p)
		}
		if changed {
			m.Content = utils.NewPartsContent(newParts)
		}
		out = append(out, m)
	}
	return out
}

// estimateImageTokens counts the token cost of every image part (base64
// data URI payload) in the messages about to be shipped. Without this the
// eco tail budget reserves room for tools + system + tail but NOT for the
// images that are re-inflated to base64 on every request — observed: a
// conversation with 15 recent attachments shipped base64 images worth
// ~96k tokens on top of a ~6k tail, blowing the context window (prompt
// 131478 tokens vs 131072 → provider 500 → dead turn).
func estimateImageTokens(messages []utils.Message) int {
	tokens := 0
	for _, m := range messages {
		parts := m.ContentParts()
		if len(parts) == 0 {
			continue
		}
		for _, p := range parts {
			if p.Type != "image_url" || p.ImageURL == nil {
				continue
			}
			u := p.ImageURL.URL
			if i := strings.Index(u, ";base64,"); i >= 0 {
				// base64 payload: ~4 chars per 3 bytes; ~1 token per 4 chars.
				tokens += (len(u) - i - len(";base64,")) / 4
			} else {
				tokens += len(u) / 4
			}
		}
	}
	return tokens
}

// reInflateImageURLs reads image files from disk for any ContentPart that uses
// an internal /attachments/... URL, converting back to a data: URI.
// This is called on the ephemeral copy of messages before sending to LLMs.
func reInflateImageURLs(messages []utils.Message) []utils.Message {
	for i, msg := range messages {
		parts := msg.ContentParts()
		if len(parts) == 0 {
			continue
		}

		changed := false
		newParts := make([]utils.ContentPart, len(parts))
		copy(newParts, parts)

		for j, part := range newParts {
			if part.Type == "image_url" && part.ImageURL != nil && storage.IsInternalURL(part.ImageURL.URL) {
				data, mimeType, err := storage.ReadBlob(part.ImageURL.URL)
				if err != nil {
					utils.Error("[Attachments] Error reading blob for re-inflation", err)
					// Replace with text so the LLM doesn't see an opaque internal URL
					newParts[j] = utils.ContentPart{
						Type: "text",
						Text: "[Image unavailable: file could not be read from storage]",
					}
					changed = true
					continue
				}
				b64 := base64.StdEncoding.EncodeToString(data)
				newParts[j].ImageURL = &utils.ContentImageURL{
					URL: fmt.Sprintf("data:%s;base64,%s", mimeType, b64),
				}
				changed = true
			}
		}

		if changed {
			messages[i].Content = utils.NewPartsContent(newParts)
		}
	}
	return messages
}

// tailEntry is one attachment's real content, queued for the trailing row.
type tailEntry struct {
	parts  []utils.ContentPart
	id     string
	size   int
	exempt bool // images: the vision contract, never evicted by the budget
}

// attachmentTailBudgetChars bounds how much attachment content the trailing
// row may ship on a turn.
//
// WHY A BOUND IS NEEDED. The trailing row re-ships the real content of every
// attachment the delivery rule calls fresh, and the freshness window is
// measured in USER messages. An agentic run is ONE user message: the human
// asks once and the loop runs dozens of assistant/tool rows behind it, so a
// tool row's userMessagesSince stays 0 for the whole run and every window
// (1, 2, 3) passes. Nothing ages out, and the row grows with the run.
//
// Measured on the deployed build, every turn of one conversation logged
//
//	[Attachments] Found 29 attachment(s) in conversation
//	[Attachments] Indexed and removed 29 attachment(s) (254 KB saved)
//
// and the row shipped 28 prior tool outputs (~180 KB of the 254 KB). That is
// ~55K tokens re-sent on EVERY turn of the loop, on top of the history, for
// content the model can always re-read through conversation_attachments. It is
// a token cost, not a cache-bust claim: cache_read advances on a 32768-token
// block ladder (measured 21541 -> 54309, 33213 -> 65981 -> 98749, every step
// exactly 32768), so a flat cache_read across turns is quantization and says
// nothing about this row.
//
// 32 KB is ~10K tokens — the size of one large tool result, which is what the
// row is for. Eviction is oldest-first, so the content the loop just produced
// is always present; what goes is content from earlier in the run, which the
// model has already acted on and can re-read.
const attachmentTailBudgetChars = 32 * kb

// budgetAttachmentTail flattens the queued entries into the trailing row's
// parts, evicting the OLDEST non-exempt entries until the billed content fits
// the budget. Entries arrive in row order (oldest first), so eviction is
// oldest-first by construction. The newest entry always survives: a row over
// budget is a cost, an empty row is a missing attachment.
func budgetAttachmentTail(entries []tailEntry, budget int) ([]utils.ContentPart, []string) {
	keep := entries
	billed, billable := 0, 0
	for _, e := range keep {
		if !e.exempt {
			billed += e.size
			billable++
		}
	}
	// Evict the OLDEST billable entry while over budget AND more than one
	// billable entry remains. The floor is ONE billable entry, not one entry:
	// exempt images do not count, so a row of [40 KB snippet, fresh image]
	// must keep the snippet. Evicting it left the model with a placeholder for
	// the file the user had just attached — caught by the race gate on
	// TestRecallResultStripIsStable, which pins that the trailing row
	// references the fresh snippet.
	for billed > budget && billable > 1 {
		idx := -1
		for j, e := range keep {
			if !e.exempt {
				idx = j
				break
			}
		}
		if idx < 0 {
			break // only exempt entries left: images are not evictable
		}
		billed -= keep[idx].size
		billable--
		keep = append(keep[:idx], keep[idx+1:]...)
	}

	parts := make([]utils.ContentPart, 0, len(keep)+1)
	ids := make([]string, 0, len(keep))
	for _, e := range keep {
		parts = append(parts, e.parts...)
		if e.id != "" {
			ids = append(ids, e.id)
		}
	}
	return parts, ids
}
