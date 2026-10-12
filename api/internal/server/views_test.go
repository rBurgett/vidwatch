package server

import (
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/rBurgett/vidwatch/api/internal/store"
)

func ids(vs []*store.Video) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.ID
	}
	return out
}

func dur(d float64) *float64 { return &d }

func TestFeedFilter(t *testing.T) {
	now := time.Now().UnixMilli()
	vs := []*store.Video{
		{ID: "a", UserID: "u1", Title: "Cats on Parade", AgeRating: "G", DurationSeconds: dur(60), CreatedAt: now},
		{ID: "b", UserID: "u2", Title: "Dogs", Description: "a CAT appears", AgeRating: "PG-13", DurationSeconds: dur(600), CreatedAt: now - int64(2*day/time.Millisecond)},
		{ID: "c", UserID: "u1", Title: "Night drive", AgeRating: "R", CreatedAt: now - int64(40*day/time.Millisecond)},
	}
	run := func(f feedFilter) []string {
		var out []*store.Video
		for _, v := range vs {
			if f.match(v) {
				out = append(out, v)
			}
		}
		return ids(out)
	}
	tests := []struct {
		name string
		f    feedFilter
		want []string
	}{
		{"everything", feedFilter{}, []string{"a", "b", "c"}},
		{"search title or description, case-insensitive", feedFilter{query: "cat"}, []string{"a", "b"}},
		{"mine", feedFilter{userID: "u1"}, []string{"a", "c"}},
		{"following", feedFilter{creators: map[string]bool{"u2": true}}, []string{"b"}},
		{"following nobody", feedFilter{creators: map[string]bool{}}, nil},
		{"kids mode PG", feedFilter{allowed: allowedRatings("PG")}, []string{"a"}},
		{"exact rating", feedFilter{rating: "R"}, []string{"c"}},
		{"short", feedFilter{duration: "short"}, []string{"a"}},
		{"medium", feedFilter{duration: "medium"}, []string{"b"}},
		{"since 7d", feedFilter{since: now - int64(7*day/time.Millisecond)}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		if got := run(tt.f); !slices.Equal(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSorting(t *testing.T) {
	now := time.Now().UnixMilli()
	today := time.UnixMilli(now).UTC().Format("2006-01-02")
	old := time.UnixMilli(now - int64(10*day/time.Millisecond)).UTC().Format("2006-01-02")
	newest := func() []*store.Video {
		return []*store.Video{
			{ID: "new", Views: 1, Likes: 5, CreatedAt: now, ViewDays: map[string]int64{today: 1}},
			{ID: "mid", Views: 50, Likes: 1, CreatedAt: now - 1000, ViewDays: map[string]int64{old: 50}},
			{ID: "old", Views: 9, Likes: 5, CreatedAt: now - 2000, ViewDays: map[string]int64{today: 9}},
		}
	}

	vs := newest()
	sortFeed(vs, "views")
	if got := ids(vs); !slices.Equal(got, []string{"mid", "old", "new"}) {
		t.Errorf("views: %v", got)
	}
	vs = newest()
	sortFeed(vs, "likes") // ties keep newest first
	if got := ids(vs); !slices.Equal(got, []string{"new", "old", "mid"}) {
		t.Errorf("likes: %v", got)
	}
	vs = newest()
	sortTrending(vs, now) // recent views beat all-time views
	if got := ids(vs); !slices.Equal(got, []string{"old", "new", "mid"}) {
		t.Errorf("trending: %v", got)
	}
}

func TestPaginate(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	if page, more := paginate(items, 0, 2); !slices.Equal(page, []int{1, 2}) || !more {
		t.Errorf("first page: %v %v", page, more)
	}
	if page, more := paginate(items, 4, 2); !slices.Equal(page, []int{5}) || more {
		t.Errorf("last page: %v %v", page, more)
	}
	if page, more := paginate(items, 9, 2); len(page) != 0 || more {
		t.Errorf("past the end: %v %v", page, more)
	}
}

func TestDownloadName(t *testing.T) {
	cases := map[string]string{
		"My <cool> video?":     "My _cool_ video_.mp4",
		"   ":                  "video.mp4",
		"Ünïcödé \"quotes\"/x": "Ünïcödé _quotes_x.mp4",
	}
	for title, want := range cases {
		if got := downloadName(title, "videos/u/abc.mp4"); got != want {
			t.Errorf("downloadName(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestClientIP(t *testing.T) {
	behindCF := &Server{cfg: Config{OriginVerifySecret: "x"}}
	direct := &Server{}
	cases := []struct {
		s      *Server
		header string
		want   string
	}{
		{behindCF, "198.51.100.10:46532", "198.51.100.10"},
		{behindCF, "[2001:db8::1]:443", "2001:db8::1"},
		{behindCF, "", "192.0.2.1"},
		{direct, "198.51.100.10:46532", "192.0.2.1"}, // header not trusted without CloudFront
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/videos/x", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		if c.header != "" {
			r.Header.Set("CloudFront-Viewer-Address", c.header)
		}
		if got := c.s.clientIP(r); got != c.want {
			t.Errorf("clientIP(%q) = %q, want %q", c.header, got, c.want)
		}
	}
}
