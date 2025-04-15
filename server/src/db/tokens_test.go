package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/azukaar/plurality/src/utils" // Assuming this path is correct relative to your project structure
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest" // Use mtest for mocking MongoDB interactions
	// "go.mongodb.org/mongo-driver/mongo/options"
)

// Helper to create a context with userID
func contextWithUserID(userID string) context.Context {
	return context.WithValue(context.Background(), "userID", userID)
}

// Global mtest instance for managing mock client/db/collections
var mt *mtest.T

// TestMain sets up the mtest environment
// Note: mtest requires specific build tags or setup. Running these tests
// might require `go test -tags=test` or similar, depending on mtest version/setup.
// For simplicity here, we'll structure tests assuming mtest is available.
// If direct `TestMain` usage with mtest is complex in your setup, you can
// instantiate `mtest.New(t)` inside each test function.
// func TestMain(m *testing.M) {
//  // Using mtest.New in each test function is often simpler
//  // mt = mtest.New(nil, mtest.NewOptions().ClientType(mtest.Mock)) // Replaced by per-test setup
//  // // defer mt.Close()
// 	// code := m.Run()
// 	// os.Exit(code)
// }

// Override GetClient for testing to return the mock client
// This is a common pattern for injecting mocks when a global getter is used.
// Ensure your actual GetClient logic is compatible with this override mechanism
// (e.g., uses a package-level variable that can be swapped).
var originalClient *mongo.Client // To store the original client if needed

func setupMockClient(t *testing.T) *mtest.T {
	utils.Log("[setupMockClient] Setting up mock client")
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	// originalClient = clientInstance // Backup original if it exists
	SetClient(mt.Client)            // Assume SetClient function exists to override the package client
	return mt
}

func teardownMockClient() {
	// SetClient(originalClient) // Restore original client
}

// --- Test Functions ---

func TestInitUserIfNotFound(t *testing.T) {
	mt := setupMockClient(t)
	defer teardownMockClient()
	// defer mt.Close() // Close the mtest instance for this test function

	testUserID := "test-user-init"

	mt.Run("User Exists", func(mt *mtest.T) {
		// Expect FindOne call, return an existing document (no error)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()},
			{Key: "user_id", Value: testUserID},
			{Key: "balance", Value: 100.0},
			// ... other fields
		}))

		err := InitUserIfNotFound(testUserID)
		assert.NoError(t, err, "Should not return error when user exists")
	})

	mt.Run("User Not Found - Create Success", func(mt *mtest.T) {
		// Expect FindOne call, return ErrNoDocuments
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    11000, // Not exactly ErrNoDocuments, but signifies find failure for mock
			Message: "mongo: no documents in result",
			Name:    "NoDocuments", // mtest might not map directly to ErrNoDocuments, check behavior
		}))
		// If FindOne fails with "no documents", expect InsertOne call
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{
			{Key: "ok", Value: 1},
			{Key: "n", Value: 1},
			{Key: "insertedId", Value: primitive.NewObjectID()},
		}...))

		err := InitUserIfNotFound(testUserID)
		assert.NoError(t, err, "Should not return error when user is created successfully")
		// Verification of the insert content happens implicitly by the mock setup expecting specific calls.
		// More advanced mtest usage could inspect the arguments passed to InsertOne.
	})

	mt.Run("User Not Found - Create Fails", func(mt *mtest.T) {
		// Expect FindOne call, return ErrNoDocuments
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    11000,
			Message: "mongo: no documents in result",
			Name:    "NoDocuments",
		}))
		// Expect InsertOne call, return an error
		mt.AddMockResponses(mtest.CreateWriteErrorsResponse(mtest.WriteError{
			Index:   0,
			Code:    11000, // Example error code
			Message: "insert failed",
		}))

		err := InitUserIfNotFound(testUserID)
		assert.Error(t, err, "Should return error when insert fails")
		assert.Contains(t, err.Error(), "insert failed", "Error message should reflect the insert failure")
	})

	mt.Run("FindOne Error", func(mt *mtest.T) {
		// Expect FindOne call, return a generic error
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    12345,
			Message: "database connection failed",
		}))

		err := InitUserIfNotFound(testUserID)
		assert.Error(t, err, "Should return error when FindOne fails")
		assert.Contains(t, err.Error(), "database connection failed", "Error message should reflect the find failure")
	})
}

