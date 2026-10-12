package store

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ---------- per-user video lists: likes, history, watch later ----------

// RefKind is the kind of per-user video list. Each entry lives in the user's
// partition, is ordered by time through LSI1, and points at its video through
// GSI2 (REF#<videoId>) so deleting the video can clean it up.
type RefKind string

const (
	Likes      RefKind = "LIKE"
	History    RefKind = "HIST"
	WatchLater RefKind = "WL"
)

type Ref struct {
	VideoID  string  `dynamodbav:"videoId"`
	At       int64   `dynamodbav:"at"`
	Position float64 `dynamodbav:"position"` // history only: resume point in seconds
}

type refItem struct {
	PK, SK, LSI1SK, GSI2PK, GSI2SK string
	Ref
}

func refKey(userID string, kind RefKind, videoID string) Key {
	return Key{"USER#" + userID, string(kind) + "#" + videoID}
}

func newRefItem(userID string, kind RefKind, ref Ref) refItem {
	k := refKey(userID, kind, ref.VideoID)
	return refItem{
		PK: k.PK, SK: k.SK,
		LSI1SK: string(kind) + "#" + ts(ref.At),
		GSI2PK: "REF#" + ref.VideoID, GSI2SK: k.PK + "#" + string(kind),
		Ref: ref,
	}
}

// ListRefs returns one page of a user's list, newest first.
func (st *Store) ListRefs(ctx context.Context, userID string, kind RefKind, offset, limit int) ([]Ref, bool, error) {
	var refs []Ref
	err := st.query(ctx, &dynamodb.QueryInput{
		IndexName:                 aws.String("LSI1"),
		KeyConditionExpression:    aws.String("PK = :pk AND begins_with(LSI1SK, :k)"),
		ExpressionAttributeValues: map[string]AV{":pk": s("USER#" + userID), ":k": s(string(kind) + "#")},
		ScanIndexForward:          aws.Bool(false),
	}, func(it Item) (bool, error) {
		var r Ref
		if err := attributevalue.UnmarshalMap(it, &r); err != nil {
			return false, err
		}
		refs = append(refs, r)
		return len(refs) <= offset+limit, nil
	})
	if err != nil {
		return nil, false, err
	}
	if offset >= len(refs) {
		return nil, false, nil
	}
	refs = refs[offset:]
	hasMore := len(refs) > limit
	if hasMore {
		refs = refs[:limit]
	}
	return refs, hasMore, nil
}

func (st *Store) DeleteRef(ctx context.Context, userID string, kind RefKind, videoID string) error {
	return st.delete(ctx, refKey(userID, kind, videoID))
}

func (st *Store) ClearRefs(ctx context.Context, userID string, kind RefKind) error {
	keys, err := st.keysWithPrefix(ctx, "USER#"+userID, string(kind)+"#")
	if err != nil {
		return err
	}
	return st.batchDelete(ctx, keys)
}

// TouchHistory moves a video to the top of the user's history, keeping the
// saved position, and returns that position.
func (st *Store) TouchHistory(ctx context.Context, userID, videoID string, at int64) (float64, error) {
	return st.upsertHistory(ctx, userID, videoID, at, nil)
}

func (st *Store) SaveProgress(ctx context.Context, userID, videoID string, position float64, at int64) error {
	_, err := st.upsertHistory(ctx, userID, videoID, at, &position)
	return err
}

func (st *Store) upsertHistory(ctx context.Context, userID, videoID string, at int64, position *float64) (float64, error) {
	it := newRefItem(userID, History, Ref{VideoID: videoID, At: at})
	vals := map[string]AV{
		":l": s(it.LSI1SK), ":g": s(it.GSI2PK), ":g2": s(it.GSI2SK),
		":v": s(videoID), ":t": n(at),
	}
	// "position" and "at" are DynamoDB reserved words, hence the aliases.
	pos := "#pos = if_not_exists(#pos, :zero)"
	if position != nil {
		pos = "#pos = :p"
		vals[":p"] = f(*position)
	} else {
		vals[":zero"] = n(0)
	}
	res, err := st.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &st.table,
		Key:                       Key{it.PK, it.SK}.av(),
		UpdateExpression:          aws.String("SET LSI1SK = :l, GSI2PK = :g, GSI2SK = :g2, videoId = :v, #at = :t, " + pos),
		ExpressionAttributeNames:  map[string]string{"#at": "at", "#pos": "position"},
		ExpressionAttributeValues: vals,
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		return 0, err
	}
	var r Ref
	err = attributevalue.UnmarshalMap(res.Attributes, &r)
	return r.Position, err
}

// AddWatchLater saves a video for later. It reports false if it was already saved.
func (st *Store) AddWatchLater(ctx context.Context, userID, videoID string, at int64) (bool, error) {
	err := st.put(ctx, newRefItem(userID, WatchLater, Ref{VideoID: videoID, At: at}), "attribute_not_exists(PK)")
	if isConditionFailed(err) {
		return false, nil
	}
	return err == nil, err
}

