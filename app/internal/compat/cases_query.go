package compat

import (
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

// seed writes n items under one partition key: sk "0000".."00nn", num = i,
// lsi_sk in reverse order, parity "even"/"odd", plus the index keys given by extra.
func seed(t *T, pk string, n int, extra func(i int, it item)) {
	table := t.Fixture()
	for i := 0; i < n; i++ {
		it := item{
			"pk": S(pk), "sk": S(fmt.Sprintf("%04d", i)), "num": N(int64(i)),
			"lsi_sk": S(fmt.Sprintf("%04d", n-1-i)), "parity": S([]string{"even", "odd"}[i%2]),
		}
		if extra != nil {
			extra(i, it)
		}
		t.Put(table, it)
	}
}

func sks(items []item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = ddbx.Str(it["sk"])
	}
	return out
}

func queryCases() []Case {
	const g = "query-scan"
	return []Case{
		{ID: "query/key-conditions", Group: g, Ops: []string{"Query"}, run: func(t *T) {
			table, pk := t.Fixture(), t.PK("kc")
			seed(t, pk, 10, nil)
			query := func(cond string, values item) []string {
				values[":pk"] = S(pk)
				out, err := t.DB.Query(t, &dynamodb.QueryInput{
					TableName: aws.String(table), ConsistentRead: aws.Bool(true),
					KeyConditionExpression: aws.String("pk = :pk" + cond), ExpressionAttributeValues: values,
				})
				t.NoErr(err, "Query"+cond)
				t.Check(int(out.Count) == len(out.Items), "Count %d != %d items", out.Count, len(out.Items))
				return sks(out.Items)
			}
			eq := func(what string, got []string, want ...string) {
				t.Check(slices.Equal(got, want), "%s = %v, want %v", what, got, want)
			}
			t.Check(len(query("", item{})) == 10, "partition query did not return 10 items")
			eq("sk =", query(" AND sk = :a", item{":a": S("0003")}), "0003")
			eq("sk <", query(" AND sk < :a", item{":a": S("0002")}), "0000", "0001")
			eq("sk <=", query(" AND sk <= :a", item{":a": S("0001")}), "0000", "0001")
			eq("sk >", query(" AND sk > :a", item{":a": S("0007")}), "0008", "0009")
			eq("sk >=", query(" AND sk >= :a", item{":a": S("0009")}), "0009")
			eq("BETWEEN", query(" AND sk BETWEEN :a AND :b", item{":a": S("0003"), ":b": S("0005")}), "0003", "0004", "0005")
			eq("begins_with", query(" AND begins_with(sk, :a)", item{":a": S("000")}),
				"0000", "0001", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009")

			_, err := t.DB.Query(t, &dynamodb.QueryInput{
				TableName: aws.String(table), KeyConditionExpression: aws.String("sk = :a"),
				ExpressionAttributeValues: item{":a": S("0001")},
			})
			t.ErrCode(err, "ValidationException", "Query without the partition key")
			_, err = t.DB.Query(t, &dynamodb.QueryInput{
				TableName: aws.String(table), KeyConditionExpression: aws.String("pk = :pk AND num = :n"),
				ExpressionAttributeValues: item{":pk": S(pk), ":n": N(1)},
			})
			t.ErrCode(err, "ValidationException", "Query with a non-key attribute in the key condition")
		}},

		{ID: "query/pagination-order-filter", Group: g, Ops: []string{"Query"}, run: func(t *T) {
			table, pk := t.Fixture(), t.PK("page")
			seed(t, pk, 10, nil)

			var got []string
			pages := 0
			var start item
			for {
				out, err := t.DB.Query(t, &dynamodb.QueryInput{
					TableName: aws.String(table), ConsistentRead: aws.Bool(true),
					KeyConditionExpression: aws.String("pk = :pk"), ExpressionAttributeValues: item{":pk": S(pk)},
					ScanIndexForward: aws.Bool(false), Limit: aws.Int32(4), ExclusiveStartKey: start,
				})
				t.NoErr(err, "Query page")
				t.Check(len(out.Items) <= 4, "Limit=4 returned %d items", len(out.Items))
				got = append(got, sks(out.Items)...)
				pages++
				if start = out.LastEvaluatedKey; start == nil {
					break
				}
				t.Check(len(start) == 2, "LastEvaluatedKey = %v", start)
			}
			want := []string{"0009", "0008", "0007", "0006", "0005", "0004", "0003", "0002", "0001", "0000"}
			t.Check(slices.Equal(got, want), "descending paginated query = %v", got)
			t.Check(pages >= 3, "10 items with Limit=4 came in %d pages", pages)

			// Limit applies before the filter: 4 items are read, 2 of them are even.
			out, err := t.DB.Query(t, &dynamodb.QueryInput{
				TableName: aws.String(table), ConsistentRead: aws.Bool(true),
				KeyConditionExpression: aws.String("pk = :pk"), FilterExpression: aws.String("parity = :p AND num >= :zero"),
				ExpressionAttributeValues: item{":pk": S(pk), ":p": S("even"), ":zero": N(0)},
				Limit:                     aws.Int32(4),
			})
			t.NoErr(err, "Query with filter")
			t.Check(out.Count == 2 && out.ScannedCount == 4, "filter with Limit=4: Count=%d ScannedCount=%d, want 2 and 4", out.Count, out.ScannedCount)
			t.Check(out.LastEvaluatedKey != nil, "filtered page has no LastEvaluatedKey")

			count, err := t.DB.Query(t, &dynamodb.QueryInput{
				TableName: aws.String(table), ConsistentRead: aws.Bool(true), Select: types.SelectCount,
				KeyConditionExpression: aws.String("pk = :pk"), ExpressionAttributeValues: item{":pk": S(pk)},
			})
			t.NoErr(err, "Query Select=COUNT")
			t.Check(count.Count == 10 && len(count.Items) == 0, "Select=COUNT: Count=%d Items=%d", count.Count, len(count.Items))

			proj, err := t.DB.Query(t, &dynamodb.QueryInput{
				TableName: aws.String(table), ConsistentRead: aws.Bool(true),
				KeyConditionExpression: aws.String("pk = :pk AND sk = :sk"), ProjectionExpression: aws.String("num"),
				ExpressionAttributeValues: item{":pk": S(pk), ":sk": S("0004")},
			})
			t.NoErr(err, "Query with projection")
			t.Check(len(proj.Items) == 1 && len(proj.Items[0]) == 1 && ddbx.Int(proj.Items[0]["num"]) == 4, "projection = %v", proj.Items)
		}},

		{ID: "query/lsi", Group: g, Ops: []string{"Query"}, run: func(t *T) {
			table, pk := t.Fixture(), t.PK("lsi")
			seed(t, pk, 6, nil)
			// lsi_sk is the reverse of sk, so the LSI returns items in reverse sk
			// order. A consistent read on an LSI must see the write immediately.
			out, err := t.DB.Query(t, &dynamodb.QueryInput{
				TableName: aws.String(table), IndexName: aws.String(lsi1), ConsistentRead: aws.Bool(true),
				KeyConditionExpression:    aws.String("pk = :pk AND lsi_sk < :x"),
				ExpressionAttributeValues: item{":pk": S(pk), ":x": S("0003")},
			})
			t.NoErr(err, "consistent Query on the LSI")
			t.Check(slices.Equal(sks(out.Items), []string{"0005", "0004", "0003"}), "LSI order = %v", sks(out.Items))
			t.Check(len(out.Items) == 3 && ddbx.Int(out.Items[0]["num"]) == 5, "LSI with ALL projection misses attributes: %v", out.Items)

			// An item without the LSI sort key is not in the index.
			t.Put(table, item{"pk": S(pk), "sk": S("no-lsi-key")})
			all, err := t.DB.Query(t, &dynamodb.QueryInput{
				TableName: aws.String(table), IndexName: aws.String(lsi1), ConsistentRead: aws.Bool(true), Select: types.SelectCount,
				KeyConditionExpression: aws.String("pk = :pk"), ExpressionAttributeValues: item{":pk": S(pk)},
			})
			t.NoErr(err, "Query the whole LSI partition")
			t.Check(all.Count == 6, "LSI holds %d items, want 6 (sparse)", all.Count)
		}},

		{ID: "query/gsi", Group: g, Ops: []string{"Query"}, run: func(t *T) {
			table, pk, gpk := t.Fixture(), t.PK("gsi"), t.PK("gsi-partition")
			seed(t, pk, 6, func(i int, it item) {
				if i < 5 { // item 5 has no index key: the GSI is sparse.
					it["gsi_pk"], it["gsi_sk"] = S(gpk), N(int64(100-i))
				}
				it["gsi2_pk"], it["gsi3_pk"], it["data"], it["other"] = S(gpk), S(gpk), S("payload"), S("not projected")
			})
			query := func(index string, cond string, values item) *dynamodb.QueryOutput {
				values[":g"] = S(gpk)
				out, err := t.DB.Query(t, &dynamodb.QueryInput{
					TableName: aws.String(table), IndexName: aws.String(index),
					KeyConditionExpression: aws.String(cond), ExpressionAttributeValues: values,
				})
				t.NoErr(err, "Query "+index)
				return out
			}

			var out *dynamodb.QueryOutput
			t.Eventually(15*time.Second, "GSI reflects the writes", func() bool {
				out = query(gsiAll, "gsi_pk = :g", item{})
				return out.Count == 5
			})
			t.Check(slices.Equal(sks(out.Items), []string{"0004", "0003", "0002", "0001", "0000"}), "GSI order by numeric sort key = %v", sks(out.Items))
			t.Check(ddbx.Str(out.Items[0]["other"]) == "not projected", "ALL projection misses attributes: %v", out.Items[0])
			ranged := query(gsiAll, "gsi_pk = :g AND gsi_sk BETWEEN :a AND :b", item{":a": N(97), ":b": N(98)})
			t.Check(slices.Equal(sks(ranged.Items), []string{"0003", "0002"}), "GSI numeric range = %v", sks(ranged.Items))

			t.Eventually(15*time.Second, "KEYS_ONLY GSI reflects the writes", func() bool {
				out = query(gsiKeys, "gsi2_pk = :g", item{})
				return out.Count == 6
			})
			t.Check(len(out.Items[0]) == 3, "KEYS_ONLY projection returned %v", out.Items[0])
			t.Eventually(15*time.Second, "INCLUDE GSI reflects the writes", func() bool {
				out = query(gsiInclu, "gsi3_pk = :g", item{})
				return out.Count == 6
			})
			t.Check(len(out.Items[0]) == 4 && ddbx.Str(out.Items[0]["data"]) == "payload", "INCLUDE projection returned %v", out.Items[0])

			// Updating and deleting base items moves and removes index entries.
			_, err := t.DB.UpdateItem(t, &dynamodb.UpdateItemInput{
				TableName: aws.String(table), Key: key(pk, "0000"),
				UpdateExpression: aws.String("REMOVE gsi_pk"),
			})
			t.NoErr(err, "UpdateItem REMOVE gsi_pk")
			_, err = t.DB.DeleteItem(t, &dynamodb.DeleteItemInput{TableName: aws.String(table), Key: key(pk, "0001")})
			t.NoErr(err, "DeleteItem")
			t.Eventually(15*time.Second, "GSI reflects the update and delete", func() bool {
				return query(gsiAll, "gsi_pk = :g", item{}).Count == 3
			})

			_, err = t.DB.Query(t, &dynamodb.QueryInput{
				TableName: aws.String(table), IndexName: aws.String(gsiAll), ConsistentRead: aws.Bool(true),
				KeyConditionExpression: aws.String("gsi_pk = :g"), ExpressionAttributeValues: item{":g": S(gpk)},
			})
			t.ErrCode(err, "ValidationException", "consistent Query on a GSI")
			_, err = t.DB.Query(t, &dynamodb.QueryInput{
				TableName: aws.String(table), IndexName: aws.String("no_such_index"),
				KeyConditionExpression: aws.String("gsi_pk = :g"), ExpressionAttributeValues: item{":g": S(gpk)},
			})
			t.ErrCode(err, "ValidationException", "Query on a missing index")
		}},

		{ID: "scan/pagination-filter", Group: g, Ops: []string{"Scan"}, run: func(t *T) {
			table, pk := t.Fixture(), t.PK("scan")
			seed(t, pk, 12, nil)
			mine := item{":pk": S(pk)}

			seen := map[string]bool{}
			pager := dynamodb.NewScanPaginator(t.DB, &dynamodb.ScanInput{
				TableName: aws.String(table), ConsistentRead: aws.Bool(true), Limit: aws.Int32(5),
				FilterExpression: aws.String("pk = :pk"), ExpressionAttributeValues: mine,
			})
			for pager.HasMorePages() {
				page, err := pager.NextPage(t)
				t.NoErr(err, "Scan page")
				t.Check(page.ScannedCount <= 5, "Limit=5 scanned %d items", page.ScannedCount)
				for _, sk := range sks(page.Items) {
					t.Check(!seen[sk], "Scan returned %s twice", sk)
					seen[sk] = true
				}
			}
			t.Check(len(seen) == 12, "paginated Scan found %d of 12 items", len(seen))

			out, err := t.DB.Scan(t, &dynamodb.ScanInput{
				TableName: aws.String(table), ConsistentRead: aws.Bool(true), Select: types.SelectCount,
				FilterExpression:          aws.String("pk = :pk AND (num IN (:a, :b) OR contains(sk, :sub)) AND NOT attribute_exists(nope)"),
				ExpressionAttributeValues: item{":pk": S(pk), ":a": N(1), ":b": N(2), ":sub": S("011")},
			})
			t.NoErr(err, "Scan with filter functions")
			total := out.Count
			for out.LastEvaluatedKey != nil {
				out, err = t.DB.Scan(t, &dynamodb.ScanInput{
					TableName: aws.String(table), ConsistentRead: aws.Bool(true), Select: types.SelectCount,
					FilterExpression:          aws.String("pk = :pk AND (num IN (:a, :b) OR contains(sk, :sub)) AND NOT attribute_exists(nope)"),
					ExpressionAttributeValues: item{":pk": S(pk), ":a": N(1), ":b": N(2), ":sub": S("011")},
					ExclusiveStartKey:         out.LastEvaluatedKey,
				})
				t.NoErr(err, "Scan with filter functions")
				total += out.Count
			}
			t.Check(total == 3, "filter IN/contains/NOT matched %d items, want 3", total)
		}},

		{ID: "scan/parallel-segments", Group: g, Ops: []string{"Scan"}, run: func(t *T) {
			table, pk := t.Fixture(), t.PK("pscan")
			seed(t, pk, 12, nil)
			const segments = 4
			seen := map[string]int{}
			for seg := int32(0); seg < segments; seg++ {
				pager := dynamodb.NewScanPaginator(t.DB, &dynamodb.ScanInput{
					TableName: aws.String(table), ConsistentRead: aws.Bool(true),
					Segment: aws.Int32(seg), TotalSegments: aws.Int32(segments),
					FilterExpression: aws.String("pk = :pk"), ExpressionAttributeValues: item{":pk": S(pk)},
				})
				for pager.HasMorePages() {
					page, err := pager.NextPage(t)
					t.NoErr(err, "Scan segment")
					for _, sk := range sks(page.Items) {
						seen[sk]++
					}
				}
			}
			t.Check(len(seen) == 12, "segments together returned %d of 12 items", len(seen))
			for sk, n := range seen {
				t.Check(n == 1, "item %s is in %d segments", sk, n)
			}
			_, err := t.DB.Scan(t, &dynamodb.ScanInput{TableName: aws.String(table), Segment: aws.Int32(4), TotalSegments: aws.Int32(4)})
			t.ErrCode(err, "ValidationException", "Scan with Segment >= TotalSegments")
		}},

		{ID: "scan/index", Group: g, Ops: []string{"Scan"}, run: func(t *T) {
			table, pk, gpk := t.Fixture(), t.PK("iscan"), t.PK("iscan-partition")
			seed(t, pk, 4, func(i int, it item) { it["gsi2_pk"] = S(gpk) })
			scan := func(index string, consistent bool, filter string, values item) (int32, error) {
				var total int32
				pager := dynamodb.NewScanPaginator(t.DB, &dynamodb.ScanInput{
					TableName: aws.String(table), IndexName: aws.String(index), ConsistentRead: aws.Bool(consistent),
					Select: types.SelectCount, FilterExpression: aws.String(filter), ExpressionAttributeValues: values,
				})
				for pager.HasMorePages() {
					page, err := pager.NextPage(t)
					if err != nil {
						return 0, err
					}
					total += page.Count
				}
				return total, nil
			}
			n, err := scan(lsi1, true, "pk = :pk", item{":pk": S(pk)})
			t.NoErr(err, "consistent Scan on the LSI")
			t.Check(n == 4, "LSI scan found %d of 4 items", n)
			t.Eventually(15*time.Second, "GSI scan reflects the writes", func() bool {
				n, err := scan(gsiKeys, false, "gsi2_pk = :g", item{":g": S(gpk)})
				t.NoErr(err, "Scan on the GSI")
				return n == 4
			})
			_, err = scan(gsiKeys, true, "gsi2_pk = :g", item{":g": S(gpk)})
			t.ErrCode(err, "ValidationException", "consistent Scan on a GSI")
		}},
		{ID: "vectors/search", Group: g, Ops: []string{"SearchVectors"}, run: func(t *T) {
			name := t.TableName("vectors")
			spec := simpleTable(name)
			spec.VectorIndexes = []types.VectorIndex{{
				IndexName: aws.String("by_embedding"), Dimensions: aws.Int64(3),
				DistanceFunction: types.VectorDistanceFunctionEuclidean,
				VectorAttribute:  &types.VectorAttributeDefinition{AttributeName: aws.String("embedding")},
				Projection:       &types.Projection{ProjectionType: types.ProjectionTypeAll},
			}}
			t.CreateTable(spec)
			defer t.DropTable(name)
			vector := func(x, y, z int64) []types.AttributeValue { return []types.AttributeValue{N(x), N(y), N(z)} }
			for pk, v := range map[string][]types.AttributeValue{"near": vector(1, 0, 0), "mid": vector(5, 5, 0), "far": vector(50, 50, 50)} {
				t.Put(name, item{"pk": S(pk), "embedding": &types.AttributeValueMemberL{Value: v}})
			}
			// Vector indexes are maintained asynchronously, like a GSI.
			var out *dynamodb.SearchVectorsOutput
			t.Eventually(30*time.Second, "vector index reflects the writes", func() bool {
				var err error
				out, err = t.DB.SearchVectors(t, &dynamodb.SearchVectorsInput{
					TableName: aws.String(name), IndexName: aws.String("by_embedding"),
					SearchVector: vector(0, 0, 0), TopK: aws.Int32(2),
				})
				t.NoErr(err, "SearchVectors")
				return len(out.SearchResults) == 2
			})
			first, second := out.SearchResults[0], out.SearchResults[1]
			t.Check(ddbx.Str(first.Item["pk"]) == "near" && ddbx.Str(second.Item["pk"]) == "mid",
				"nearest neighbours = %s, %s", ddbx.Str(first.Item["pk"]), ddbx.Str(second.Item["pk"]))
			t.Check(first.Score <= second.Score, "scores are not ordered by distance: %v, %v", first.Score, second.Score)
		}},
	}
}
