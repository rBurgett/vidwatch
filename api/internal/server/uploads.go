package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rBurgett/vidwatch/api/internal/media"
	"github.com/rBurgett/vidwatch/api/internal/store"
)

const imageMaxBytes = 10 << 20

var imageTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

type uploadKind struct {
	prefix string            // S3 key prefix
	types  map[string]string // allowed content type -> file extension
}

// Only types that can't execute script in a browser are allowed (no SVG, no
// HTML), and S3 stores the exact Content-Type the upload was signed for.
var uploadKinds = map[string]uploadKind{
	"video":     {"videos", map[string]string{"video/mp4": ".mp4", "video/webm": ".webm"}},
	"thumbnail": {"thumbnails", imageTypes},
	"avatar":    {"avatars", imageTypes},
	"banner":    {"banners", imageTypes},
}

func (s *Server) maxBytes(kind string) int64 {
	if kind == "video" {
		return int64(s.cfg.MaxUploadMB) << 20
	}
	return imageMaxBytes
}

func (s *Server) presignUpload(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Kind        string `json:"kind"`
		ContentType string `json:"contentType"`
		Size        int64  `json:"size"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	kind, okKind := uploadKinds[in.Kind]
	if !okKind {
		return errBadRequest("Unknown upload kind")
	}
	ext, okType := kind.types[in.ContentType]
	if !okType {
		if in.Kind == "video" {
			return errBadRequest("Video must be MP4 or WebM")
		}
		return errBadRequest("Image must be JPEG, PNG, WebP, or GIF")
	}
	max := s.maxBytes(in.Kind)
	if in.Size > max {
		return errBadRequest(fmt.Sprintf("File is too large (max %d MB)", max>>20))
	}
	key := fmt.Sprintf("%s/%s/%s%s", kind.prefix, currentUser(r).ID, store.NewID(24), ext)
	up, err := s.media.PresignUpload(r.Context(), key, in.ContentType, max)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"key": up.Key, "url": up.URL, "fields": up.Fields, "maxBytes": max})
}

// checkUpload makes sure key is a finished upload of the given kind that
// belongs to the current user.
func (s *Server) checkUpload(r *http.Request, kind, key string) (*media.Object, error) {
	k := uploadKinds[kind]
	if !strings.HasPrefix(key, k.prefix+"/"+currentUser(r).ID+"/") || strings.Contains(key, "..") {
		return nil, errBadRequest("Invalid upload")
	}
	obj, err := s.media.Head(r.Context(), key)
	if errors.Is(err, media.ErrNotFound) {
		return nil, errBadRequest("Upload not found — please try again")
	}
	if err != nil {
		return nil, err
	}
	if _, allowed := k.types[obj.ContentType]; !allowed || obj.Size <= 0 || obj.Size > s.maxBytes(kind) {
		return nil, errBadRequest("Upload has the wrong type or size")
	}
	return obj, nil
}

// discardUploads deletes uploads the user made for a request that failed, so
// they don't linger. Keys outside the user's own uploads are ignored.
func (s *Server) discardUploads(r *http.Request, keys ...string) {
	user := currentUser(r)
	var mine []string
	for _, key := range keys {
		for _, k := range uploadKinds {
			if key != "" && strings.HasPrefix(key, k.prefix+"/"+user.ID+"/") {
				mine = append(mine, key)
				break
			}
		}
	}
	s.deleteObjects(r, mine...)
}

// deleteObjects removes media in the background of the request; failures are
// only logged (the database is already the source of truth).
func (s *Server) deleteObjects(r *http.Request, keys ...string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	for _, key := range keys {
		if key == "" {
			continue
		}
		if err := s.media.Delete(ctx, key); err != nil {
			s.log.WarnContext(ctx, "delete media", "key", key, "err", err)
		}
	}
}
