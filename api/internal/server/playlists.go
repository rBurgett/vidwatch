package server

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/rBurgett/vidwatch/api/internal/store"
)

const playlistTitleMax = 120

type playlistSummary struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"createdAt"`
	Count     int    `json:"count"`
	Contains  *bool  `json:"contains,omitempty"`
}

func (s *Server) listPlaylists(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	lists, err := s.db.UserPlaylists(ctx, currentUser(r).ID)
	if err != nil {
		return err
	}
	var contains map[string]bool
	videoID := r.URL.Query().Get("videoId")
	if videoID != "" {
		ids := make([]string, 0, len(lists))
		for _, p := range lists {
			ids = append(ids, p.ID)
		}
		if contains, err = s.db.PlaylistsContaining(ctx, ids, videoID); err != nil {
			return err
		}
	}
	out := make([]playlistSummary, 0, len(lists))
	for _, p := range lists {
		count, err := s.db.PlaylistCount(ctx, p.ID)
		if err != nil {
			return err
		}
		sum := playlistSummary{ID: p.ID, Title: p.Title, CreatedAt: p.CreatedAt, Count: count}
		if contains != nil {
			c := contains[p.ID]
			sum.Contains = &c
		}
		out = append(out, sum)
	}
	return ok(w, map[string]any{"playlists": out})
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Title string `json:"title"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	title := strings.TrimSpace(in.Title)
	switch {
	case title == "":
		return errBadRequest("Playlist needs a name")
	case utf8.RuneCountInString(title) > playlistTitleMax:
		return errBadRequest("Name too long")
	}
	p, err := s.db.CreatePlaylist(r.Context(), currentUser(r).ID, title)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"playlist": playlistSummary{ID: p.ID, Title: p.Title, CreatedAt: p.CreatedAt}})
}

func (s *Server) loadPlaylist(r *http.Request) (*store.Playlist, error) {
	p, err := s.db.GetPlaylist(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errNotFound("Playlist not found")
	}
	return p, nil
}

func (s *Server) loadOwnPlaylist(r *http.Request) (*store.Playlist, error) {
	p, err := s.loadPlaylist(r)
	if err != nil {
		return nil, err
	}
	if p.UserID != currentUser(r).ID {
		return nil, errForbidden("Not your playlist")
	}
	return p, nil
}

func (s *Server) getPlaylist(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p, err := s.loadPlaylist(r)
	if err != nil {
		return err
	}
	viewer := currentUser(r)
	isOwner := viewer != nil && viewer.ID == p.UserID
	ids, err := s.db.PlaylistVideoIDs(ctx, p.ID)
	if err != nil {
		return err
	}
	vs, err := s.videosByID(ctx, ids)
	if err != nil {
		return err
	}
	if !isOwner {
		public := vs[:0]
		for _, v := range vs {
			if v.Visibility == "public" {
				public = append(public, v)
			}
		}
		vs = public
	}
	items, err := s.videosJSON(ctx, vs)
	if err != nil {
		return err
	}
	owners, err := s.users(ctx, p.UserID)
	if err != nil {
		return err
	}
	owner := s.authorJSON(p.UserID, owners)
	return ok(w, map[string]any{
		"playlist": map[string]any{
			"id": p.ID, "title": p.Title, "createdAt": p.CreatedAt,
			"owner": map[string]string{"id": owner.ID, "username": owner.Username}, "isOwner": isOwner,
		},
		"items": items,
	})
}

func (s *Server) addPlaylistItem(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		VideoID flexString `json:"videoId"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	p, err := s.loadOwnPlaylist(r)
	if err != nil {
		return err
	}
	v, err := s.db.GetVideo(r.Context(), string(in.VideoID))
	if err != nil {
		return err
	}
	if v == nil {
		return errNotFound("Video not found")
	}
	if err := s.db.AddPlaylistItem(r.Context(), p.ID, v.ID); err != nil {
		return err
	}
	count, err := s.db.PlaylistCount(r.Context(), p.ID)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"ok": true, "count": count})
}

func (s *Server) removePlaylistItem(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadOwnPlaylist(r)
	if err != nil {
		return err
	}
	if err := s.db.RemovePlaylistItem(r.Context(), p.ID, r.PathValue("videoId")); err != nil {
		return err
	}
	return ok(w, okBody)
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadOwnPlaylist(r)
	if err != nil {
		return err
	}
	if err := s.db.DeletePlaylist(r.Context(), p.ID); err != nil {
		return err
	}
	return ok(w, okBody)
}
