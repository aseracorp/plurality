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
	UpdatedAt time.Time          `json:"updated_at" bson:"updated_at"`

	Balance   float64            `json:"balance" bson:"balance"`
	Plan 		  float64            `json:"plan" bson:"plan,omitempty"`
	PlanName 		  string         `json:"plan_name" bson:"plan_name,omitempty"`
	PlanEnd  		  int64          `json:"plan_end" bson:"plan_end,omitempty"`
	PlanNextRenewal time.Time    `json:"plan_next_renewal" bson:"plan_next_renewal,omitempty"`

	ManualPlan 		  float64      `json:"manual_plan" bson:"manual_plan,omitempty"`
	LastPlanApplied time.Time    `json:"last_plan_applied" bson:"last_plan_applied,omitempty"`
}

func InitUserIfNotFound(userId string) error {
	client := GetClient()
	collection := client.Database("plurality").Collection("balances")
	
	err := collection.FindOne(context.Background(), bson.M{"user_id": userId}).Err()
	if err == mongo.ErrNoDocuments {
		utils.Log("No balance record found for user %s, creating new record", userId)
		balance := UserBalance{
			UserID:    userId,
			PlanName: "Free",
			Balance:   500000,
			Plan:   500000,
			UpdatedAt: time.Now(),
			LastPlanApplied: time.Now(),
		}
		_, err := collection.InsertOne(context.Background(), balance)
		if err != nil {
			return err
		}
		utils.Log("New balance record created for user %s", userId)
	} else if err != nil {
		return err
	}

	return nil
}

func UpdateUserPlan(userId string, planName string, plan float64, planEnd int64) error {
	utils.Log("Updating user %s plan to %s with plan %f", userId, planName, plan)
	
	client := GetClient()
	collection := client.Database("plurality").Collection("balances")

	err := InitUserIfNotFound(userId)

	if err != nil {
		return err
	}

	// get user balance. Only update Plan if the name is different
	if err != nil {
		return err
	}

	var balance UserBalance
	err = collection.FindOne(context.Background(), bson.M{"user_id": userId}).Decode(&balance)

	if err != nil {
		return err
	}

	finalBalance := max(balance.Balance, plan)

	planNextRenewal := time.Time{}
	// if yearly plan (plan end is in more than a month and a day), set planNextRenewal to the same date next month
	if planEnd > time.Now().AddDate(0, 1, 0).Unix() {
		utils.Log("PlanEnd is more than a month and a day, setting planNextRenewal to the same date next month for user %s", userId)
		planNextRenewal = time.Now().AddDate(0, 1, 0)
	}
	

	// If plan did not change, do not update the plan's allowance
	if balance.PlanName == planName {
		utils.Log("Plan did not change for user %s, updating only the balance", userId)
		_, err = collection.UpdateOne(
			context.Background(), 
			bson.M{"user_id": userId},
			bson.M{"$set": bson.M{
				"plan_name": planName,
				"plan_end": planEnd,
				"updated_at": time.Now(),
				"balance": finalBalance,
				"manual_plan": 0,
				"last_plan_applied": time.Now(),
				"plan_next_renewal": planNextRenewal,
		}})
	
		if err != nil {
			return err
		}
	} else {
		utils.Log("Plan changed for user %s, updating the plan's allowance", userId)
		_, err = collection.UpdateOne(
			context.Background(), 
			bson.M{"user_id": userId},
			bson.M{"$set": bson.M{
				"plan_name": planName,
				"plan": plan,
				"balance": finalBalance,
				"plan_end": planEnd,
				"updated_at": time.Now(),
				"manual_plan": 0,
				"last_plan_applied": time.Now(),
				"plan_next_renewal": planNextRenewal,
		}})
	
		if err != nil {
			return err
		}
	}

	utils.Log("User %s plan updated to %s with plan %f", userId, planName, plan)

	return nil
}

