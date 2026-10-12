package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type Video struct {
	ID              string           `dynamodbav:"id"`
	UserID          string           `dynamodbav:"userId"`
	Title           string           `dynamodbav:"title"`
	Description     string           `dynamodbav:"description"`
	VideoKey        string           `dynamodbav:"videoKey"`
	ThumbnailKey    string           `dynamodbav:"thumbnailKey,omitempty"`
	ContentType     string           `dynamodbav:"contentType"`
	DurationSeconds *float64         `dynamodbav:"durationSeconds,omitempty"`
	AgeRating       string           `dynamodbav:"ageRating"`
	Visibility      string           `dynamodbav:"visibility"` // public | unlisted
	CreatedAt       int64            `dynamodbav:"createdAt"`
	Views           int64            `dynamodbav:"views"`
	Likes           int64            `dynamodbav:"likes"`
	ViewDays        map[string]int64 `dynamodbav:"viewDays"` // "YYYY-MM-DD" -> unique views that day
}

type videoItem struct {
	PK, SK string
	GSI1PK string `dynamodbav:",omitempty"`
	GSI1SK string `dynamodbav:",omitempty"`
	GSI2PK string
	GSI2SK string
	Video
}

const publicFeed = "PUBLIC"

func videoKey(id string) Key       { return Key{"VIDEO#" + id, "META"} }
func videoSortKey(v *Video) string { return ts(v.CreatedAt) + "#" + v.ID }

const dayLayout = "2006-01-02"

// viewDaysKept is how many days of per-day view counts a video keeps.
const viewDaysKept = 8

func day(ms int64) string { return time.UnixMilli(ms).UTC().Format(dayLayout) }

// RecentViews sums the unique views over the last `days` days (today included).
func (v *Video) RecentViews(at int64, days int) int64 {
	cutoff := day(at - int64(days-1)*24*int64(time.Hour/time.Millisecond))
	var total int64
	for d, c := range v.ViewDays {
		if d >= cutoff {
			total += c
		}
	}
	return total
}

func (st *Store) CreateVideo(ctx context.Context, v *Video) error {
	if v.ID == "" {
		v.ID = NewID(11)
	}
	if v.CreatedAt == 0 {
		v.CreatedAt = now()
	}
	if v.ViewDays == nil {
		v.ViewDays = map[string]int64{} // must be a map (not NULL) so views can SET viewDays.<day>
	}
	k := videoKey(v.ID)
	it := videoItem{PK: k.PK, SK: k.SK, GSI2PK: "CREATOR#" + v.UserID, GSI2SK: videoSortKey(v), Video: *v}
	if v.Visibility == "public" {
		it.GSI1PK, it.GSI1SK = publicFeed, videoSortKey(v)
	}
	return st.put(ctx, it, "attribute_not_exists(PK)")
}

// GetVideo returns nil if the video doesn't exist.
func (st *Store) GetVideo(ctx context.Context, id string) (*Video, error) {
	if id == "" {
		return nil, nil
	}
	var v Video
	ok, err := st.get(ctx, videoKey(id), &v)
	if err != nil || !ok {
		return nil, err
	}
	return &v, nil
}

// GetVideos fetches several videos at once, keyed by ID. Missing ones are absent.
func (st *Store) GetVideos(ctx context.Context, ids []string) (map[string]*Video, error) {
	keys := make([]Key, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, videoKey(id))
	}
	items, err := st.batchGet(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*Video, len(items))
	for _, it := range items {
		var v Video
		if err := attributevalue.UnmarshalMap(it, &v); err != nil {
			return nil, err
		}
		out[v.ID] = &v
	}
	return out, nil
}

// VideoUpdate holds the fields to change; nil means "leave alone".
type VideoUpdate struct {
	Title, Description, AgeRating, Visibility, ThumbnailKey *string
}