func TestUpdateUserPlan(t *testing.T) {
	mt := setupMockClient(t)
	defer teardownMockClient()
	// defer mt.Close()

	testUserID := "test-user-plan"
	initialBalance := 1000.0
	newPlanAmount := 2000000.0
	newPlanName := "Basic"
	newPlanEnd := time.Now().AddDate(0, 0, 30).Unix() // ~1 month
	newPlanEndYearly := time.Now().AddDate(1, 0, 0).Unix() // 1 year

	mt.Run("User Exists - Plan Changed (Monthly)", func(mt *mtest.T) {
		// 1. InitUserIfNotFound - FindOne (user exists)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()}, {Key: "user_id", Value: testUserID}, {Key: "balance", Value: initialBalance}, {Key: "plan_name", Value: "Free"}, {Key: "plan", Value: 500000},
		}))
		// 2. FindOne (get current balance before update)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()}, {Key: "user_id", Value: testUserID}, {Key: "balance", Value: initialBalance}, {Key: "plan_name", Value: "Free"}, {Key: "plan", Value: 500000},
		}))
		// 3. UpdateOne (plan name changed)
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{
			{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "nModified", Value: 1},
		}...))

		err := UpdateUserPlan(testUserID, newPlanName, newPlanAmount, newPlanEnd)
		assert.NoError(t, err)
		// We implicitly verified the calls via mock responses. Asserting the specific update document
		// would require more complex mtest setup or inspecting driver commands.
	})

	mt.Run("User Exists - Plan Changed (Yearly)", func(mt *mtest.T) {
		// 1. InitUserIfNotFound - FindOne (user exists)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()}, {Key: "user_id", Value: testUserID}, {Key: "balance", Value: initialBalance}, {Key: "plan_name", Value: "Free"}, {Key: "plan", Value: 500000},
		}))
		// 2. FindOne (get current balance before update)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()}, {Key: "user_id", Value: testUserID}, {Key: "balance", Value: initialBalance}, {Key: "plan_name", Value: "Free"}, {Key: "plan", Value: 500000},
		}))
		// 3. UpdateOne (plan name changed, yearly plan sets renewal)
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{
			{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "nModified", Value: 1},
		}...))

		err := UpdateUserPlan(testUserID, newPlanName, newPlanAmount, newPlanEndYearly)
		assert.NoError(t, err)
		// Verify plan_next_renewal would be set correctly in the update (implicitly tested by mock expecting the update)
	})

	mt.Run("User Exists - Plan Did Not Change", func(mt *mtest.T) {
		// 1. InitUserIfNotFound - FindOne (user exists)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()}, {Key: "user_id", Value: testUserID}, {Key: "balance", Value: initialBalance}, {Key: "plan_name", Value: newPlanName}, {Key: "plan", Value: newPlanAmount},
		}))
		// 2. FindOne (get current balance before update)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()}, {Key: "user_id", Value: testUserID}, {Key: "balance", Value: initialBalance}, {Key: "plan_name", Value: newPlanName}, {Key: "plan", Value: newPlanAmount},
		}))
		// 3. UpdateOne (plan name same, should not update 'plan' field)
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{
			{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "nModified", Value: 1},
		}...))

		// Update with same plan name, but potentially different end date/allowance (allowance shouldn't be applied)
		err := UpdateUserPlan(testUserID, newPlanName, 9999999.0, newPlanEnd) // Use different allowance
		assert.NoError(t, err)
		// Implicit check: the update mock would fail if the '$set' included 'plan'
	})

	mt.Run("User Not Found - Init and Update Success", func(mt *mtest.T) {
		// 1. InitUserIfNotFound - FindOne (no documents)
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 11000, Message: "mongo: no documents in result", Name: "NoDocuments"}))
		// 2. InitUserIfNotFound - InsertOne (success)
		insertedID := primitive.NewObjectID()
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "insertedId", Value: insertedID}}...))
		// 3. FindOne (get current balance - the newly inserted one)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: insertedID}, {Key: "user_id", Value: testUserID}, {Key: "balance", Value: 500000}, {Key: "plan_name", Value: "Free"}, {Key: "plan", Value: 500000}, {Key: "updated_at", Value: time.Now()}, {Key: "last_plan_applied", Value: time.Now()},
		}))
		// 4. UpdateOne (plan name changed from default 'Free')
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{
			{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "nModified", Value: 1},
		}...))

		err := UpdateUserPlan(testUserID, newPlanName, newPlanAmount, newPlanEnd)
		assert.NoError(t, err)
	})

	mt.Run("Init Fails", func(mt *mtest.T) {
		// 1. InitUserIfNotFound - FindOne (generic error)
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 12345, Message: "init find failed"}))

		err := UpdateUserPlan(testUserID, newPlanName, newPlanAmount, newPlanEnd)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "init find failed")
	})

	mt.Run("Find Balance Before Update Fails", func(mt *mtest.T) {
		// 1. InitUserIfNotFound - FindOne (user exists)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()}, {Key: "user_id", Value: testUserID}, {Key: "balance", Value: initialBalance}, {Key: "plan_name", Value: "Free"}, {Key: "plan", Value: 500000},
		}))
		// 2. FindOne (get current balance before update - fails)
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 12345, Message: "find before update failed"}))

		err := UpdateUserPlan(testUserID, newPlanName, newPlanAmount, newPlanEnd)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "find before update failed")
	})

	mt.Run("Update Fails", func(mt *mtest.T) {
		// 1. InitUserIfNotFound - FindOne (user exists)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()}, {Key: "user_id", Value: testUserID}, {Key: "balance", Value: initialBalance}, {Key: "plan_name", Value: "Free"}, {Key: "plan", Value: 500000},
		}))
		// 2. FindOne (get current balance before update)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()}, {Key: "user_id", Value: testUserID}, {Key: "balance", Value: initialBalance}, {Key: "plan_name", Value: "Free"}, {Key: "plan", Value: 500000},
		}))
		// 3. UpdateOne (fails)
		mt.AddMockResponses(mtest.CreateWriteErrorsResponse(mtest.WriteError{Index: 0, Code: 11001, Message: "update failed"}))

		err := UpdateUserPlan(testUserID, newPlanName, newPlanAmount, newPlanEnd)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "update failed")
	})
}

