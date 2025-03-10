package db

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/azukaar/plurality/src/utils"
)

// CreditTransaction represents a credit transaction record
type CreditTransaction struct {
	ID          primitive.ObjectID `bson:"_id,omitempty"`
	UserID      string             `bson:"user_id"`
	Amount      float64            `bson:"amount"`
	Description string             `bson:"description"`
	PaymentID   string             `bson:"payment_id,omitempty"`
	CreatedAt   time.Time          `bson:"created_at"`
	Action      utils.UserAction				 `bson:"action"`
}

// UserBalance represents the current balance of a user
type UserBalance struct {
	ID        primitive.ObjectID `json:"_" bson:"_id,omitempty"`
	UserID    string             `json:"_" bson:"user_id"`
	Balance   float64            `json:"balance" bson:"balance"`
	UpdatedAt time.Time          `json:"updated_at" bson:"updated_at"`
	Plan 		  float64            `json:"plan" bson:"plan,omitempty"`
	ManualPlan 		  float64      `json:"manual_plan" bson:"manual_plan,omitempty"`
	LastManualPlan 		  time.Time      `json:"last_manual_plan" bson:"last_manual_plan,omitempty"`
	PlanName 		  string      `json:"plan_name" bson:"plan_name,omitempty"`
}

// GetUserBalance retrieves the current balance for a user
func GetUserBalance(ctx context.Context) (*UserBalance, error) {
	client := GetClient()
	collection := client.Database("plurality").Collection("balances")
	userID, ok := ctx.Value("userID").(string)

	if !ok {
		return nil, errors.New("user ID not found in request context")
	}

	var balance UserBalance
	err := collection.FindOne(ctx, bson.M{"user_id": userID}).Decode(&balance)

	// If no balance record exists, create one with free plan
	if err == mongo.ErrNoDocuments {
		utils.Log("No balance record found for user %s, creating new record", userID)

		balance = UserBalance{
			UserID:    userID,
			PlanName: "Free",
			// TODO : DOWNGRADE TO 100k
			Balance:   100000,
			Plan:   100000,
			ManualPlan:   100000,
			UpdatedAt: time.Now(),
			LastManualPlan: time.Now(),
		}

		result, err := collection.InsertOne(ctx, balance)
		if err != nil {
			return nil, err
		}

		if oid, ok := result.InsertedID.(primitive.ObjectID); ok {
			balance.ID = oid
		}

		return &balance, nil
	}

	// if LastManualPlan was last Month, set Balance to ManualPlan
	if balance.LastManualPlan.Month() != time.Now().Month() {
		utils.Log("LastManualPlan was last Month, setting Balance to ManualPlan for user %s", userID)
		balance.Balance = balance.ManualPlan
		balance.LastManualPlan = time.Now()
		_, err :=
			collection.UpdateOne(ctx, bson.M{"user_id": userID}, bson.M{"$set": bson.M{"balance": balance.ManualPlan, "last_manual_plan": time.Now()}})
		if err != nil {
			utils.Error("Error updating balance: %v", err)
		}
	}

	if err != nil {
		return nil, err
	}

	return &balance, nil
}

// AddCredits adds credits to a user's balance and records the transaction
func AddCredits(ctx context.Context, amount float64, description string, paymentID string) (*UserBalance, error) {
	return updateCredits(ctx, amount, description, paymentID, utils.UserAction{})
}

// RemoveCredits removes credits from a user's balance and records the transaction
func RemoveCredits(ctx context.Context, amount float64, action utils.UserAction) (*UserBalance, error) {
	description := "Use Credits"
	return updateCredits(ctx, -amount, description, "", action)
}

// updateCredits is a helper function that handles both adding and removing credits
func updateCredits(ctx context.Context, amount float64, description string, paymentID string, action utils.UserAction) (*UserBalance, error) {
	client := GetClient()
	balanceCollection := client.Database("plurality").Collection("balances")
	historyCollection := client.Database("plurality").Collection("credit_history")
	userID, ok := ctx.Value("userID").(string)

	utils.Log("Updating credits for user %s with amount %f for action '%s'", userID, amount, description)

	if !ok {
		return nil, errors.New("user ID not found in request context")
	}

	// Start a session for transaction
	session, err := client.StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(ctx)

	var updatedBalance *UserBalance
	err = mongo.WithSession(ctx, session, func(sessionContext mongo.SessionContext) error {
		// Begin transaction
		if err := session.StartTransaction(); err != nil {
			return err
		}

		// Create the transaction record
		transaction := CreditTransaction{
			UserID:      userID,
			Amount:      amount,
			Description: description,
			PaymentID:   paymentID,
			Action:      action,
			CreatedAt:   time.Now(),
		}

		// Insert transaction history
		_, err := historyCollection.InsertOne(sessionContext, transaction)
		if err != nil {
			return err
		}

		// Update user balance with upsert option
		opts := options.FindOneAndUpdate().SetReturnDocument(options.After).SetUpsert(true)
		currentTime := time.Now()

		var result UserBalance
		err = balanceCollection.FindOneAndUpdate(
			sessionContext,
			bson.M{"user_id": userID},
			bson.M{
				"$inc": bson.M{"balance": amount},
				"$set": bson.M{"updated_at": currentTime},
				"$setOnInsert": bson.M{
					"user_id": userID,
				},
			},
			opts,
		).Decode(&result)

		if err != nil {
			return err
		}

		// Check if balance would go negative
		// if result.Balance < 0 {
		// 	return errors.New("insufficient credits")
		// }

		updatedBalance = &result
		
		// Commit transaction
		return session.CommitTransaction(sessionContext)
	})

	if err != nil {
		// Abort transaction if an error occurred
		abortErr := session.AbortTransaction(ctx)
		if abortErr != nil {
			utils.Debug("Error aborting transaction: %v", abortErr)
		}
		return nil, err
	}

	return updatedBalance, nil
}

// GetCreditHistory retrieves the credit transaction history for a user
func GetCreditHistory(ctx context.Context, limit int64, skip int64) ([]CreditTransaction, error) {
	client := GetClient()
	collection := client.Database("plurality").Collection("credit_history")
	userID, ok := ctx.Value("userID").(string)

	if !ok {
		return nil, errors.New("user ID not found in request context")
	}

	findOptions := options.Find()
	findOptions.SetSort(bson.D{{Key: "created_at", Value: -1}}) // Sort by created_at descending
	
	if limit > 0 {
		findOptions.SetLimit(limit)
	}
	
	if skip > 0 {
		findOptions.SetSkip(skip)
	}

	cursor, err := collection.Find(ctx, bson.M{"user_id": userID}, findOptions)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var transactions []CreditTransaction
	if err = cursor.All(ctx, &transactions); err != nil {
		return nil, err
	}

	return transactions, nil
}

// CheckSufficientCredits checks if a user has enough credits for an operation
func CheckSufficientCredits(ctx context.Context, requiredAmount float64) (bool, error) {
	balance, err := GetUserBalance(ctx)
	if err != nil {
		return false, err
	}

	return balance.Balance >= requiredAmount, nil
}

// Delete balnce
func DeleteBalance(ctx context.Context) error {
	client := GetClient()
	collection := client.Database("plurality").Collection("balances")
	userID, ok := ctx.Value("userID").(string)

	utils.Log("Deleting balance for user %s", userID)

	if !ok {
		return errors.New("user ID not found in request context")
	}

	c, err := collection.DeleteOne(ctx, bson.M{"user_id": userID})

	// count 
	if c.DeletedCount == 0 {
		return errors.New("balance not found")
	}

	return err
}