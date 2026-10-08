package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

var ErrNotFound = errors.New("user not found")

type User struct {
	ID        string    `json:"id" dynamodbav:"id"`
	Name      string    `json:"name" dynamodbav:"name"`
	Email     string    `json:"email" dynamodbav:"email"`
	Cell      string    `json:"cell" dynamodbav:"cell"`
	CreatedAt time.Time `json:"created_at" dynamodbav:"created_at"`
}

// UserStore persists users in a DynamoDB table keyed by "id".
type UserStore struct {
	client *dynamodb.Client
	table  string
}

func NewUserStore(client *dynamodb.Client, table string) *UserStore {
	return &UserStore{client: client, table: table}
}

// EnsureTable creates the table if it does not exist and waits until it is active.
func (s *UserStore) EnsureTable(ctx context.Context) error {
	return ddbx.EnsureTable(ctx, s.client, &dynamodb.CreateTableInput{
		TableName:   aws.String(s.table),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("id"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("id"), KeyType: types.KeyTypeHash},
		},
	})
}

// Ping verifies the table is reachable through ExtendDB.
func (s *UserStore) Ping(ctx context.Context) error {
	_, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(s.table)})
	return err
}

func (s *UserStore) Put(ctx context.Context, u User) error {
	item, err := attributevalue.MarshalMap(u)
	if err != nil {
		return fmt.Errorf("marshal user: %w", err)
	}
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      item,
	})
	return err
}

func (s *UserStore) Get(ctx context.Context, id string) (User, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: id}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return User{}, err
	}
	if out.Item == nil {
		return User{}, ErrNotFound
	}
	var u User
	if err := attributevalue.UnmarshalMap(out.Item, &u); err != nil {
		return User{}, fmt.Errorf("unmarshal user: %w", err)
	}
	return u, nil
}
