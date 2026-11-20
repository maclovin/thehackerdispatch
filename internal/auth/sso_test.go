package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

type fakeIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]pendingCode
	claims map[string]any
}

type pendingCode struct {
	nonce, challenge string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key, codes: map[string]pendingCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.srv.URL,
			"authorization_endpoint":                idp.srv.URL + "/auth",
			"token_endpoint":                        idp.srv.URL + "/token",
			"jwks_uri":                              idp.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		pc, ok := idp.codes[r.PostFormValue("code")]
		claims := idp.claims
		idp.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != pc.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		payload := map[string]any{
			"iss": idp.srv.URL, "aud": "portal", "nonce": pc.nonce,
			"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
		}
		for k, v := range claims {
			payload[k] = v
		}
		raw, _ := json.Marshal(payload)
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		jws, _ := signer.Sign(raw)
		token, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": token})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (idp *fakeIdP) authorize(t *testing.T, authURL string, claims map[string]any) (code, state string) {
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("auth URL without PKCE: %s", authURL)
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	code = "code-" + q.Get("state")
	idp.codes[code] = pendingCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	idp.claims = claims
	return code, q.Get("state")
}

func TestSSOFlow(t *testing.T) {
	svc, pool, _ := newTestService(t)
	idp := newFakeIdP(t)
	sso := NewSSO(idp.srv.URL, "portal", "secret", "http://news.test/auth/callback")
	ctx := context.Background()

	login := func(claims map[string]any, nonce string) (Session, error) {
		t.Helper()
		verifier := oauth2.GenerateVerifier()
		authURL, err := sso.AuthCodeURL(ctx, "state-1", "nonce-1", verifier)
		if err != nil {
			t.Fatal(err)
		}
		code, state := idp.authorize(t, authURL, claims)
		if state != "state-1" {
			t.Fatalf("state = %q", state)
		}
		c, err := sso.Exchange(ctx, code, verifier, nonce)
		if err != nil {
			return Session{}, err
		}
		return svc.LoginSSO(ctx, c, "10.0.0.1", "test")
	}

	claims := map[string]any{"sub": "kc-123", "email": "Bob@Example.com", "email_verified": true, "name": "Bob Smith"}
	session, err := login(claims, "nonce-1")
	if err != nil {
		t.Fatalf("first SSO login: %v", err)
	}
	user, err := svc.Authenticate(ctx, session.Token)
	if err != nil || user.Email != "bob@example.com" || user.RealName != "Bob Smith" || user.Nickname != "" {
		t.Fatalf("user = %+v, err = %v", user, err)
	}

	if _, err := login(claims, "nonce-1"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users WHERE sso_subject = 'kc-123'").Scan(&count); err != nil || count != 1 {
		t.Errorf("%d accounts for the subject, want 1 (err=%v)", count, err)
	}

	if _, err := login(claims, "another-nonce"); err == nil {
		t.Error("a nonce mismatch must fail")
	}
}

func TestLoginSSORules(t *testing.T) {
	svc, pool, _ := newTestService(t)
	ctx := context.Background()
	admin := addAdmin(t, svc)

	tests := []struct {
		name   string
		claims Claims
		want   error
	}{
		{"unverified email", Claims{Subject: "s1", Email: "x@example.com"}, ErrEmailNotVerified},
		{"other domain", Claims{Subject: "s2", Email: "x@evil.com", EmailVerified: true}, ErrDomainNotAllowed},
		{"local account with that email", Claims{Subject: "s3", Email: "alice@example.com", EmailVerified: true}, ErrLocalAccount},
	}
	for _, tt := range tests {
		if _, err := svc.LoginSSO(ctx, tt.claims, "", ""); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}

	id, _, err := svc.CreateAccount(ctx, admin, NewAccount{Email: "carol@example.com", Role: "admin", AuthMode: "sso"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := svc.LoginSSO(ctx, Claims{Subject: "kc-carol", Email: "carol@example.com", EmailVerified: true}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	user, _ := svc.Authenticate(ctx, session.Token)
	if user.ID != id || !user.IsAdmin() {
		t.Errorf("pre-registered account not bound: %+v", user)
	}

	if _, err := pool.Exec(ctx, "UPDATE users SET banned_at = now() WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LoginSSO(ctx, Claims{Subject: "kc-carol", Email: "carol@example.com", EmailVerified: true}, "", ""); !errors.Is(err, ErrBanned) {
		t.Errorf("banned: err = %v", err)
	}
}