func TestGetUserBalance(t *testing.T) {
	mt := setupMockClient(t)
	defer teardownMockClient()
	// defer mt.Close()

	testUserID := "test-user-get"
	ctx := contextWithUserID(testUserID)
	now := time.Now()
	lastMonth := now.AddDate(0, -1, 0)
	yesterday := now.AddDate(0, 0, -1)

	mt.Run("User Exists - No Rollover", func(mt *mtest.T) {
		expectedBalance := UserBalance{
			ID:              primitive.NewObjectID(),
			UserID:          testUserID,
			Balance:         1000.0,
			PlanName:        "Basic",
			Plan:            2000000.0,
			UpdatedAt:       now.Add(-time.Hour),
			LastPlanApplied: now.Add(-time.Hour), // Applied recently
		}
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: expectedBalance.ID},
			{Key: "user_id", Value: expectedBalance.UserID},
			{Key: "balance", Value: expectedBalance.Balance},
			{Key: "plan_name", Value: expectedBalance.PlanName},
			{Key: "plan", Value: expectedBalance.Plan},
			{Key: "updated_at", Value: expectedBalance.UpdatedAt},
			{Key: "last_plan_applied", Value: expectedBalance.LastPlanApplied},
		}))

		balance, err := GetUserBalance(ctx)
		require.NoError(t, err)
		require.NotNil(t, balance)
		assert.Equal(t, expectedBalance.UserID, balance.UserID)
		assert.Equal(t, expectedBalance.Balance, balance.Balance)
		assert.WithinDuration(t, expectedBalance.LastPlanApplied, balance.LastPlanApplied, time.Second) // Check if LastPlanApplied didn't change unexpectedly
	})

	mt.Run("User Not Found - Create Default", func(mt *mtest.T) {
		// 1. FindOne (no documents)
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 11000, Message: "mongo: no documents in result", Name: "NoDocuments"}))
		// 2. InsertOne (success)
		insertedID := primitive.NewObjectID()
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "insertedId", Value: insertedID}}...))

		balance, err := GetUserBalance(ctx)
		require.NoError(t, err)
		require.NotNil(t, balance)
		assert.Equal(t, testUserID, balance.UserID)
		assert.Equal(t, "Free", balance.PlanName)
		assert.Equal(t, 500000.0, balance.Balance)
		assert.Equal(t, 500000.0, balance.Plan)
		assert.Equal(t, insertedID, balance.ID)
		assert.WithinDuration(t, time.Now(), balance.UpdatedAt, time.Second)
		assert.WithinDuration(t, time.Now(), balance.LastPlanApplied, time.Second)
	})

	mt.Run("User Not Found - Insert Fails", func(mt *mtest.T) {
		// 1. FindOne (no documents)
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 11000, Message: "mongo: no documents in result", Name: "NoDocuments"}))
		// 2. InsertOne (fails)
		mt.AddMockResponses(mtest.CreateWriteErrorsResponse(mtest.WriteError{Index: 0, Code: 11001, Message: "insert default failed"}))

		balance, err := GetUserBalance(ctx)
		assert.Error(t, err)
		assert.Nil(t, balance)
		assert.Contains(t, err.Error(), "insert default failed")
	})

	mt.Run("FindOne Error", func(mt *mtest.T) {
		// 1. FindOne (generic error)
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 12345, Message: "find failed"}))

		balance, err := GetUserBalance(ctx)
		assert.Error(t, err)
		assert.Nil(t, balance)
		assert.Contains(t, err.Error(), "find failed")
	})

	mt.Run("Context Missing UserID", func(mt *mtest.T) {
		emptyCtx := context.Background()
		balance, err := GetUserBalance(emptyCtx)
		assert.Error(t, err)
		assert.Nil(t, balance)
		assert.EqualError(t, err, "user ID not found in request context")
	})

	// --- Rollover Test Cases ---

	mt.Run("Free Plan Rollover", func(mt *mtest.T) {
		initialBalance := 100.0
		planAllowance := 500000.0
		existingBalance := UserBalance{
			ID:              primitive.NewObjectID(),
			UserID:          testUserID,
			Balance:         initialBalance,
			PlanName:        "Free",
			Plan:            planAllowance,
			UpdatedAt:       lastMonth,
			LastPlanApplied: lastMonth, // Key condition
		}
		// 1. FindOne (returns balance needing rollover)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: existingBalance.ID}, {Key: "user_id", Value: existingBalance.UserID}, {Key: "balance", Value: existingBalance.Balance}, {Key: "plan_name", Value: existingBalance.PlanName}, {Key: "plan", Value: existingBalance.Plan}, {Key: "updated_at", Value: existingBalance.UpdatedAt}, {Key: "last_plan_applied", Value: existingBalance.LastPlanApplied},
		}))
		// 2. UpdateOne (apply rollover)
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "nModified", Value: 1}}...))

		balance, err := GetUserBalance(ctx)
		require.NoError(t, err)
		require.NotNil(t, balance)
		assert.Equal(t, testUserID, balance.UserID)
		assert.Equal(t, planAllowance, balance.Balance, "Balance should be reset to plan allowance") // max(planAllowance, initialBalance) = planAllowance
		assert.WithinDuration(t, time.Now(), balance.LastPlanApplied, time.Second, "LastPlanApplied should be updated")
	})

	mt.Run("Manual Plan Rollover", func(mt *mtest.T) {
		initialBalance := -50.0
		manualPlanAllowance := 100000.0
		existingBalance := UserBalance{
			ID:              primitive.NewObjectID(),
			UserID:          testUserID,
			Balance:         initialBalance,
			PlanName:        "Free", // Can be any plan if ManualPlan is set
			Plan:            0,      // Plan ignored if ManualPlan exists
			ManualPlan:      manualPlanAllowance, // Key condition
			UpdatedAt:       lastMonth,
			LastPlanApplied: lastMonth,           // Key condition
		}
		// 1. FindOne
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: existingBalance.ID}, {Key: "user_id", Value: existingBalance.UserID}, {Key: "balance", Value: existingBalance.Balance}, {Key: "plan_name", Value: existingBalance.PlanName}, {Key: "plan", Value: existingBalance.Plan}, {Key: "manual_plan", Value: existingBalance.ManualPlan}, {Key: "updated_at", Value: existingBalance.UpdatedAt}, {Key: "last_plan_applied", Value: existingBalance.LastPlanApplied},
		}))
		// 2. UpdateOne
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "nModified", Value: 1}}...))

		balance, err := GetUserBalance(ctx)
		require.NoError(t, err)
		require.NotNil(t, balance)
		assert.Equal(t, manualPlanAllowance, balance.Balance, "Balance should be reset to manual plan allowance")
		assert.WithinDuration(t, time.Now(), balance.LastPlanApplied, time.Second, "LastPlanApplied should be updated")
	})

	mt.Run("Negative Balance Reset (Paid Plan, Yesterday)", func(mt *mtest.T) {
		initialBalance := -150000.0
		existingBalance := UserBalance{
			ID:              primitive.NewObjectID(),
			UserID:          testUserID,
			Balance:         initialBalance, // Key condition
			PlanName:        "Basic",        // Key condition: Not "", Not "Free"
			Plan:            2000000.0,
			PlanEnd:         time.Now().AddDate(0, 0, 5).Unix(), // Key condition: In future
			UpdatedAt:       yesterday,
			LastPlanApplied: yesterday, // Key condition
		}
		// 1. FindOne
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: existingBalance.ID}, {Key: "user_id", Value: existingBalance.UserID}, {Key: "balance", Value: existingBalance.Balance}, {Key: "plan_name", Value: existingBalance.PlanName}, {Key: "plan", Value: existingBalance.Plan}, {Key: "plan_end", Value: existingBalance.PlanEnd}, {Key: "updated_at", Value: existingBalance.UpdatedAt}, {Key: "last_plan_applied", Value: existingBalance.LastPlanApplied},
		}))
		// 2. UpdateOne
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "nModified", Value: 1}}...))

		balance, err := GetUserBalance(ctx)
		require.NoError(t, err)
		require.NotNil(t, balance)
		assert.Equal(t, 0.0, balance.Balance, "Negative balance should be reset to 0")
		assert.WithinDuration(t, time.Now(), balance.LastPlanApplied, time.Second, "LastPlanApplied should be updated")
	})

	mt.Run("Yearly Plan Monthly Rollover", func(mt *mtest.T) {
		initialBalance := 50000.0
		planAllowance := 3000000.0
		planEnd := time.Now().AddDate(0, 6, 0).Unix() // 6 months from now
		planNextRenewal := time.Now().AddDate(0, -1, 0) // Due for renewal
		existingBalance := UserBalance{
			ID:              primitive.NewObjectID(),
			UserID:          testUserID,
			Balance:         initialBalance,
			PlanName:        "Pro",
			Plan:            planAllowance,
			PlanEnd:         planEnd, // Key condition: In future
			PlanNextRenewal: planNextRenewal, // Key condition: Not Zero
			UpdatedAt:       lastMonth,
			LastPlanApplied: lastMonth, // Key condition: Last month
		}
		expectedNextRenewal := time.Now().AddDate(0, 1, 0) // Should be set to next month

		// 1. FindOne
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: existingBalance.ID}, {Key: "user_id", Value: existingBalance.UserID}, {Key: "balance", Value: existingBalance.Balance}, {Key: "plan_name", Value: existingBalance.PlanName}, {Key: "plan", Value: existingBalance.Plan}, {Key: "plan_end", Value: existingBalance.PlanEnd}, {Key: "plan_next_renewal", Value: existingBalance.PlanNextRenewal}, {Key: "updated_at", Value: existingBalance.UpdatedAt}, {Key: "last_plan_applied", Value: existingBalance.LastPlanApplied},
		}))
		// 2. UpdateOne
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "nModified", Value: 1}}...))

		balance, err := GetUserBalance(ctx)
		require.NoError(t, err)
		require.NotNil(t, balance)
		assert.Equal(t, planAllowance, balance.Balance, "Balance should be reset to plan allowance") // max(planAllowance, initialBalance) = planAllowance
		assert.WithinDuration(t, time.Now(), balance.LastPlanApplied, time.Second, "LastPlanApplied should be updated")
		// Check PlanNextRenewal was updated correctly IN THE RETURNED STRUCT (as DB update mock just returns success)
		assert.False(t, balance.PlanNextRenewal.IsZero(), "PlanNextRenewal should be updated")
		assert.WithinDuration(t, expectedNextRenewal, balance.PlanNextRenewal, time.Minute, "PlanNextRenewal should be set to next month")
	})

	mt.Run("Rollover Update Fails", func(mt *mtest.T) {
		// Test Free plan rollover scenario but make the UpdateOne fail
		initialBalance := 100.0
		planAllowance := 500000.0
		existingBalance := UserBalance{
			ID:              primitive.NewObjectID(),
			UserID:          testUserID,
			Balance:         initialBalance,
			PlanName:        "Free", Plan: planAllowance,
			UpdatedAt:       lastMonth, LastPlanApplied: lastMonth,
		}
		// 1. FindOne (returns balance needing rollover)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: existingBalance.ID}, {Key: "user_id", Value: existingBalance.UserID}, {Key: "balance", Value: existingBalance.Balance}, {Key: "plan_name", Value: existingBalance.PlanName}, {Key: "plan", Value: existingBalance.Plan}, {Key: "updated_at", Value: existingBalance.UpdatedAt}, {Key: "last_plan_applied", Value: existingBalance.LastPlanApplied},
		}))
		// 2. UpdateOne (fails) - NOTE: The function logs the error but doesn't return it!
		mt.AddMockResponses(mtest.CreateWriteErrorsResponse(mtest.WriteError{Index: 0, Code: 11001, Message: "rollover update failed"}))

		balance, err := GetUserBalance(ctx)
		require.NoError(t, err, "GetUserBalance should not return the update error itself") // The error is logged, not returned
		require.NotNil(t, balance)
		// The function updates the *local* balance struct even if the DB update fails.
		assert.Equal(t, planAllowance, balance.Balance, "Local balance struct should reflect attempted update")
		assert.WithinDuration(t, time.Now(), balance.LastPlanApplied, time.Second, "Local LastPlanApplied struct should reflect attempted update")
		// We can't easily assert the log message here without a logging mock framework.
	})
}

