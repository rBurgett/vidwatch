//go:build integration

// End-to-end API test against DynamoDB Local and an S3 emulator (LocalStack).
// See api/README.md for how to start them; the test is skipped without
//
//	VIDWATCH_IT_DYNAMODB=http://127.0.0.1:18000 VIDWATCH_IT_S3=http://127.0.0.1:4566 \
//	  go test -tags integration ./internal/server/
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/rBurgett/vidwatch/api/internal/auth"
	"github.com/rBurgett/vidwatch/api/internal/media"
	"github.com/rBurgett/vidwatch/api/internal/store"
)

const (
	itIssuer = "https://issuer.test"
	itClient = "test-client"
	itBucket = "vidwatch-media"
)

type itEnv struct {
	t       *testing.T
	api     *httptest.Server
	key     *rsa.PrivateKey
	s3      *s3.Client
	tokens  map[string]string
	baseURL string
}

func setup(t *testing.T, cfg Config) *itEnv {
	ddbURL, s3URL := os.Getenv("VIDWATCH_IT_DYNAMODB"), os.Getenv("VIDWATCH_IT_S3")
	if ddbURL == "" || s3URL == "" {
		t.Skip("set VIDWATCH_IT_DYNAMODB and VIDWATCH_IT_S3 to run the integration test")
	}
	ctx := context.Background()
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")))
	if err != nil {
		t.Fatal(err)
	}
	ddb := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) { o.BaseEndpoint = &ddbURL })
	s3c := s3.NewFromConfig(awsCfg, func(o *s3.Options) { o.BaseEndpoint = &s3URL; o.UsePathStyle = true })

	table := fmt.Sprintf("vidwatch-it-%d", time.Now().UnixNano())
	if _, err := ddb.CreateTable(ctx, store.TableDefinition(table)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ddb.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &table}) })
	if _, err := s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(itBucket)}); err != nil &&
		!strings.Contains(err.Error(), "BucketAlready") {
		t.Fatal(err)
	}

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(jwks.Close)

	cfg.ClientID = itClient
	mediaBase := strings.TrimRight(s3URL, "/") + "/" + itBucket
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	srv := New(cfg, store.New(ddb, table), media.New(s3c, itBucket, mediaBase), auth.NewVerifier(itIssuer, jwks.URL, itClient), log)
	api := httptest.NewServer(srv.Handler())
	t.Cleanup(api.Close)
	return &itEnv{t: t, api: api, key: key, s3: s3c, tokens: map[string]string{}, baseURL: mediaBase}
}

// token mints an access token for a user, the way Cognito would.
func (e *itEnv) token(username string) string {
	if tok, ok := e.tokens[username]; ok {
		return tok
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": itIssuer, "sub": "sub-" + username, "username": username, "client_id": itClient,
		"token_use": "access", "exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(e.key)
	if err != nil {
		e.t.Fatal(err)
	}
	e.tokens[username] = s
	return s
}

type resp struct {
	status int
	body   map[string]any
	header http.Header
}

// call makes an API request as user ("" = anonymous) and decodes the JSON body.
func (e *itEnv) call(user, method, path string, body any) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.api.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+e.token(user))
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{status: res.StatusCode, header: res.Header}
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 && strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			e.t.Fatalf("%s %s: bad JSON %q", method, path, raw)
		}
	}
	return out
}

// must is call() that fails the test unless the status matches.
func (e *itEnv) must(status int, user, method, path string, body any) map[string]any {
	e.t.Helper()
	r := e.call(user, method, path, body)
	if r.status != status {
		e.t.Fatalf("%s %s as %q: status %d, want %d (body %v)", method, path, user, r.status, status, r.body)
	}
	return r.body
}

