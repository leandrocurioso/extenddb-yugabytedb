package compat

import (
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func tableCases() []Case {
	const g = "tables"
	return []Case{
		{ID: "tables/create-describe-delete", Group: g, Ops: []string{"CreateTable", "DescribeTable", "DeleteTable"}, run: func(t *T) {
			name := t.TableName("lifecycle")
			out, err := t.DB.CreateTable(t, simpleTable(name))
			t.NoErr(err, "CreateTable")
			status := out.TableDescription.TableStatus
			t.Check(status == types.TableStatusCreating || status == types.TableStatusActive, "CreateTable status = %s", status)

			desc := t.WaitActive(name)
			t.Check(aws.ToString(desc.TableName) == name, "DescribeTable name = %q", aws.ToString(desc.TableName))
			t.Check(aws.ToString(desc.TableArn) != "", "DescribeTable has no TableArn")
			t.Check(len(desc.KeySchema) == 1 && aws.ToString(desc.KeySchema[0].AttributeName) == "pk" && desc.KeySchema[0].KeyType == types.KeyTypeHash,
				"DescribeTable key schema = %+v", desc.KeySchema)
			t.Check(desc.BillingModeSummary != nil && desc.BillingModeSummary.BillingMode == types.BillingModePayPerRequest,
				"DescribeTable billing mode = %+v", desc.BillingModeSummary)
			t.Check(desc.CreationDateTime != nil, "DescribeTable has no CreationDateTime")

			_, err = t.DB.CreateTable(t, simpleTable(name))
			t.ErrCode(err, "ResourceInUseException", "CreateTable on an existing table")

			t.DropTable(name)
			_, err = t.DB.PutItem(t, &dynamodb.PutItemInput{TableName: aws.String(name), Item: item{"pk": S("x")}})
			t.ErrCode(err, "ResourceNotFoundException", "PutItem on a deleted table")
			_, err = t.DB.DeleteTable(t, &dynamodb.DeleteTableInput{TableName: aws.String(name)})
			t.ErrCode(err, "ResourceNotFoundException", "DeleteTable on a deleted table")
		}},

		{ID: "tables/describe-indexes", Group: g, Ops: []string{"DescribeTable"}, run: func(t *T) {
			desc := t.WaitActive(t.Fixture())
			t.Check(len(desc.LocalSecondaryIndexes) == 1 && aws.ToString(desc.LocalSecondaryIndexes[0].IndexName) == lsi1,
				"LSIs = %+v", desc.LocalSecondaryIndexes)
			projections := map[string]types.ProjectionType{}
			for _, gsi := range desc.GlobalSecondaryIndexes {
				t.Check(gsi.Projection != nil, "GSI %s has no projection", aws.ToString(gsi.IndexName))
				projections[aws.ToString(gsi.IndexName)] = gsi.Projection.ProjectionType
			}
			t.Check(len(projections) == 3 && projections[gsiAll] == types.ProjectionTypeAll &&
				projections[gsiKeys] == types.ProjectionTypeKeysOnly && projections[gsiInclu] == types.ProjectionTypeInclude,
				"GSI projections = %v", projections)
			t.Check(desc.StreamSpecification != nil && aws.ToBool(desc.StreamSpecification.StreamEnabled) &&
				desc.StreamSpecification.StreamViewType == types.StreamViewTypeNewAndOldImages,
				"stream specification = %+v", desc.StreamSpecification)
			t.Check(aws.ToString(desc.LatestStreamArn) != "", "no LatestStreamArn")
			t.Check(len(desc.AttributeDefinitions) == 7, "attribute definitions = %d, want 7", len(desc.AttributeDefinitions))
		}},

		{ID: "tables/list", Group: g, Ops: []string{"ListTables"}, run: func(t *T) {
			a, b := t.Fixture(), t.AdminTable()
			var all []string
			pages := 0
			pager := dynamodb.NewListTablesPaginator(t.DB, &dynamodb.ListTablesInput{Limit: aws.Int32(1)})
			for pager.HasMorePages() {
				page, err := pager.NextPage(t)
				t.NoErr(err, "ListTables")
				t.Check(len(page.TableNames) <= 1, "ListTables Limit=1 returned %d names", len(page.TableNames))
				all = append(all, page.TableNames...)
				pages++
			}
			t.Check(slices.Contains(all, a) && slices.Contains(all, b), "ListTables misses %s or %s", a, b)
			t.Check(pages >= 2, "ListTables Limit=1 did not paginate")
			t.Check(slices.IsSorted(all), "ListTables names are not sorted")

			out, err := t.DB.ListTables(t, &dynamodb.ListTablesInput{ExclusiveStartTableName: aws.String(a)})
			t.NoErr(err, "ListTables ExclusiveStartTableName")
			t.Check(!slices.Contains(out.TableNames, a), "ExclusiveStartTableName is not exclusive")
		}},

		{ID: "tables/update-gsi", Group: g, Ops: []string{"UpdateTable"}, run: func(t *T) {
			name := t.TableName("updgsi")
			t.CreateTable(simpleTable(name))
			defer t.DropTable(name)
			for i, color := range []string{"red", "red", "blue"} {
				t.Put(name, item{"pk": S(string(rune('a' + i))), "color": S(color)})
			}

			_, err := t.DB.UpdateTable(t, &dynamodb.UpdateTableInput{
				TableName:            aws.String(name),
				AttributeDefinitions: []types.AttributeDefinition{attr("color", types.ScalarAttributeTypeS)},
				GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{Create: &types.CreateGlobalSecondaryIndexAction{
					IndexName: aws.String("by_color"), KeySchema: keySchema("color"),
					Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
				}}},
			})
			t.NoErr(err, "UpdateTable create GSI")
			desc := t.WaitActive(name)
			t.Check(len(desc.GlobalSecondaryIndexes) == 1, "GSIs after create = %d", len(desc.GlobalSecondaryIndexes))

			// The new index must be backfilled with the existing items.
			t.Eventually(30*time.Second, "new GSI backfilled", func() bool {
				out, err := t.DB.Query(t, &dynamodb.QueryInput{
					TableName: aws.String(name), IndexName: aws.String("by_color"),
					KeyConditionExpression:    aws.String("color = :c"),
					ExpressionAttributeValues: item{":c": S("red")},
				})
				t.NoErr(err, "Query new GSI")
				return out.Count == 2
			})

			_, err = t.DB.UpdateTable(t, &dynamodb.UpdateTableInput{
				TableName: aws.String(name),
				GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
					Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("by_color")},
				}},
			})
			t.NoErr(err, "UpdateTable delete GSI")
			t.Eventually(2*time.Minute, "GSI removed", func() bool {
				out, err := t.DB.DescribeTable(t, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
				t.NoErr(err, "DescribeTable")
				return len(out.Table.GlobalSecondaryIndexes) == 0
			})
		}},

		{ID: "tables/update-settings", Group: g, Ops: []string{"UpdateTable"}, run: func(t *T) {
			name := t.TableName("updset")
			t.CreateTable(simpleTable(name))
			defer t.DropTable(name)

			_, err := t.DB.UpdateTable(t, &dynamodb.UpdateTableInput{
				TableName: aws.String(name),
				StreamSpecification: &types.StreamSpecification{
					StreamEnabled: aws.Bool(true), StreamViewType: types.StreamViewTypeKeysOnly,
				},
			})
			t.NoErr(err, "UpdateTable enable stream")
			desc := t.WaitActive(name)
			t.Check(aws.ToString(desc.LatestStreamArn) != "" && desc.StreamSpecification != nil &&
				desc.StreamSpecification.StreamViewType == types.StreamViewTypeKeysOnly, "stream not enabled: %+v", desc.StreamSpecification)

			_, err = t.DB.UpdateTable(t, &dynamodb.UpdateTableInput{
				TableName:   aws.String(name),
				BillingMode: types.BillingModeProvisioned,
				ProvisionedThroughput: &types.ProvisionedThroughput{
					ReadCapacityUnits: aws.Int64(7), WriteCapacityUnits: aws.Int64(3),
				},
			})
			t.NoErr(err, "UpdateTable to PROVISIONED")
			desc = t.WaitActive(name)
			t.Check(desc.ProvisionedThroughput != nil && aws.ToInt64(desc.ProvisionedThroughput.ReadCapacityUnits) == 7 &&
				aws.ToInt64(desc.ProvisionedThroughput.WriteCapacityUnits) == 3, "provisioned throughput = %+v", desc.ProvisionedThroughput)
		}},

		{ID: "tables/deletion-protection", Group: g, Ops: []string{"CreateTable", "UpdateTable", "DeleteTable"}, run: func(t *T) {
			name := t.TableName("protected")
			spec := simpleTable(name)
			spec.DeletionProtectionEnabled = aws.Bool(true)
			desc := t.CreateTable(spec)
			t.Check(aws.ToBool(desc.DeletionProtectionEnabled), "DeletionProtectionEnabled not reported")

			_, err := t.DB.DeleteTable(t, &dynamodb.DeleteTableInput{TableName: aws.String(name)})
			t.ErrCode(err, "ValidationException", "DeleteTable on a protected table")

			_, err = t.DB.UpdateTable(t, &dynamodb.UpdateTableInput{TableName: aws.String(name), DeletionProtectionEnabled: aws.Bool(false)})
			t.NoErr(err, "UpdateTable disable deletion protection")
			t.WaitActive(name)
			t.DropTable(name)
		}},

		{ID: "tables/create-validation", Group: g, Ops: []string{"CreateTable"}, run: func(t *T) {
			// Key attribute missing from AttributeDefinitions.
			_, err := t.DB.CreateTable(t, &dynamodb.CreateTableInput{
				TableName: aws.String(t.TableName("invalid")), BillingMode: types.BillingModePayPerRequest,
				AttributeDefinitions: []types.AttributeDefinition{attr("other", types.ScalarAttributeTypeS)},
				KeySchema:            keySchema("pk"),
			})
			t.ErrCode(err, "ValidationException", "CreateTable with an undefined key attribute")

			// An LSI must share the table's hash key.
			_, err = t.DB.CreateTable(t, &dynamodb.CreateTableInput{
				TableName: aws.String(t.TableName("invalid")), BillingMode: types.BillingModePayPerRequest,
				AttributeDefinitions: []types.AttributeDefinition{
					attr("pk", types.ScalarAttributeTypeS), attr("sk", types.ScalarAttributeTypeS), attr("x", types.ScalarAttributeTypeS),
				},
				KeySchema: keySchema("pk", "sk"),
				LocalSecondaryIndexes: []types.LocalSecondaryIndex{{
					IndexName: aws.String("bad"), KeySchema: keySchema("x", "sk"),
					Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
				}},
			})
			t.ErrCode(err, "ValidationException", "CreateTable with an LSI on another hash key")
		}},

		{ID: "tables/describe-limits", Group: g, Ops: []string{"DescribeLimits"}, run: func(t *T) {
			out, err := t.DB.DescribeLimits(t, &dynamodb.DescribeLimitsInput{})
			t.NoErr(err, "DescribeLimits")
			t.Check(aws.ToInt64(out.AccountMaxReadCapacityUnits) > 0 && aws.ToInt64(out.TableMaxWriteCapacityUnits) > 0,
				"DescribeLimits returned no limits: %+v", out)
		}},

		{ID: "tables/describe-endpoints", Group: g, Ops: []string{"DescribeEndpoints"}, run: func(t *T) {
			out, err := t.DB.DescribeEndpoints(t, &dynamodb.DescribeEndpointsInput{})
			t.NoErr(err, "DescribeEndpoints")
			t.Check(len(out.Endpoints) > 0 && aws.ToString(out.Endpoints[0].Address) != "", "DescribeEndpoints returned no endpoint")
		}},
	}
}
