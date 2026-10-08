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

	"github.com/curioso/extenddb-yugabytedb/app/internal/config"
	"github.com/curioso/extenddb-yugabytedb/app/internal/httpapi"
	"github.com/curioso/extenddb-yugabytedb/app/internal/store"
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

	users := store.NewUserStore(client, cfg.DynamoDBTable)
	initCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = users.EnsureTable(initCtx)
	cancel()
	if err != nil {
		log.Fatalf("ensure table: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewRouter(cfg.CellID, users),
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