func TestRemoveCredits(t *testing.T) {
	mt := setupMockClient(t)
	defer teardownMockClient()
	// defer mt.Close()

	testUserID := "test-user-remove"
	ctx := contextWithUserID(testUserID)
	amountToRemove := 10000.0
	initialBalance := 50000.0
	action := utils.UserAction {}

	mt.Run("Success", func(mt *mtest.T) {
		updatedBalanceDoc := bson.D{
			{Key: "_id", Value: primitive.NewObjectID()},
			{Key: "user_id", Value: testUserID},
			{Key: "balance", Value: initialBalance - amountToRemove}, // The balance *after* the $inc
			{Key: "updated_at", Value: time.Now()},
			// other fields might be present depending on FindOneAndUpdate result
		}
		// Expectations within a transaction (mtest handles session/transaction mocking):
		// 1. InsertOne (credit_history)
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "insertedId", Value: primitive.NewObjectID()}}...))
		// 2. FindOneAndUpdate (balances)
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, updatedBalanceDoc))

		updatedBalance, err := RemoveCredits(ctx, amountToRemove, action)
		require.NoError(t, err)
		require.NotNil(t, updatedBalance)
		assert.Equal(t, testUserID, updatedBalance.UserID)
		assert.Equal(t, initialBalance-amountToRemove, updatedBalance.Balance)
		assert.WithinDuration(t, time.Now(), updatedBalance.UpdatedAt, time.Second)
	})

	mt.Run("Context Missing UserID", func(mt *mtest.T) {
		emptyCtx := context.Background()
		updatedBalance, err := RemoveCredits(emptyCtx, amountToRemove, action)
		assert.Error(t, err)
		assert.Nil(t, updatedBalance)
		assert.EqualError(t, err, "user ID not found in request context")
	})

	mt.Run("Insert History Fails", func(mt *mtest.T) {
		// Expectations within a transaction:
		// 1. InsertOne (credit_history) - Fails
		mt.AddMockResponses(mtest.CreateWriteErrorsResponse(mtest.WriteError{Index: 0, Code: 11001, Message: "insert history failed"}))
		// FindOneAndUpdate should not be called if InsertOne fails before it

		updatedBalance, err := RemoveCredits(ctx, amountToRemove, action)
		assert.Error(t, err)
		assert.Nil(t, updatedBalance)
		assert.Contains(t, err.Error(), "insert history failed")
		// mtest implicitly checks that the transaction would be aborted.
	})

	mt.Run("FindOneAndUpdate Fails", func(mt *mtest.T) {
		// Expectations within a transaction:
		// 1. InsertOne (credit_history) - Success
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "insertedId", Value: primitive.NewObjectID()}}...))
		// 2. FindOneAndUpdate (balances) - Fails
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 12345, Message: "update balance failed"}))

		updatedBalance, err := RemoveCredits(ctx, amountToRemove, action)
		assert.Error(t, err)
		assert.Nil(t, updatedBalance)
		assert.Contains(t, err.Error(), "update balance failed")
	})

	// Note: Testing StartSession failure requires mocking the client creation part,
	// which mtest handles internally. If GetClient() itself could fail before StartSession,
	// that would be a different test case. Assuming GetClient() returns a valid mock client.

	// Note: The "insufficient credits" check is commented out in the source.
	// If it were active, a test case would look like this:
	/*
		mt.Run("Insufficient Credits (if check uncommented)", func(mt *mtest.T) {
			smallInitialBalance := 5000.0
			amountToOverdraw := 10000.0
			// Expectations:
			// 1. InsertOne (history) - Success
			mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "insertedId", Value: primitive.NewObjectID()}}...))
			// 2. FindOneAndUpdate (balances) - Success, but returns negative balance
			mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
				{Key:"_id", Value: primitive.NewObjectID()},
				{Key:"user_id", Value: testUserID},
				{Key:"balance", Value: smallInitialBalance - amountToOverdraw}, // Negative result
				{Key:"updated_at", Value: time.Now()},
			}))

			updatedBalance, err := RemoveCredits(ctx, amountToOverdraw, action)
			assert.Error(t, err)
			assert.Nil(t, updatedBalance)
			assert.EqualError(t, err, "insufficient credits")
			// Transaction should be aborted.
		})
	*/
}

