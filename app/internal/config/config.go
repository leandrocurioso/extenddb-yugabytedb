package config

import (
	"fmt"
	"os"
)

// Config is the cell-local runtime configuration, read from the environment.
// AWS credentials and AWS_CA_BUNDLE are read by the AWS SDK's default chain.
type Config struct {
	CellID           string
	HTTPAddr         string
	AWSRegion        string
	DynamoDBEndpoint string
	DynamoDBTable    string
}

func Load() (Config, error) {
	cfg := Config{
		CellID:           getenv("CELL_ID", "local"),
		HTTPAddr:         getenv("HTTP_ADDR", ":8080"),
		AWSRegion:        getenv("AWS_REGION", "us-east-1"),
		DynamoDBEndpoint: os.Getenv("DYNAMODB_ENDPOINT"),
		DynamoDBTable:    getenv("DYNAMODB_TABLE", "users"),
	}
	if cfg.DynamoDBEndpoint == "" {
		return Config{}, fmt.Errorf("DYNAMODB_ENDPOINT is required")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
