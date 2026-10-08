package compat

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

func batchCases() []Case {
	const g = "batch"
	return []Case{
		{ID: "batch/write", Group: g, Ops: []string{"BatchWriteItem"}, run: func(t *T) {
			fixture, admin, pk := t.Fixture(), t.AdminTable(), t.PK("bw")
			var puts []types.WriteRequest
			for i := 0; i < 24; i++ {
				puts = append(puts, types.WriteRequest{PutRequest: &types.PutRequest{Item: item{
					"pk": S(pk), "sk": S(fmt.Sprintf("%02d", i)), "v": N(int64(i)),
				}}})
			}
			out, err := t.DB.BatchWriteItem(t, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
				fixture: puts,
				admin:   {{PutRequest: &types.PutRequest{Item: item{"pk": S(pk)}}}},
			}})
			t.NoErr(err, "BatchWriteItem 25 puts over two tables")
			t.Check(len(out.UnprocessedItems) == 0, "UnprocessedItems = %v", out.UnprocessedItems)
			t.Check(ddbx.Int(t.Get(fixture, key(pk, "23"))["v"]) == 23, "batch put missing in the first table")
			t.Check(t.Get(admin, item{"pk": S(pk)}) != nil, "batch put missing in the second table")

			_, err = t.DB.BatchWriteItem(t, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
				fixture: {
					{DeleteRequest: &types.DeleteRequest{Key: key(pk, "00")}},
					{PutRequest: &types.PutRequest{Item: item{"pk": S(pk), "sk": S("new"), "v": N(99)}}},
				},
			}})
			t.NoErr(err, "BatchWriteItem put + delete")
			t.Check(t.Get(fixture, key(pk, "00")) == nil && t.Get(fixture, key(pk, "new")) != nil, "batch put + delete not applied")

			over := append(slices.Clone(puts),
				types.WriteRequest{PutRequest: &types.PutRequest{Item: item{"pk": S(pk), "sk": S("x1")}}},
				types.WriteRequest{PutRequest: &types.PutRequest{Item: item{"pk": S(pk), "sk": S("x2")}}})
			_, err = t.DB.BatchWriteItem(t, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{fixture: over}})
			t.ErrCode(err, "ValidationException", "BatchWriteItem with 26 requests")

			_, err = t.DB.BatchWriteItem(t, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{fixture: {puts[0], puts[0]}}})
			t.ErrCode(err, "ValidationException", "BatchWriteItem with a duplicate key")
		}},

		{ID: "batch/get", Group: g, Ops: []string{"BatchGetItem"}, run: func(t *T) {
			fixture, admin, pk := t.Fixture(), t.AdminTable(), t.PK("bg")
			var keys []item
			for i := 0; i < 20; i++ {
				k := key(pk, fmt.Sprintf("%02d", i))
				t.Put(fixture, item{"pk": k["pk"], "sk": k["sk"], "v": N(int64(i)), "other": S("x")})
				keys = append(keys, k)
			}
			t.Put(admin, item{"pk": S(pk), "v": N(7)})

			request := map[string]types.KeysAndAttributes{
				fixture: {
					Keys: append(keys, key(pk, "missing")), ConsistentRead: aws.Bool(true),
					ProjectionExpression: aws.String("sk, #v"), ExpressionAttributeNames: map[string]string{"#v": "v"},
				},
				admin: {Keys: []item{{"pk": S(pk)}}, ConsistentRead: aws.Bool(true)},
			}
			got := map[string][]item{}
			for len(request) > 0 {
				out, err := t.DB.BatchGetItem(t, &dynamodb.BatchGetItemInput{RequestItems: request})
				t.NoErr(err, "BatchGetItem")
				for table, items := range out.Responses {
					got[table] = append(got[table], items...)
				}
				request = out.UnprocessedKeys
			}
			t.Check(len(got[fixture]) == 20, "got %d of 20 items (a missing key must be skipped)", len(got[fixture]))
			t.Check(len(got[fixture][0]) == 2, "projection returned %v", got[fixture][0])
			t.Check(len(got[admin]) == 1 && ddbx.Int(got[admin][0]["v"]) == 7, "second table returned %v", got[admin])

			var tooMany []item
			for i := 0; i < 101; i++ {
				tooMany = append(tooMany, key(pk, fmt.Sprintf("k%03d", i)))
			}
			_, err := t.DB.BatchGetItem(t, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{fixture: {Keys: tooMany}}})
			t.ErrCode(err, "ValidationException", "BatchGetItem with 101 keys")
			_, err = t.DB.BatchGetItem(t, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{fixture: {Keys: []item{keys[0], keys[0]}}}})
			t.ErrCode(err, "ValidationException", "BatchGetItem with a duplicate key")
		}},
	}
}

