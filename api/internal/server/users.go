package server

import (
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/rBurgett/vidwatch/api/internal/store"
)

const bioMax = 500

func (s *Server) loadUserByName(r *http.Request) (*store.User, error) {
	u, err := s.db.UserByUsername(r.Context(), r.PathValue("username"))
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, errNotFound("User not found")
	}
	return u, nil
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	u, err := s.loadUserByName(r)
	if err != nil {
		return err
	}
	viewer := currentUser(r)
	isSelf := viewer != nil && viewer.ID == u.ID
	offset := offsetParam(r)
	// Creators see their unlisted videos on their own page.
	vs, err := s.db.CreatorVideos(ctx, u.ID, !isSelf, offset+pageSize+1)
	if err != nil {
		return err
	}
	page, hasMore := paginate(vs, offset, pageSize)
	subscribed := false
	if viewer != nil && !isSelf {
		if subscribed, err = s.db.IsSubscribed(ctx, viewer.ID, u.ID); err != nil {
			return err
		}
	}
	users := map[string]*store.User{u.ID: u}
	videos := make([]videoJSON, 0, len(page))
	for _, v := range page {
		videos = append(videos, s.videoJSON(v, users))
	}
	return ok(w, map[string]any{
		"user": map[string]any{
			"id": u.ID, "username": u.Username, "bio": u.Bio,
			"avatarUrl": s.mediaURL(u.AvatarKey), "bannerUrl": s.mediaURL(u.BannerKey),
			"createdAt": u.CreatedAt, "subscribers": u.Subscribers, "subscribed": subscribed, "isSelf": isSelf,
		},
		"videos":  videos,
		"hasMore": hasMore,
	})
}

func (s *Server) listLiked(w http.ResponseWriter, r *http.Request) error {
	u, err := s.loadUserByName(r)
	if err != nil {
		return err
	}
	if u.ID != currentUser(r).ID {
		return errForbidden("Not allowed")
	}
	refs, hasMore, err := s.db.ListRefs(r.Context(), u.ID, store.Likes, offsetParam(r), pageSize)
	if err != nil {
		return err
	}
	vs, _, err := s.refVideos(r.Context(), refs)
	if err != nil {
		return err
	}
	public := make([]videoJSON, 0, len(vs))
	for _, v := range vs {
		if v.Visibility == "public" {
			public = append(public, v)
		}
	}
	return ok(w, map[string]any{"videos": public, "hasMore": hasMore})
}

func (s *Server) toggleSubscribe(w http.ResponseWriter, r *http.Request) error {
	creator, err := s.loadUserByName(r)
	if err != nil {
		return err
	}
	user := currentUser(r)
	if creator.ID == user.ID {
		return errBadRequest("You can't save yourself")
	}
	subscribed, subscribers, err := s.db.ToggleSubscription(r.Context(), user.ID, creator.ID)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"subscribed": subscribed, "subscribers": subscribers})
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) error {
	user := currentUser(r)
	var in struct {
		Bio           *string    `json:"bio"`
		PlaybackSpeed flexFloat  `json:"playbackSpeed"`
		AvatarKey     flexString `json:"avatarKey"`
		BannerKey     flexString `json:"bannerKey"`
		RemoveBanner  bool       `json:"removeBanner"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	avatarKey, bannerKey := string(in.AvatarKey), string(in.BannerKey)
	fail := func(err error) error {
		s.discardUploads(r, avatarKey, bannerKey)
		return err
	}

	var upd store.UserUpdate
	if in.Bio != nil {
		bio := strings.TrimSpace(*in.Bio)
		if utf8.RuneCountInString(bio) > bioMax {
			return fail(errBadRequest("Bio must be 500 characters or fewer"))
		}
		upd.Bio = &bio
	}
	if in.PlaybackSpeed.set {
		speed := in.PlaybackSpeed.v
		if math.IsNaN(speed) || math.IsInf(speed, 0) {
			speed = 1
		}
		speed = math.Min(3, math.Max(0.25, speed))
		upd.PlaybackSpeed = &speed
	}
	if avatarKey != "" {
		if _, err := s.checkUpload(r, "avatar", avatarKey); err != nil {
			return fail(err)
		}
		upd.AvatarKey = &avatarKey
	}
	if bannerKey != "" {
		if _, err := s.checkUpload(r, "banner", bannerKey); err != nil {
			return fail(err)
		}
		upd.BannerKey = &bannerKey
	} else if in.RemoveBanner {
		none := ""
		upd.BannerKey = &none
	}

	updated, err := s.db.UpdateUser(r.Context(), user.ID, upd)
	if err != nil {
		return fail(err)
	}
	if upd.AvatarKey != nil && user.AvatarKey != "" && user.AvatarKey != avatarKey {
		s.deleteObjects(r, user.AvatarKey)
	}
	if upd.BannerKey != nil && user.BannerKey != "" && user.BannerKey != bannerKey {
		s.deleteObjects(r, user.BannerKey)
	}
	return ok(w, map[string]any{"user": s.userJSON(updated)})
}
