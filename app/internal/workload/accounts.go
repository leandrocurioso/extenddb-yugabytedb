package workload

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

func acctPK(run string, i int) string   { return fmt.Sprintf("acct#%s#%d", run, i) }
func ledgerPK(run, token string) string { return "xfer#" + run + "#" + token }

// AccountsSeed (re)creates accounts 0..count-1 of a run with the same balance.
func (s *Store) AccountsSeed(ctx context.Context, run string, count int, balance int64) error {
	var reqs []types.WriteRequest
	for i := 0; i < count; i++ {
		reqs = append(reqs, types.WriteRequest{PutRequest: &types.PutRequest{Item: map[string]types.AttributeValue{
			"pk": ddbx.S(acctPK(run, i)), "run": ddbx.S(run), "balance": ddbx.N(balance),
		}}})
	}
	return s.batchWrite(ctx, KVTable, reqs)
}

// batchWrite sends requests in chunks of 25, resubmitting UnprocessedItems.
func (s *Store) batchWrite(ctx context.Context, table string, reqs []types.WriteRequest) error {
	for len(reqs) > 0 {
		n := min(len(reqs), 25)
		pending := reqs[:n]
		reqs = reqs[n:]
		for len(pending) > 0 {
			out, err := s.db.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
				RequestItems: map[string][]types.WriteRequest{table: pending},
			})
			if err != nil {
				return err
			}
			pending = out.UnprocessedItems[table]
		}
	}
	return nil
}

type Transfer struct {
	Run    string `json:"run" binding:"required"`
	From   *int   `json:"from" binding:"required"`
	To     *int   `json:"to" binding:"required"`
	Amount int64  `json:"amount" binding:"required,gt=0"`
	// Token identifies the transfer: it is the transaction's ClientRequestToken
	// and the ledger entry's key, so a retry through any cell applies it once.
	Token string `json:"token" binding:"required,max=36"`
}

// Transfer moves Amount between two accounts and writes a ledger entry, all in
// one TransactWriteItems. The request is identical whichever cell sends it, so
// a retry with the same token is recognised as the same transaction. duplicate is true when the ledger already had the
// token, i.e. an earlier attempt had been applied.
func (s *Store) Transfer(ctx context.Context, t Transfer) (duplicate bool, err error) {
	amount := map[string]types.AttributeValue{":a": ddbx.N(t.Amount)}
	_, err = s.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		ClientRequestToken: aws.String(t.Token),
		TransactItems: []types.TransactWriteItem{
			{Update: &types.Update{
				TableName:                 aws.String(KVTable),
				Key:                       pkKey(acctPK(t.Run, *t.From)),
				UpdateExpression:          aws.String("SET balance = balance - :a"),
				ConditionExpression:       aws.String("balance >= :a"),
				ExpressionAttributeValues: amount,
			}},
			{Update: &types.Update{
				TableName:                 aws.String(KVTable),
				Key:                       pkKey(acctPK(t.Run, *t.To)),
				UpdateExpression:          aws.String("SET balance = balance + :a"),
				ConditionExpression:       aws.String("attribute_exists(pk)"),
				ExpressionAttributeValues: amount,
			}},
			{Put: &types.Put{
				TableName: aws.String(KVTable),
				Item: map[string]types.AttributeValue{
					"pk": ddbx.S(ledgerPK(t.Run, t.Token)), "run": ddbx.S(t.Run),
					"from": ddbx.N(int64(*t.From)), "to": ddbx.N(int64(*t.To)),
					"amount": ddbx.N(t.Amount),
				},
				ConditionExpression: aws.String("attribute_not_exists(pk)"),
			}},
		},
	})
	if err == nil {
		return false, nil
	}
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		return false, err
	}
	reason := func(i int) string {
		if i < len(canceled.CancellationReasons) {
			return aws.ToString(canceled.CancellationReasons[i].Code)
		}
		return ""
	}
	switch {
	case reason(2) == "ConditionalCheckFailed":
		return true, nil
	case reason(0) == "ConditionalCheckFailed":
		return false, ErrInsufficientFunds
	}
	for i := range canceled.CancellationReasons {
		if reason(i) == "TransactionConflict" {
			return false, ErrTransactionConflict
		}
	}
	return false, err
}

