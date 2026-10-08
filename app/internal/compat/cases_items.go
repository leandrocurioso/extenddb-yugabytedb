package compat

import (
	"bytes"
	"errors"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

func key(pk, sk string) item { return item{"pk": S(pk), "sk": S(sk)} }

func itemCases() []Case {
	const g = "items"
	return []Case{
		{ID: "items/put-get-all-types", Group: g, Ops: []string{"PutItem", "GetItem"}, run: func(t *T) {
			table, k := t.Fixture(), key(t.PK("types"), "a")
			in := item{
				"pk": k["pk"], "sk": k["sk"],
				"s":    S("héllo wörld ✓"),
				"n":    &types.AttributeValueMemberN{Value: "-123.456789012345678901234567890123456"},
				"b":    &types.AttributeValueMemberB{Value: []byte{0, 1, 2, 255}},
				"bool": &types.AttributeValueMemberBOOL{Value: true},
				"null": &types.AttributeValueMemberNULL{Value: true},
				"l":    &types.AttributeValueMemberL{Value: []types.AttributeValue{S("x"), N(1), &types.AttributeValueMemberBOOL{Value: false}}},
				"m": &types.AttributeValueMemberM{Value: item{
					"nested": &types.AttributeValueMemberM{Value: item{"deep": N(42)}},
					"empty":  S(""),
				}},
				"ss": &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
				"ns": &types.AttributeValueMemberNS{Value: []string{"1", "2.5"}},
				"bs": &types.AttributeValueMemberBS{Value: [][]byte{{1}, {2, 3}}},
			}
			t.Put(table, in)
			got := t.Get(table, k)
			t.Check(len(got) == len(in), "got %d attributes, want %d", len(got), len(in))
			t.Check(ddbx.Str(got["s"]) == "héllo wörld ✓", "S = %q", ddbx.Str(got["s"]))
			n, _ := got["n"].(*types.AttributeValueMemberN)
			t.Check(n != nil && n.Value == "-123.456789012345678901234567890123456", "N (38 digits) = %+v", got["n"])
			b, _ := got["b"].(*types.AttributeValueMemberB)
			t.Check(b != nil && bytes.Equal(b.Value, []byte{0, 1, 2, 255}), "B = %+v", got["b"])
			bl, _ := got["bool"].(*types.AttributeValueMemberBOOL)
			t.Check(bl != nil && bl.Value, "BOOL = %+v", got["bool"])
			nl, _ := got["null"].(*types.AttributeValueMemberNULL)
			t.Check(nl != nil && nl.Value, "NULL = %+v", got["null"])
			l, _ := got["l"].(*types.AttributeValueMemberL)
			t.Check(l != nil && len(l.Value) == 3 && ddbx.Str(l.Value[0]) == "x" && ddbx.Int(l.Value[1]) == 1, "L = %+v", got["l"])
			m, _ := got["m"].(*types.AttributeValueMemberM)
			t.Check(m != nil, "M = %+v", got["m"])
			nested, _ := m.Value["nested"].(*types.AttributeValueMemberM)
			t.Check(nested != nil && ddbx.Int(nested.Value["deep"]) == 42, "nested M = %+v", m.Value["nested"])
			empty, isS := m.Value["empty"].(*types.AttributeValueMemberS)
			t.Check(isS && empty.Value == "", "empty string in M = %+v", m.Value["empty"])
			ss, _ := got["ss"].(*types.AttributeValueMemberSS)
			t.Check(ss != nil && sameSet(ss.Value, []string{"a", "b"}), "SS = %+v", got["ss"])
			ns, _ := got["ns"].(*types.AttributeValueMemberNS)
			t.Check(ns != nil && sameSet(ns.Value, []string{"1", "2.5"}), "NS = %+v", got["ns"])
			bs, _ := got["bs"].(*types.AttributeValueMemberBS)
			t.Check(bs != nil && len(bs.Value) == 2, "BS = %+v", got["bs"])
		}},

		{ID: "items/put-return-values", Group: g, Ops: []string{"PutItem"}, run: func(t *T) {
			table, k := t.Fixture(), key(t.PK("rv"), "a")
			first, err := t.DB.PutItem(t, &dynamodb.PutItemInput{
				TableName: aws.String(table), Item: item{"pk": k["pk"], "sk": k["sk"], "v": N(1)},
				ReturnValues: types.ReturnValueAllOld, ReturnConsumedCapacity: types.ReturnConsumedCapacityTotal,
			})
			t.NoErr(err, "PutItem")
			t.Check(len(first.Attributes) == 0, "ALL_OLD on a new item returned %v", first.Attributes)
			t.Check(first.ConsumedCapacity != nil && aws.ToFloat64(first.ConsumedCapacity.CapacityUnits) > 0, "no ConsumedCapacity")

			second, err := t.DB.PutItem(t, &dynamodb.PutItemInput{
				TableName: aws.String(table), Item: item{"pk": k["pk"], "sk": k["sk"], "v": N(2)},
				ReturnValues: types.ReturnValueAllOld,
			})
			t.NoErr(err, "PutItem overwrite")
			t.Check(ddbx.Int(second.Attributes["v"]) == 1, "ALL_OLD on overwrite = %v", second.Attributes)
			got := t.Get(table, k)
			t.Check(ddbx.Int(got["v"]) == 2 && len(got) == 3, "PutItem did not replace the item: %v", got)
		}},

		{ID: "items/put-conditional", Group: g, Ops: []string{"PutItem"}, run: func(t *T) {
			table, k := t.Fixture(), key(t.PK("cond"), "a")
			put := func(v int64, cond string, values item) error {
				_, err := t.DB.PutItem(t, &dynamodb.PutItemInput{
					TableName: aws.String(table), Item: item{"pk": k["pk"], "sk": k["sk"], "v": N(v)},
					ConditionExpression: aws.String(cond), ExpressionAttributeValues: values,
					ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
				})
				return err
			}
			t.NoErr(put(1, "attribute_not_exists(pk)", nil), "first conditional put")

			err := put(2, "attribute_not_exists(pk)", nil)
			t.ErrCode(err, "ConditionalCheckFailedException", "put over an existing item")
			var ccf *types.ConditionalCheckFailedException
			t.Check(errors.As(err, &ccf) && ddbx.Int(ccf.Item["v"]) == 1, "ReturnValuesOnConditionCheckFailure did not return the old item: %v", err)
			t.Check(ddbx.Int(t.Get(table, k)["v"]) == 1, "a failed condition modified the item")

			t.NoErr(put(3, "v = :old AND attribute_exists(sk)", item{":old": N(1)}), "put with a true condition")
			t.ErrCode(put(4, "v BETWEEN :lo AND :hi", item{":lo": N(10), ":hi": N(20)}), "ConditionalCheckFailedException", "put with a false BETWEEN")
		}},

		{ID: "items/get", Group: g, Ops: []string{"GetItem"}, run: func(t *T) {
			table, k := t.Fixture(), key(t.PK("get"), "a")
			t.Put(table, item{"pk": k["pk"], "sk": k["sk"], "a": N(1), "name": S("x"),
				"m": &types.AttributeValueMemberM{Value: item{"in": N(7), "out": N(8)}},
				"l": &types.AttributeValueMemberL{Value: []types.AttributeValue{N(10), N(11)}}})

			out, err := t.DB.GetItem(t, &dynamodb.GetItemInput{
				TableName: aws.String(table), Key: k, ConsistentRead: aws.Bool(true),
				ProjectionExpression:     aws.String("a, #n, m.#in, l[1]"),
				ExpressionAttributeNames: map[string]string{"#n": "name", "#in": "in"},
				ReturnConsumedCapacity:   types.ReturnConsumedCapacityTotal,
			})
			t.NoErr(err, "GetItem with projection")
			m, _ := out.Item["m"].(*types.AttributeValueMemberM)
			l, _ := out.Item["l"].(*types.AttributeValueMemberL)
			t.Check(len(out.Item) == 4 && ddbx.Int(out.Item["a"]) == 1 && ddbx.Str(out.Item["name"]) == "x", "projection = %v", out.Item)
			t.Check(m != nil && len(m.Value) == 1 && ddbx.Int(m.Value["in"]) == 7, "nested projection m.in = %+v", out.Item["m"])
			t.Check(l != nil && len(l.Value) == 1 && ddbx.Int(l.Value[0]) == 11, "list projection l[1] = %+v", out.Item["l"])
			t.Check(out.ConsumedCapacity != nil, "no ConsumedCapacity")

			missing, err := t.DB.GetItem(t, &dynamodb.GetItemInput{TableName: aws.String(table), Key: key(t.PK("get"), "nope")})
			t.NoErr(err, "GetItem missing")
			t.Check(missing.Item == nil, "GetItem of a missing key returned %v", missing.Item)

			_, err = t.DB.GetItem(t, &dynamodb.GetItemInput{TableName: aws.String(table), Key: item{"pk": k["pk"]}})
			t.ErrCode(err, "ValidationException", "GetItem without the sort key")
			_, err = t.DB.GetItem(t, &dynamodb.GetItemInput{TableName: aws.String(table), Key: item{"pk": N(1), "sk": S("a")}})
			t.ErrCode(err, "ValidationException", "GetItem with a key of the wrong type")
			_, err = t.DB.GetItem(t, &dynamodb.GetItemInput{TableName: aws.String(t.TableName("missing")), Key: k})
			t.ErrCode(err, "ResourceNotFoundException", "GetItem on a missing table")
		}},

		{ID: "items/update-expressions", Group: g, Ops: []string{"UpdateItem"}, run: func(t *T) {
			table, k := t.Fixture(), key(t.PK("upd"), "a")
			update := func(expr string, names map[string]string, values item, rv types.ReturnValue) item {
				out, err := t.DB.UpdateItem(t, &dynamodb.UpdateItemInput{
					TableName: aws.String(table), Key: k, UpdateExpression: aws.String(expr),
					ExpressionAttributeNames: names, ExpressionAttributeValues: values, ReturnValues: rv,
				})
				t.NoErr(err, "UpdateItem "+expr)
				return out.Attributes
			}

			// Upsert: SET on a missing item creates it.
			got := update("SET n = :n, #l = :l, tags = :tags, m = :m", map[string]string{"#l": "list"},
				item{":n": N(10), ":l": &types.AttributeValueMemberL{Value: []types.AttributeValue{S("a")}},
					":tags": &types.AttributeValueMemberSS{Value: []string{"x", "y"}},
					":m":    &types.AttributeValueMemberM{Value: item{"a": N(1)}}}, types.ReturnValueAllNew)
			t.Check(ddbx.Int(got["n"]) == 10 && ddbx.Str(got["pk"]) == t.PK("upd"), "upsert ALL_NEW = %v", got)

			got = update("SET n = n + :inc, fallback = if_not_exists(fallback, :d), #l = list_append(#l, :more), m.b = :two",
				map[string]string{"#l": "list"},
				item{":inc": N(5), ":d": S("default"), ":more": &types.AttributeValueMemberL{Value: []types.AttributeValue{S("b")}}, ":two": N(2)},
				types.ReturnValueUpdatedNew)
			l, _ := got["list"].(*types.AttributeValueMemberL)
			m, _ := got["m"].(*types.AttributeValueMemberM)
			t.Check(ddbx.Int(got["n"]) == 15 && ddbx.Str(got["fallback"]) == "default", "arithmetic / if_not_exists = %v", got)
			t.Check(l != nil && len(l.Value) == 2 && ddbx.Str(l.Value[1]) == "b", "list_append = %+v", got["list"])
			t.Check(m != nil && ddbx.Int(m.Value["b"]) == 2, "nested SET with UPDATED_NEW = %+v", got["m"])
			_, hasPK := got["pk"]
			t.Check(!hasPK, "UPDATED_NEW returned unmodified attributes: %v", got)

			got = update("SET n = if_not_exists(n, :zero) - :one", nil, item{":zero": N(0), ":one": N(1)}, types.ReturnValueUpdatedOld)
			t.Check(ddbx.Int(got["n"]) == 15 && len(got) == 1, "UPDATED_OLD = %v", got)

			got = update("ADD n :five, tags :t, hits :one DELETE tags :x REMOVE fallback, #l[0]",
				map[string]string{"#l": "list"},
				item{":five": N(5), ":one": N(1), ":t": &types.AttributeValueMemberSS{Value: []string{"z"}},
					":x": &types.AttributeValueMemberSS{Value: []string{"x"}}}, types.ReturnValueAllNew)
			tags, _ := got["tags"].(*types.AttributeValueMemberSS)
			l, _ = got["list"].(*types.AttributeValueMemberL)
			_, hasMissing := got["fallback"]
			t.Check(ddbx.Int(got["n"]) == 19 && ddbx.Int(got["hits"]) == 1, "ADD numbers = %v", got)
			t.Check(tags != nil && sameSet(tags.Value, []string{"y", "z"}), "ADD/DELETE on a set = %+v", got["tags"])
			t.Check(!hasMissing && l != nil && len(l.Value) == 1 && ddbx.Str(l.Value[0]) == "b", "REMOVE = %v", got)

			old := update("SET n = :n", nil, item{":n": N(0)}, types.ReturnValueAllOld)
			t.Check(ddbx.Int(old["n"]) == 19 && ddbx.Str(old["sk"]) == "a", "ALL_OLD = %v", old)
		}},

		{ID: "items/update-conditional", Group: g, Ops: []string{"UpdateItem"}, run: func(t *T) {
			table, k := t.Fixture(), key(t.PK("updc"), "a")
			t.Put(table, item{"pk": k["pk"], "sk": k["sk"], "ver": N(1), "name": S("abc")})
			cas := func(expected int64) error {
				_, err := t.DB.UpdateItem(t, &dynamodb.UpdateItemInput{
					TableName: aws.String(table), Key: k,
					UpdateExpression:          aws.String("SET ver = ver + :one"),
					ConditionExpression:       aws.String("ver = :expected AND begins_with(#n, :p) AND size(#n) = :len AND attribute_type(ver, :t)"),
					ExpressionAttributeNames:  map[string]string{"#n": "name"},
					ExpressionAttributeValues: item{":one": N(1), ":expected": N(expected), ":p": S("ab"), ":len": N(3), ":t": S("N")},
				})
				return err
			}
			t.NoErr(cas(1), "conditional update")
			t.ErrCode(cas(1), "ConditionalCheckFailedException", "conditional update with a stale version")
			t.Check(ddbx.Int(t.Get(table, k)["ver"]) == 2, "version after one applied update = %d", ddbx.Int(t.Get(table, k)["ver"]))

			_, err := t.DB.UpdateItem(t, &dynamodb.UpdateItemInput{
				TableName: aws.String(table), Key: k,
				UpdateExpression:          aws.String("SET sk = :v"),
				ExpressionAttributeValues: item{":v": S("b")},
			})
			t.ErrCode(err, "ValidationException", "UpdateItem changing a key attribute")
			_, err = t.DB.UpdateItem(t, &dynamodb.UpdateItemInput{
				TableName: aws.String(table), Key: k,
				UpdateExpression:          aws.String("SET name = :v"),
				ExpressionAttributeValues: item{":v": S("b")},
			})
			t.ErrCode(err, "ValidationException", "UpdateItem using the reserved word NAME")
			_, err = t.DB.UpdateItem(t, &dynamodb.UpdateItemInput{
				TableName: aws.String(table), Key: k,
				UpdateExpression:          aws.String("SET ver = ver + :one"),
				ExpressionAttributeValues: item{":one": N(1), ":unused": N(1)},
			})
			t.ErrCode(err, "ValidationException", "UpdateItem with an unused expression value")
		}},

		{ID: "items/delete", Group: g, Ops: []string{"DeleteItem"}, run: func(t *T) {
			table, k := t.Fixture(), key(t.PK("del"), "a")
			t.Put(table, item{"pk": k["pk"], "sk": k["sk"], "v": N(1)})

			_, err := t.DB.DeleteItem(t, &dynamodb.DeleteItemInput{
				TableName: aws.String(table), Key: k,
				ConditionExpression: aws.String("v > :n"), ExpressionAttributeValues: item{":n": N(5)},
			})
			t.ErrCode(err, "ConditionalCheckFailedException", "conditional delete")
			t.Check(t.Get(table, k) != nil, "a failed conditional delete removed the item")

			out, err := t.DB.DeleteItem(t, &dynamodb.DeleteItemInput{TableName: aws.String(table), Key: k, ReturnValues: types.ReturnValueAllOld})
			t.NoErr(err, "DeleteItem")
			t.Check(ddbx.Int(out.Attributes["v"]) == 1, "ALL_OLD = %v", out.Attributes)
			t.Check(t.Get(table, k) == nil, "item still there after DeleteItem")

			again, err := t.DB.DeleteItem(t, &dynamodb.DeleteItemInput{TableName: aws.String(table), Key: k, ReturnValues: types.ReturnValueAllOld})
			t.NoErr(err, "DeleteItem of a missing item")
			t.Check(len(again.Attributes) == 0, "deleting a missing item returned %v", again.Attributes)
		}},

		{ID: "items/validation", Group: g, Ops: []string{"PutItem"}, run: func(t *T) {
			table := t.Fixture()
			put := func(it item) error {
				_, err := t.DB.PutItem(t, &dynamodb.PutItemInput{TableName: aws.String(table), Item: it})
				return err
			}
			t.ErrCode(put(item{"pk": S(t.PK("v"))}), "ValidationException", "PutItem without the sort key")
			t.ErrCode(put(item{"pk": S(t.PK("v")), "sk": N(1)}), "ValidationException", "PutItem with a key of the wrong type")
			t.ErrCode(put(item{"pk": S(t.PK("v")), "sk": S("")}), "ValidationException", "PutItem with an empty key")
			t.ErrCode(put(item{"pk": S(t.PK("v")), "sk": S("a"), "gsi_sk": S("not a number")}), "ValidationException", "PutItem with an index key of the wrong type")
			t.ErrCode(put(item{"pk": S(t.PK("v")), "sk": S("a"), "e": &types.AttributeValueMemberSS{Value: []string{}}}), "ValidationException", "PutItem with an empty set")
			big := bytes.Repeat([]byte("x"), 401*1024)
			t.ErrCode(put(item{"pk": S(t.PK("v")), "sk": S("a"), "big": S(string(big))}), "ValidationException", "PutItem over 400 KB")
			_, err := t.DB.PutItem(t, &dynamodb.PutItemInput{TableName: aws.String(t.TableName("missing")), Item: key("a", "b")})
			t.ErrCode(err, "ResourceNotFoundException", "PutItem on a missing table")
		}},
	}
}

func sameSet(got, want []string) bool {
	got, want = slices.Clone(got), slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}
