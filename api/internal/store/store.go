// Package store is vidwatch's DynamoDB data layer. Everything lives in one
// table; docs/architecture.md describes the key layout.
package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var (
	// ErrNotFound means the item a write depends on doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means a uniqueness check failed (e.g. a username is taken).
	ErrConflict = errors.New("conflict")
)

type AV = types.AttributeValue
type Item = map[string]AV

type Store struct {
	db    *dynamodb.Client
	table string
}

func New(db *dynamodb.Client, table string) *Store {
	return &Store{db: db, table: table}
}

// Key is a primary key.
type Key struct{ PK, SK string }

func (k Key) av() Item {
	return Item{"PK": &types.AttributeValueMemberS{Value: k.PK}, "SK": &types.AttributeValueMemberS{Value: k.SK}}
}

func keyOf(it Item) Key {
	return Key{PK: str(it["PK"]), SK: str(it["SK"])}
}

func str(v AV) string {
	if s, ok := v.(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

// ts renders a millisecond timestamp so that string order is time order.
func ts(ms int64) string { return fmt.Sprintf("%013d", ms) }

func now() int64 { return time.Now().UnixMilli() }

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// NewID returns n random base62 characters.
func NewID(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(base62)))
	for i := range b {
		x, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err) // crypto/rand doesn't fail on supported platforms
		}
		b[i] = base62[x.Int64()]
	}
	return string(b)
}

// sortableID is a time-ordered ID: millisecond timestamp plus a random suffix.
func sortableID() string { return ts(now()) + NewID(6) }

func s(v string) AV  { return &types.AttributeValueMemberS{Value: v} }
func n(v int64) AV   { return &types.AttributeValueMemberN{Value: fmt.Sprint(v)} }
func f(v float64) AV { return &types.AttributeValueMemberN{Value: fmt.Sprint(v)} }

// ---------- low-level helpers ----------

func (st *Store) get(ctx context.Context, k Key, out any) (bool, error) {
	res, err := st.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &st.table,
		Key:            k.av(),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return false, err
	}
	if res.Item == nil {
		return false, nil
	}
	if out != nil {
		if err := attributevalue.UnmarshalMap(res.Item, out); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (st *Store) put(ctx context.Context, item any, cond string) error {
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return err
	}
	in := &dynamodb.PutItemInput{TableName: &st.table, Item: av}
	if cond != "" {
		in.ConditionExpression = &cond
	}
	_, err = st.db.PutItem(ctx, in)
	return err
}

func (st *Store) delete(ctx context.Context, k Key) error {
	_, err := st.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &st.table, Key: k.av()})
	return err
}

// query runs a query page by page, calling fn for each item until fn returns
// false or the results run out.
func (st *Store) query(ctx context.Context, in *dynamodb.QueryInput, fn func(Item) (bool, error)) error {
	in.TableName = &st.table
	p := dynamodb.NewQueryPaginator(st.db, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, it := range page.Items {
			more, err := fn(it)
			if err != nil || !more {
				return err
			}
		}
	}
	return nil
}

// keysWithPrefix lists the primary keys in a partition whose sort key starts
// with prefix (every key in the partition if prefix is "").
func (st *Store) keysWithPrefix(ctx context.Context, pk, prefix string) ([]Key, error) {
	in := &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]AV{":pk": s(pk)},
		ProjectionExpression:      aws.String("PK, SK"),
	}
	if prefix != "" {
		in.KeyConditionExpression = aws.String("PK = :pk AND begins_with(SK, :p)")
		in.ExpressionAttributeValues[":p"] = s(prefix)
	}
	var keys []Key
	err := st.query(ctx, in, func(it Item) (bool, error) {
		keys = append(keys, keyOf(it))
		return true, nil
	})
	return keys, err
}

// batchGet fetches up to any number of items by key. Missing items are absent
// from the result.
func (st *Store) batchGet(ctx context.Context, keys []Key) (map[Key]Item, error) {
	out := make(map[Key]Item, len(keys))
	seen := map[Key]bool{}
	var pending []Item
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			pending = append(pending, k.av())
		}
	}
	for len(pending) > 0 {
		chunk := pending
		if len(chunk) > 100 {
			chunk = chunk[:100]
		}
		pending = pending[len(chunk):]
		req := map[string]types.KeysAndAttributes{st.table: {Keys: chunk, ConsistentRead: aws.Bool(true)}}
		for attempt := 0; len(req) > 0; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt*50) * time.Millisecond)
			}
			if attempt > 8 {
				return nil, errors.New("batch get: too many unprocessed keys")
			}
			res, err := st.db.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: req})
			if err != nil {
				return nil, err
			}
			for _, it := range res.Responses[st.table] {
				out[keyOf(it)] = it
			}
			req = res.UnprocessedKeys
		}
	}
	return out, nil
}

func (st *Store) batchDelete(ctx context.Context, keys []Key) error {
	for len(keys) > 0 {
		chunk := keys
		if len(chunk) > 25 {
			chunk = chunk[:25]
		}
		keys = keys[len(chunk):]
		var reqs []types.WriteRequest
		for _, k := range chunk {
			reqs = append(reqs, types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: k.av()}})
		}
		req := map[string][]types.WriteRequest{st.table: reqs}
		for attempt := 0; len(req) > 0; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt*50) * time.Millisecond)
			}
			if attempt > 8 {
				return errors.New("batch delete: too many unprocessed items")
			}
			res, err := st.db.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: req})
			if err != nil {
				return err
			}
			req = res.UnprocessedItems
		}
	}
	return nil
}

// transact runs a write transaction. It returns ErrConflict when one of the
// conditions failed.
func (st *Store) transact(ctx context.Context, items ...types.TransactWriteItem) error {
	for i := range items {
		switch {
		case items[i].Put != nil:
			items[i].Put.TableName = &st.table
		case items[i].Update != nil:
			items[i].Update.TableName = &st.table
		case items[i].Delete != nil:
			items[i].Delete.TableName = &st.table
		case items[i].ConditionCheck != nil:
			items[i].ConditionCheck.TableName = &st.table
		}
	}
	_, err := st.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
	var tce *types.TransactionCanceledException
	if errors.As(err, &tce) {
		for _, r := range tce.CancellationReasons {
			if r.Code != nil && *r.Code == "ConditionalCheckFailed" {
				return ErrConflict
			}
		}
	}
	return err
}

func isConditionFailed(err error) bool {
	var ccf *types.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

func unmarshalAll[T any](items []Item) ([]T, error) {
	out := make([]T, 0, len(items))
	for _, it := range items {
		var v T
		if err := attributevalue.UnmarshalMap(it, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
