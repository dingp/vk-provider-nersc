package superfacility

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPrivateKeyJWTTokenSourceFetchesAndCachesToken(t *testing.T) {
	key := testRSAKey(t)
	jwk := testJWK(t, key)

	requests := 0
	tokenURL := "https://oidc.example/c2id/token"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.String() != tokenURL {
			t.Fatalf("token URL = %s, want %s", r.URL.String(), tokenURL)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("content type = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.PostForm.Get("grant_type"); got != "client_credentials" {
			t.Fatalf("grant_type = %q", got)
		}
		if got := r.PostForm.Get("client_id"); got != "client-1234567" {
			t.Fatalf("client_id = %q", got)
		}
		if got := r.PostForm.Get("client_assertion_type"); got != clientAssertionType {
			t.Fatalf("client_assertion_type = %q", got)
		}

		assertion := r.PostForm.Get("client_assertion")
		parts := strings.Split(assertion, ".")
		if len(parts) != 3 {
			t.Fatalf("assertion has %d parts, want 3", len(parts))
		}
		var claims map[string]any
		claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("decode claims: %v", err)
		}
		if err := json.Unmarshal(claimsJSON, &claims); err != nil {
			t.Fatalf("unmarshal claims: %v", err)
		}
		if claims["iss"] != "client-1234567" || claims["sub"] != "client-1234567" || claims["aud"] != tokenURL {
			t.Fatalf("unexpected claims: %+v", claims)
		}

		resp := response(http.StatusOK, `{"access_token":"access-token-1","token_type":"Bearer","expires_in":600}`)
		resp.Header.Set("Content-Type", "application/json")
		return resp, nil
	})

	source, err := NewPrivateKeyJWTTokenSource("client-1234567", jwk, tokenURL)
	if err != nil {
		t.Fatalf("NewPrivateKeyJWTTokenSource returned error: %v", err)
	}
	source.HTTP = &http.Client{Transport: transport}
	now := time.Unix(1700000000, 0)
	source.Now = func() time.Time { return now }

	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token returned error: %v", err)
	}
	if token != "access-token-1" {
		t.Fatalf("token = %q", token)
	}

	token, err = source.Token(context.Background())
	if err != nil {
		t.Fatalf("cached Token returned error: %v", err)
	}
	if token != "access-token-1" {
		t.Fatalf("cached token = %q", token)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want cached token to avoid second request", requests)
	}
	now = now.Add(10 * time.Minute)
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("expired access token was not refreshed: requests=%d", requests)
	}
}

func TestParseRSAPrivateJWKRejectsMissingFields(t *testing.T) {
	_, _, err := ParseRSAPrivateJWK([]byte(`{"kty":"RSA"}`))
	if err == nil || !strings.Contains(err.Error(), `missing`) {
		t.Fatalf("error = %v, want missing field error", err)
	}
}

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

func testJWK(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	encode := func(v *big.Int) string {
		return base64.RawURLEncoding.EncodeToString(v.Bytes())
	}
	e := big.NewInt(int64(key.PublicKey.E))
	jwk := map[string]string{
		"kty": "RSA",
		"n":   encode(key.PublicKey.N),
		"e":   encode(e),
		"d":   encode(key.D),
		"p":   encode(key.Primes[0]),
		"q":   encode(key.Primes[1]),
	}
	data, err := json.Marshal(jwk)
	if err != nil {
		t.Fatalf("marshal JWK: %v", err)
	}
	return data
}
