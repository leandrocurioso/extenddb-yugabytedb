// Package workload implements the data operations the consistency and chaos
// tests drive through the HTTP API. Every operation is a plain AWS SDK call
// against this cell's ExtendDB; the invariants are checked by the test client.
//
// Two tables are shared by all cells:
//
//	kv      hash key pk. Holds registers (reg#), claims (claim#), counters
//	        (ctr#), accounts (acct#) and the transfer ledger (xfer#).
//	orders  hash key customer_id, range key order_id, with an LSI
//	        (customer_id, created_at), a GSI (gsi_pk = run#status, created_at)
//	        and a NEW_AND_OLD_IMAGES stream.
package workload

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

const (
	KVTable     = "chaos_kv"
	OrdersTable = "chaos_orders"

	OrdersLSI = "by_created"
	OrdersGSI = "by_status"
)

var (
	// ErrConflict means a condition failed: the write was rejected, not applied.
	ErrConflict = errors.New("condition failed")
	// ErrInsufficientFunds means the source account's balance condition failed.
	ErrInsufficientFunds = errors.New("insufficient funds")
	// ErrTransactionConflict means the transaction lost to a concurrent one and was not applied.
	ErrTransactionConflict = errors.New("transaction conflict")
)

type Store struct {
	db      *dynamodb.Client
	streams *dynamodbstreams.Client
	cell    string
}

// New returns a Store. db should not retry on its own: the tests need to know
// whether a single attempt was acknowledged, rejected or left in doubt.
func New(db *dynamodb.Client, streams *dynamodbstreams.Client, cell string) *Store {
	return &Store{db: db, streams: streams, cell: cell}
}

// EnsureTables creates the workload tables if missing. Several cells may run
// this at once against the shared catalog, so it retries until ctx expires.
func EnsureTables(ctx context.Context, db *dynamodb.Client) error {
	specs := []*dynamodb.CreateTableInput{
		{
			TableName:   aws.String(KVTable),
			BillingMode: types.BillingModePayPerRequest,
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			},
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			},
		},
		{
			TableName:   aws.String(OrdersTable),
			BillingMode: types.BillingModePayPerRequest,
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("customer_id"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("order_id"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("created_at"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("gsi_pk"), AttributeType: types.ScalarAttributeTypeS},
			},
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("customer_id"), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String("order_id"), KeyType: types.KeyTypeRange},
			},
			LocalSecondaryIndexes: []types.LocalSecondaryIndex{{
				IndexName: aws.String(OrdersLSI),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("customer_id"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("created_at"), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			}},
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName: aws.String(OrdersGSI),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("gsi_pk"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("created_at"), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			}},
			StreamSpecification: &types.StreamSpecification{
				StreamEnabled:  aws.Bool(true),
				StreamViewType: types.StreamViewTypeNewAndOldImages,
			},
		},
	}
	for _, spec := range specs {
		if err := ddbx.EnsureTable(ctx, db, spec); err != nil {
			return err
		}
	}
	return nil
}

func pkKey(pk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": ddbx.S(pk)}
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// ---------------------------------------------------------------- registers

type Register struct {
	ID      string `json:"id"`
	Value   string `json:"value"`
	Version int64  `json:"version"`
	Cell    string `json:"cell"`
}

func registerFrom(id string, item map[string]types.AttributeValue) Register {
	return Register{ID: id, Value: ddbx.Str(item["val"]), Version: ddbx.Int(item["ver"]), Cell: ddbx.Str(item["cell"])}
}

// RegisterWrite overwrites the register and bumps its version atomically.
func (s *Store) RegisterWrite(ctx context.Context, id, value string) (Register, error) {
	out, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(KVTable),
		Key:              pkKey("reg#" + id),
		UpdateExpression: aws.String("SET val = :v, cell = :c ADD ver :one"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":v": ddbx.S(value), ":c": ddbx.S(s.cell), ":one": ddbx.N(1),
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil {
		return Register{}, err
	}
	return registerFrom(id, out.Attributes), nil
}

// RegisterCAS writes only if the register is at expected (0 = must not exist).
func (s *Store) RegisterCAS(ctx context.Context, id string, expected int64, value string) (Register, error) {
	in := &dynamodb.UpdateItemInput{
		TableName:        aws.String(KVTable),
		Key:              pkKey("reg#" + id),
		UpdateExpression: aws.String("SET val = :v, cell = :c, ver = :next"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":v": ddbx.S(value), ":c": ddbx.S(s.cell), ":next": ddbx.N(expected + 1),
		},
		ReturnValues: types.ReturnValueAllNew,
	}
	if expected == 0 {
		in.ConditionExpression = aws.String("attribute_not_exists(pk)")
	} else {
		in.ConditionExpression = aws.String("ver = :expected")
		in.ExpressionAttributeValues[":expected"] = ddbx.N(expected)
	}
	out, err := s.db.UpdateItem(ctx, in)
	if err != nil {
		return Register{}, conflictOr(err)
	}
	return registerFrom(id, out.Attributes), nil
}

func (s *Store) RegisterGet(ctx context.Context, id string, consistent bool) (Register, bool, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(KVTable),
		Key:            pkKey("reg#" + id),
		ConsistentRead: aws.Bool(consistent),
	})
	if err != nil || out.Item == nil {
		return Register{}, false, err
	}
	return registerFrom(id, out.Item), true, nil
}

func conflictOr(err error) error {
	if ddbx.Code(err) == "ConditionalCheckFailedException" {
		return ErrConflict
	}
	return err
}

// ------------------------------------------------------------------- claims

// Claim takes id for owner; exactly one caller can ever succeed per id.
func (s *Store) Claim(ctx context.Context, id, owner string) error {
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(KVTable),
		Item: map[string]types.AttributeValue{
			"pk": ddbx.S("claim#" + id), "owner": ddbx.S(owner), "cell": ddbx.S(s.cell),
		},
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	return conflictOr(err)
}

// ClaimOwner returns the owner of a claim ("" when unclaimed).
func (s *Store) ClaimOwner(ctx context.Context, id string) (string, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(KVTable),
		Key:            pkKey("claim#" + id),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return "", err
	}
	return ddbx.Str(out.Item["owner"]), nil
}

// ----------------------------------------------------------------- counters

type Counter struct {
	ID    string   `json:"id"`
	Value int64    `json:"value"`
	Ops   []string `json:"ops"`
}

// CounterIncr adds 1 to the counter and records opID in the same update, so
// the item itself shows which increments were applied.
func (s *Store) CounterIncr(ctx context.Context, id, opID string) (int64, error) {
	out, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(KVTable),
		Key:              pkKey("ctr#" + id),
		UpdateExpression: aws.String("ADD cnt :one, ops :op"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one": ddbx.N(1),
			":op":  &types.AttributeValueMemberSS{Value: []string{opID}},
		},
		ReturnValues: types.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, err
	}
	return ddbx.Int(out.Attributes["cnt"]), nil
}

func (s *Store) CounterGet(ctx context.Context, id string) (Counter, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(KVTable),
		Key:            pkKey("ctr#" + id),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return Counter{}, err
	}
	c := Counter{ID: id, Value: ddbx.Int(out.Item["cnt"]), Ops: []string{}}
	if ss, ok := out.Item["ops"].(*types.AttributeValueMemberSS); ok {
		c.Ops = ss.Value
	}
	return c, nil
}
