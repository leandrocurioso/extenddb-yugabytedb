package workload

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	streamtypes "github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

// OrderStatuses are the statuses an order may have; the GSI audit queries one
// partition per status.
var OrderStatuses = []string{"NEW", "PAID", "SHIPPED"}

type Order struct {
	CustomerID string `json:"customer_id" binding:"required"`
	OrderID    string `json:"order_id" binding:"required"`
	Run        string `json:"run" binding:"required"`
	Status     string `json:"status" binding:"required"`
	CreatedAt  string `json:"created_at"`
	Version    int64  `json:"version"`
	Cell       string `json:"cell"`
}

type OrderKey struct {
	CustomerID string `json:"customer_id" binding:"required"`
	OrderID    string `json:"order_id" binding:"required"`
}

func (k OrderKey) String() string { return k.CustomerID + "/" + k.OrderID }

func (k OrderKey) key() map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"customer_id": ddbx.S(k.CustomerID), "order_id": ddbx.S(k.OrderID)}
}

func (s *Store) orderItem(o Order) map[string]types.AttributeValue {
	if o.CreatedAt == "" {
		o.CreatedAt = now()
	}
	return map[string]types.AttributeValue{
		"customer_id": ddbx.S(o.CustomerID), "order_id": ddbx.S(o.OrderID),
		"run": ddbx.S(o.Run), "status": ddbx.S(o.Status), "gsi_pk": ddbx.S(o.Run + "#" + o.Status),
		"created_at": ddbx.S(o.CreatedAt), "ver": ddbx.N(1), "cell": ddbx.S(s.cell),
	}
}

func orderFrom(item map[string]types.AttributeValue) Order {
	return Order{
		CustomerID: ddbx.Str(item["customer_id"]), OrderID: ddbx.Str(item["order_id"]),
		Run: ddbx.Str(item["run"]), Status: ddbx.Str(item["status"]),
		CreatedAt: ddbx.Str(item["created_at"]), Version: ddbx.Int(item["ver"]), Cell: ddbx.Str(item["cell"]),
	}
}

func ordersFrom(items []map[string]types.AttributeValue) []Order {
	orders := make([]Order, len(items))
	for i, item := range items {
		orders[i] = orderFrom(item)
	}
	return orders
}

func (s *Store) OrderPut(ctx context.Context, o Order) error {
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(OrdersTable), Item: s.orderItem(o)})
	return err
}

// OrdersBatchPut writes the orders with BatchWriteItem.
func (s *Store) OrdersBatchPut(ctx context.Context, orders []Order) error {
	reqs := make([]types.WriteRequest, len(orders))
	for i, o := range orders {
		reqs[i] = types.WriteRequest{PutRequest: &types.PutRequest{Item: s.orderItem(o)}}
	}
	return s.batchWrite(ctx, OrdersTable, reqs)
}

// OrderSetStatus moves an existing order to status and bumps its version, which
// also moves it to another GSI partition.
func (s *Store) OrderSetStatus(ctx context.Context, k OrderKey, run, status string) (Order, error) {
	out, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                aws.String(OrdersTable),
		Key:                      k.key(),
		UpdateExpression:         aws.String("SET #s = :s, gsi_pk = :g, cell = :c ADD ver :one"),
		ConditionExpression:      aws.String("attribute_exists(order_id)"),
		ExpressionAttributeNames: map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":s": ddbx.S(status), ":g": ddbx.S(run + "#" + status), ":c": ddbx.S(s.cell), ":one": ddbx.N(1),
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil {
		return Order{}, conflictOr(err)
	}
	return orderFrom(out.Attributes), nil
}

func (s *Store) OrderDelete(ctx context.Context, k OrderKey) error {
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(OrdersTable), Key: k.key()})
	return err
}

// OrdersGet returns the orders that exist among keys (consistent BatchGetItem).
func (s *Store) OrdersGet(ctx context.Context, keys []OrderKey) ([]Order, error) {
	avKeys := make([]map[string]types.AttributeValue, len(keys))
	for i, k := range keys {
		avKeys[i] = k.key()
	}
	items, err := s.batchGet(ctx, OrdersTable, avKeys)
	return ordersFrom(items), err
}

// OrdersByCustomer queries one customer's orders on the base table (ordered by
// order_id) or, with lsi, on the LSI (ordered by created_at).
func (s *Store) OrdersByCustomer(ctx context.Context, customer string, lsi, consistent bool) ([]Order, error) {
	in := &dynamodb.QueryInput{
		TableName:                 aws.String(OrdersTable),
		KeyConditionExpression:    aws.String("customer_id = :c"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":c": ddbx.S(customer)},
		ConsistentRead:            aws.Bool(consistent),
	}
	if lsi {
		in.IndexName = aws.String(OrdersLSI)
	}
	return s.queryAll(ctx, in)
}

