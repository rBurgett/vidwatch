package server

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/rBurgett/vidwatch/api/internal/store"
)

var nan = math.NaN()

// ---------- response shapes (same as the original Node API) ----------

type authorJSON struct {
	ID        string  `json:"id"`
	Username  string  `json:"username"`
	AvatarURL *string `json:"avatarUrl"`
}

type videoJSON struct {
	ID              string     `json:"id"`
	Title           string     `json:"title"`
	Description     string     `json:"description"`
	VideoURL        string     `json:"videoUrl"`
	ThumbnailURL    *string    `json:"thumbnailUrl"`
	Views           int64      `json:"views"`
	Likes           int64      `json:"likes"`
	DurationSeconds *float64   `json:"durationSeconds"`
	AgeRating       string     `json:"ageRating"`
	Visibility      string     `json:"visibility"`
	CreatedAt       int64      `json:"createdAt"`
	Author          authorJSON `json:"author"`
}

type userJSON struct {
	ID            string  `json:"id"`
	Username      string  `json:"username"`
	Bio           string  `json:"bio"`
	AvatarURL     *string `json:"avatarUrl"`
	BannerURL     *string `json:"bannerUrl"`
	PlaybackSpeed float64 `json:"playbackSpeed"`
}

type commentJSON struct {
	ID        string     `json:"id"`
	Body      string     `json:"body"`
	CreatedAt int64      `json:"createdAt"`
	EditedAt  *int64     `json:"editedAt"`
	ParentID  *string    `json:"parentId"`
	Author    authorJSON `json:"author"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *Server) mediaURL(key string) *string { return nullable(s.media.URL(key)) }

func (s *Server) userJSON(u *store.User) userJSON {
	speed := u.PlaybackSpeed
	if speed == 0 {
		speed = 1
	}
	return userJSON{
		ID: u.ID, Username: u.Username, Bio: u.Bio,
		AvatarURL: s.mediaURL(u.AvatarKey), BannerURL: s.mediaURL(u.BannerKey),
		PlaybackSpeed: speed,
	}
}

func (s *Server) authorJSON(id string, users map[string]*store.User) authorJSON {
	if u := users[id]; u != nil {
		return authorJSON{ID: u.ID, Username: u.Username, AvatarURL: s.mediaURL(u.AvatarKey)}
	}
	return authorJSON{ID: id, Username: "unknown"}
}

func (s *Server) videoJSON(v *store.Video, users map[string]*store.User) videoJSON {
	rating := v.AgeRating
	if rating == "" {
		rating = "G"
	}
	return videoJSON{
		ID: v.ID, Title: v.Title, Description: v.Description,
		VideoURL: s.media.URL(v.VideoKey), ThumbnailURL: s.mediaURL(v.ThumbnailKey),
		Views: v.Views, Likes: v.Likes, DurationSeconds: v.DurationSeconds,
		AgeRating: rating, Visibility: v.Visibility, CreatedAt: v.CreatedAt,
		Author: s.authorJSON(v.UserID, users),
	}
}

// users loads the profiles behind a set of user IDs (e.g. video authors), so
// names and avatars are always current.
func (s *Server) users(ctx context.Context, ids ...string) (map[string]*store.User, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 {
		return map[string]*store.User{}, nil
	}
	return s.db.GetUsers(ctx, ids)
}

func (s *Server) videosJSON(ctx context.Context, vs []*store.Video) ([]videoJSON, error) {
	ids := make([]string, 0, len(vs))
	for _, v := range vs {
		ids = append(ids, v.UserID)
	}
	users, err := s.users(ctx, ids...)
	if err != nil {
		return nil, err
	}
	out := make([]videoJSON, 0, len(vs))
	for _, v := range vs {
		out = append(out, s.videoJSON(v, users))
	}
	return out, nil
}

// videosByID loads videos in the given order, skipping any that are gone.
func (s *Server) videosByID(ctx context.Context, ids []string) ([]*store.Video, error) {
	byID, err := s.db.GetVideos(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Video, 0, len(ids))
	for _, id := range ids {
		if v := byID[id]; v != nil {
			out = append(out, v)
		}
	}
	return out, nil
}

func (s *Server) commentJSON(c store.Comment, users map[string]*store.User) commentJSON {
	cj := commentJSON{
		ID: c.ID, Body: c.Body, CreatedAt: c.CreatedAt,
		ParentID: nullable(c.ParentID), Author: s.authorJSON(c.UserID, users),
	}
	if c.EditedAt != 0 {
		cj.EditedAt = &c.EditedAt
	}
	return cj
}

// ---------- feed filtering and sorting ----------

var ratings = []string{"G", "PG", "PG-13", "R"}

func ratingRank(r string) int { return slices.Index(ratings, r) }

// allowedRatings is every rating at or below max ("kids mode"), or nil if max
// isn't a rating.
func allowedRatings(max string) map[string]bool {
	rank := ratingRank(max)
	if rank < 0 {
		return nil
	}
	out := map[string]bool{}
	for _, r := range ratings[:rank+1] {
		out[r] = true
	}
	return out
}

const day = 24 * time.Hour

var sinceWindows = map[string]time.Duration{"24h": day, "7d": 7 * day, "30d": 30 * day, "year": 365 * day}

// feedFilter mirrors the query-string filters of GET /api/videos.
type feedFilter struct {
	query    string          // lowercase substring of title or description
	userID   string          // only this creator ("mine")
	creators map[string]bool // only these creators ("following"); nil = anyone
	rating   string          // exactly this rating
	allowed  map[string]bool // ratings allowed by kids mode; nil = all
	duration string          // short | medium | long
	since    int64           // created after (ms)
}

func (f feedFilter) match(v *store.Video) bool {
	if f.query != "" && !strings.Contains(strings.ToLower(v.Title), f.query) &&
		!strings.Contains(strings.ToLower(v.Description), f.query) {
		return false
	}
	if f.userID != "" && v.UserID != f.userID {
		return false
	}
	if f.creators != nil && !f.creators[v.UserID] {
		return false
	}
	if f.rating != "" && v.AgeRating != f.rating {
		return false
	}
	if f.allowed != nil && !f.allowed[v.AgeRating] {
		return false
	}
	if f.duration != "" {
		if v.DurationSeconds == nil {
			return false
		}
		d := *v.DurationSeconds
		switch f.duration {
		case "short":
			if d >= 240 {
				return false
			}
		case "medium":
			if d < 240 || d >= 1200 {
				return false
			}
		case "long":
			if d < 1200 {
				return false
			}
		}
	}
	return v.CreatedAt > f.since
}

// sortFeed orders videos (which arrive newest first) by views or likes,
// newest first among ties. "new" keeps the existing order.
func sortFeed(vs []*store.Video, by string) {
	var key func(*store.Video) int64
	switch by {
	case "views":
		key = func(v *store.Video) int64 { return v.Views }
	case "likes":
		key = func(v *store.Video) int64 { return v.Likes }
	default:
		return
	}
	slices.SortStableFunc(vs, func(a, b *store.Video) int { return compareDesc(key(a), key(b)) })
}

// sortTrending orders by views in the last 7 days, then all-time views, then newest.
func sortTrending(vs []*store.Video, at int64) {
	recent := make(map[string]int64, len(vs))
	for _, v := range vs {
		recent[v.ID] = v.RecentViews(at, 7)
	}
	slices.SortStableFunc(vs, func(a, b *store.Video) int {
		if c := compareDesc(recent[a.ID], recent[b.ID]); c != 0 {
			return c
		}
		if c := compareDesc(a.Views, b.Views); c != 0 {
			return c
		}
		return compareDesc(a.CreatedAt, b.CreatedAt)
	})
}

func sortNewest(vs []*store.Video) {
	slices.SortStableFunc(vs, func(a, b *store.Video) int { return compareDesc(a.CreatedAt, b.CreatedAt) })
}

func compareDesc(a, b int64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}
