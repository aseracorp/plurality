package user

import (
	"context"
	"errors"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// DeleteUser deletes a user from Firebase
func DeleteUser(ctx context.Context) error {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return errors.New("user ID not found in request context")
	}

	// Verify Firebase Auth client is initialized
	if utils.FirebaseAuth == nil {
		return errors.New("Firebase Auth client not initialized")
	}

	// Delete all conversations first
	deletedCount, err := db.DeleteAllConversations(ctx, userID)
	if err != nil {
		utils.Error("[DeleteUser] Error deleting user conversations", err)
	}

	// Delete balance
	err = db.DeleteBalance(ctx)
	if err != nil {
		utils.Error("[DeleteUser] Error deleting user balance", err)
	}

	utils.Log("[DeleteUser] Deleted %d conversations for user", deletedCount)

	// Delete the Firebase user record
	err = utils.FirebaseAuth.DeleteUser(ctx, userID)
	if err != nil {
		utils.Error("[DeleteUser] Error deleting Firebase user", err)
	}

	utils.Log("[DeleteUser] Successfully deleted user ID: %s from Firebase", userID)

	return nil
}
