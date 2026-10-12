package server

import (
	"math"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rBurgett/vidwatch/api/internal/store"
)

const (
	titleMax       = 120
	descriptionMax = 5000
	commentMax     = 2000
	reportMax      = 500
	relatedCount   = 12
)

var visibilities = []string{"public", "unlisted"}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) error {
	return ok(w, map[string]any{
		"cognitoDomain": s.cfg.CognitoDomain,
		"clientId":      s.cfg.ClientID,
		"maxUploadMB":   s.cfg.MaxUploadMB,
	})
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) error {
	return ok(w, map[string]any{"user": s.userJSON(currentUser(r))})
}

// ---------- lists ----------

func (s *Server) listVideos(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	user := currentUser(r)
	f := feedFilter{
		query:    strings.ToLower(strings.TrimSpace(q.Get("q"))),
		allowed:  allowedRatings(q.Get("maxRating")),
		duration: q.Get("duration"),
	}
	if ratingRank(q.Get("rating")) >= 0 {
		f.rating = q.Get("rating")
	}
	if window, ok := sinceWindows[q.Get("since")]; ok {
		f.since = time.Now().Add(-window).UnixMilli()
	}
	if user != nil && q.Get("mine") == "1" {
		f.userID = user.ID
	}
	if user != nil && q.Get("following") == "1" {
		subs, err := s.db.Subscriptions(r.Context(), user.ID)
		if err != nil {
			return err
		}
		f.creators = map[string]bool{}
		for _, sub := range subs {
			f.creators[sub.CreatorID] = true
		}
	}
	sort := q.Get("sort")
	offset := offsetParam(r)

	// Newest-first can stop as soon as it has the page; other sorts need everything.
	var vs []*store.Video
	err := s.db.PublicVideos(r.Context(), func(v *store.Video) bool {
		if f.match(v) {
			vs = append(vs, v)
		}
		return sort == "views" || sort == "likes" || len(vs) <= offset+pageSize
	})
	if err != nil {
		return err
	}
	sortFeed(vs, sort)
	return s.writeVideoPage(w, r, vs, offset)
}

func (s *Server) trending(w http.ResponseWriter, r *http.Request) error {
	allowed := allowedRatings(r.URL.Query().Get("maxRating"))
	var vs []*store.Video
	err := s.db.PublicVideos(r.Context(), func(v *store.Video) bool {
		if allowed == nil || allowed[v.AgeRating] {
			vs = append(vs, v)
		}
		return true
	})
	if err != nil {
		return err
	}
	sortTrending(vs, time.Now().UnixMilli())
	return s.writeVideoPage(w, r, vs, offsetParam(r))
}

func (s *Server) writeVideoPage(w http.ResponseWriter, r *http.Request, vs []*store.Video, offset int) error {
	page, hasMore := paginate(vs, offset, pageSize)
	out, err := s.videosJSON(r.Context(), page)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"videos": out, "hasMore": hasMore})
}

// ---------- one video ----------

func (s *Server) loadVideo(r *http.Request) (*store.Video, error) {
	v, err := s.db.GetVideo(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, errNotFound("Video not found")
	}
	return v, nil
}

// loadOwnVideo loads a video the current user owns.
func (s *Server) loadOwnVideo(r *http.Request) (*store.Video, error) {
	v, err := s.loadVideo(r)
	if err != nil {
		return nil, err
	}
	if v.UserID != currentUser(r).ID {
		return nil, errForbidden("Not your video")
	}
	return v, nil
}

