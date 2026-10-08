package compat

import (
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	streamtypes "github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

func streamS(av streamtypes.AttributeValue) string {
	if s, ok := av.(*streamtypes.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func streamN(av streamtypes.AttributeValue) string {
	if n, ok := av.(*streamtypes.AttributeValueMemberN); ok {
		return n.Value
	}
	return ""
}

// fixtureStream returns the fixture table's stream ARN and all its shards.
func fixtureStream(t *T) (*string, []streamtypes.Shard) {
	desc, err := t.DB.DescribeTable(t, &dynamodb.DescribeTableInput{TableName: aws.String(t.Fixture())})
	t.NoErr(err, "DescribeTable")
	arn := desc.Table.LatestStreamArn
	t.Check(arn != nil, "the fixture table has no LatestStreamArn")

	var shards []streamtypes.Shard
	var start *string
	for {
		out, err := t.Streams.DescribeStream(t, &dynamodbstreams.DescribeStreamInput{StreamArn: arn, ExclusiveStartShardId: start})
		t.NoErr(err, "DescribeStream")
		d := out.StreamDescription
		t.Check(d.StreamStatus == streamtypes.StreamStatusEnabled, "StreamStatus = %s", d.StreamStatus)
		t.Check(d.StreamViewType == streamtypes.StreamViewTypeNewAndOldImages, "StreamViewType = %s", d.StreamViewType)
		t.Check(aws.ToString(d.TableName) == t.Fixture(), "stream TableName = %q", aws.ToString(d.TableName))
		shards = append(shards, d.Shards...)
		if start = d.LastEvaluatedShardId; start == nil {
			break
		}
	}
	t.Check(len(shards) > 0, "DescribeStream returned no shards")
	return arn, shards
}

// recordsFor reads every shard from iterators made by newIterator and returns
// the records whose partition key is pk, polling until want of them arrived.
func recordsFor(t *T, pk string, want int, shards []streamtypes.Shard, newIterator func(shard streamtypes.Shard) *string) []streamtypes.Record {
	iterators := make([]*string, len(shards))
	for i, shard := range shards {
		iterators[i] = newIterator(shard)
	}
	var records []streamtypes.Record
	t.Eventually(20*time.Second, "stream records arrive", func() bool {
		for i, it := range iterators {
			if it == nil {
				continue
			}
			out, err := t.Streams.GetRecords(t, &dynamodbstreams.GetRecordsInput{ShardIterator: it})
			t.NoErr(err, "GetRecords")
			for _, rec := range out.Records {
				if streamS(rec.Dynamodb.Keys["pk"]) == pk {
					records = append(records, rec)
				}
			}
			iterators[i] = out.NextShardIterator
		}
		return len(records) >= want
	})
	return records
}

func streamCases() []Case {
	const g = "streams"
	return []Case{
		{ID: "streams/list-describe", Group: g, Ops: []string{"streams:ListStreams", "streams:DescribeStream"}, run: func(t *T) {
			arn, _ := fixtureStream(t)
			out, err := t.Streams.ListStreams(t, &dynamodbstreams.ListStreamsInput{TableName: aws.String(t.Fixture())})
			t.NoErr(err, "ListStreams")
			t.Check(len(out.Streams) == 1 && aws.ToString(out.Streams[0].StreamArn) == aws.ToString(arn),
				"ListStreams for the table = %+v, want the table's LatestStreamArn", out.Streams)
			t.Check(aws.ToString(out.Streams[0].StreamLabel) != "", "stream has no StreamLabel")
		}},

		{ID: "streams/records", Group: g, Ops: []string{"streams:GetShardIterator", "streams:GetRecords"}, run: func(t *T) {
			table, pk := t.Fixture(), t.PK("events")
			arn, shards := fixtureStream(t)
			// LATEST iterators taken before the writes must return exactly them.
			latest := func(shard streamtypes.Shard) *string {
				out, err := t.Streams.GetShardIterator(t, &dynamodbstreams.GetShardIteratorInput{
					StreamArn: arn, ShardId: shard.ShardId, ShardIteratorType: streamtypes.ShardIteratorTypeLatest,
				})
				t.NoErr(err, "GetShardIterator LATEST")
				return out.ShardIterator
			}
			iterators := map[string]*string{}
			for _, shard := range shards {
				iterators[aws.ToString(shard.ShardId)] = latest(shard)
			}

			k := key(pk, "a")
			t.Delete(table, k)
			t.Put(table, item{"pk": k["pk"], "sk": k["sk"], "v": N(1)})
			t.Put(table, item{"pk": k["pk"], "sk": k["sk"], "v": N(2)})
			t.Delete(table, k)

			records := recordsFor(t, pk, 3, shards, func(shard streamtypes.Shard) *string { return iterators[aws.ToString(shard.ShardId)] })
			// A leftover item from an earlier execution adds a leading REMOVE.
			if len(records) == 4 && records[0].EventName == streamtypes.OperationTypeRemove {
				records = records[1:]
			}
			t.Check(len(records) == 3, "got %d records for 3 writes", len(records))
			ins, mod, rem := records[0], records[1], records[2]
			t.Check(ins.EventName == streamtypes.OperationTypeInsert && mod.EventName == streamtypes.OperationTypeModify &&
				rem.EventName == streamtypes.OperationTypeRemove, "event names = %s, %s, %s", ins.EventName, mod.EventName, rem.EventName)
			t.Check(streamN(ins.Dynamodb.NewImage["v"]) == "1" && len(ins.Dynamodb.OldImage) == 0, "INSERT images: new=%v old=%v", ins.Dynamodb.NewImage, ins.Dynamodb.OldImage)
			t.Check(streamN(mod.Dynamodb.NewImage["v"]) == "2" && streamN(mod.Dynamodb.OldImage["v"]) == "1", "MODIFY images: new=%v old=%v", mod.Dynamodb.NewImage, mod.Dynamodb.OldImage)
			t.Check(streamN(rem.Dynamodb.OldImage["v"]) == "2" && len(rem.Dynamodb.NewImage) == 0, "REMOVE images: new=%v old=%v", rem.Dynamodb.NewImage, rem.Dynamodb.OldImage)
			t.Check(streamS(ins.Dynamodb.Keys["sk"]) == "a" && len(ins.Dynamodb.Keys) == 2, "record Keys = %v", ins.Dynamodb.Keys)
			t.Check(aws.ToString(ins.EventID) != "" && aws.ToString(ins.Dynamodb.SequenceNumber) != "" &&
				ins.Dynamodb.StreamViewType == streamtypes.StreamViewTypeNewAndOldImages, "record metadata incomplete: %+v", ins)
			t.Check(ddbx.SeqLess(aws.ToString(ins.Dynamodb.SequenceNumber), aws.ToString(mod.Dynamodb.SequenceNumber)) &&
				ddbx.SeqLess(aws.ToString(mod.Dynamodb.SequenceNumber), aws.ToString(rem.Dynamodb.SequenceNumber)), "sequence numbers do not increase")

			// TRIM_HORIZON replays them; AFTER_SEQUENCE_NUMBER resumes past the INSERT.
			replay := recordsFor(t, pk, 3, shards, func(shard streamtypes.Shard) *string {
				out, err := t.Streams.GetShardIterator(t, &dynamodbstreams.GetShardIteratorInput{
					StreamArn: arn, ShardId: shard.ShardId, ShardIteratorType: streamtypes.ShardIteratorTypeTrimHorizon,
				})
				t.NoErr(err, "GetShardIterator TRIM_HORIZON")
				return out.ShardIterator
			})
			t.Check(len(replay) >= 3, "TRIM_HORIZON replayed %d records", len(replay))
			after := recordsFor(t, pk, 2, shards, func(shard streamtypes.Shard) *string {
				out, err := t.Streams.GetShardIterator(t, &dynamodbstreams.GetShardIteratorInput{
					StreamArn: arn, ShardId: shard.ShardId, ShardIteratorType: streamtypes.ShardIteratorTypeAfterSequenceNumber,
					SequenceNumber: ins.Dynamodb.SequenceNumber,
				})
				if err != nil { // the sequence number belongs to one shard only
					return nil
				}
				return out.ShardIterator
			})
			t.Check(len(after) == 2 && after[0].EventName == streamtypes.OperationTypeModify, "AFTER_SEQUENCE_NUMBER returned %d records", len(after))
		}},
	}
}