// OrdersByStatus queries the GSI partition of a run's status (eventually consistent).
func (s *Store) OrdersByStatus(ctx context.Context, run, status string) ([]Order, error) {
	return s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String(OrdersTable),
		IndexName:                 aws.String(OrdersGSI),
		KeyConditionExpression:    aws.String("gsi_pk = :g"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":g": ddbx.S(run + "#" + status)},
	})
}

func (s *Store) queryAll(ctx context.Context, in *dynamodb.QueryInput) ([]Order, error) {
	orders := []Order{}
	pager := dynamodb.NewQueryPaginator(s.db, in)
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		orders = append(orders, ordersFrom(page.Items)...)
	}
	return orders, nil
}

// ordersOfRun scans the base table for a run's orders (consistent, paginated).
func (s *Store) ordersOfRun(ctx context.Context, run string) (map[OrderKey]Order, error) {
	base := map[OrderKey]Order{}
	pager := dynamodb.NewScanPaginator(s.db, &dynamodb.ScanInput{
		TableName:                 aws.String(OrdersTable),
		ConsistentRead:            aws.Bool(true),
		FilterExpression:          aws.String("#r = :r"),
		ExpressionAttributeNames:  map[string]string{"#r": "run"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":r": ddbx.S(run)},
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range ordersFrom(page.Items) {
			base[OrderKey{o.CustomerID, o.OrderID}] = o
		}
	}
	return base, nil
}

type OrdersAudit struct {
	Base int `json:"base"`
	GSI  int `json:"gsi"`
	// GSI entries compared with the base table: absent, carrying another
	// version/status, or present for an order the base table no longer has.
	GSIMissing []string `json:"gsi_missing"`
	GSIStale   []string `json:"gsi_stale"`
	GSIExtra   []string `json:"gsi_extra"`
	// LSIMismatch lists customers whose consistent LSI query differs from the
	// base table or is not ordered by created_at.
	LSIMismatch []string `json:"lsi_mismatch"`
	Converged   bool     `json:"converged"`
}

const auditSample = 20

func sample(list []string) []string {
	sort.Strings(list)
	if len(list) > auditSample {
		return list[:auditSample]
	}
	return list
}

// OrdersAudit compares a run's orders in the base table with what the GSI and
// the LSI return. The GSI is eventually consistent, so callers poll until
// Converged; the LSI is read consistently and must match at once. Meant to run
// once the run's writes have stopped.
func (s *Store) OrdersAudit(ctx context.Context, run string) (OrdersAudit, error) {
	audit := OrdersAudit{GSIMissing: []string{}, GSIStale: []string{}, GSIExtra: []string{}, LSIMismatch: []string{}}
	base, err := s.ordersOfRun(ctx, run)
	if err != nil {
		return audit, err
	}
	audit.Base = len(base)

	seen := map[OrderKey]bool{}
	for _, status := range OrderStatuses {
		indexed, err := s.OrdersByStatus(ctx, run, status)
		if err != nil {
			return audit, err
		}
		audit.GSI += len(indexed)
		for _, o := range indexed {
			k := OrderKey{o.CustomerID, o.OrderID}
			seen[k] = true
			if b, ok := base[k]; !ok {
				audit.GSIExtra = append(audit.GSIExtra, k.String())
			} else if b.Version != o.Version || b.Status != o.Status || o.Status != status {
				audit.GSIStale = append(audit.GSIStale, k.String())
			}
		}
	}
	byCustomer := map[string][]Order{}
	for k, o := range base {
		if !seen[k] {
			audit.GSIMissing = append(audit.GSIMissing, k.String())
		}
		byCustomer[k.CustomerID] = append(byCustomer[k.CustomerID], o)
	}

	for customer, want := range byCustomer {
		got, err := s.OrdersByCustomer(ctx, customer, true, true)
		if err != nil {
			return audit, err
		}
		ok := len(got) == len(want)
		for i, o := range got {
			if b, found := base[OrderKey{o.CustomerID, o.OrderID}]; !found || b.Version != o.Version {
				ok = false
			}
			if i > 0 && got[i-1].CreatedAt > o.CreatedAt {
				ok = false
			}
		}
		if !ok {
			audit.LSIMismatch = append(audit.LSIMismatch, customer)
		}
	}

	audit.Converged = len(audit.GSIMissing)+len(audit.GSIStale)+len(audit.GSIExtra) == 0
	audit.GSIMissing, audit.GSIStale, audit.GSIExtra = sample(audit.GSIMissing), sample(audit.GSIStale), sample(audit.GSIExtra)
	audit.LSIMismatch = sample(audit.LSIMismatch)
	return audit, nil
}

type StreamAudit struct {
	Shards  int `json:"shards"`
	Records int `json:"records"`
	Keys    int `json:"keys"`
	// OutOfOrder lists orders whose stream records do not carry strictly
	// increasing versions in sequence-number order.
	OutOfOrder []string `json:"out_of_order"`
	// FinalMismatch lists orders whose last stream record disagrees with the
	// base table (wrong version, or a REMOVE for an order that still exists).
	FinalMismatch []string `json:"final_mismatch"`
	// Missing lists orders in the base table with no stream record at all.
	Missing []string `json:"missing"`
}

type streamEvent struct {
	seq     string
	remove  bool
	version int64
}

// OrdersStreamAudit reads the orders stream from TRIM_HORIZON and checks, for a
// run's orders, that every change is there, in order, ending at the base
// table's current state.
func (s *Store) OrdersStreamAudit(ctx context.Context, run string) (StreamAudit, error) {
	audit := StreamAudit{OutOfOrder: []string{}, FinalMismatch: []string{}, Missing: []string{}}
	table, err := s.db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(OrdersTable)})
	if err != nil {
		return audit, err
	}
	streamArn := table.Table.LatestStreamArn
	if streamArn == nil {
		return audit, fmt.Errorf("table %s has no stream", OrdersTable)
	}

	events := map[OrderKey][]streamEvent{}
	var lastShard *string
	for {
		desc, err := s.streams.DescribeStream(ctx, &dynamodbstreams.DescribeStreamInput{
			StreamArn: streamArn, ExclusiveStartShardId: lastShard,
		})
		if err != nil {
			return audit, err
		}
		for _, shard := range desc.StreamDescription.Shards {
			audit.Shards++
			if err := s.readShard(ctx, streamArn, shard.ShardId, run, events, &audit); err != nil {
				return audit, err
			}
		}
		lastShard = desc.StreamDescription.LastEvaluatedShardId
		if lastShard == nil {
			break
		}
	}

	base, err := s.ordersOfRun(ctx, run)
	if err != nil {
		return audit, err
	}
	audit.Keys = len(events)
	for k, evs := range events {
		sort.Slice(evs, func(i, j int) bool { return ddbx.SeqLess(evs[i].seq, evs[j].seq) })
		var prev int64
		for _, ev := range evs {
			if ev.remove {
				prev = 0
				continue
			}
			if ev.version <= prev {
				audit.OutOfOrder = append(audit.OutOfOrder, k.String())
				break
			}
			prev = ev.version
		}
		last := evs[len(evs)-1]
		if b, exists := base[k]; exists == last.remove || (exists && b.Version != last.version) {
			audit.FinalMismatch = append(audit.FinalMismatch, k.String())
		}
	}
	for k := range base {
		if _, ok := events[k]; !ok {
			audit.Missing = append(audit.Missing, k.String())
		}
	}
	audit.OutOfOrder, audit.FinalMismatch, audit.Missing = sample(audit.OutOfOrder), sample(audit.FinalMismatch), sample(audit.Missing)
	return audit, nil
}

