package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type Playlist struct {
	ID        string `dynamodbav:"id"`
	UserID    string `dynamodbav:"userId"`
	Title     string `dynamodbav:"title"`
	CreatedAt int64  `dynamodbav:"createdAt"`
	NextPos   int64  `dynamodbav:"nextPos"`
}

func playlistKey(id string) Key { return Key{"PLAYLIST#" + id, "META"} }
func playlistItemKey(playlistID, videoID string) Key {
	return Key{"PLAYLIST#" + playlistID, "ITEM#" + videoID}
}

func (st *Store) CreatePlaylist(ctx context.Context, userID, title string) (*Playlist, error) {
	p := Playlist{ID: NewID(12), UserID: userID, Title: title, CreatedAt: now()}
	k := playlistKey(p.ID)
	err := st.put(ctx, struct {
		PK, SK, GSI2PK, GSI2SK string
		Playlist
	}{k.PK, k.SK, "PLAYLISTS#" + userID, ts(p.CreatedAt) + "#" + p.ID, p}, "attribute_not_exists(PK)")
	return &p, err
}

// GetPlaylist returns nil if the playlist doesn't exist.
func (st *Store) GetPlaylist(ctx context.Context, id string) (*Playlist, error) {
	if id == "" {
		return nil, nil
	}
	var p Playlist
	ok, err := st.get(ctx, playlistKey(id), &p)
	if err != nil || !ok {
		return nil, err
	}
	return &p, nil
}

// UserPlaylists lists a user's playlists, newest first.
func (st *Store) UserPlaylists(ctx context.Context, userID string) ([]Playlist, error) {
	var items []Item
	err := st.query(ctx, &dynamodb.QueryInput{
		IndexName:                 aws.String("GSI2"),
		KeyConditionExpression:    aws.String("GSI2PK = :p"),
		ExpressionAttributeValues: map[string]AV{":p": s("PLAYLISTS#" + userID)},
		ScanIndexForward:          aws.Bool(false),
	}, func(it Item) (bool, error) {
		items = append(items, it)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return unmarshalAll[Playlist](items)
}

// PlaylistVideoIDs lists the videos in a playlist, in playlist order.
func (st *Store) PlaylistVideoIDs(ctx context.Context, playlistID string) ([]string, error) {
	var ids []string
	err := st.query(ctx, &dynamodb.QueryInput{
		IndexName:                 aws.String("LSI1"),
		KeyConditionExpression:    aws.String("PK = :pk AND begins_with(LSI1SK, :p)"),
		ExpressionAttributeValues: map[string]AV{":pk": s("PLAYLIST#" + playlistID), ":p": s("ITEM#")},
	}, func(it Item) (bool, error) {
		ids = append(ids, str(it["videoId"]))
		return true, nil
	})
	return ids, err
}

func (st *Store) PlaylistCount(ctx context.Context, playlistID string) (int, error) {
	keys, err := st.keysWithPrefix(ctx, "PLAYLIST#"+playlistID, "ITEM#")
	return len(keys), err
}

// PlaylistsContaining reports which of the given playlists contain the video.
func (st *Store) PlaylistsContaining(ctx context.Context, playlistIDs []string, videoID string) (map[string]bool, error) {
	keys := make([]Key, 0, len(playlistIDs))
	for _, id := range playlistIDs {
		keys = append(keys, playlistItemKey(id, videoID))
	}
	items, err := st.batchGet(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i, k := range keys {
		if _, ok := items[k]; ok {
			out[playlistIDs[i]] = true
		}
	}
	return out, nil
}

// AddPlaylistItem appends a video to a playlist; adding one that's already
// there is a no-op.
func (st *Store) AddPlaylistItem(ctx context.Context, playlistID, videoID string) error {
	ik := playlistItemKey(playlistID, videoID)
	if exists, err := st.get(ctx, ik, nil); err != nil || exists {
		return err
	}
	res, err := st.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &st.table,
		Key:                       playlistKey(playlistID).av(),
		UpdateExpression:          aws.String("ADD nextPos :one"),
		ConditionExpression:       aws.String("attribute_exists(PK)"),
		ExpressionAttributeValues: map[string]AV{":one": n(1)},
		ReturnValues:              types.ReturnValueUpdatedNew,
	})
	if isConditionFailed(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var pos struct {
		NextPos int64 `dynamodbav:"nextPos"`
	}
	if err := attributevalue.UnmarshalMap(res.Attributes, &pos); err != nil {
		return err
	}
	item := Item{
		"PK": s(ik.PK), "SK": s(ik.SK),
		"LSI1SK": s(fmt.Sprintf("ITEM#%010d", pos.NextPos)),
		"GSI2PK": s("REF#" + videoID), "GSI2SK": s(ik.PK),
		"videoId": s(videoID), "position": n(pos.NextPos), "addedAt": n(now()),
	}
	_, err = st.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &st.table, Item: item, ConditionExpression: aws.String("attribute_not_exists(PK)"),
	})
	if isConditionFailed(err) {
		return nil // added concurrently
	}
	return err
}

func (st *Store) RemovePlaylistItem(ctx context.Context, playlistID, videoID string) error {
	return st.delete(ctx, playlistItemKey(playlistID, videoID))
}

func (st *Store) DeletePlaylist(ctx context.Context, playlistID string) error {
	keys, err := st.keysWithPrefix(ctx, "PLAYLIST#"+playlistID, "")
	if err != nil {
		return err
	}
	return st.batchDelete(ctx, keys)
}

func sortByDesc[T any](items []T, by func(T) int64) {
	slices.SortStableFunc(items, func(a, b T) int {
		x, y := by(a), by(b)
		switch {
		case x > y:
			return -1
		case x < y:
			return 1
		}
		return 0
	})
}