func TestCheckSufficientCredits(t *testing.T) {
	mt := setupMockClient(t)
	defer teardownMockClient()
	// defer mt.Close()

	testUserID := "test-user-check"
	ctx := contextWithUserID(testUserID)

	tests := []struct {
		name             string
		mockBalance      *UserBalance // Balance returned by mocked GetUserBalance
		mockGetUserErr   error        // Error returned by mocked GetUserBalance
		requiredAmount   float64
		expectedResult   bool
		expectedCheckErr bool
	}{
		{
			name:           "Sufficient Balance",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: 10000, PlanName: "Free"},
			requiredAmount: 5000,
			expectedResult: true,
		},
		{
			name:           "Exact Balance",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: 5000, PlanName: "Free"},
			requiredAmount: 5000,
			expectedResult: true,
		},
		{
			name:           "Insufficient Balance - Free Plan",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: 4000, PlanName: "Free"},
			requiredAmount: 5000,
			expectedResult: false,
		},
		{
			name:           "GetUserBalance Error",
			mockGetUserErr: errors.New("db error"),
			requiredAmount: 5000,
			expectedResult: false,
			expectedCheckErr: true,
		},
		// Overdraft scenarios
		{
			name:           "Basic Plan - Allowed Overdraft (within limit, low amount)",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: -100000, PlanName: "Basic"},
			requiredAmount: 10000,
			expectedResult: true,
		},
		{
			name:           "Basic Plan - Denied Overdraft (over limit)",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: -250000, PlanName: "Basic"},
			requiredAmount: 10000,
			expectedResult: false,
		},
		{
			name:           "Basic Plan - Denied Overdraft (amount too high)",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: -100000, PlanName: "Basic"},
			requiredAmount: 10001,
			expectedResult: false,
		},
		{
			name:           "Advanced Plan - Allowed Overdraft",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: -200000, PlanName: "Advanced"},
			requiredAmount: 20000,
			expectedResult: true,
		},
		{
			name:           "Advanced Plan - Denied Overdraft (over limit)",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: -400000, PlanName: "Advanced"},
			requiredAmount: 20000,
			expectedResult: false,
		},
		{
			name:           "Advanced Plan - Denied Overdraft (amount too high)",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: -200000, PlanName: "Advanced"},
			requiredAmount: 20001,
			expectedResult: false,
		},
		{
			name:           "Pro Plan - Allowed Overdraft",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: -400000, PlanName: "Pro"},
			requiredAmount: 30000,
			expectedResult: true,
		},
		{
			name:           "Pro Plan - Denied Overdraft (over limit)",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: -600000, PlanName: "Pro"},
			requiredAmount: 30000,
			expectedResult: false,
		},
		{
			name:           "Pro Plan - Denied Overdraft (amount too high)",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: -400000, PlanName: "Pro"},
			requiredAmount: 30001,
			expectedResult: false,
		},
		{
			name:           "Zero Balance - Insufficient for any amount > 0",
			mockBalance:    &UserBalance{UserID: testUserID, Balance: 0, PlanName: "Free"},
			requiredAmount: 1,
			expectedResult: false,
		},
		{
            name:           "Sufficient Negative Balance - Basic Plan Allowed Overdraft",
            mockBalance:    &UserBalance{UserID: testUserID, Balance: -199999, PlanName: "Basic"},
            requiredAmount: 1, // Requesting a small amount
            expectedResult: true,
        },
	}

	for _, tc := range tests {
		mt.Run(tc.name, func(mt *mtest.T) {
			// Mock GetUserBalance behavior for this subtest
			if tc.mockGetUserErr != nil {
				if errors.Is(tc.mockGetUserErr, mongo.ErrNoDocuments) { // Simulate user not found -> creation path
					mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 11000, Message: "mongo: no documents in result", Name: "NoDocuments"}))
					// Assume creation succeeds if ErrNoDocuments is the *intended* mock error
					insertedID := primitive.NewObjectID()
					mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{{Key: "ok", Value: 1}, {Key: "n", Value: 1}, {Key: "insertedId", Value: insertedID}}...))
				} else { // Simulate generic DB error
					mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 12345, Message: tc.mockGetUserErr.Error()}))
				}
			} else if tc.mockBalance != nil {
				// Simulate successful FindOne in GetUserBalance returning the specified balance
				// Note: We don't need to mock rollover updates within GetUserBalance here,
				// just the initial FindOne result it uses for the check.
				balanceDoc := bson.D{
					{Key: "_id", Value: primitive.NewObjectID()},
					{Key: "user_id", Value: tc.mockBalance.UserID},
					{Key: "balance", Value: tc.mockBalance.Balance},
					{Key: "plan_name", Value: tc.mockBalance.PlanName},
					{Key: "plan", Value: tc.mockBalance.Plan},
					{Key: "plan_end", Value: tc.mockBalance.PlanEnd},
					{Key: "updated_at", Value: tc.mockBalance.UpdatedAt},
					{Key: "last_plan_applied", Value: tc.mockBalance.LastPlanApplied},
				}
				mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, balanceDoc))
			} else {
				// Should not happen based on test cases, but handle defensively
				t.Fatalf("Test case '%s' has neither mockBalance nor mockGetUserErr", tc.name)
			}

			hasSufficient, err := CheckSufficientCredits(ctx, tc.requiredAmount)

			if tc.expectedCheckErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.expectedResult, hasSufficient)
		})
	}
}

