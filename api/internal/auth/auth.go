// Package auth verifies Cognito access tokens.
package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is what the API needs from a verified token.
type Claims struct {
	Sub      string // stable user ID
	Username string
}

// Verifier checks RS256 access tokens issued by a Cognito user pool.
type Verifier struct {
	issuer   string
	clientID string
	jwksURL  string
	client   *http.Client

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	lastFetch time.Time
}

// NewCognitoVerifier builds a verifier for the given user pool and app client.
func NewCognitoVerifier(region, userPoolID, clientID string) *Verifier {
	issuer := fmt.Sprintf("https://cognito-idp.%s.amazonaws.com/%s", region, userPoolID)
	return NewVerifier(issuer, issuer+"/.well-known/jwks.json", clientID)
}

// NewVerifier builds a verifier for any issuer/JWKS pair (used by tests).
func NewVerifier(issuer, jwksURL, clientID string) *Verifier {
	return &Verifier{
		issuer:   issuer,
		clientID: clientID,
		jwksURL:  jwksURL,
		client:   &http.Client{Timeout: 5 * time.Second},
		keys:     map[string]*rsa.PublicKey{},
	}
}

var ErrInvalidToken = errors.New("invalid token")

func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	mc, _ := parsed.Claims.(jwt.MapClaims)
	use, _ := mc["token_use"].(string)
	clientID, _ := mc["client_id"].(string)
	sub, _ := mc["sub"].(string)
	username, _ := mc["username"].(string)
	switch {
	case use != "access":
		return nil, fmt.Errorf("%w: not an access token", ErrInvalidToken)
	case clientID != v.clientID:
		return nil, fmt.Errorf("%w: wrong client", ErrInvalidToken)
	case sub == "" || username == "":
		return nil, fmt.Errorf("%w: missing sub or username", ErrInvalidToken)
	}
	return &Claims{Sub: sub, Username: username}, nil
}

// key returns the signing key for kid, refetching the JWKS when it sees a kid
// it doesn't know (keys rotate) — but at most once a minute.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	k, ok := v.keys[kid]
	recent := time.Since(v.lastFetch) < time.Minute
	v.mu.RUnlock()
	if ok {
		return k, nil
	}
	if recent {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown key id %q", kid)
}

func (v *Verifier) refresh(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Since(v.lastFetch) < time.Minute && len(v.keys) > 0 {
		return nil // another request just did it
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	res, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: status %d", res.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
	}
	v.keys = keys
	v.lastFetch = time.Now()
	return nil
}
