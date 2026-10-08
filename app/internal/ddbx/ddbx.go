// Package ddbx holds small helpers shared by the packages that call DynamoDB
// through the AWS SDK: attribute constructors and error classification.
package ddbx

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

func S(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func N(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

// Str returns the string value of an S attribute ("" for anything else).
func Str(av types.AttributeValue) string {
	if s, ok := av.(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

// Int returns the integer value of an N attribute (0 for anything else).
func Int(av types.AttributeValue) int64 {
	if n, ok := av.(*types.AttributeValueMemberN); ok {
		v, _ := strconv.ParseInt(n.Value, 10, 64)
		return v
	}
	return 0
}

// Code returns the service error code (e.g. "ConditionalCheckFailedException"),
// or "" when err is not an API error (transport failure, timeout, ...).
func Code(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// Rejected reports whether the service answered with a 4xx: the request was
// refused and had no effect. Anything else (5xx, connection errors, timeouts)
// leaves the outcome of a write unknown.
func Rejected(err error) bool {
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		status := respErr.HTTPStatusCode()
		return status >= 400 && status < 500
	}
	return false
}

func TimedOut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// EnsureTable creates one table unless it exists and waits until it is ACTIVE,
// retrying failures (another cell creating it, catalog contention) until ctx
// expires.
func EnsureTable(ctx context.Context, db *dynamodb.Client, spec *dynamodb.CreateTableInput) error {
	name := aws.ToString(spec.TableName)
	var lastErr error
	retry := func(op string, err error) {
		if lastErr == nil || Code(lastErr) != Code(err) {
			log.Printf("ensure table %s: %s failed, retrying: %v", name, op, err)
		}
		lastErr = err
	}
	for {
		out, err := db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: spec.TableName})
		switch {
		case err == nil && out.Table.TableStatus == types.TableStatusActive:
			return nil
		case err == nil:
			// CREATING: keep polling.
		case Code(err) == "ResourceNotFoundException":
			if _, err := db.CreateTable(ctx, spec); err != nil && Code(err) != "ResourceInUseException" {
				retry("CreateTable", err)
			}
		default:
			retry("DescribeTable", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ensure table %s: %w (last error: %v)", name, ctx.Err(), lastErr)
		case <-time.After(time.Second):
		}
	}
}

// SeqLess orders DynamoDB Streams sequence numbers, which are decimal strings.
func SeqLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
