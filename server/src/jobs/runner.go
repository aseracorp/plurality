package jobs

import (
	"context"
	"time"

	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// RunOptions describes one background invocation: a stored prompt fired
// against a preset, tagged with what triggered it.
type RunOptions struct {
	// TitlePrefix is what goes before the truncated prompt in the conversation
	// title — e.g. "Cron" or "Webhook".
	TitlePrefix string

	// Prompt is the user-stored prompt to send.
	Prompt string

	// PresetID resolves to a MiniApp via ResolvePreset.
	PresetID string

	// ExtraContext is optional. When non-empty it's appended below the prompt
	// (separated by ---). Webhook uses this to surface the request payload.
	ExtraContext string

	// TriggerType / TriggerID are stored on the resulting Conversation so
	// the UI can link back to whatever fired it. Empty values are skipped.
	TriggerType string
	TriggerID   string

	// ConversationID, when non-empty, makes the run append to that existing
	// conversation instead of creating a new one. If it doesn't resolve
	// (deleted, never existed) we silently fall back to creating a new
	// conversation and report the new ID through OnConversationResolved.
	ConversationID string

	// OnConversationResolved fires at most once with the conversation ID
	// that messages actually landed on, and only when ConversationID was
	// non-empty to begin with. Callers compare it to whatever they passed
	// in and persist the new value back when it differs (i.e. after a
	// fallback). It never fires for runs that intentionally start fresh
	// (empty ConversationID), so "new conversation every run" stays sticky.
	// Optional.
	OnConversationResolved func(conversationID string)
}

// RunPrompt creates a new conversation, kicks off the LLM loop in a
// goroutine, and returns. No SSE client is attached — the conversation
// shows up in the user's history when the run finishes.
func RunPrompt(ctx context.Context, userID string, opts RunOptions) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, "userID", userID)

	preset, modelSel := ResolvePreset(userID, opts.PresetID)

	content := opts.Prompt
	if opts.ExtraContext != "" {
		content = opts.Prompt + "\n\n---\n" + opts.ExtraContext
	}

	msg := utils.Message{
		Role:      "user",
		Content:   utils.NewTextContent(content),
		Timestamp: time.Now().Format(time.RFC3339),
	}
	partial := utils.Conversation{
		Title:         opts.TitlePrefix + ": " + truncate(opts.Prompt, 40),
		ModelSelected: modelSel,
		MiniApp:       preset,
	}

	// If the caller asked to append to a specific conversation, verify it
	// exists; otherwise fall back to creating a new one. db.PushMessage
	// branches purely on partial.ID being non-empty, so existence has to
	// be checked here.
	if opts.ConversationID != "" {
		if _, err := db.GetConversationById(ctx, opts.ConversationID); err == nil {
			partial.ID = opts.ConversationID
			// Don't clobber the user's existing title on append.
			partial.Title = ""
		} else {
			utils.Warn("[%s] configured conversation_id %s not found, creating new (%s)", opts.TitlePrefix, opts.ConversationID, err.Error())
		}
	}

	updated, _, err := db.PushMessage(ctx, partial, msg)
	if err != nil {
		utils.Error("["+opts.TitlePrefix+"] PushMessage failed", err)
		return
	}

	// Only report back when the caller configured an append target that had
	// to be replaced. An intentionally empty ConversationID means "new
	// conversation every run" — persisting the new ID would flip the job
	// into append mode from the second fire onwards.
	if opts.OnConversationResolved != nil && opts.ConversationID != "" {
		opts.OnConversationResolved(updated.ID)
	}

	if opts.TriggerType != "" && opts.TriggerID != "" {
		if err := db.SetConversationTrigger(ctx, updated.ID, opts.TriggerType, opts.TriggerID); err != nil {
			utils.Error("["+opts.TitlePrefix+"] SetConversationTrigger failed", err)
		}
		updated.TriggerType = opts.TriggerType
		updated.TriggerID = opts.TriggerID
	}

	model := ai.SelectModel(modelSel, updated)
	ar := ai.NewActiveRequest(updated.ID, userID, model, modelSel)
	// NewActiveRequest builds ar.Ctx from context.Background() — override it
	// so the LLM loop's saves carry the userID (same pattern as HandleChat).
	cancelCtx, cancelFunc := context.WithCancel(ctx)
	ar.Ctx = cancelCtx
	ar.Cancel = cancelFunc
	ai.RequestRegistry.Set(updated.ID, ar)
	if err := db.UpdateConversationState(ctx, updated.ID, utils.StateProcessing); err != nil {
		utils.Error("["+opts.TitlePrefix+"] UpdateConversationState failed", err)
	}

	// Broadcast the new conversation's existence so the sidebar updates
	// without waiting for the LLM loop's first event.
	ai.StatusRegistry.BroadcastToUser(userID, ai.StatusEvent{
		ConversationID: updated.ID,
		State:          string(utils.StateProcessing),
		Title:          updated.Title,
	})

	payload := ai.ChatPayload{
		ConversationID: updated.ID,
		ModelSelected:  modelSel,
		Messages:       []utils.Message{msg},
	}
	if preset != nil {
		payload.MiniApp = *preset
	}

	go ar.RunLLMLoop(ctx, updated, payload)
}