func (s *Server) getVideo(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	v, err := s.loadVideo(r)
	if err != nil {
		return err
	}
	user := currentUser(r)
	now := time.Now().UnixMilli()

	// One view per signed-in user, otherwise one per IP. Owners don't count.
	// A failure here shouldn't break the page.
	if user == nil || user.ID != v.UserID {
		viewer := "ip:" + s.clientIP(r)
		if user != nil {
			viewer = "u:" + user.ID
		}
		if counted, err := s.db.RecordView(ctx, v, viewer, now); err != nil {
			s.log.WarnContext(ctx, "record view", "video", v.ID, "err", err)
		} else if counted {
			v.Views++
		}
	}

	resp := map[string]any{
		"liked": false, "subscribed": false, "savedForLater": false,
		"resumePosition": 0.0, "reports": []any{},
	}
	if user != nil {
		pos, err := s.db.TouchHistory(ctx, user.ID, v.ID, now)
		if err != nil {
			return err
		}
		liked, saved, subscribed, err := s.db.ViewerState(ctx, user.ID, v.ID, v.UserID)
		if err != nil {
			return err
		}
		resp["resumePosition"], resp["liked"], resp["savedForLater"], resp["subscribed"] = pos, liked, saved, subscribed
	}

	related, err := s.related(r, v)
	if err != nil {
		return err
	}
	var reports []store.Report
	authorIDs := []string{v.UserID}
	if user != nil && user.ID == v.UserID {
		if reports, err = s.db.Reports(ctx, v.ID); err != nil {
			return err
		}
		for _, rep := range reports {
			authorIDs = append(authorIDs, rep.ReporterID)
		}
	}
	for _, rv := range related {
		authorIDs = append(authorIDs, rv.UserID)
	}
	users, err := s.users(ctx, authorIDs...)
	if err != nil {
		return err
	}

	resp["video"] = s.videoJSON(v, users)
	relatedJSON := make([]videoJSON, 0, len(related))
	for _, rv := range related {
		relatedJSON = append(relatedJSON, s.videoJSON(rv, users))
	}
	resp["related"] = relatedJSON
	if len(reports) > 0 {
		out := make([]map[string]any, 0, len(reports))
		for _, rep := range reports {
			out = append(out, map[string]any{
				"reason": rep.Reason, "createdAt": rep.CreatedAt,
				"reporter": s.authorJSON(rep.ReporterID, users).Username,
			})
		}
		resp["reports"] = out
	}
	return ok(w, resp)
}

// related is "more videos": the same creator's public videos first, then the
// newest public videos, up to 12.
func (s *Server) related(r *http.Request, v *store.Video) ([]*store.Video, error) {
	same, err := s.db.CreatorVideos(r.Context(), v.UserID, true, relatedCount+1)
	if err != nil {
		return nil, err
	}
	var out []*store.Video
	for _, c := range same {
		if c.ID != v.ID && len(out) < relatedCount {
			out = append(out, c)
		}
	}
	if len(out) >= relatedCount {
		return out, nil
	}
	err = s.db.PublicVideos(r.Context(), func(c *store.Video) bool {
		if c.ID != v.ID && c.UserID != v.UserID {
			out = append(out, c)
		}
		return len(out) < relatedCount
	})
	return out, err
}

var unsafeFilenameChars = regexp.MustCompile(`[^\p{L}\p{N}_.\- ]+`)

func downloadName(title, key string) string {
	name := strings.Join(strings.Fields(unsafeFilenameChars.ReplaceAllString(title, "_")), " ")
	if utf8.RuneCountInString(name) > 80 {
		name = string([]rune(name)[:80])
	}
	if name == "" {
		name = "video"
	}
	return name + path.Ext(key)
}

func (s *Server) downloadVideo(w http.ResponseWriter, r *http.Request) error {
	v, err := s.loadVideo(r)
	if err != nil {
		return err
	}
	u, err := s.media.DownloadURL(r.Context(), v.VideoKey, downloadName(v.Title, v.VideoKey))
	if err != nil {
		return err
	}
	http.Redirect(w, r, u, http.StatusFound)
	return nil
}

// ---------- create / edit / delete ----------

type videoInput struct {
	Title           *string    `json:"title"`
	Description     *string    `json:"description"`
	AgeRating       *string    `json:"ageRating"`
	Visibility      *string    `json:"visibility"`
	DurationSeconds flexFloat  `json:"durationSeconds"`
	VideoKey        flexString `json:"videoKey"`
	ThumbnailKey    flexString `json:"thumbnailKey"`
}

func validTitle(title string) error {
	switch {
	case title == "":
		return errBadRequest("Title is required")
	case utf8.RuneCountInString(title) > titleMax:
		return errBadRequest("Title must be 120 characters or fewer")
	}
	return nil
}

func validDescription(desc string) error {
	if utf8.RuneCountInString(desc) > descriptionMax {
		return errBadRequest("Description must be 5000 characters or fewer")
	}
	return nil
}

