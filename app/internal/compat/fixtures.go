package compat

import (
	"context"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

type item = map[string]types.AttributeValue

var (
	S = ddbx.S
	N = ddbx.N
)

func attr(name string, typ types.ScalarAttributeType) types.AttributeDefinition {
	return types.AttributeDefinition{AttributeName: aws.String(name), AttributeType: typ}
}

func keySchema(hash string, rng ...string) []types.KeySchemaElement {
	ks := []types.KeySchemaElement{{AttributeName: aws.String(hash), KeyType: types.KeyTypeHash}}
	for _, r := range rng {
		ks = append(ks, types.KeySchemaElement{AttributeName: aws.String(r), KeyType: types.KeyTypeRange})
	}
	return ks
}

// simpleTable is a pay-per-request table with only a string hash key "pk".
func simpleTable(name string) *dynamodb.CreateTableInput {
	return &dynamodb.CreateTableInput{
		TableName:            aws.String(name),
		BillingMode:          types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{attr("pk", types.ScalarAttributeTypeS)},
		KeySchema:            keySchema("pk"),
	}
}

// Index names of the fixture table.
const (
	lsi1     = "lsi1"      // (pk, lsi_sk), ALL
	gsiAll   = "gsi_all"   // (gsi_pk, gsi_sk N), ALL
	gsiKeys  = "gsi_keys"  // (gsi2_pk), KEYS_ONLY
	gsiInclu = "gsi_inclu" // (gsi3_pk), INCLUDE [data]
)

// Fixture returns the run's shared table for item-level cases, creating it on
// first use: keys (pk S, sk S), one LSI, three GSIs (one per projection type)
// and a NEW_AND_OLD_IMAGES stream. Cases keep to their own partition keys.
func (t *T) Fixture() string {
	return t.ensure(&dynamodb.CreateTableInput{
		TableName:   aws.String(t.TableName("fixture")),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			attr("pk", types.ScalarAttributeTypeS), attr("sk", types.ScalarAttributeTypeS),
			attr("lsi_sk", types.ScalarAttributeTypeS),
			attr("gsi_pk", types.ScalarAttributeTypeS), attr("gsi_sk", types.ScalarAttributeTypeN),
			attr("gsi2_pk", types.ScalarAttributeTypeS), attr("gsi3_pk", types.ScalarAttributeTypeS),
		},
		KeySchema: keySchema("pk", "sk"),
		LocalSecondaryIndexes: []types.LocalSecondaryIndex{{
			IndexName: aws.String(lsi1), KeySchema: keySchema("pk", "lsi_sk"),
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
		}},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName: aws.String(gsiAll), KeySchema: keySchema("gsi_pk", "gsi_sk"),
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
			{
				IndexName: aws.String(gsiKeys), KeySchema: keySchema("gsi2_pk"),
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
			},
			{
				IndexName: aws.String(gsiInclu), KeySchema: keySchema("gsi3_pk"),
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeInclude, NonKeyAttributes: []string{"data"}},
			},
		},
		StreamSpecification: &types.StreamSpecification{
			StreamEnabled: aws.Bool(true), StreamViewType: types.StreamViewTypeNewAndOldImages,
		},
	})
}

// AdminTable returns the run's shared table for table-level settings (tags,
// TTL, backups), creating it on first use.
func (t *T) AdminTable() string {
	return t.ensure(simpleTable(t.TableName("admin")))
}

// ensure creates a shared table once per process and returns its name.
func (t *T) ensure(spec *dynamodb.CreateTableInput) string {
	name := aws.ToString(spec.TableName)
	s := t.suite
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready[name] {
		ctx, cancel := context.WithTimeout(t, 3*time.Minute)
		defer cancel()
		t.NoErr(ddbx.EnsureTable(ctx, t.DB, spec), "create shared table "+name)
		s.ready[name] = true
	}
	return name
}

// CreateTable creates a table private to the case and waits until it is ACTIVE.
func (t *T) CreateTable(spec *dynamodb.CreateTableInput) *types.TableDescription {
	_, err := t.DB.CreateTable(t, spec)
	t.NoErr(err, "CreateTable")
	return t.WaitActive(aws.ToString(spec.TableName))
}

// WaitActive waits until the table and all its GSIs are ACTIVE.
func (t *T) WaitActive(name string) *types.TableDescription {
	var desc *types.TableDescription
	t.Eventually(3*time.Minute, "table "+name+" ACTIVE", func() bool {
		out, err := t.DB.DescribeTable(t, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
		if err != nil {
			return false
		}
		desc = out.Table
		if desc.TableStatus != types.TableStatusActive {
			return false
		}
		for _, gsi := range desc.GlobalSecondaryIndexes {
			if gsi.IndexStatus != types.IndexStatusActive {
				return false
			}
		}
		return true
	})
	return desc
}

// DropTable deletes a table and waits until it is gone.
func (t *T) DropTable(name string) {
	_, err := t.DB.DeleteTable(t, &dynamodb.DeleteTableInput{TableName: aws.String(name)})
	t.NoErr(err, "DeleteTable "+name)
	t.Eventually(2*time.Minute, "table "+name+" deleted", func() bool {
		_, err := t.DB.DescribeTable(t, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
		return ddbx.Code(err) == "ResourceNotFoundException"
	})
}

func (t *T) Put(table string, it item) {
	_, err := t.DB.PutItem(t, &dynamodb.PutItemInput{TableName: aws.String(table), Item: it})
	t.NoErr(err, "PutItem")
}

// Get reads an item consistently (nil when missing).
func (t *T) Get(table string, key item) item {
	out, err := t.DB.GetItem(t, &dynamodb.GetItemInput{TableName: aws.String(table), Key: key, ConsistentRead: aws.Bool(true)})
	t.NoErr(err, "GetItem")
	return out.Item
}

func (t *T) Delete(table string, key item) {
	_, err := t.DB.DeleteItem(t, &dynamodb.DeleteItemInput{TableName: aws.String(table), Key: key})
	t.NoErr(err, "DeleteItem")
}

// Cleanup deletes every table the run created through this cell.
func (s *Suite) Cleanup(ctx context.Context, run string) ([]string, error) {
	prefix := tablePrefix(run, s.cell)
	var deleted []string
	pager := dynamodb.NewListTablesPaginator(s.db, &dynamodb.ListTablesInput{})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return deleted, err
		}
		for _, name := range page.TableNames {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			_, err := s.db.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(name)})
			if err != nil && ddbx.Code(err) != "ResourceNotFoundException" {
				return deleted, err
			}
			deleted = append(deleted, name)
		}
	}
	s.mu.Lock()
	for _, name := range deleted {
		delete(s.ready, name)
	}
	s.mu.Unlock()
	return deleted, nil
}