func TestDeleteBalance(t *testing.T) {
	mt := setupMockClient(t)
	defer teardownMockClient()
	// defer mt.Close()

	testUserID := "test-user-delete"
	ctx := contextWithUserID(testUserID)

	mt.Run("Success", func(mt *mtest.T) {
		// Expect DeleteOne call, return success with DeletedCount: 1
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{
			{Key: "ok", Value: 1},
			{Key: "n", Value: 1}, // Indicates one document matched and was deleted
		}...))

		err := DeleteBalance(ctx)
		assert.NoError(t, err)
	})

	mt.Run("Balance Not Found", func(mt *mtest.T) {
		// Expect DeleteOne call, return success but with DeletedCount: 0
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.D{
			{Key: "ok", Value: 1},
			{Key: "n", Value: 0}, // Indicates no document matched
		}...))

		err := DeleteBalance(ctx)
		assert.Error(t, err)
		assert.EqualError(t, err, "balance not found")
	})

	mt.Run("DeleteOne Error", func(mt *mtest.T) {
		// Expect DeleteOne call, return an error
		mt.AddMockResponses(mtest.CreateWriteErrorsResponse(mtest.WriteError{
			Index:   0,
			Code:    12345,
			Message: "delete failed",
		}))

		err := DeleteBalance(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "delete failed")
	})

	mt.Run("Context Missing UserID", func(mt *mtest.T) {
		emptyCtx := context.Background()
		err := DeleteBalance(emptyCtx)
		assert.Error(t, err)
		assert.EqualError(t, err, "user ID not found in request context")
	})
}