func oneOf(v *string, allowed []string, fallback string) string {
	if v != nil {
		for _, a := range allowed {
			if *v == a {
				return a
			}
		}
	}
	return fallback
}

func (s *Server) createVideo(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := currentUser(r)
	var in videoInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	videoKey, thumbKey := string(in.VideoKey), string(in.ThumbnailKey)
	fail := func(err error) error {
		s.discardUploads(r, videoKey, thumbKey)
		return err
	}
	if videoKey == "" {
		return errBadRequest("A video file is required")
	}
	title := strings.TrimSpace(deref(in.Title))
	desc := strings.TrimSpace(deref(in.Description))
	if err := validTitle(title); err != nil {
		return fail(err)
	}
	if err := validDescription(desc); err != nil {
		return fail(err)
	}
	obj, err := s.checkUpload(r, "video", videoKey)
	if err != nil {
		return fail(err)
	}
	if thumbKey != "" {
		if _, err := s.checkUpload(r, "thumbnail", thumbKey); err != nil {
			return fail(err)
		}
	}
	v := &store.Video{
		UserID: user.ID, Title: title, Description: desc,
		VideoKey: videoKey, ThumbnailKey: thumbKey, ContentType: obj.ContentType,
		AgeRating:  oneOf(in.AgeRating, ratings, "G"),
		Visibility: oneOf(in.Visibility, visibilities, "public"),
	}
	if d := in.DurationSeconds.v; in.DurationSeconds.set && !math.IsNaN(d) && !math.IsInf(d, 0) && d > 0 && d <= 24*3600 {
		v.DurationSeconds = &d
	}
	if err := s.db.CreateVideo(ctx, v); err != nil {
		return err
	}
	out, err := s.videosJSON(ctx, []*store.Video{v})
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"video": out[0]})
}

func (s *Server) updateVideo(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var in videoInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	thumbKey := string(in.ThumbnailKey)
	v, err := s.loadOwnVideo(r)
	if err != nil {
		s.discardUploads(r, thumbKey)
		return err
	}
	var upd store.VideoUpdate
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if err := validTitle(t); err != nil {
			s.discardUploads(r, thumbKey)
			return err
		}
		upd.Title = &t
	}
	if in.Description != nil {
		d := strings.TrimSpace(*in.Description)
		if err := validDescription(d); err != nil {
			s.discardUploads(r, thumbKey)
			return err
		}
		upd.Description = &d
	}
	if rating := oneOf(in.AgeRating, ratings, ""); rating != "" {
		upd.AgeRating = &rating
	}
	if vis := oneOf(in.Visibility, visibilities, ""); vis != "" {
		upd.Visibility = &vis
	}
	if thumbKey != "" && thumbKey != v.ThumbnailKey {
		if _, err := s.checkUpload(r, "thumbnail", thumbKey); err != nil {
			s.discardUploads(r, thumbKey)
			return err
		}
		upd.ThumbnailKey = &thumbKey
	}
	updated, err := s.db.UpdateVideo(ctx, v, upd)
	if err != nil {
		return err
	}
	if upd.ThumbnailKey != nil && v.ThumbnailKey != "" {
		s.deleteObjects(r, v.ThumbnailKey)
	}
	out, err := s.videosJSON(ctx, []*store.Video{updated})
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"video": out[0]})
}

func (s *Server) deleteVideo(w http.ResponseWriter, r *http.Request) error {
	v, err := s.loadOwnVideo(r)
	if err != nil {
		return err
	}
	if err := s.db.DeleteVideo(r.Context(), v.ID); err != nil {
		return err
	}
	s.deleteObjects(r, v.VideoKey, v.ThumbnailKey)
	return ok(w, okBody)
}

// ---------- progress, likes, reports ----------