func checkPeriodEnd(periodEnd int64) bool {
	currentTime := time.Now().Unix()
	return periodEnd > currentTime
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
			Balance:   500000,
			Plan:   500000,
			UpdatedAt: time.Now(),
			LastPlanApplied: time.Now(),
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

	// if LastPlanApplied was last Month, set Balance to plan's allowance
	if balance.LastPlanApplied.Month() != time.Now().Month() && balance.PlanName == "Free" && balance.Plan != 0 {
		utils.Log("LastPlanApplied was last Month, setting Free Balance to plan's allowance for user %s", userID)
		planAllowance := balance.Plan
		balance.Balance = max(planAllowance, balance.Balance)
		balance.LastPlanApplied = time.Now()
		_, err :=
			collection.UpdateOne(ctx, bson.M{"user_id": userID}, bson.M{"$set": bson.M{"balance": balance.Plan, "last_plan_applied": time.Now()}})
		if err != nil {
			utils.Error("Error updating balance: %v", err)
		}
	} else if balance.LastPlanApplied.Month() != time.Now().Month() && balance.ManualPlan != 0 {
		// if LastPlanApplied was last Month, set Balance to ManualPlan
		utils.Log("LastPlanApplied was last Month, setting Balance to ManualPlan for user %s", userID)
		balance.Balance = max(balance.ManualPlan, balance.Balance)
		balance.LastPlanApplied = time.Now()
		_, err :=
			collection.UpdateOne(ctx, bson.M{"user_id": userID}, bson.M{"$set": bson.M{"balance": balance.ManualPlan, "last_plan_applied": time.Now()}})
		if err != nil {
			utils.Error("Error updating balance: %v", err)
		}
	} 
	
	// if LastPlanApplied was yesterday, and balance is negative, set to 0
	if balance.LastPlanApplied.Day() != time.Now().Day() && balance.PlanName != "" && balance.PlanName != "Free" && balance.Balance < 0 && checkPeriodEnd(balance.PlanEnd) {
		utils.Log("LastPlanApplied was yesterday, setting Balance to 0 for user %s", userID)
		balance.Balance = 0
		balance.LastPlanApplied = time.Now()
		_, err :=
			collection.UpdateOne(ctx, bson.M{"user_id": userID}, bson.M{"$set": bson.M{"balance": 0, "last_plan_applied": time.Now()}})
		if err != nil {
			utils.Error("Error updating balance: %v", err)
		}
	}

	// monthly allowance of yearly plan
	// if planNextRenewal is not 0 and planEnd is not 0, and planEnd is less than now, apply the plan if it wasnt applied yet
	if !balance.PlanNextRenewal.IsZero() && balance.PlanEnd != 0 && checkPeriodEnd(balance.PlanEnd) && balance.LastPlanApplied.Month() != time.Now().Month() {
		utils.Log("PlanNextRenewal is not 0 and planEnd is not 0, and planEnd is less than now, applying the plan for user %s", userID)
		balance.Balance = max(balance.Plan, balance.Balance)
		balance.LastPlanApplied = time.Now()

		if balance.PlanEnd > time.Now().AddDate(0, 1, 0).Unix() {
			utils.Log("PlanEnd is more than a month and a day, setting planNextRenewal to the same date next month for user %s", userID)
			balance.PlanNextRenewal = time.Now().AddDate(0, 1, 0)
		} else {
			utils.Log("PlanEnd is less than a month and a day, setting planNextRenewal to 0 for user %s", userID)
			balance.PlanNextRenewal = time.Time{}
		}

		balance.UpdatedAt = time.Now()

		_, err :=
			collection.UpdateOne(ctx, bson.M{"user_id": userID}, bson.M{"$set": bson.M{"balance": balance.Plan, "last_plan_applied": time.Now()}})
		if err != nil {
			utils.Error("Error updating balance: %v", err)
		}
	}

	if err != nil {
		return nil, err
	}

	return &balance, nil
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

// CheckSufficientCredits checks if a user has enough credits for an operation
func CheckSufficientCredits(ctx context.Context, requiredAmount float64) (bool, error) {
	balance, err := GetUserBalance(ctx)
	if err != nil {
		return false, err
	}

	if balance.Balance >= requiredAmount {
		return true, nil
	}

	if balance.PlanName == "Basic" && balance.Balance >= -200000 && requiredAmount <= 10000 {
		return true, nil
	}
	if balance.PlanName == "Advanced" && balance.Balance >= -300000 && requiredAmount <= 20000 {
		return true, nil
	}
	if balance.PlanName == "Pro" && balance.Balance >= -500000 && requiredAmount <= 30000 {
		return true, nil
	}

	return false, nil
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

func GetPlanName(ctx context.Context) (string, error) {
	client := GetClient()
	collection := client.Database("plurality").Collection("balances")
	userID, ok := ctx.Value("userID").(string)

	if !ok {
		return "", errors.New("user ID not found in request context")
	}

	var balance UserBalance
	err := collection.FindOne(ctx, bson.M{"user_id": userID}).Decode(&balance)

	if err != nil {
		return "", err
	}

	return balance.PlanName, nil
}