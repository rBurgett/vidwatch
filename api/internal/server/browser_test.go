//go:build integration

// Browser tests: same setup as integration_test.go, plus headless Chromium.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func findBrowser(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no Chromium/Chrome on PATH")
	return ""
}

// dumpDOM loads a page in headless Chromium, lets its scripts run, and
// returns the resulting DOM.
func dumpDOM(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// A fresh profile per run, so no storage leaks between tests.
	out, err := exec.CommandContext(ctx, findBrowser(t), "--headless=new", "--disable-gpu", "--no-first-run",
		"--user-data-dir="+t.TempDir(), "--virtual-time-budget=8000", "--dump-dom", url).Output()
	if err != nil {
		t.Logf("browser error: %v", err)
	}
	return string(out)
}

// browse opens url in headless Chromium and watches the tab (through the
// DevTools HTTP endpoint) until done(url, title) is true or time runs out.
// It returns the tab's last URL and title.
func browse(t *testing.T, url string, done func(url, title string) bool) (string, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, findBrowser(t), "--headless=new", "--disable-gpu", "--no-first-run",
		"--user-data-dir="+t.TempDir(), fmt.Sprintf("--remote-debugging-port=%d", port), url)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); cmd.Wait() }()

	var lastURL, lastTitle string
	for ctx.Err() == nil {
		time.Sleep(200 * time.Millisecond)
		res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
		if err != nil {
			continue
		}
		var tabs []struct{ Type, URL, Title string }
		json.NewDecoder(res.Body).Decode(&tabs)
		res.Body.Close()
		for _, tab := range tabs {
			if tab.Type == "page" {
				lastURL, lastTitle = tab.URL, tab.Title
				if done(lastURL, lastTitle) {
					return lastURL, lastTitle
				}
			}
		}
	}
	return lastURL, lastTitle
}

// TestBrowserPages renders the main pages against the real API and checks
// the seeded data shows up.
func TestBrowserPages(t *testing.T) {
	e := setup(t, Config{StaticDir: "../../../web", CognitoDomain: "https://auth.test", MaxUploadMB: 5})
	jpeg := append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte("j"), 512)...)
	vkey := e.upload("alice", "video", "video/mp4", bytes.Repeat([]byte("v"), 4096))
	tkey := e.upload("alice", "thumbnail", "image/jpeg", jpeg)
	vid := obj(e.must(200, "alice", "POST", "/api/videos", map[string]any{
		"title": "Browser Smoke Video", "description": "Chapters at 0:30", "videoKey": vkey, "thumbnailKey": tkey, "durationSeconds": 95,
	})["video"])["id"].(string)
	e.must(200, "bob", "POST", "/api/videos/"+vid+"/comments", map[string]any{"body": "Smoke comment from bob"})

	checks := []struct {
		path string
		want []string
	}{
		{"/", []string{"Browser Smoke Video", "alice", "1:35", "Sign in"}},
		{"/watch?v=" + vid, []string{"Browser Smoke Video", "Smoke comment from bob", "ts-link", e.baseURL + "/" + vkey}},
		{"/trending", []string{"Browser Smoke Video"}},
		{"/u/alice", []string{"Browser Smoke Video", "alice"}},
		{"/?q=smoke", []string{"Browser Smoke Video"}},
	}
	for _, c := range checks {
		dom := dumpDOM(t, e.api.URL+c.path)
		for _, w := range c.want {
			if !strings.Contains(dom, w) {
				t.Errorf("%s: missing %q", c.path, w)
			}
		}
		if t.Failed() {
			t.Logf("%s DOM (first 3000 chars):\n%s", c.path, dom[:min(len(dom), 3000)])
		}
	}
}

// TestBrowserLoginFlow signs in through a fake Cognito: /login -> authorize
// -> /auth/callback -> token exchange (PKCE checked) -> /api/auth/me -> next.
func TestBrowserLoginFlow(t *testing.T) {
	var e *itEnv
	var challenge, gotVerifier string
	var authorizeQuery url.Values
	cognito := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/authorize":
			authorizeQuery = r.URL.Query()
			challenge = authorizeQuery.Get("code_challenge")
			http.Redirect(w, r, authorizeQuery.Get("redirect_uri")+"?code=the-code&state="+url.QueryEscape(authorizeQuery.Get("state")), http.StatusFound)
		case "/oauth2/token":
			w.Header().Set("Access-Control-Allow-Origin", "*")
			r.ParseForm()
			gotVerifier = r.PostForm.Get("code_verifier")
			sum := sha256.Sum256([]byte(gotVerifier))
			if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "the-code" ||
				base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.PostForm.Get("client_id") != itClient {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": e.token("carol"), "id_token": "unused", "refresh_token": "refresh-1",
				"expires_in": 3600, "token_type": "Bearer",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cognito.Close()

	e = setup(t, Config{StaticDir: "../../../web", CognitoDomain: cognito.URL, MaxUploadMB: 5})
	// upload.html sends signed-out visitors back to /login, so landing on it
	// and staying there for a second means the sign-in stuck.
	var arrived time.Time
	finalURL, title := browse(t, e.api.URL+"/login?next=/upload", func(u, title string) bool {
		if u != e.api.URL+"/upload" || !strings.HasPrefix(title, "Upload") {
			arrived = time.Time{}
			return false
		}
		if arrived.IsZero() {
			arrived = time.Now()
		}
		return time.Since(arrived) > time.Second
	})
	if finalURL != e.api.URL+"/upload" {
		t.Fatalf("ended at %q (%q), want %s/upload", finalURL, title, e.api.URL)
	}

	for k, want := range map[string]string{
		"response_type": "code", "client_id": itClient, "code_challenge_method": "S256",
		"redirect_uri": e.api.URL + "/auth/callback", "scope": "openid email profile",
	} {
		if got := authorizeQuery.Get(k); got != want {
			t.Errorf("authorize %s = %q, want %q", k, got, want)
		}
	}
	if authorizeQuery.Get("state") == "" || challenge == "" || gotVerifier == "" {
		t.Errorf("missing state/challenge/verifier: %v verifier=%q", authorizeQuery, gotVerifier)
	}
	// the API saw carol and created her profile
	if u := e.must(200, "carol", "GET", "/api/auth/me", nil); obj(u["user"])["username"] != "carol" {
		t.Errorf("me = %v", u)
	}
}
