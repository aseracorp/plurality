package user

import (
	"context"
	"errors"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// DeleteUser removes the authenticated user's per-user SQLite data, attachment
// files, and (for local users) the entry in data/user.json. OpenID-only users
// have no account row to remove.
func DeleteUser(ctx context.Context) error {
	username, ok := ctx.Value("userID").(string)
	if !ok || username == "" {
		return errors.New("user ID not found in request context")
	}

	if _, err := db.DeleteAllConversations(ctx, username); err != nil {
		utils.Error("[DeleteUser] error deleting conversations", err)
	}

	if err := auth.DeleteUserData(ctx, username); err != nil {
		utils.Error("[DeleteUser] error deleting user data dir", err)
	}

	if auth.UserExists(username) {
		if err := auth.RemoveUser(username); err != nil {
			utils.Error("[DeleteUser] error removing local user", err)
			return err
		}
	}

	utils.Log("[DeleteUser] removed user %s", username)
	return nil
}
