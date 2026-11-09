package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestTokenContract(t *testing.T) {
	const key = "test-only-signing-key-not-a-live-secret"
	svc := NewTokenService(key, "test-issuer")
	valid, err := svc.Issue(uuid.New(), []string{"ta"}, uuid.New(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Parse(valid); err != nil {
		t.Fatalf("issued token rejected: %v", err)
	}
	for _, tc := range []struct {
		name    string
		method  jwt.SigningMethod
		issuer  string
		expires *jwt.NumericDate
	}{
		{"wrong algorithm", jwt.SigningMethodHS384, "test-issuer", jwt.NewNumericDate(time.Now().Add(time.Hour))},
		{"wrong issuer", jwt.SigningMethodHS256, "other-issuer", jwt.NewNumericDate(time.Now().Add(time.Hour))},
		{"missing expiration", jwt.SigningMethodHS256, "test-issuer", nil},
		{"expired", jwt.SigningMethodHS256, "test-issuer", jwt.NewNumericDate(time.Now().Add(-time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := jwt.NewWithClaims(tc.method, Claims{RegisteredClaims: jwt.RegisteredClaims{
				Issuer: tc.issuer, ExpiresAt: tc.expires,
			}}).SignedString([]byte(key))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Parse(raw); err == nil {
				t.Fatal("invalid token contract accepted")
			}
		})
	}
}
