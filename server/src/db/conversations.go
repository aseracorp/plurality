package db

import (
	"context"
	"time"
  "errors"
  "sort"

	"go.mongodb.org/mongo-driver/bson"
  "go.mongodb.org/mongo-driver/mongo/options"
  "go.mongodb.org/mongo-driver/bson/primitive"
	
	"github.com/azukaar/plurality/src/utils"
)

// push new message to conversation (or create new conversation if none)
func PushMessage(ctx context.Context, conversation utils.Conversation, message utils.Message) (utils.Conversation, bool, error) {
  client := GetClient()
  collection := client.Database("plurality").Collection("conversations")
	userID, ok := ctx.Value("userID").(string)

  if !ok {
    return utils.Conversation{}, false, errors.New("user ID not found in request context")
  }
 
  // Get current time for LastMessageAt update
  currentTime := time.Now()

  if conversation.ID == primitive.NilObjectID {
    // For new conversations, set the LastMessageAt field
    conversation.LastMessageAt = currentTime
    conversation.UserID = userID
    
    // Ensure the message is in the messages array
    conversation.Messages = append(conversation.Messages, message)
   
    // Insert the new conversation
    result, err := collection.InsertOne(ctx, conversation)
    if err != nil {
      return utils.Conversation{}, false, err
    }
   
    // Extract the ID from the InsertOne result and set it on the returned conversation
    if oid, ok := result.InsertedID.(primitive.ObjectID); ok {
      conversation.ID = oid
    }

    utils.Log("Created new conversation ID: ", conversation.ID, " for user ID: ", userID)
   
    return conversation, true, nil
  } else {
    // For existing conversations, push the new message and update LastMessageAt
    // Options to return the document after update
    opts := options.FindOneAndUpdate().SetReturnDocument(options.After)

    utils.Debug("Pushing message to conversation ID: ", conversation.ID, " for user ID: ", userID)

    // Perform FindOneAndUpdate to get the updated document
    var updatedConversation utils.Conversation
    toSet := bson.M{
      "last_message_at": currentTime,
    }

    if conversation.Title != "" {
      toSet["title"] = conversation.Title
    }

    if conversation.ModelSelected.Text.Name != "" {
      toSet["model_selected"] = conversation.ModelSelected
    }

    err := collection.FindOneAndUpdate(
      ctx,
      bson.M{
        "_id": conversation.ID,
        "user_id": userID,
      },
      bson.M{
        "$push": bson.M{"messages": message},
        "$set": toSet,
      },
      opts,
    ).Decode(&updatedConversation)
   
    if err != nil {
      return utils.Conversation{}, false, err
    }

    utils.Debug("Updated conversation: ", updatedConversation.UserID, updatedConversation.LastMessageAt)
   
    return updatedConversation, false, nil
  }
}

func ListConversations(ctx context.Context) ([]utils.Conversation, error) {
  client := GetClient()
  collection := client.Database("plurality").Collection("conversations")
  userID, ok := ctx.Value("userID").(string)

  if !ok {
    return nil, errors.New("user ID not found in request context")
  }

  cursor, err := collection.Find(ctx, bson.M{"user_id": userID})
  if err != nil {
    return nil, err
  }

  var conversations []utils.Conversation
  if err = cursor.All(ctx, &conversations); err != nil {
    return nil, err
  }

  // sort conversations by LastMessageAt
  sort.Slice(conversations, func(i, j int) bool {
      return conversations[i].LastMessageAt.After(conversations[j].LastMessageAt)
  })

  // remove messages from conversations
  for i := range conversations {
      conversations[i].Messages = nil
  }

  return conversations, nil
}

func DeleteConversation(ctx context.Context, id string) error {
  client := GetClient()
  collection := client.Database("plurality").Collection("conversations")
  userID, ok := ctx.Value("userID").(string)

  if !ok {
    return errors.New("user ID not found in request context")
  }

  utils.Debug("Deleting conversation ID %s for user ID %s", id, userID)

  i, _ := primitive.ObjectIDFromHex(id)

  res, err := collection.DeleteOne(ctx, bson.M{
    // convert to ObjectID
    "_id": i,
    "user_id": userID,
  })

  // check res
  if res.DeletedCount == 0 {
    return errors.New("conversation not found")
  }

  return err
}

func GetConversationById(ctx context.Context, id string) (*utils.Conversation, error) {
	client := GetClient()
  userID, ok := ctx.Value("userID").(string)

  if !ok {
    return nil, errors.New("user ID not found in request context")
  }

	collection := client.Database("plurality").Collection("conversations")

  i, _ := primitive.ObjectIDFromHex(id)

	var conversation utils.Conversation
	err := collection.FindOne(ctx, bson.M{
    "_id": i,
    "user_id": userID,
  }).Decode(&conversation)

	if err != nil {
		return nil, err
	}

	return &conversation, nil
}

func UpdateConversationMetadata(ctx context.Context, id primitive.ObjectID, title string) error {
  client := GetClient()
  collection := client.Database("plurality").Collection("conversations")
  userID, ok := ctx.Value("userID").(string)

  if !ok {
    return errors.New("user ID not found in request context")
  }


  res, err := collection.UpdateOne(ctx, bson.M{
    "_id": id,
    "user_id": userID,
  }, bson.M{
    "$set": bson.M{
      "title": title,
    },
  })

  if err != nil {
    return err
  }

  if res.MatchedCount == 0 {
    return errors.New("conversation not found")
  }

  return err
}

func UpdateConversationFolder(ctx context.Context, id primitive.ObjectID, folder string) error {
  client := GetClient()
  collection := client.Database("plurality").Collection("conversations")
  userID, ok := ctx.Value("userID").(string)

  if !ok {
    return errors.New("user ID not found in request context")
  }


  res, err := collection.UpdateOne(ctx, bson.M{
    "_id": id,
    "user_id": userID,
  }, bson.M{
    "$set": bson.M{
      "folder": folder,
    },
  })

  if err != nil {
    return err
  }

  if res.MatchedCount == 0 {
    return errors.New("conversation not found")
  }

  return err
}

// DeleteAllConversations deletes all conversations for a specific user
func DeleteAllConversations(ctx context.Context, userID string) (int64, error) {
  client := GetClient()
  collection := client.Database("plurality").Collection("conversations")

  if userID == "" {
    return 0, errors.New("user ID cannot be empty")
  }

  utils.Log("[DeleteAllConversations] Deleting all conversations for user ID: %s", userID)

  // Delete all conversations for the user
  deleteResult, err := collection.DeleteMany(ctx, bson.M{"user_id": userID})
  if err != nil {
    utils.Error("[DeleteAllConversations] Error deleting user conversations", err)
    return 0, err
  }

  utils.Log("[DeleteAllConversations] Deleted %d conversations for user ID: %s", deleteResult.DeletedCount, userID)
  
  return deleteResult.DeletedCount, nil
}
