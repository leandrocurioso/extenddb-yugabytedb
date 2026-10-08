package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"

	"github.com/curioso/extenddb-yugabytedb/app/internal/compat"
	"github.com/curioso/extenddb-yugabytedb/app/internal/config"
	"github.com/curioso/extenddb-yugabytedb/app/internal/httpapi"
	"github.com/curioso/extenddb-yugabytedb/app/internal/store"
	"github.com/curioso/extenddb-yugabytedb/app/internal/workload"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Credentials and AWS_CA_BUNDLE (ExtendDB's self-signed cert) come from the
	// default chain; only the endpoint is overridden to this cell's ExtendDB.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}
	client := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(cfg.DynamoDBEndpoint)
	})

	// The workload endpoints get a client that never retries: the chaos tests
	// need each request to be one attempt that was acknowledged, rejected or
	// left in doubt, and they do their own failover between cells.
	singleAttempt := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(cfg.DynamoDBEndpoint)
		o.RetryMaxAttempts = 1
	})
	streams := dynamodbstreams.NewFromConfig(awsCfg, func(o *dynamodbstreams.Options) {
		o.BaseEndpoint = aws.String(cfg.DynamoDBEndpoint)
	})

	// With a shared database every cell creates the same tables at startup, so
	// the workload tables are created with retries.
	users := store.NewUserStore(client, cfg.DynamoDBTable)
	initCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	err = users.EnsureTable(initCtx)
	if err == nil {
		err = workload.EnsureTables(initCtx, client)
	}
	cancel()
	if err != nil {
		log.Fatalf("ensure tables: %v", err)
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewRouter(cfg.CellID, cfg.OpTimeout, users,
			workload.New(singleAttempt, streams, cfg.CellID),
			compat.NewSuite(client, streams, cfg.CellID, cfg.ExchangeDir)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("cell %s listening on %s (dynamodb endpoint %s)", cfg.CellID, cfg.HTTPAddr, cfg.DynamoDBEndpoint)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