// SubAgentOptions describes one sub-agent spawn.
type SubAgentOptions struct {
	// Prompt is the user-seed message for the sub-agent.
	Prompt string

	// Title for the new conversation (e.g. "Sub-agent: <truncated prompt>").
	Title string

	// ParentID is the spawning conversation's ID. Stored on the new child as
	// (trigger_type='sub_agent', trigger_id=ParentID).
	ParentID string

	// ModelSelected drives the LLM and tool gating. The caller is responsible
	// for building this (e.g. via ai.ShortcutModelSelected + intersected tools).
	ModelSelected utils.ModelSelected
}

// RunSubAgent creates a child conversation linked to ParentID, kicks off the
// LLM loop in a goroutine, and returns the new conversation ID plus a channel
// that closes when the loop exits. Unlike RunPrompt, no preset is resolved —
// the caller supplies ModelSelected verbatim.
func RunSubAgent(ctx context.Context, userID string, opts SubAgentOptions) (string, <-chan struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Use a long-lived context for the sub-agent so it survives the parent's
	// tool-exec context being canceled at end of turn.
	subCtx := context.WithValue(context.Background(), "userID", userID)

	msg := utils.Message{
		Role:      "user",
		Content:   utils.NewTextContent(opts.Prompt),
		Timestamp: time.Now().Format(time.RFC3339),
	}

	partial := utils.Conversation{
		Title:         opts.Title,
		ModelSelected: opts.ModelSelected,
	}

	updated, _, err := db.PushMessage(subCtx, partial, msg)
	if err != nil {
		return "", nil, err
	}

	if opts.ParentID != "" {
		if err := db.SetConversationTrigger(subCtx, updated.ID, "sub_agent", opts.ParentID); err != nil {
			utils.Error("[sub-agent] SetConversationTrigger failed", err)
		}
		updated.TriggerType = "sub_agent"
		updated.TriggerID = opts.ParentID
	}

	model := ai.SelectModel(opts.ModelSelected, updated)
	ar := ai.NewActiveRequest(updated.ID, userID, model, opts.ModelSelected)
	cancelCtx, cancelFunc := context.WithCancel(subCtx)
	ar.Ctx = cancelCtx
	ar.Cancel = cancelFunc
	ai.RequestRegistry.Set(updated.ID, ar)
	if err := db.UpdateConversationState(subCtx, updated.ID, utils.StateProcessing); err != nil {
		utils.Error("[sub-agent] UpdateConversationState failed", err)
	}

	// Broadcast the sub-agent's existence to the sidebar before the LLM loop
	// gets a chance to. RunLLMLoop will emit its own "typing" event shortly,
	// but races (early cancellation, slow first chunk) can leave the sidebar
	// in the dark until manual refresh.
	ai.StatusRegistry.BroadcastToUser(userID, ai.StatusEvent{
		ConversationID: updated.ID,
		State:          string(utils.StateProcessing),
		Title:          opts.Title,
	})

	payload := ai.ChatPayload{
		ConversationID: updated.ID,
		ModelSelected:  opts.ModelSelected,
		Messages:       []utils.Message{msg},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ar.RunLLMLoop(subCtx, updated, payload)
	}()

	return updated.ID, done, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
