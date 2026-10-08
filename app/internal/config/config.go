package config

import (
	"fmt"
	"os"
	"time"
)

// Config is the cell-local runtime configuration, read from the environment.
// AWS credentials and AWS_CA_BUNDLE are read by the AWS SDK's default chain.
type Config struct {
	CellID           string
	HTTPAddr         string
	AWSRegion        string
	DynamoDBEndpoint string
	DynamoDBTable    string
	// OpTimeout bounds each DynamoDB call made for a workload request, so a
	// hung backend turns into an error instead of a stuck request.
	OpTimeout time.Duration
	// ExchangeDir is the directory ExtendDB may import from and export to
	// (its [import]/[export] paths); used by the compat suite.
	ExchangeDir string
}

func Load() (Config, error) {
	cfg := Config{
		CellID:           getenv("CELL_ID", "local"),
		HTTPAddr:         getenv("HTTP_ADDR", ":8080"),
		AWSRegion:        getenv("AWS_REGION", "us-east-1"),
		DynamoDBEndpoint: os.Getenv("DYNAMODB_ENDPOINT"),
		DynamoDBTable:    getenv("DYNAMODB_TABLE", "users"),
		ExchangeDir:      getenv("EXTENDDB_EXCHANGE_DIR", "/var/lib/extenddb-exchange"),
	}
	var err error
	if cfg.OpTimeout, err = time.ParseDuration(getenv("WORKLOAD_OP_TIMEOUT", "5s")); err != nil {
		return Config{}, fmt.Errorf("WORKLOAD_OP_TIMEOUT: %w", err)
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
