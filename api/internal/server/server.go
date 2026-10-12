// Package server is vidwatch's HTTP API.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/rBurgett/vidwatch/api/internal/auth"
	"github.com/rBurgett/vidwatch/api/internal/media"
	"github.com/rBurgett/vidwatch/api/internal/store"
)

type Config struct {
	CognitoDomain string // full URL, e.g. https://vidwatch-burgett.auth.us-east-1.amazoncognito.com
	ClientID      string
	MaxUploadMB   int
	// OriginVerifySecret, when set, must arrive in the X-Origin-Verify header
	// (CloudFront adds it). It also means the request came through CloudFront,
	// so CloudFront-Viewer-Address can be trusted for the client IP.
	OriginVerifySecret string
	// StaticDir, when set, serves the frontend too (local dev).
	StaticDir string
}

type Verifier interface {
	Verify(ctx context.Context, token string) (*auth.Claims, error)
}

type Server struct {
	cfg   Config
	db    *store.Store
	media *media.Store
	auth  Verifier
	log   *slog.Logger
}

func New(cfg Config, db *store.Store, m *media.Store, v Verifier, log *slog.Logger) *Server {
	if cfg.MaxUploadMB <= 0 {
		cfg.MaxUploadMB = 500
	}
	return &Server{cfg: cfg, db: db, media: m, auth: v, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, fn handlerFunc) { mux.Handle(pattern, s.handle(fn)) }

	route("GET /api/config", s.getConfig)
	route("GET /api/auth/me", s.authed(s.getMe))

	route("GET /api/videos", s.listVideos)
	route("GET /api/trending", s.trending)
	route("GET /api/videos/{id}", s.getVideo)
	route("GET /api/videos/{id}/download", s.downloadVideo)
	route("POST /api/videos", s.authed(s.createVideo))
	route("PATCH /api/videos/{id}", s.authed(s.updateVideo))
	route("DELETE /api/videos/{id}", s.authed(s.deleteVideo))
	route("POST /api/videos/{id}/progress", s.authed(s.saveProgress))
	route("POST /api/videos/{id}/like", s.authed(s.toggleLike))
	route("POST /api/videos/{id}/report", s.authed(s.report))
	route("GET /api/videos/{id}/comments", s.listComments)
	route("POST /api/videos/{id}/comments", s.authed(s.addComment))
	route("PATCH /api/videos/{id}/comments/{commentId}", s.authed(s.editComment))
	route("DELETE /api/videos/{id}/comments/{commentId}", s.authed(s.deleteComment))
	route("POST /api/uploads", s.authed(s.presignUpload))

	route("GET /api/me/history", s.authed(s.listHistory))
	route("DELETE /api/me/history", s.authed(s.clearHistory))
	route("DELETE /api/me/history/{videoId}", s.authed(s.deleteHistory))
	route("GET /api/me/watch-later", s.authed(s.listWatchLater))
	route("POST /api/me/watch-later", s.authed(s.addWatchLater))
	route("DELETE /api/me/watch-later", s.authed(s.clearWatchLater))
	route("DELETE /api/me/watch-later/{videoId}", s.authed(s.deleteWatchLater))
	route("GET /api/me/notifications", s.authed(s.notifications))
	route("POST /api/me/notifications/seen", s.authed(s.notificationsSeen))

	route("GET /api/users/{username}", s.getProfile)
	route("GET /api/users/{username}/liked", s.authed(s.listLiked))
	route("POST /api/users/{username}/subscribe", s.authed(s.toggleSubscribe))
	route("PATCH /api/users/me", s.authed(s.updateMe))

	route("GET /api/playlists", s.authed(s.listPlaylists))
	route("POST /api/playlists", s.authed(s.createPlaylist))
	route("GET /api/playlists/{id}", s.getPlaylist)
	route("DELETE /api/playlists/{id}", s.authed(s.deletePlaylist))
	route("POST /api/playlists/{id}/items", s.authed(s.addPlaylistItem))
	route("DELETE /api/playlists/{id}/items/{videoId}", s.authed(s.removePlaylistItem))

	route("/api/", func(http.ResponseWriter, *http.Request) error { return errNotFound("Not found") })
	if s.cfg.StaticDir != "" {
		mux.Handle("/", staticHandler(s.cfg.StaticDir))
	}

	return s.logRequests(s.recoverPanics(s.verifyOrigin(s.authenticate(mux))))
}

