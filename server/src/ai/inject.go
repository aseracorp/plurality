package ai

import (
	"context"
	"errors"
	"time"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// InjectAndReinvoke appends a synthetic user message to a conversation and, if
// that conversation is idle, kicks off a new LLM loop for it. When the
// conversation is already processing, the message lands in the DB and the
// in-flight loop picks it up on its next iteration (RunLLMLoop reloads the
// conversation from DB after every turn). Used by sub-agent background
// completion callbacks to deliver results back to the spawning agent.
func InjectAndReinvoke(parentID, userID, content string) error {
	if parentID == "" || userID == "" {
		return errors.New("parentID and userID are required")
	}

	ctx := context.WithValue(context.Background(), "userID", userID)

	parent, err := db.GetConversationById(ctx, parentID)
	if err != nil {
		return err
	}

	msg := utils.Message{
		Role:      "user",
		Content:   utils.NewTextContent(content),
		Timestamp: time.Now().Format(time.RFC3339),
	}

	updated, _, err := db.PushMessage(ctx, *parent, msg)
	if err != nil {
		return err
	}

	if updated.State != utils.StateIdle {
		// In-flight loop will see this message on its next DB reload.
		return nil
	}

	model := SelectModel(updated.ModelSelected, updated)
	ar := NewActiveRequest(updated.ID, userID, model, updated.ModelSelected)
	cancelCtx, cancelFunc := context.WithCancel(ctx)
	ar.Ctx = cancelCtx
	ar.Cancel = cancelFunc
	RequestRegistry.Set(updated.ID, ar)
	if err := db.UpdateConversationState(ctx, updated.ID, utils.StateProcessing); err != nil {
		return err
	}

	payload := ChatPayload{
		ConversationID: updated.ID,
		ModelSelected:  updated.ModelSelected,
		Messages:       []utils.Message{msg},
	}
	if updated.MiniApp != nil {
		payload.MiniApp = *updated.MiniApp
	}

	go ar.RunLLMLoop(ctx, updated, payload)
	return nil
}
