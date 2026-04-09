package db

import (
	"context"
	"errors"
	"time"

	"github.com/azukaar/plurality/src/utils"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// GetAllMiniApps retrieves all available mini-apps from the database
func GetAllMiniApps(ctx context.Context) ([]utils.MiniApp, error) {
	start := time.Now() // Start timing
	client := GetClient()
	collection := client.Database("plurality").Collection("miniapps")

	// Set up options to sort by name
	findOptions := options.Find()
	findOptions.SetSort(bson.D{{Key: "name", Value: 1}})

	// Find all mini-apps
	cursor, err := collection.Find(ctx, bson.M{}, findOptions)
	if err != nil {
		utils.Error("[GetAllMiniApps] Error finding mini-apps", err)
		return nil, err
	}
	defer cursor.Close(ctx)

	// Decode results
	var miniApps []utils.MiniApp
	if err = cursor.All(ctx, &miniApps); err != nil {
		utils.Error("[GetAllMiniApps] Error decoding mini-apps", err)
		return nil, err
	}

	elapsed := time.Since(start) // Calculate elapsed time
	utils.Debug("Retrieved %d mini-apps in %s", len(miniApps), elapsed)
	return miniApps, nil
}

// GetUserPinnedMiniApps retrieves the mini-apps that a user has pinned
func GetUserPinnedMiniApps(ctx context.Context) ([]utils.MiniApp, error) {
	client := GetClient()
	userID, ok := ctx.Value("userID").(string)

	if !ok {
		return nil, errors.New("user ID not found in request context")
	}

	// First get the user's pinned mini-app IDs
	usersCollection := client.Database("plurality").Collection("users")
	var user struct {
		PinnedMiniApps []primitive.ObjectID `bson:"pinned_mini_apps"`
	}

	err := usersCollection.FindOne(ctx, bson.M{"_id": userID}).Decode(&user)
	if err != nil {
		utils.Error("[GetUserPinnedMiniApps] Error finding user", err)
		return nil, err
	}

	// If user has no pinned mini-apps, return empty slice
	if len(user.PinnedMiniApps) == 0 {
		return []utils.MiniApp{}, nil
	}

	// Get the mini-apps that match the pinned IDs
	miniAppsCollection := client.Database("plurality").Collection("miniapps")
	cursor, err := miniAppsCollection.Find(ctx, bson.M{
		"_id": bson.M{"$in": user.PinnedMiniApps},
	})
	if err != nil {
		utils.Error("[GetUserPinnedMiniApps] Error finding pinned mini-apps", err)
		return nil, err
	}
	defer cursor.Close(ctx)

	// Decode results
	var pinnedApps []utils.MiniApp
	if err = cursor.All(ctx, &pinnedApps); err != nil {
		utils.Error("[GetUserPinnedMiniApps] Error decoding pinned mini-apps", err)
		return nil, err
	}

	utils.Debug("Retrieved %d pinned mini-apps for user %s", len(pinnedApps), userID)
	return pinnedApps, nil
}

// PinMiniApp adds a mini-app to a user's pinned list
func PinMiniApp(ctx context.Context, miniAppID string) error {
	client := GetClient()
	userID, ok := ctx.Value("userID").(string)

	if !ok {
		return errors.New("user ID not found in request context")
	}

	// Convert string ID to ObjectID
	miniAppObjID, err := primitive.ObjectIDFromHex(miniAppID)
	if err != nil {
		return errors.New("invalid mini-app ID format")
	}

	// Add to user's pinned mini-apps if not already pinned
	usersCollection := client.Database("plurality").Collection("users")
	result, err := usersCollection.UpdateOne(
		ctx,
		bson.M{"_id": userID},
		bson.M{
			"$addToSet": bson.M{"pinned_mini_apps": miniAppObjID},
		},
	)

	if err != nil {
		utils.Error("[PinMiniApp] Error pinning mini-app", err)
		return err
	}

	if result.MatchedCount == 0 {
		return errors.New("user not found")
	}

	utils.Debug("Mini-app %s pinned for user %s", miniAppID, userID)
	return nil
}

// UnpinMiniApp removes a mini-app from a user's pinned list
func UnpinMiniApp(ctx context.Context, miniAppID string) error {
	client := GetClient()
	userID, ok := ctx.Value("userID").(string)

	if !ok {
		return errors.New("user ID not found in request context")
	}

	// Convert string ID to ObjectID
	miniAppObjID, err := primitive.ObjectIDFromHex(miniAppID)
	if err != nil {
		return errors.New("invalid mini-app ID format")
	}

	// Remove from user's pinned mini-apps
	usersCollection := client.Database("plurality").Collection("users")
	result, err := usersCollection.UpdateOne(
		ctx,
		bson.M{"_id": userID},
		bson.M{
			"$pull": bson.M{"pinned_mini_apps": miniAppObjID},
		},
	)

	if err != nil {
		utils.Error("[UnpinMiniApp] Error unpinning mini-app", err)
		return err
	}

	if result.MatchedCount == 0 {
		return errors.New("user not found")
	}

	utils.Debug("Mini-app %s unpinned for user %s", miniAppID, userID)
	return nil
}

// GetMiniAppByID retrieves a specific mini-app by its ID
func GetMiniAppByID(ctx context.Context, miniAppID string) (*utils.MiniApp, error) {
	client := GetClient()
	collection := client.Database("plurality").Collection("miniapps")

	// Convert string ID to ObjectID
	id, err := primitive.ObjectIDFromHex(miniAppID)
	if err != nil {
		return nil, errors.New("invalid mini-app ID format")
	}

	// Find the mini-app
	var miniApp utils.MiniApp
	err = collection.FindOne(ctx, bson.M{"_id": id}).Decode(&miniApp)
	if err != nil {
		utils.Error("[GetMiniAppByID] Error finding mini-app", err)
		return nil, err
	}

	return &miniApp, nil
}

// CreateMiniApp adds a new mini-app to the database
func CreateMiniApp(ctx context.Context, miniApp utils.MiniApp) (*utils.MiniApp, error) {
	client := GetClient()
	collection := client.Database("plurality").Collection("miniapps")

	// Ensure new ID if not provided
	if miniApp.ID == primitive.NilObjectID {
		miniApp.ID = primitive.NewObjectID()
	}

	// Set author from context if available and not already set
	if miniApp.Author == "" {
		if userID, ok := ctx.Value("userID").(string); ok {
			miniApp.Author = userID
		}
	}

	// Insert the new mini-app
	_, err := collection.InsertOne(ctx, miniApp)
	if err != nil {
		utils.Error("[CreateMiniApp] Error creating mini-app", err)
		return nil, err
	}

	utils.Debug("Created new mini-app with ID %s", miniApp.ID.Hex())
	return &miniApp, nil
}

// UpdateMiniApp updates an existing mini-app
func UpdateMiniApp(ctx context.Context, miniAppID string, updates utils.MiniApp) (*utils.MiniApp, error) {
	client := GetClient()
	collection := client.Database("plurality").Collection("miniapps")
	userID, ok := ctx.Value("userID").(string)

	if !ok {
		return nil, errors.New("user ID not found in request context")
	}

	// Convert string ID to ObjectID
	id, err := primitive.ObjectIDFromHex(miniAppID)
	if err != nil {
		return nil, errors.New("invalid mini-app ID format")
	}

	// Ensure ID matches in the update
	updates.ID = id

	// Options to return the updated document
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)

	// Update the mini-app (only if user is the author or an admin)
	var updatedMiniApp utils.MiniApp
	err = collection.FindOneAndUpdate(
		ctx,
		bson.M{
			"_id": id,
			"$or": []bson.M{
				{"author": userID},
				// Add admin check here if needed
			},
		},
		bson.M{"$set": updates},
		opts,
	).Decode(&updatedMiniApp)

	if err != nil {
		utils.Error("[UpdateMiniApp] Error updating mini-app", err)
		return nil, err
	}

	utils.Debug("Updated mini-app with ID %s", miniAppID)
	return &updatedMiniApp, nil
}

// DeleteMiniApp removes a mini-app from the database
func DeleteMiniApp(ctx context.Context, miniAppID string) error {
	client := GetClient()
	collection := client.Database("plurality").Collection("miniapps")
	userID, ok := ctx.Value("userID").(string)

	if !ok {
		return errors.New("user ID not found in request context")
	}

	// Convert string ID to ObjectID
	id, err := primitive.ObjectIDFromHex(miniAppID)
	if err != nil {
		return errors.New("invalid mini-app ID format")
	}

	// Delete the mini-app (only if user is the author or an admin)
	result, err := collection.DeleteOne(
		ctx,
		bson.M{
			"_id": id,
			"$or": []bson.M{
				{"author": userID},
				// Add admin check here if needed
			},
		},
	)

	if err != nil {
		utils.Error("[DeleteMiniApp] Error deleting mini-app", err)
		return err
	}

	if result.DeletedCount == 0 {
		return errors.New("mini-app not found or you don't have permission to delete it")
	}

	utils.Debug("Deleted mini-app with ID %s", miniAppID)
	return nil
}