func streamStr(av streamtypes.AttributeValue) string {
	if s, ok := av.(*streamtypes.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

// readShard collects the run's records of one shard. An open shard keeps
// returning an iterator, so reading stops after a few empty pages.
func (s *Store) readShard(ctx context.Context, streamArn, shardID *string, run string, events map[OrderKey][]streamEvent, audit *StreamAudit) error {
	it, err := s.streams.GetShardIterator(ctx, &dynamodbstreams.GetShardIteratorInput{
		StreamArn: streamArn, ShardId: shardID, ShardIteratorType: streamtypes.ShardIteratorTypeTrimHorizon,
	})
	if err != nil {
		return err
	}
	iterator := it.ShardIterator
	for empty := 0; iterator != nil && empty < 3; {
		out, err := s.streams.GetRecords(ctx, &dynamodbstreams.GetRecordsInput{ShardIterator: iterator})
		if err != nil {
			return err
		}
		if len(out.Records) == 0 {
			empty++
		} else {
			empty = 0
		}
		for _, rec := range out.Records {
			image := rec.Dynamodb.NewImage
			if rec.EventName == streamtypes.OperationTypeRemove {
				image = rec.Dynamodb.OldImage
			}
			if streamStr(image["run"]) != run {
				continue
			}
			audit.Records++
			k := OrderKey{streamStr(rec.Dynamodb.Keys["customer_id"]), streamStr(rec.Dynamodb.Keys["order_id"])}
			ev := streamEvent{seq: aws.ToString(rec.Dynamodb.SequenceNumber), remove: rec.EventName == streamtypes.OperationTypeRemove}
			if n, ok := image["ver"].(*streamtypes.AttributeValueMemberN); ok && !ev.remove {
				fmt.Sscan(n.Value, &ev.version)
			}
			events[k] = append(events[k], ev)
		}
		iterator = out.NextShardIterator
	}
	return nil
}