func (s *Server) saveProgress(w http.ResponseWriter, r *http.Request) error {
	v, err := s.loadVideo(r)
	if err != nil {
		return err
	}
	var in struct {
		Position flexFloat `json:"position"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	pos := in.Position.v
	if math.IsNaN(pos) || math.IsInf(pos, 0) || pos < 0 {
		pos = 0
	}
	if v.DurationSeconds != nil && pos > *v.DurationSeconds {
		pos = *v.DurationSeconds
	}
	if err := s.db.SaveProgress(r.Context(), currentUser(r).ID, v.ID, pos, time.Now().UnixMilli()); err != nil {
		return err
	}
	return ok(w, okBody)
}

func (s *Server) toggleLike(w http.ResponseWriter, r *http.Request) error {
	v, err := s.loadVideo(r)
	if err != nil {
		return err
	}
	liked, likes, err := s.db.ToggleLike(r.Context(), currentUser(r).ID, v.ID)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"liked": liked, "likes": likes})
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	reason := strings.TrimSpace(in.Reason)
	switch {
	case reason == "":
		return errBadRequest("Please describe the problem")
	case utf8.RuneCountInString(reason) > reportMax:
		return errBadRequest("Report too long")
	}
	v, err := s.loadVideo(r)
	if err != nil {
		return err
	}
	added, err := s.db.AddReport(r.Context(), v.ID, currentUser(r).ID, reason)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"ok": true, "alreadyReported": !added})
}

// ---------- comments ----------

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) error {
	comments, err := s.db.ListComments(r.Context(), r.PathValue("id"), r.URL.Query().Get("sort") != "old", 500)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(comments))
	for _, c := range comments {
		ids = append(ids, c.UserID)
	}
	users, err := s.users(r.Context(), ids...)
	if err != nil {
		return err
	}
	out := make([]commentJSON, 0, len(comments))
	for _, c := range comments {
		out = append(out, s.commentJSON(c, users))
	}
	return ok(w, map[string]any{"comments": out})
}

func commentBody(raw string) (string, error) {
	body := strings.TrimSpace(raw)
	switch {
	case body == "":
		return "", errBadRequest("Comment cannot be empty")
	case utf8.RuneCountInString(body) > commentMax:
		return "", errBadRequest("Comment too long")
	}
	return body, nil
}

func (s *Server) writeComment(w http.ResponseWriter, r *http.Request, c *store.Comment) error {
	users, err := s.users(r.Context(), c.UserID)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"comment": s.commentJSON(*c, users)})
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var in struct {
		Body     string     `json:"body"`
		ParentID flexString `json:"parentId"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	body, err := commentBody(in.Body)
	if err != nil {
		return err
	}
	v, err := s.loadVideo(r)
	if err != nil {
		return err
	}
	c := &store.Comment{VideoID: v.ID, UserID: currentUser(r).ID, Body: body}
	if in.ParentID != "" {
		parent, err := s.db.GetComment(ctx, v.ID, string(in.ParentID))
		if err != nil {
			return err
		}
		if parent == nil {
			return errBadRequest("Parent comment not found")
		}
		if parent.ParentID != "" {
			return errBadRequest("Cannot reply to a reply")
		}
		c.ParentID = parent.ID
	}
	if err := s.db.AddComment(ctx, c); err != nil {
		return err
	}
	return s.writeComment(w, r, c)
}

func (s *Server) loadComment(r *http.Request) (*store.Comment, error) {
	c, err := s.db.GetComment(r.Context(), r.PathValue("id"), r.PathValue("commentId"))
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errNotFound("Not found")
	}
	return c, nil
}

func (s *Server) editComment(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Body string `json:"body"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	body, err := commentBody(in.Body)
	if err != nil {
		return err
	}
	c, err := s.loadComment(r)
	if err != nil {
		return err
	}
	if c.UserID != currentUser(r).ID {
		return errForbidden("Not your comment")
	}
	updated, err := s.db.EditComment(r.Context(), c.VideoID, c.ID, body)
	if err != nil {
		return err
	}
	return s.writeComment(w, r, updated)
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) error {
	c, err := s.loadComment(r)
	if err != nil {
		return err
	}
	user := currentUser(r)
	if c.UserID != user.ID {
		// The video's owner can moderate their own comments section.
		v, err := s.db.GetVideo(r.Context(), c.VideoID)
		if err != nil {
			return err
		}
		if v == nil || v.UserID != user.ID {
			return errForbidden("Not allowed")
		}
	}
	if err := s.db.DeleteComment(r.Context(), c.VideoID, c.ID); err != nil {
		return err
	}
	return ok(w, okBody)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
