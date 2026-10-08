// Package compat is a DynamoDB API compatibility suite: named cases that each
// exercise one operation or feature through the AWS SDK and assert the
// behaviour DynamoDB documents. The HTTP API exposes the cases so the k6
// compat test can run them against every cell and build a matrix.
package compat

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

const (
	StatusPass        = "pass"        // behaves like DynamoDB
	StatusFail        = "fail"        // implemented, but an assertion failed
	StatusUnsupported = "unsupported" // answered UnknownOperationException
)

// Case is one compatibility check. Ops are the SDK operations it is about
// (prefixed "streams:" for the DynamoDB Streams client).
type Case struct {
	ID    string   `json:"id"`
	Group string   `json:"group"`
	Ops   []string `json:"ops"`
	// Unsupported marks operations ExtendDB is expected not to implement; the
	// case then passes its expectation by getting UnknownOperationException.
	Unsupported bool `json:"expect_unsupported"`
	// Documented tells whether ExtendDB's docs list the operation as not implemented.
	Documented bool `json:"documented,omitempty"`
	run        func(t *T)
}

type Result struct {
	Case
	Cell   string `json:"cell"`
	Status string `json:"status"`
	// AsExpected is false for a failed assertion and for an operation whose
	// support differs from what the case expects.
	AsExpected bool   `json:"as_expected"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type Suite struct {
	db      *dynamodb.Client
	streams *dynamodbstreams.Client
	cell    string
	// exchangeDir is the directory ExtendDB may import from and export to.
	exchangeDir string
	cases       []Case

	// ready holds the shared tables already created; mu also serializes their creation.
	mu    sync.Mutex
	ready map[string]bool
}

func NewSuite(db *dynamodb.Client, streams *dynamodbstreams.Client, cell, exchangeDir string) *Suite {
	s := &Suite{db: db, streams: streams, cell: cell, exchangeDir: exchangeDir, ready: map[string]bool{}}
	for _, group := range [][]Case{
		tableCases(), itemCases(), queryCases(), batchCases(), transactionCases(),
		streamCases(), adminCases(), unsupportedCases(),
	} {
		s.cases = append(s.cases, group...)
	}
	return s
}

func (s *Suite) Cases() []Case { return s.cases }

// Coverage lists every operation of the SDK clients and the ones no case covers.
type Coverage struct {
	SDKOperations []string `json:"sdk_operations"`
	Uncovered     []string `json:"uncovered"`
}

func (s *Suite) Coverage() Coverage {
	covered := map[string]bool{}
	for _, c := range s.cases {
		for _, op := range c.Ops {
			covered[op] = true
		}
	}
	cov := Coverage{Uncovered: []string{}}
	add := func(client any, prefix string) {
		t := reflect.TypeOf(client)
		for i := 0; i < t.NumMethod(); i++ {
			if name := t.Method(i).Name; name != "Options" {
				cov.SDKOperations = append(cov.SDKOperations, prefix+name)
			}
		}
	}
	add(s.db, "")
	add(s.streams, "streams:")
	sort.Strings(cov.SDKOperations)
	for _, op := range cov.SDKOperations {
		if !covered[op] {
			cov.Uncovered = append(cov.Uncovered, op)
		}
	}
	return cov
}

type failure struct{ msg string }

// T is what a case uses to talk to DynamoDB and to fail.
type T struct {
	context.Context
	DB      *dynamodb.Client
	Streams *dynamodbstreams.Client
	suite   *Suite
	run     string
	id      string
}

// Fatalf fails the case.
func (t *T) Fatalf(format string, args ...any) {
	panic(failure{fmt.Sprintf(format, args...)})
}

// NoErr fails the case if err is not nil.
func (t *T) NoErr(err error, what string) {
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// Check fails the case unless cond holds.
func (t *T) Check(cond bool, format string, args ...any) {
	if !cond {
		t.Fatalf(format, args...)
	}
}

// ErrCode fails the case unless err is an API error with the given code.
func (t *T) ErrCode(err error, code, what string) {
	if err == nil {
		t.Fatalf("%s: expected %s, got success", what, code)
	}
	if got := ddbx.Code(err); got != code {
		t.Fatalf("%s: expected %s, got %v", what, code, err)
	}
}

// Eventually polls cond until it holds, failing the case after timeout.
func (t *T) Eventually(timeout time.Duration, what string, cond func() bool) {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not true after %s", what, timeout)
		}
		select {
		case <-t.Done():
			t.Fatalf("%s: %v", what, t.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// PK returns a partition key value private to this case and run.
func (t *T) PK(suffix string) string { return t.id + "#" + suffix }

// TableName returns a table name private to this run and cell.
func (t *T) TableName(suffix string) string {
	return tablePrefix(t.run, t.suite.cell) + suffix
}

func tablePrefix(run, cell string) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				return r
			}
			return '_'
		}, s)
	}
	return "compat_" + clean(run) + "_" + clean(cell) + "_"
}

// Run executes one case. run namespaces the tables it creates.
func (s *Suite) Run(ctx context.Context, id, run string) (Result, bool) {
	for _, c := range s.cases {
		if c.ID == id {
			return s.runCase(ctx, c, run), true
		}
	}
	return Result{}, false
}

func (s *Suite) runCase(ctx context.Context, c Case, run string) (res Result) {
	res = Result{Case: c, Cell: s.cell, Status: StatusPass}
	start := time.Now()
	t := &T{Context: ctx, DB: s.db, Streams: s.streams, suite: s, run: run, id: c.ID}
	defer func() {
		res.DurationMS = time.Since(start).Milliseconds()
		if r := recover(); r != nil {
			f, ok := r.(failure)
			if !ok {
				panic(r)
			}
			res.Status, res.Error = StatusFail, f.msg
			if strings.Contains(f.msg, "UnknownOperationException") {
				res.Status = StatusUnsupported
			}
		}
		res.AsExpected = res.Status == StatusPass && !c.Unsupported || res.Status == StatusUnsupported && c.Unsupported
		if res.Status == StatusPass && c.Unsupported {
			res.Error = "operation answered although it is expected to be unsupported"
		}
	}()
	c.run(t)
	return res
}
