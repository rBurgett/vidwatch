package store

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type User struct {
	ID                  string  `dynamodbav:"id"`
	Username            string  `dynamodbav:"username"`
	Bio                 string  `dynamodbav:"bio"`
	AvatarKey           string  `dynamodbav:"avatarKey,omitempty"`
	BannerKey           string  `dynamodbav:"bannerKey,omitempty"`
	PlaybackSpeed       float64 `dynamodbav:"playbackSpeed"`
	NotificationsSeenAt int64   `dynamodbav:"notificationsSeenAt"`
	Subscribers         int64   `dynamodbav:"subscribers"`
	CreatedAt           int64   `dynamodbav:"createdAt"`
}

type userItem struct {
	PK, SK string
	User
}

type usernameItem struct {
	PK, SK string
	UserID string `dynamodbav:"userId"`
}

func userKey(id string) Key { return Key{"USER#" + id, "PROFILE"} }
func usernameKey(name string) Key {
	return Key{"UNAME#" + strings.ToLower(name), "UNAME"}
}

// GetUser returns nil if the user doesn't exist.
func (st *Store) GetUser(ctx context.Context, id string) (*User, error) {
	var u User
	ok, err := st.get(ctx, userKey(id), &u)
	if err != nil || !ok {
		return nil, err
	}
	return &u, nil
}

// GetUsers fetches several users at once, keyed by ID.
func (st *Store) GetUsers(ctx context.Context, ids []string) (map[string]*User, error) {
	keys := make([]Key, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, userKey(id))
	}
	items, err := st.batchGet(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*User, len(items))
	for _, it := range items {
		var u User
		if err := attributevalue.UnmarshalMap(it, &u); err != nil {
			return nil, err
		}
		out[u.ID] = &u
	}
	return out, nil
}

// UserByUsername returns nil if nobody has that username (case-insensitive).
func (st *Store) UserByUsername(ctx context.Context, name string) (*User, error) {
	var ui usernameItem
	ok, err := st.get(ctx, usernameKey(name), &ui)
	if err != nil || !ok {
		return nil, err
	}
	return st.GetUser(ctx, ui.UserID)
}

// CreateUser stores a new profile and claims its username. It returns
// ErrConflict if the username belongs to someone else, and is a no-op if the
// profile already exists.
func (st *Store) CreateUser(ctx context.Context, u *User) error {
	if u.PlaybackSpeed == 0 {
		u.PlaybackSpeed = 1
	}
	if u.CreatedAt == 0 {
		u.CreatedAt = now()
	}
	profile, err := attributevalue.MarshalMap(userItem{PK: userKey(u.ID).PK, SK: userKey(u.ID).SK, User: *u})
	if err != nil {
		return err
	}
	uk := usernameKey(u.Username)
	claim, err := attributevalue.MarshalMap(usernameItem{PK: uk.PK, SK: uk.SK, UserID: u.ID})
	if err != nil {
		return err
	}
	err = st.transact(ctx,
		types.TransactWriteItem{Put: &types.Put{Item: profile, ConditionExpression: aws.String("attribute_not_exists(PK)")}},
		types.TransactWriteItem{Put: &types.Put{
			Item:                      claim,
			ConditionExpression:       aws.String("attribute_not_exists(PK) OR userId = :id"),
			ExpressionAttributeValues: map[string]AV{":id": s(u.ID)},
		}},
	)
	if err == ErrConflict {
		// Either the profile was created concurrently (fine) or the name is taken.
		if existing, gerr := st.GetUser(ctx, u.ID); gerr == nil && existing != nil {
			return nil
		}
	}
	return err
}

// UserUpdate holds the profile fields to change; nil means "leave alone".
type UserUpdate struct {
	Bio           *string
	PlaybackSpeed *float64
	AvatarKey     *string
	BannerKey     *string // "" removes the banner
}

func (st *Store) UpdateUser(ctx context.Context, id string, upd UserUpdate) (*User, error) {
	var sets, removes []string
	vals := map[string]AV{}
	if upd.Bio != nil {
		sets = append(sets, "bio = :bio")
		vals[":bio"] = s(*upd.Bio)
	}
	if upd.PlaybackSpeed != nil {
		sets = append(sets, "playbackSpeed = :ps")
		vals[":ps"] = f(*upd.PlaybackSpeed)
	}
	if upd.AvatarKey != nil {
		sets = append(sets, "avatarKey = :ak")
		vals[":ak"] = s(*upd.AvatarKey)
	}
	if upd.BannerKey != nil {
		if *upd.BannerKey == "" {
			removes = append(removes, "bannerKey")
		} else {
			sets = append(sets, "bannerKey = :bk")
			vals[":bk"] = s(*upd.BannerKey)
		}
	}
	if len(sets) == 0 && len(removes) == 0 {
		return st.GetUser(ctx, id)
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
		Key:                 userKey(id).av(),
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
	var u User
	if err := attributevalue.UnmarshalMap(res.Attributes, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (st *Store) SetNotificationsSeen(ctx context.Context, id string, at int64) error {
	_, err := st.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &st.table,
		Key:                       userKey(id).av(),
		UpdateExpression:          aws.String("SET notificationsSeenAt = :t"),
		ConditionExpression:       aws.String("attribute_exists(PK)"),
		ExpressionAttributeValues: map[string]AV{":t": n(at)},
	})
	if isConditionFailed(err) {
		return ErrNotFound
	}
	return err
}