// ToggleLike likes or unlikes a video and returns the new state and count.
func (st *Store) ToggleLike(ctx context.Context, userID, videoID string) (bool, int64, error) {
	lk := refKey(userID, Likes, videoID)
	liked, err := st.get(ctx, lk, nil)
	if err != nil {
		return false, 0, err
	}
	vk := videoKey(videoID)
	countUpdate := func(delta int64) types.TransactWriteItem {
		return types.TransactWriteItem{Update: &types.Update{
			Key:                       vk.av(),
			UpdateExpression:          aws.String("SET likes = likes + :d"),
			ConditionExpression:       aws.String("attribute_exists(PK)"),
			ExpressionAttributeValues: map[string]AV{":d": n(delta)},
		}}
	}
	if liked {
		err = st.transact(ctx,
			types.TransactWriteItem{Delete: &types.Delete{Key: lk.av(), ConditionExpression: aws.String("attribute_exists(PK)")}},
			countUpdate(-1))
	} else {
		item, merr := attributevalue.MarshalMap(newRefItem(userID, Likes, Ref{VideoID: videoID, At: now()}))
		if merr != nil {
			return false, 0, merr
		}
		err = st.transact(ctx,
			types.TransactWriteItem{Put: &types.Put{Item: item, ConditionExpression: aws.String("attribute_not_exists(PK)")}},
			countUpdate(1))
	}
	// ErrConflict here means a concurrent toggle won the race; report whatever
	// the state is now.
	if err != nil && !errors.Is(err, ErrConflict) {
		return false, 0, err
	}
	v, err := st.GetVideo(ctx, videoID)
	if err != nil {
		return false, 0, err
	}
	if v == nil {
		return false, 0, ErrNotFound
	}
	liked, err = st.get(ctx, lk, nil)
	return liked, v.Likes, err
}

// ViewerState reports whether a user has liked a video, saved it for later,
// and follows its creator.
func (st *Store) ViewerState(ctx context.Context, userID, videoID, creatorID string) (liked, savedLater, subscribed bool, err error) {
	lk, wl, sk := refKey(userID, Likes, videoID), refKey(userID, WatchLater, videoID), subKey(userID, creatorID)
	items, err := st.batchGet(ctx, []Key{lk, wl, sk})
	if err != nil {
		return false, false, false, err
	}
	_, liked = items[lk]
	_, savedLater = items[wl]
	_, subscribed = items[sk]
	return liked, savedLater, subscribed, nil
}

// ---------- subscriptions ("Save & Notify") ----------

type Subscription struct {
	CreatorID string `dynamodbav:"creatorId"`
	CreatedAt int64  `dynamodbav:"createdAt"`
}

func subKey(followerID, creatorID string) Key { return Key{"USER#" + followerID, "SUB#" + creatorID} }

func (st *Store) IsSubscribed(ctx context.Context, followerID, creatorID string) (bool, error) {
	return st.get(ctx, subKey(followerID, creatorID), nil)
}

func (st *Store) Subscriptions(ctx context.Context, followerID string) ([]Subscription, error) {
	var items []Item
	err := st.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :pk AND begins_with(SK, :p)"),
		ExpressionAttributeValues: map[string]AV{":pk": s("USER#" + followerID), ":p": s("SUB#")},
	}, func(it Item) (bool, error) {
		items = append(items, it)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return unmarshalAll[Subscription](items)
}

// ToggleSubscription follows or unfollows a creator and returns the new state
// and the creator's follower count.
func (st *Store) ToggleSubscription(ctx context.Context, followerID, creatorID string) (bool, int64, error) {
	sk := subKey(followerID, creatorID)
	subscribed, err := st.get(ctx, sk, nil)
	if err != nil {
		return false, 0, err
	}
	countUpdate := func(delta int64) types.TransactWriteItem {
		return types.TransactWriteItem{Update: &types.Update{
			Key:                       userKey(creatorID).av(),
			UpdateExpression:          aws.String("SET subscribers = subscribers + :d"),
			ConditionExpression:       aws.String("attribute_exists(PK)"),
			ExpressionAttributeValues: map[string]AV{":d": n(delta)},
		}}
	}
	if subscribed {
		err = st.transact(ctx,
			types.TransactWriteItem{Delete: &types.Delete{Key: sk.av(), ConditionExpression: aws.String("attribute_exists(PK)")}},
			countUpdate(-1))
	} else {
		item := Item{"PK": s(sk.PK), "SK": s(sk.SK), "creatorId": s(creatorID), "createdAt": n(now())}
		err = st.transact(ctx,
			types.TransactWriteItem{Put: &types.Put{Item: item, ConditionExpression: aws.String("attribute_not_exists(PK)")}},
			countUpdate(1))
	}
	if err != nil && !errors.Is(err, ErrConflict) {
		return false, 0, err
	}
	creator, err := st.GetUser(ctx, creatorID)
	if err != nil {
		return false, 0, err
	}
	if creator == nil {
		return false, 0, ErrNotFound
	}
	subscribed, err = st.get(ctx, sk, nil)
	return subscribed, creator.Subscribers, err
}

