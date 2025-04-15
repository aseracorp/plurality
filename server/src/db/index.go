package db

import (
	"os"
	"time"
	"context"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	"github.com/azukaar/plurality/src/utils"
)

var client *mongo.Client

func InitDB() {
	utils.Log("[InitDB] Connecting to MongoDB")

	mongoURL := os.Getenv("MONGODB")

	opts := options.Client().
	  SetConnectTimeout(7 * time.Second).
		ApplyURI(mongoURL).
		SetRetryWrites(true).
		SetWriteConcern(writeconcern.New(writeconcern.WMajority()))
		
	var err error
	client, err = mongo.Connect(context.TODO(), opts)

	if err != nil {
		utils.Fatal("[InitDB] Failed to connect to MongoDB", err)
	}

	utils.Log("[InitDB] Connected to MongoDB!")
}

func GetClient() *mongo.Client {
	if client == nil {
		InitDB()
	}
	return client
}

func SetClient(c *mongo.Client) {
	utils.Log("[SetClient] Setting MongoDB client to %v ", c)
	client = c
}