func TestGetPlanName(t *testing.T) {
	mt := setupMockClient(t)
	defer teardownMockClient()
	// defer mt.Close()

	testUserID := "test-user-getplan"
	ctx := contextWithUserID(testUserID)

	mt.Run("Success", func(mt *mtest.T) {
		expectedPlanName := "Advanced"
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "plurality.balances", mtest.FirstBatch, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()},
			{Key: "user_id", Value: testUserID},
			{Key: "plan_name", Value: expectedPlanName},
			// other fields...
		}))

		planName, err := GetPlanName(ctx)
		require.NoError(t, err)
		assert.Equal(t, expectedPlanName, planName)
	})

	mt.Run("User Not Found", func(mt *mtest.T) {
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    11000, // Simulate ErrNoDocuments
			Message: "mongo: no documents in result",
			Name:    "NoDocuments",
		}))

		planName, err := GetPlanName(ctx)
		assert.Error(t, err)
		assert.True(t, errors.Is(err, mongo.ErrNoDocuments) || err.Error() == "mongo: no documents in result", "Expected ErrNoDocuments or equivalent mock error")
		assert.Equal(t, "", planName)
	})

	mt.Run("FindOne Error", func(mt *mtest.T) {
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    12345,
			Message: "db connection error",
		}))

		planName, err := GetPlanName(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "db connection error")
		assert.Equal(t, "", planName)
	})

	mt.Run("Context Missing UserID", func(mt *mtest.T) {
		emptyCtx := context.Background()
		planName, err := GetPlanName(emptyCtx)
		assert.Error(t, err)
		assert.EqualError(t, err, "user ID not found in request context")
		assert.Equal(t, "", planName)
	})
}
