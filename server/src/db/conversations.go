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

func GetConversationById(ctx context.Context, id string) (*utils.Conversation, error) {
	client := GetClient()
	collection := client.Database("plurality").Collection("conversations")

	var conversation utils.Conversation
	err := collection.FindOne(ctx, bson.M{"id": id}).Decode(&conversation)
	if err != nil {
		return nil, err
	}

	return &conversation, nil
}

// push new message to conversation (or create new conversation if none)
func PushMessage(ctx context.Context, conversation utils.Conversation, message utils.Message) (utils.Conversation, error) {
  client := GetClient()
  collection := client.Database("plurality").Collection("conversations")
	userID, ok := ctx.Value("userID").(string)

  if !ok {
    return utils.Conversation{}, errors.New("user ID not found in request context")
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
      return utils.Conversation{}, err
    }
   
    // Extract the ID from the InsertOne result and set it on the returned conversation
    if oid, ok := result.InsertedID.(primitive.ObjectID); ok {
      conversation.ID = oid
    }
   
    return conversation, nil
  } else {
    // For existing conversations, push the new message and update LastMessageAt
    // Options to return the document after update
    opts := options.FindOneAndUpdate().SetReturnDocument(options.After)

    utils.Debug("Pushing message to conversation ID: ", conversation.ID, " for user ID: ", userID)

    // Perform FindOneAndUpdate to get the updated document
    var updatedConversation utils.Conversation
    err := collection.FindOneAndUpdate(
      ctx,
      bson.M{
        "_id": conversation.ID,
        "user_id": userID,
      },
      bson.M{
        "$push": bson.M{"messages": message},
        "$set": bson.M{
          "last_message_at": currentTime,
          "title": conversation.Title,
        },
      },
      opts,
    ).Decode(&updatedConversation)
   
    if err != nil {
      return utils.Conversation{}, err
    }

    utils.Debug("Updated conversation: ", updatedConversation.UserID, updatedConversation.LastMessageAt)
   
    return updatedConversation, nil
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
      return conversations[i].LastMessageAt.Before(conversations[j].LastMessageAt)
  })

  // remove messages from conversations
  for i := range conversations {
      conversations[i].Messages = nil
  }

  return conversations, nil
}