func (st *Store) UpdateVideo(ctx context.Context, v *Video, upd VideoUpdate) (*Video, error) {
	sets := []string{}
	var removes []string
	vals := map[string]AV{}
	add := func(attr string, val *string) {
		if val != nil {
			sets = append(sets, fmt.Sprintf("%s = :%s", attr, attr))
			vals[":"+attr] = s(*val)
		}
	}
	add("title", upd.Title)
	add("description", upd.Description)
	add("ageRating", upd.AgeRating)
	add("thumbnailKey", upd.ThumbnailKey)
	if upd.Visibility != nil {
		add("visibility", upd.Visibility)
		if *upd.Visibility == "public" {
			sets = append(sets, "GSI1PK = :g1pk", "GSI1SK = :g1sk")
			vals[":g1pk"], vals[":g1sk"] = s(publicFeed), s(videoSortKey(v))
		} else {
			removes = append(removes, "GSI1PK", "GSI1SK")
		}
	}
	if len(sets) == 0 && len(removes) == 0 {
		return v, nil
	}
	expr := ""
	if len(sets) > 0 {
		expr = "SET " + strings.Join(sets, ", ")
	}
	if len(removes) > 0 {
		expr += " REMOVE " + strings.Join(removes, ", ")
	}
	in := &dynamodb.UpdateItemInput{
		TableName:           &st.table,
		Key:                 videoKey(v.ID).av(),
		UpdateExpression:    aws.String(expr),
		ConditionExpression: aws.String("attribute_exists(PK)"),
		ReturnValues:        types.ReturnValueAllNew,
	}
	if len(vals) > 0 {
		in.ExpressionAttributeValues = vals
	}
	res, err := st.db.UpdateItem(ctx, in)
	if isConditionFailed(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var out Video
	if err := attributevalue.UnmarshalMap(res.Attributes, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteVideo removes the video, everything in its partition (views,
// comments, reports) and every item elsewhere that points at it (likes,
// history, watch later, playlist entries).
func (st *Store) DeleteVideo(ctx context.Context, id string) error {
	keys, err := st.keysWithPrefix(ctx, "VIDEO#"+id, "")
	if err != nil {
		return err
	}
	err = st.query(ctx, &dynamodb.QueryInput{
		IndexName:                 aws.String("GSI2"),
		KeyConditionExpression:    aws.String("GSI2PK = :r"),
		ExpressionAttributeValues: map[string]AV{":r": s("REF#" + id)},
		ProjectionExpression:      aws.String("PK, SK"),
	}, func(it Item) (bool, error) {
		keys = append(keys, keyOf(it))
		return true, nil
	})
	if err != nil {
		return err
	}
	// Delete the video itself last so a partial failure can be retried.
	meta := videoKey(id)
	keys = slices.DeleteFunc(keys, func(k Key) bool { return k == meta })
	return st.batchDelete(ctx, append(keys, meta))
}

// PublicVideos walks every public video, newest first, until fn returns false.
func (st *Store) PublicVideos(ctx context.Context, fn func(*Video) bool) error {
	return st.query(ctx, &dynamodb.QueryInput{
		IndexName:                 aws.String("GSI1"),
		KeyConditionExpression:    aws.String("GSI1PK = :p"),
		ExpressionAttributeValues: map[string]AV{":p": s(publicFeed)},
		ScanIndexForward:          aws.Bool(false),
	}, func(it Item) (bool, error) {
		var v Video
		if err := attributevalue.UnmarshalMap(it, &v); err != nil {
			return false, err
		}
		return fn(&v), nil
	})
}

// CreatorVideos lists a creator's videos newest first. limit <= 0 means all.
func (st *Store) CreatorVideos(ctx context.Context, userID string, publicOnly bool, limit int) ([]*Video, error) {
	var out []*Video
	err := st.query(ctx, &dynamodb.QueryInput{
		IndexName:                 aws.String("GSI2"),
		KeyConditionExpression:    aws.String("GSI2PK = :c"),
		ExpressionAttributeValues: map[string]AV{":c": s("CREATOR#" + userID)},
		ScanIndexForward:          aws.Bool(false),
	}, func(it Item) (bool, error) {
		var v Video
		if err := attributevalue.UnmarshalMap(it, &v); err != nil {
			return false, err
		}
		if !publicOnly || v.Visibility == "public" {
			out = append(out, &v)
		}
		return limit <= 0 || len(out) < limit, nil
	})
	return out, err
}

// RecordView counts a unique view (one per viewerKey per video). It reports
// whether this viewer was new.
func (st *Store) RecordView(ctx context.Context, v *Video, viewerKey string, at int64) (bool, error) {
	k := videoKey(v.ID)
	viewItem := Item{"PK": s(k.PK), "SK": s("VIEW#" + viewerKey), "createdAt": n(at)}

	names := map[string]string{"#d": day(at)}
	expr := "SET #v = #v + :one, viewDays.#d = if_not_exists(viewDays.#d, :zero) + :one"
	names["#v"] = "views"
	cutoff := day(at - int64(viewDaysKept-1)*24*int64(time.Hour/time.Millisecond))
	var stale []string
	for d := range v.ViewDays {
		if d < cutoff {
			alias := fmt.Sprintf("#s%d", len(stale))
			names[alias] = d
			stale = append(stale, "viewDays."+alias)
		}
	}
	if len(stale) > 0 {
		expr += " REMOVE " + strings.Join(stale, ", ")
	}
	err := st.transact(ctx,
		types.TransactWriteItem{Put: &types.Put{Item: viewItem, ConditionExpression: aws.String("attribute_not_exists(PK)")}},
		types.TransactWriteItem{Update: &types.Update{
			Key:                       k.av(),
			UpdateExpression:          aws.String(expr),
			ConditionExpression:       aws.String("attribute_exists(PK)"),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: map[string]AV{":one": n(1), ":zero": n(0)},
		}},
	)
	if err == ErrConflict {
		return false, nil
	}
	return err == nil, err
}
