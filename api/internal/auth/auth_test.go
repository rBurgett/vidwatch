package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type testIssuer struct {
	key    *rsa.PrivateKey
	kid    string
	server *httptest.Server
	hits   int
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ti := &testIssuer{key: key, kid: "test-key"}
	ti.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ti.hits++
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": ti.kid, "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(ti.server.Close)
	return ti
}

func (ti *testIssuer) token(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = ti.kid
	s, err := tok.SignedString(ti.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerify(t *testing.T) {
	ti := newTestIssuer(t)
	const issuer = "https://issuer.example"
	v := NewVerifier(issuer, ti.server.URL, "client-1")
	good := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss": issuer, "sub": "user-1", "username": "alice", "client_id": "client-1",
			"token_use": "access", "exp": time.Now().Add(time.Hour).Unix(),
		}
	}

	c, err := v.Verify(context.Background(), ti.token(t, good()))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if c.Sub != "user-1" || c.Username != "alice" {
		t.Fatalf("claims = %+v", c)
	}

	cases := map[string]func(jwt.MapClaims){
		"id token":     func(m jwt.MapClaims) { m["token_use"] = "id" },
		"other client": func(m jwt.MapClaims) { m["client_id"] = "client-2" },
		"other issuer": func(m jwt.MapClaims) { m["iss"] = "https://evil.example" },
		"expired":      func(m jwt.MapClaims) { m["exp"] = time.Now().Add(-time.Hour).Unix() },
		"no exp":       func(m jwt.MapClaims) { delete(m, "exp") },
		"no username":  func(m jwt.MapClaims) { delete(m, "username") },
	}
	for name, mutate := range cases {
		claims := good()
		mutate(claims)
		if _, err := v.Verify(context.Background(), ti.token(t, claims)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A token signed by a different key with the same kid must fail.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, good())
	forged.Header["kid"] = ti.kid
	fs, _ := forged.SignedString(other)
	if _, err := v.Verify(context.Background(), fs); err == nil {
		t.Error("forged token accepted")
	}

	// HS256 with the public key as secret (alg confusion) must fail.
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, good())
	hs.Header["kid"] = ti.kid
	hss, _ := hs.SignedString(ti.key.N.Bytes())
	if _, err := v.Verify(context.Background(), hss); err == nil {
		t.Error("HS256 token accepted")
	}

	if ti.hits != 1 {
		t.Errorf("JWKS fetched %d times, want 1 (keys should be cached)", ti.hits)
	}
}