// upload presigns and performs a browser-style POST upload, returning the key.
func (e *itEnv) upload(user, kind, contentType string, data []byte) string {
	e.t.Helper()
	p := e.must(200, user, "POST", "/api/uploads", map[string]any{"kind": kind, "contentType": contentType, "size": len(data)})
	status, msg := postForm(e.t, p["url"].(string), p["fields"].(map[string]any), contentType, data)
	if status/100 != 2 {
		e.t.Fatalf("S3 upload failed: %d %s", status, msg)
	}
	return p["key"].(string)
}

func postForm(t *testing.T, url string, fields map[string]any, contentType string, data []byte) (int, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v.(string))
	}
	fw, _ := mw.CreateFormFile("file", "upload.bin")
	fw.Write(data)
	mw.Close()
	res, err := http.Post(url, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func list(m map[string]any, key string) []any {
	v, _ := m[key].([]any)
	return v
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func videoIDs(m map[string]any, key string) []string {
	var out []string
	for _, v := range list(m, key) {
		out = append(out, obj(v)["id"].(string))
	}
	return out
}

func has(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func TestAPIEndToEnd(t *testing.T) {
	e := setup(t, Config{CognitoDomain: "https://auth.test", MaxUploadMB: 1})
	videoBytes := bytes.Repeat([]byte("v"), 4096)
	jpeg := append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte("j"), 512)...)

	// config + first-login profile creation
	cfg := e.must(200, "", "GET", "/api/config", nil)
	if cfg["clientId"] != itClient || cfg["maxUploadMB"].(float64) != 1 {
		t.Fatalf("config = %v", cfg)
	}
	e.must(401, "", "GET", "/api/auth/me", nil)
	me := obj(e.must(200, "alice", "GET", "/api/auth/me", nil)["user"])
	if me["username"] != "alice" || me["id"] != "sub-alice" || me["playbackSpeed"].(float64) != 1 {
		t.Fatalf("me = %v", me)
	}
	e.must(200, "bob", "GET", "/api/auth/me", nil)

	// upload validation
	if r := e.call("alice", "POST", "/api/uploads", map[string]any{"kind": "video", "contentType": "text/html", "size": 10}); r.status != 400 {
		t.Fatalf("html upload allowed: %d", r.status)
	}
	if r := e.call("alice", "POST", "/api/uploads", map[string]any{"kind": "avatar", "contentType": "image/svg+xml", "size": 10}); r.status != 400 {
		t.Fatalf("svg upload allowed: %d", r.status)
	}
	if r := e.call("alice", "POST", "/api/uploads", map[string]any{"kind": "video", "contentType": "video/mp4", "size": 2 << 20}); r.status != 400 {
		t.Fatalf("oversized upload allowed: %d", r.status)
	}
	// S3 itself enforces the signed content type and size.
	p := e.must(200, "alice", "POST", "/api/uploads", map[string]any{"kind": "thumbnail", "contentType": "image/png", "size": 10})
	fields := p["fields"].(map[string]any)
	fields["Content-Type"] = "text/html"
	if status, _ := postForm(t, p["url"].(string), fields, "text/html", []byte("<script>")); status/100 == 2 {
		t.Errorf("S3 accepted an upload with a different Content-Type than signed")
	}
	p = e.must(200, "alice", "POST", "/api/uploads", map[string]any{"kind": "video", "contentType": "video/mp4", "size": 10})
	if status, _ := postForm(t, p["url"].(string), p["fields"].(map[string]any), "video/mp4", bytes.Repeat([]byte("x"), 2<<20)); status/100 == 2 {
		t.Errorf("S3 accepted an upload larger than the signed limit")
	}

	// create a video
	vkey := e.upload("alice", "video", "video/mp4", videoBytes)
	tkey := e.upload("alice", "thumbnail", "image/jpeg", jpeg)
	if !strings.HasPrefix(vkey, "videos/sub-alice/") || !strings.HasSuffix(vkey, ".mp4") {
		t.Fatalf("video key %q", vkey)
	}
	if r := e.call("bob", "POST", "/api/videos", map[string]any{"title": "Stolen", "videoKey": vkey}); r.status != 400 {
		t.Fatalf("bob used alice's upload: %d", r.status)
	}
	e.must(400, "alice", "POST", "/api/videos", map[string]any{"title": " ", "videoKey": vkey})
	if _, err := e.s3.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String(itBucket), Key: &vkey}); err == nil {
		t.Fatal("upload not discarded after a failed create")
	}
	vkey = e.upload("alice", "video", "video/mp4", videoBytes)
	created := obj(e.must(200, "alice", "POST", "/api/videos", map[string]any{
		"title": "Cats on Parade", "description": "A parade at 1:23", "ageRating": "PG",
		"visibility": "public", "durationSeconds": 75.5, "videoKey": vkey, "thumbnailKey": tkey,
	})["video"])
	vid := created["id"].(string)
	if len(vid) != 11 || created["videoUrl"] != e.baseURL+"/"+vkey || created["thumbnailUrl"] != e.baseURL+"/"+tkey ||
		obj(created["author"])["username"] != "alice" || created["durationSeconds"].(float64) != 75.5 {
		t.Fatalf("created = %v", created)
	}
	// the media URL really serves the file
	if res, err := http.Get(created["videoUrl"].(string)); err != nil || res.StatusCode != 200 || res.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("media GET: %v %v", err, res)
	}

	// feed, search, sort
	if ids := videoIDs(e.must(200, "", "GET", "/api/videos", nil), "videos"); !has(ids, vid) {
		t.Fatalf("feed missing video: %v", ids)
	}
	if ids := videoIDs(e.must(200, "", "GET", "/api/videos?q=PARADE", nil), "videos"); !has(ids, vid) {
		t.Fatal("search missed")
	}
	if ids := videoIDs(e.must(200, "", "GET", "/api/videos?q=nothing-matches", nil), "videos"); len(ids) != 0 {
		t.Fatal("search matched nonsense")
	}
	if ids := videoIDs(e.must(200, "", "GET", "/api/videos?maxRating=G", nil), "videos"); has(ids, vid) {
		t.Fatal("kids mode showed a PG video")
	}

	// unique views
	v := obj(e.must(200, "bob", "GET", "/api/videos/"+vid, nil)["video"])
	if v["views"].(float64) != 1 {
		t.Fatalf("views after bob = %v", v["views"])
	}
	e.must(200, "bob", "GET", "/api/videos/"+vid, nil)
	e.must(200, "alice", "GET", "/api/videos/"+vid, nil) // owner doesn't count
	v = obj(e.must(200, "", "GET", "/api/videos/"+vid, nil)["video"])
	if v["views"].(float64) != 2 {
		t.Fatalf("views after anon = %v", v["views"])
	}
	e.must(404, "", "GET", "/api/videos/doesnotexist", nil)

	// likes
	if l := e.must(200, "bob", "POST", "/api/videos/"+vid+"/like", nil); l["liked"] != true || l["likes"].(float64) != 1 {
		t.Fatalf("like = %v", l)
	}
	if l := e.must(200, "bob", "POST", "/api/videos/"+vid+"/like", nil); l["liked"] != false || l["likes"].(float64) != 0 {
		t.Fatalf("unlike = %v", l)
	}
	e.must(200, "bob", "POST", "/api/videos/"+vid+"/like", nil)
	if ids := videoIDs(e.must(200, "bob", "GET", "/api/users/bob/liked", nil), "videos"); !has(ids, vid) {
		t.Fatal("liked list missing video")
	}
	e.must(403, "alice", "GET", "/api/users/bob/liked", nil)

	// comments
	c1 := obj(e.must(200, "bob", "POST", "/api/videos/"+vid+"/comments", map[string]any{"body": "first!"})["comment"])
	c1id := c1["id"].(string)
	reply := obj(e.must(200, "alice", "POST", "/api/videos/"+vid+"/comments", map[string]any{"body": "thanks", "parentId": c1id})["comment"])
	e.must(400, "bob", "POST", "/api/videos/"+vid+"/comments", map[string]any{"body": "nested", "parentId": reply["id"]})
	e.must(400, "bob", "POST", "/api/videos/"+vid+"/comments", map[string]any{"body": "   "})
	e.must(403, "alice", "PATCH", "/api/videos/"+vid+"/comments/"+c1id, map[string]any{"body": "hijack"})
	if ed := obj(e.must(200, "bob", "PATCH", "/api/videos/"+vid+"/comments/"+c1id, map[string]any{"body": "first! (edited)"})["comment"]); ed["editedAt"] == nil {
		t.Fatal("editedAt not set")
	}
	cs := e.must(200, "", "GET", "/api/videos/"+vid+"/comments?sort=old", nil)
	if n := len(list(cs, "comments")); n != 2 || obj(list(cs, "comments")[1])["parentId"] != c1id {
		t.Fatalf("comments = %v", cs)
	}
	e.must(200, "alice", "DELETE", "/api/videos/"+vid+"/comments/"+c1id, nil) // owner moderates
	if n := len(list(e.must(200, "", "GET", "/api/videos/"+vid+"/comments", nil), "comments")); n != 0 {
		t.Fatalf("replies not deleted with parent: %d left", n)
	}

	// reports are visible to the owner only
	if r := e.must(200, "bob", "POST", "/api/videos/"+vid+"/report", map[string]any{"reason": "spam"}); r["alreadyReported"] != false {
		t.Fatal("first report flagged as duplicate")
	}
	if r := e.must(200, "bob", "POST", "/api/videos/"+vid+"/report", map[string]any{"reason": "spam"}); r["alreadyReported"] != true {
		t.Fatal("duplicate report not flagged")
	}
	if reps := list(e.must(200, "alice", "GET", "/api/videos/"+vid, nil), "reports"); len(reps) != 1 || obj(reps[0])["reporter"] != "bob" {
		t.Fatalf("owner reports = %v", reps)
	}
	if reps := list(e.must(200, "bob", "GET", "/api/videos/"+vid, nil), "reports"); len(reps) != 0 {
		t.Fatal("non-owner sees reports")
	}

	// subscriptions + notifications
	e.must(400, "alice", "POST", "/api/users/alice/subscribe", nil)
	if s := e.must(200, "bob", "POST", "/api/users/ALICE/subscribe", nil); s["subscribed"] != true || s["subscribers"].(float64) != 1 {
		t.Fatalf("subscribe = %v", s)
	}
	if n := e.must(200, "bob", "GET", "/api/me/notifications", nil); n["unread"].(float64) != 0 {
		t.Fatal("old upload became a notification")
	}
	time.Sleep(5 * time.Millisecond)
	v2key := e.upload("alice", "video", "video/webm", videoBytes)
	vid2 := obj(e.must(200, "alice", "POST", "/api/videos", map[string]any{"title": "Second", "videoKey": v2key, "visibility": "public"})["video"])["id"].(string)
	v3key := e.upload("alice", "video", "video/mp4", videoBytes)
	vid3 := obj(e.must(200, "alice", "POST", "/api/videos", map[string]any{"title": "Secret", "videoKey": v3key, "visibility": "unlisted"})["video"])["id"].(string)
	n := e.must(200, "bob", "GET", "/api/me/notifications", nil)
	if n["unread"].(float64) != 1 || obj(list(n, "items")[0])["video"].(map[string]any)["id"] != vid2 {
		t.Fatalf("notifications = %v", n)
	}
	e.must(200, "bob", "POST", "/api/me/notifications/seen", nil)
	if n := e.must(200, "bob", "GET", "/api/me/notifications", nil); n["unread"].(float64) != 0 {
		t.Fatal("seen didn't clear unread")
	}
	if ids := videoIDs(e.must(200, "bob", "GET", "/api/videos?following=1", nil), "videos"); !has(ids, vid) || !has(ids, vid2) {
		t.Fatalf("following feed = %v", ids)
	}

	// unlisted: not in feeds or anonymous profile, but on the owner's own profile and by link
	if ids := videoIDs(e.must(200, "", "GET", "/api/videos", nil), "videos"); has(ids, vid3) {
		t.Fatal("unlisted video in feed")
	}
	if ids := videoIDs(e.must(200, "", "GET", "/api/users/alice", nil), "videos"); has(ids, vid3) || !has(ids, vid) {
		t.Fatalf("public profile = %v", ids)
	}
	prof := e.must(200, "alice", "GET", "/api/users/alice", nil)
	if ids := videoIDs(prof, "videos"); !has(ids, vid3) || obj(prof["user"])["isSelf"] != true || obj(prof["user"])["subscribers"].(float64) != 1 {
		t.Fatalf("own profile = %v", prof)
	}
	e.must(200, "", "GET", "/api/videos/"+vid3, nil)
	updated := obj(e.must(200, "alice", "PATCH", "/api/videos/"+vid3, map[string]any{"visibility": "public", "title": "Not secret"})["video"])
	if updated["title"] != "Not secret" || updated["visibility"] != "public" {
		t.Fatalf("patch = %v", updated)
	}
	if ids := videoIDs(e.must(200, "", "GET", "/api/videos", nil), "videos"); !has(ids, vid3) {
		t.Fatal("re-published video missing from feed")
	}
	e.must(403, "bob", "PATCH", "/api/videos/"+vid3, map[string]any{"title": "mine now"})
	e.must(403, "bob", "DELETE", "/api/videos/"+vid3, nil)

	// trending: vid has 2 recent views, the others none
	if ids := videoIDs(e.must(200, "", "GET", "/api/trending", nil), "videos"); len(ids) != 3 || ids[0] != vid {
		t.Fatalf("trending = %v", ids)
	}
	if ids := videoIDs(e.must(200, "", "GET", "/api/videos?sort=views", nil), "videos"); ids[0] != vid {
		t.Fatalf("sort=views = %v", ids)
	}

	// history + progress
	e.must(200, "bob", "POST", "/api/videos/"+vid+"/progress", map[string]any{"position": 30.5})
	e.must(200, "bob", "POST", "/api/videos/"+vid+"/progress", map[string]any{"position": 9999}) // clamped to duration
	h := e.must(200, "bob", "GET", "/api/me/history", nil)
	if items := list(h, "items"); len(items) != 1 || obj(items[0])["position"].(float64) != 75.5 {
		t.Fatalf("history = %v", h)
	}
	if rp := e.must(200, "bob", "GET", "/api/videos/"+vid, nil)["resumePosition"]; rp.(float64) != 75.5 {
		t.Fatalf("resumePosition = %v", rp)
	}

	// watch later
	if r := e.must(200, "bob", "POST", "/api/me/watch-later", map[string]any{"videoId": vid}); r["added"] != true {
		t.Fatal("watch later add")
	}
	if r := e.must(200, "bob", "POST", "/api/me/watch-later", map[string]any{"videoId": vid}); r["added"] != false {
		t.Fatal("watch later duplicate")
	}
	e.must(200, "bob", "POST", "/api/me/watch-later", map[string]any{"videoId": vid2})
	if ids := videoIDs(e.must(200, "bob", "GET", "/api/me/watch-later", nil), "videos"); len(ids) != 2 || ids[0] != vid2 {
		t.Fatalf("watch later = %v", ids)
	}
	e.must(200, "bob", "DELETE", "/api/me/watch-later/"+vid2, nil)
	if st := e.must(200, "bob", "GET", "/api/videos/"+vid, nil); st["savedForLater"] != true || st["liked"] != true || st["subscribed"] != true {
		t.Fatalf("viewer state = %v", st)
	}

	// playlists
	pl := obj(e.must(200, "bob", "POST", "/api/playlists", map[string]any{"title": "Faves"})["playlist"])
	pid := pl["id"].(string)
	e.must(200, "bob", "POST", "/api/playlists/"+pid+"/items", map[string]any{"videoId": vid2})
	if r := e.must(200, "bob", "POST", "/api/playlists/"+pid+"/items", map[string]any{"videoId": vid}); r["count"].(float64) != 2 {
		t.Fatalf("playlist count = %v", r)
	}
	e.must(403, "alice", "POST", "/api/playlists/"+pid+"/items", map[string]any{"videoId": vid})
	pls := list(e.must(200, "bob", "GET", "/api/playlists?videoId="+vid, nil), "playlists")
	if len(pls) != 1 || obj(pls[0])["contains"] != true || obj(pls[0])["count"].(float64) != 2 {
		t.Fatalf("playlists = %v", pls)
	}
	got := e.must(200, "", "GET", "/api/playlists/"+pid, nil)
	if ids := videoIDs(got, "items"); len(ids) != 2 || ids[0] != vid2 || ids[1] != vid || obj(got["playlist"])["isOwner"] != false {
		t.Fatalf("playlist = %v", got)
	}

	// profile edits, including an avatar upload
	akey := e.upload("bob", "avatar", "image/jpeg", jpeg)
	u := obj(e.must(200, "bob", "PATCH", "/api/users/me", map[string]any{"bio": "  hi  ", "playbackSpeed": 9, "avatarKey": akey})["user"])
	if u["bio"] != "hi" || u["playbackSpeed"].(float64) != 3 || u["avatarUrl"] != e.baseURL+"/"+akey {
		t.Fatalf("updateMe = %v", u)
	}
	e.must(400, "bob", "PATCH", "/api/users/me", map[string]any{"avatarKey": tkey}) // alice's thumbnail

	// download redirects to a presigned URL that forces a save
	dl := e.call("", "GET", "/api/videos/"+vid+"/download", nil)
	if loc := dl.header.Get("Location"); dl.status != 302 || !strings.Contains(loc, "response-content-disposition=attachment") {
		t.Fatalf("download = %d %s", dl.status, loc)
	}

	// deleting a video cleans up everything that pointed at it
	e.must(200, "alice", "DELETE", "/api/videos/"+vid, nil)
	e.must(404, "", "GET", "/api/videos/"+vid, nil)
	if items := list(e.must(200, "bob", "GET", "/api/me/history", nil), "items"); len(items) != 0 {
		t.Fatalf("history still has %d items", len(items))
	}
	if ids := videoIDs(e.must(200, "bob", "GET", "/api/users/bob/liked", nil), "videos"); len(ids) != 0 {
		t.Fatal("liked list not cleaned up")
	}
	if ids := videoIDs(e.must(200, "", "GET", "/api/playlists/"+pid, nil), "items"); len(ids) != 1 || ids[0] != vid2 {
		t.Fatalf("playlist after delete = %v", ids)
	}
	if _, err := e.s3.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String(itBucket), Key: &vkey}); err == nil {
		t.Fatal("video file not deleted")
	}
	e.must(200, "bob", "DELETE", "/api/playlists/"+pid, nil)
	e.must(404, "", "GET", "/api/playlists/"+pid, nil)
	e.must(200, "bob", "DELETE", "/api/me/history", nil)
}

func TestOriginVerify(t *testing.T) {
	e := setup(t, Config{OriginVerifySecret: "s3cret"})
	if r := e.call("", "GET", "/api/config", nil); r.status != 403 {
		t.Fatalf("request without origin header: %d", r.status)
	}
	req, _ := http.NewRequest("GET", e.api.URL+"/api/config", nil)
	req.Header.Set("X-Origin-Verify", "s3cret")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("request with origin header: %v %v", err, res)
	}
}