// ---------- comments ----------

type Comment struct {
	ID        string `dynamodbav:"id"`
	VideoID   string `dynamodbav:"videoId"`
	UserID    string `dynamodbav:"userId"`
	ParentID  string `dynamodbav:"parentId,omitempty"`
	Body      string `dynamodbav:"body"`
	CreatedAt int64  `dynamodbav:"createdAt"`
	EditedAt  int64  `dynamodbav:"editedAt,omitempty"`
}

func commentKey(videoID, commentID string) Key {
	return Key{"VIDEO#" + videoID, "COMMENT#" + commentID}
}

// ListComments returns up to limit comments (top-level and replies), in time order.
func (st *Store) ListComments(ctx context.Context, videoID string, newestFirst bool, limit int) ([]Comment, error) {
	var items []Item
	err := st.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :pk AND begins_with(SK, :p)"),
		ExpressionAttributeValues: map[string]AV{":pk": s("VIDEO#" + videoID), ":p": s("COMMENT#")},
		ScanIndexForward:          aws.Bool(!newestFirst),
	}, func(it Item) (bool, error) {
		items = append(items, it)
		return len(items) < limit, nil
	})
	if err != nil {
		return nil, err
	}
	return unmarshalAll[Comment](items)
}

// GetComment returns nil if the comment doesn't exist.
func (st *Store) GetComment(ctx context.Context, videoID, commentID string) (*Comment, error) {
	var c Comment
	ok, err := st.get(ctx, commentKey(videoID, commentID), &c)
	if err != nil || !ok {
		return nil, err
	}
	return &c, nil
}

func (st *Store) AddComment(ctx context.Context, c *Comment) error {
	c.ID = sortableID()
	c.CreatedAt = now()
	k := commentKey(c.VideoID, c.ID)
	return st.put(ctx, struct {
		PK, SK string
		Comment
	}{k.PK, k.SK, *c}, "")
}

func (st *Store) EditComment(ctx context.Context, videoID, commentID, body string) (*Comment, error) {
	res, err := st.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &st.table,
		Key:                       commentKey(videoID, commentID).av(),
		UpdateExpression:          aws.String("SET body = :b, editedAt = :t"),
		ConditionExpression:       aws.String("attribute_exists(PK)"),
		ExpressionAttributeValues: map[string]AV{":b": s(body), ":t": n(now())},
		ReturnValues:              types.ReturnValueAllNew,
	})
	if isConditionFailed(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var c Comment
	err = attributevalue.UnmarshalMap(res.Attributes, &c)
	return &c, err
}

// DeleteComment removes a comment and, for a top-level comment, its replies.
func (st *Store) DeleteComment(ctx context.Context, videoID, commentID string) error {
	keys := []Key{commentKey(videoID, commentID)}
	err := st.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :pk AND begins_with(SK, :p)"),
		FilterExpression:          aws.String("parentId = :c"),
		ExpressionAttributeValues: map[string]AV{":pk": s("VIDEO#" + videoID), ":p": s("COMMENT#"), ":c": s(commentID)},
		ProjectionExpression:      aws.String("PK, SK"),
	}, func(it Item) (bool, error) {
		keys = append(keys, keyOf(it))
		return true, nil
	})
	if err != nil {
		return err
	}
	return st.batchDelete(ctx, keys)
}

// ---------- reports ----------

type Report struct {
	ReporterID string `dynamodbav:"reporterId"`
	Reason     string `dynamodbav:"reason"`
	CreatedAt  int64  `dynamodbav:"createdAt"`
}

// AddReport files a report. It reports false if this user already reported the video.
func (st *Store) AddReport(ctx context.Context, videoID, reporterID, reason string) (bool, error) {
	err := st.put(ctx, struct {
		PK, SK string
		Report
	}{"VIDEO#" + videoID, "REPORT#" + reporterID, Report{reporterID, reason, now()}}, "attribute_not_exists(PK)")
	if isConditionFailed(err) {
		return false, nil
	}
	return err == nil, err
}

// Reports lists a video's reports, newest first.
func (st *Store) Reports(ctx context.Context, videoID string) ([]Report, error) {
	var items []Item
	err := st.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :pk AND begins_with(SK, :p)"),
		ExpressionAttributeValues: map[string]AV{":pk": s("VIDEO#" + videoID), ":p": s("REPORT#")},
	}, func(it Item) (bool, error) {
		items = append(items, it)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	reports, err := unmarshalAll[Report](items)
	sortByDesc(reports, func(r Report) int64 { return r.CreatedAt })
	return reports, err
}