// AccountsSnapshot reads all accounts of a run in one TransactGetItems, so the
// balances belong to a single point in time (count <= 100).
func (s *Store) AccountsSnapshot(ctx context.Context, run string, count int) ([]int64, error) {
	items := make([]types.TransactGetItem, count)
	for i := range items {
		items[i] = types.TransactGetItem{Get: &types.Get{TableName: aws.String(KVTable), Key: pkKey(acctPK(run, i))}}
	}
	out, err := s.db.TransactGetItems(ctx, &dynamodb.TransactGetItemsInput{TransactItems: items})
	if err != nil {
		return nil, err
	}
	balances := make([]int64, count)
	for i, r := range out.Responses {
		if r.Item == nil {
			return nil, fmt.Errorf("account %d of run %s is missing", i, run)
		}
		balances[i] = ddbx.Int(r.Item["balance"])
	}
	return balances, nil
}

type AccountsAudit struct {
	Balances []int64 `json:"balances"`
	Sum      int64   `json:"sum"`
	Ledger   int     `json:"ledger_entries"`
	// Mismatches lists accounts whose balance differs from the initial balance
	// plus the ledger's net effect on them.
	Mismatches []string `json:"mismatches"`
}

// AccountsAudit compares the accounts with the ledger: every balance must equal
// initial plus the transfers recorded for it (consistent Scan of the ledger).
func (s *Store) AccountsAudit(ctx context.Context, run string, count int, initial int64) (AccountsAudit, error) {
	audit := AccountsAudit{Mismatches: []string{}}
	expected := make([]int64, count)
	for i := range expected {
		expected[i] = initial
	}

	pager := dynamodb.NewScanPaginator(s.db, &dynamodb.ScanInput{
		TableName:        aws.String(KVTable),
		ConsistentRead:   aws.Bool(true),
		FilterExpression: aws.String("begins_with(pk, :p)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":p": ddbx.S("xfer#" + run + "#"),
		},
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return audit, err
		}
		for _, item := range page.Items {
			from, to, amount := int(ddbx.Int(item["from"])), int(ddbx.Int(item["to"])), ddbx.Int(item["amount"])
			if from >= count || to >= count {
				continue
			}
			expected[from] -= amount
			expected[to] += amount
			audit.Ledger++
		}
	}

	for i := 0; i < count; i++ {
		out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(KVTable), Key: pkKey(acctPK(run, i)), ConsistentRead: aws.Bool(true),
		})
		if err != nil {
			return audit, err
		}
		balance := ddbx.Int(out.Item["balance"])
		audit.Balances = append(audit.Balances, balance)
		audit.Sum += balance
		if out.Item == nil || balance != expected[i] {
			audit.Mismatches = append(audit.Mismatches, fmt.Sprintf("account %d: balance %d, ledger implies %d", i, balance, expected[i]))
		}
	}
	return audit, nil
}

// LedgerPresent reports which of the tokens have a ledger entry (consistent BatchGetItem).
func (s *Store) LedgerPresent(ctx context.Context, run string, tokens []string) ([]string, error) {
	keys := make([]map[string]types.AttributeValue, len(tokens))
	for i, token := range tokens {
		keys[i] = pkKey(ledgerPK(run, token))
	}
	items, err := s.batchGet(ctx, KVTable, keys)
	if err != nil {
		return nil, err
	}
	prefix := len(ledgerPK(run, ""))
	present := make([]string, 0, len(items))
	for _, item := range items {
		present = append(present, ddbx.Str(item["pk"])[prefix:])
	}
	return present, nil
}

// batchGet reads keys with consistent BatchGetItem calls of up to 100 keys,
// resubmitting UnprocessedKeys.
func (s *Store) batchGet(ctx context.Context, table string, keys []map[string]types.AttributeValue) ([]map[string]types.AttributeValue, error) {
	var items []map[string]types.AttributeValue
	for len(keys) > 0 {
		n := min(len(keys), 100)
		pending := keys[:n]
		keys = keys[n:]
		for len(pending) > 0 {
			out, err := s.db.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{
				RequestItems: map[string]types.KeysAndAttributes{
					table: {Keys: pending, ConsistentRead: aws.Bool(true)},
				},
			})
			if err != nil {
				return nil, err
			}
			items = append(items, out.Responses[table]...)
			pending = out.UnprocessedKeys[table].Keys
		}
	}
	return items, nil
}
