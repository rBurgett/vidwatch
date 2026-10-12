package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/rBurgett/vidwatch/api/internal/store"
)

const notificationsMax = 30

// ---------- history ----------

func (s *Server) listHistory(w http.ResponseWriter, r *http.Request) error {
	refs, hasMore, err := s.db.ListRefs(r.Context(), currentUser(r).ID, store.History, offsetParam(r), pageSize)
	if err != nil {
		return err
	}
	_, byID, err := s.refVideos(r.Context(), refs)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		if v, ok := byID[ref.VideoID]; ok {
			items = append(items, map[string]any{"video": v, "position": ref.Position, "updatedAt": ref.At})
		}
	}
	return ok(w, map[string]any{"items": items, "hasMore": hasMore})
}

// refVideos loads the videos behind list entries, in list order, skipping
// any that have been deleted.
func (s *Server) refVideos(ctx context.Context, refs []store.Ref) ([]videoJSON, map[string]videoJSON, error) {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.VideoID)
	}
	vs, err := s.videosByID(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	out, err := s.videosJSON(ctx, vs)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]videoJSON, len(out))
	for _, v := range out {
		byID[v.ID] = v
	}
	return out, byID, nil
}

func (s *Server) clearHistory(w http.ResponseWriter, r *http.Request) error {
	if err := s.db.ClearRefs(r.Context(), currentUser(r).ID, store.History); err != nil {
		return err
	}
	return ok(w, okBody)
}

func (s *Server) deleteHistory(w http.ResponseWriter, r *http.Request) error {
	if err := s.db.DeleteRef(r.Context(), currentUser(r).ID, store.History, r.PathValue("videoId")); err != nil {
		return err
	}
	return ok(w, okBody)
}

// ---------- watch later ----------

func (s *Server) listWatchLater(w http.ResponseWriter, r *http.Request) error {
	refs, hasMore, err := s.db.ListRefs(r.Context(), currentUser(r).ID, store.WatchLater, offsetParam(r), pageSize)
	if err != nil {
		return err
	}
	vs, _, err := s.refVideos(r.Context(), refs)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"videos": vs, "hasMore": hasMore})
}

func (s *Server) addWatchLater(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		VideoID flexString `json:"videoId"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.VideoID == "" {
		return errBadRequest("Invalid video id")
	}
	v, err := s.db.GetVideo(r.Context(), string(in.VideoID))
	if err != nil {
		return err
	}
	if v == nil {
		return errNotFound("Video not found")
	}
	added, err := s.db.AddWatchLater(r.Context(), currentUser(r).ID, v.ID, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"ok": true, "saved": true, "added": added})
}

func (s *Server) clearWatchLater(w http.ResponseWriter, r *http.Request) error {
	if err := s.db.ClearRefs(r.Context(), currentUser(r).ID, store.WatchLater); err != nil {
		return err
	}
	return ok(w, okBody)
}

func (s *Server) deleteWatchLater(w http.ResponseWriter, r *http.Request) error {
	if err := s.db.DeleteRef(r.Context(), currentUser(r).ID, store.WatchLater, r.PathValue("videoId")); err != nil {
		return err
	}
	return ok(w, okBody)
}

// ---------- notifications ----------

// notifications are derived, not stored: public uploads from creators you
// follow, posted after you started following them.
func (s *Server) notifications(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := currentUser(r)
	subs, err := s.db.Subscriptions(ctx, user.ID)
	if err != nil {
		return err
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		vs       []*store.Video
	)
	sem := make(chan struct{}, 8)
	for _, sub := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			list, err := s.db.CreatorVideos(ctx, sub.CreatorID, true, notificationsMax)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				firstErr = err
				return
			}
			for _, v := range list {
				if v.CreatedAt > sub.CreatedAt {
					vs = append(vs, v)
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	sortNewest(vs)
	if len(vs) > notificationsMax {
		vs = vs[:notificationsMax]
	}
	out, err := s.videosJSON(ctx, vs)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(out))
	unread := 0
	for i, v := range out {
		isUnread := vs[i].CreatedAt > user.NotificationsSeenAt
		if isUnread {
			unread++
		}
		items = append(items, map[string]any{"video": v, "unread": isUnread})
	}
	return ok(w, map[string]any{"items": items, "unread": unread})
}

func (s *Server) notificationsSeen(w http.ResponseWriter, r *http.Request) error {
	if err := s.db.SetNotificationsSeen(r.Context(), currentUser(r).ID, time.Now().UnixMilli()); err != nil {
		return err
	}
	return ok(w, okBody)
}