// ---------- errors ----------

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errBadRequest(msg string) error { return &apiError{http.StatusBadRequest, msg} }
func errNotFound(msg string) error   { return &apiError{http.StatusNotFound, msg} }
func errForbidden(msg string) error  { return &apiError{http.StatusForbidden, msg} }

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

func (s *Server) handle(fn handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := fn(w, r)
		if err == nil {
			return
		}
		var ae *apiError
		if errors.As(err, &ae) {
			writeJSON(w, ae.status, map[string]string{"error": ae.msg})
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
			return
		}
		s.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Server error"})
	})
}

// ---------- middleware ----------

type ctxKey struct{}

// currentUser is the signed-in user, or nil.
func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxKey{}).(*store.User)
	return u
}

// authenticate attaches the user for a valid bearer token. A missing or
// invalid token just means "anonymous"; routes that need a user say so with
// authed().
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		claims, err := s.auth.Verify(r.Context(), token)
		if err != nil {
			if !errors.Is(err, auth.ErrInvalidToken) {
				s.log.WarnContext(r.Context(), "token verification failed", "err", err)
			}
			next.ServeHTTP(w, r)
			return
		}
		user, err := s.ensureUser(r.Context(), claims)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "That username belongs to another account"})
				return
			}
			s.log.ErrorContext(r.Context(), "load user", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Server error"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, user)))
	})
}

// ensureUser loads the user's profile, creating it on their first request.
func (s *Server) ensureUser(ctx context.Context, c *auth.Claims) (*store.User, error) {
	u, err := s.db.GetUser(ctx, c.Sub)
	if err != nil || u != nil {
		return u, err
	}
	u = &store.User{ID: c.Sub, Username: c.Username}
	if err := s.db.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	return s.db.GetUser(ctx, c.Sub)
}

func (s *Server) authed(fn handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if currentUser(r) == nil {
			return &apiError{http.StatusUnauthorized, "Authentication required"}
		}
		return fn(w, r)
	}
}

func (s *Server) verifyOrigin(next http.Handler) http.Handler {
	if s.cfg.OriginVerifySecret == "" {
		return next
	}
	secret := []byte(s.cfg.OriginVerifySecret)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Origin-Verify")), secret) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Forbidden"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.log.ErrorContext(r.Context(), "panic", "err", fmt.Sprint(p), "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.log.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path,
				"status", sw.status, "ms", time.Since(start).Milliseconds())
		}
	})
}

// clientIP is the viewer's IP address. Behind CloudFront that's in
// CloudFront-Viewer-Address ("ip:port"); locally it's the connection address.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.OriginVerifySecret != "" {
		if addr := r.Header.Get("CloudFront-Viewer-Address"); addr != "" {
			if host, _, err := net.SplitHostPort(addr); err == nil {
				return host
			}
			if i := strings.LastIndex(addr, ":"); i > 0 {
				return addr[:i]
			}
			return addr
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ---------- request / response helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, v any) error {
	writeJSON(w, http.StatusOK, v)
	return nil
}

var okBody = map[string]bool{"ok": true}

func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return errBadRequest("Invalid JSON body")
	}
	return nil
}

const pageSize = 24

func offsetParam(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// paginate slices one page out of items and reports whether more follow.
func paginate[T any](items []T, offset, size int) ([]T, bool) {
	if offset >= len(items) {
		return []T{}, false
	}
	items = items[offset:]
	if len(items) > size {
		return items[:size], true
	}
	return items, false
}

// flexString accepts a JSON string or number (older clients sent numeric IDs).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case string:
		*f = flexString(t)
	case float64:
		*f = flexString(strconv.FormatFloat(t, 'f', -1, 64))
	case nil:
		*f = ""
	default:
		return fmt.Errorf("expected a string")
	}
	return nil
}

// flexFloat accepts a JSON number or numeric string; anything else is NaN.
type flexFloat struct {
	set bool
	v   float64
}

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	f.set = v != nil
	switch t := v.(type) {
	case float64:
		f.v = t
	case string:
		x, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			x = nan
		}
		f.v = x
	default:
		f.v = nan
	}
	return nil
}
