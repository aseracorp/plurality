package cron

import (
	"context"
	"time"

	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// Run fires a single CRON: starts a new conversation, kicks off the LLM loop
// in a goroutine, and returns. No SSE client is attached — the conversation
// shows up afterwards in the user's history.
func Run(userID string, job CronJob) {
	ctx := context.WithValue(context.Background(), "userID", userID)

	preset, modelSel := resolveJobPreset(userID, job)

	msg := utils.Message{
		Role:      "user",
		Content:   utils.NewTextContent(job.Prompt),
		Timestamp: time.Now().Format(time.RFC3339),
	}
	partial := utils.Conversation{
		Title:         "Cron: " + truncate(job.Prompt, 40),
		ModelSelected: modelSel,
		MiniApp:       preset,
	}

	updated, _, err := db.PushMessage(ctx, partial, msg)
	if err != nil {
		utils.Error("[Cron] PushMessage failed", err)
		return
	}

	if err := db.SetConversationCronJobID(ctx, updated.ID, job.ID); err != nil {
		utils.Error("[Cron] SetConversationCronJobID failed", err)
	}
	updated.CronJobID = job.ID

	model := ai.SelectModel(modelSel, updated)
	ar := ai.NewActiveRequest(updated.ID, userID, model, modelSel)
	// NewActiveRequest builds ar.Ctx from context.Background() — override it
	// so the LLM loop's saves carry the userID (same pattern as HandleChat).
	cancelCtx, cancelFunc := context.WithCancel(ctx)
	ar.Ctx = cancelCtx
	ar.Cancel = cancelFunc
	ai.RequestRegistry.Set(updated.ID, ar)
	if err := db.UpdateConversationState(ctx, updated.ID, utils.StateProcessing); err != nil {
		utils.Error("[Cron] UpdateConversationState failed", err)
	}

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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