func transactionCases() []Case {
	const g = "transactions"
	return []Case{
		{ID: "transact/write", Group: g, Ops: []string{"TransactWriteItems"}, run: func(t *T) {
			fixture, admin, pk := t.Fixture(), t.AdminTable(), t.PK("tw")
			t.Put(fixture, item{"pk": S(pk), "sk": S("upd"), "n": N(1)})
			t.Put(fixture, item{"pk": S(pk), "sk": S("del")})
			t.Put(fixture, item{"pk": S(pk), "sk": S("guard"), "state": S("open")})
			t.Delete(fixture, key(pk, "put"))

			_, err := t.DB.TransactWriteItems(t, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
				{Put: &types.Put{TableName: aws.String(fixture), Item: item{"pk": S(pk), "sk": S("put"), "n": N(5)},
					ConditionExpression: aws.String("attribute_not_exists(pk)")}},
				{Update: &types.Update{TableName: aws.String(fixture), Key: key(pk, "upd"),
					UpdateExpression: aws.String("SET n = n + :one"), ExpressionAttributeValues: item{":one": N(1)}}},
				{Delete: &types.Delete{TableName: aws.String(fixture), Key: key(pk, "del")}},
				{ConditionCheck: &types.ConditionCheck{TableName: aws.String(fixture), Key: key(pk, "guard"),
					ConditionExpression: aws.String("#s = :open"), ExpressionAttributeNames: map[string]string{"#s": "state"},
					ExpressionAttributeValues: item{":open": S("open")}}},
				{Put: &types.Put{TableName: aws.String(admin), Item: item{"pk": S(pk), "n": N(1)}}},
			}})
			t.NoErr(err, "TransactWriteItems")
			t.Check(ddbx.Int(t.Get(fixture, key(pk, "put"))["n"]) == 5, "Put not applied")
			t.Check(ddbx.Int(t.Get(fixture, key(pk, "upd"))["n"]) == 2, "Update not applied")
			t.Check(t.Get(fixture, key(pk, "del")) == nil, "Delete not applied")
			t.Check(t.Get(admin, item{"pk": S(pk)}) != nil, "Put in the second table not applied")
		}},

		{ID: "transact/write-canceled", Group: g, Ops: []string{"TransactWriteItems"}, run: func(t *T) {
			fixture, pk := t.Fixture(), t.PK("twc")
			t.Put(fixture, item{"pk": S(pk), "sk": S("a"), "n": N(1)})
			t.Put(fixture, item{"pk": S(pk), "sk": S("guard"), "state": S("closed")})

			t.Delete(fixture, key(pk, "new"))

			_, err := t.DB.TransactWriteItems(t, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
				{Update: &types.Update{TableName: aws.String(fixture), Key: key(pk, "a"),
					UpdateExpression: aws.String("SET n = n + :one"), ExpressionAttributeValues: item{":one": N(1)}}},
				{Put: &types.Put{TableName: aws.String(fixture), Item: item{"pk": S(pk), "sk": S("new")}}},
				{ConditionCheck: &types.ConditionCheck{TableName: aws.String(fixture), Key: key(pk, "guard"),
					ConditionExpression: aws.String("#s = :open"), ExpressionAttributeNames: map[string]string{"#s": "state"},
					ExpressionAttributeValues:           item{":open": S("open")},
					ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld}},
			}})
			var canceled *types.TransactionCanceledException
			t.Check(errors.As(err, &canceled), "expected TransactionCanceledException, got %v", err)
			reasons := canceled.CancellationReasons
			t.Check(len(reasons) == 3, "got %d cancellation reasons, want 3", len(reasons))
			t.Check(aws.ToString(reasons[0].Code) == "None" && aws.ToString(reasons[1].Code) == "None" &&
				aws.ToString(reasons[2].Code) == "ConditionalCheckFailed",
				"reason codes = %s, %s, %s", aws.ToString(reasons[0].Code), aws.ToString(reasons[1].Code), aws.ToString(reasons[2].Code))
			t.Check(ddbx.Str(reasons[2].Item["state"]) == "closed", "the failed reason does not carry the old item: %v", reasons[2].Item)
			t.Check(ddbx.Int(t.Get(fixture, key(pk, "a"))["n"]) == 1, "a canceled transaction applied its Update")
			t.Check(t.Get(fixture, key(pk, "new")) == nil, "a canceled transaction applied its Put")

			_, err = t.DB.TransactWriteItems(t, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
				{Put: &types.Put{TableName: aws.String(fixture), Item: item{"pk": S(pk), "sk": S("dup")}}},
				{Delete: &types.Delete{TableName: aws.String(fixture), Key: key(pk, "dup")}},
			}})
			t.ErrCode(err, "ValidationException", "transaction with two operations on one item")
		}},

		{ID: "transact/idempotency-token", Group: g, Ops: []string{"TransactWriteItems"}, run: func(t *T) {
			fixture, pk := t.Fixture(), t.PK("tok")
			k := key(pk, "counter")
			t.Delete(fixture, k)
			// Unique per execution: a token is remembered for 10 minutes.
			token := fmt.Sprintf("tok-%d", time.Now().UnixNano())
			incr := func(by int64) error {
				_, err := t.DB.TransactWriteItems(t, &dynamodb.TransactWriteItemsInput{
					ClientRequestToken: aws.String(token),
					TransactItems: []types.TransactWriteItem{{Update: &types.Update{
						TableName: aws.String(fixture), Key: k,
						UpdateExpression: aws.String("ADD n :by"), ExpressionAttributeValues: item{":by": N(by)},
					}}},
				})
				return err
			}
			t.NoErr(incr(1), "first call")
			t.NoErr(incr(1), "replay with the same token")
			t.Check(ddbx.Int(t.Get(fixture, k)["n"]) == 1, "the replay was applied again: n = %d", ddbx.Int(t.Get(fixture, k)["n"]))
			t.ErrCode(incr(2), "IdempotentParameterMismatchException", "same token with other parameters")
		}},

		{ID: "transact/get", Group: g, Ops: []string{"TransactGetItems"}, run: func(t *T) {
			fixture, admin, pk := t.Fixture(), t.AdminTable(), t.PK("tg")
			t.Put(fixture, item{"pk": S(pk), "sk": S("a"), "n": N(1), "other": S("x")})
			t.Put(admin, item{"pk": S(pk), "n": N(2)})
			out, err := t.DB.TransactGetItems(t, &dynamodb.TransactGetItemsInput{TransactItems: []types.TransactGetItem{
				{Get: &types.Get{TableName: aws.String(fixture), Key: key(pk, "a"), ProjectionExpression: aws.String("n")}},
				{Get: &types.Get{TableName: aws.String(fixture), Key: key(pk, "missing")}},
				{Get: &types.Get{TableName: aws.String(admin), Key: item{"pk": S(pk)}}},
			}})
			t.NoErr(err, "TransactGetItems")
			t.Check(len(out.Responses) == 3, "got %d responses, want 3 (in request order)", len(out.Responses))
			t.Check(len(out.Responses[0].Item) == 1 && ddbx.Int(out.Responses[0].Item["n"]) == 1, "first response = %v", out.Responses[0].Item)
			t.Check(len(out.Responses[1].Item) == 0, "a missing item returned %v", out.Responses[1].Item)
			t.Check(ddbx.Int(out.Responses[2].Item["n"]) == 2, "third response = %v", out.Responses[2].Item)
		}},
	}
}
