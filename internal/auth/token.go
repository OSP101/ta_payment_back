package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type Claims struct {
	UserID uuid.UUID `json:"uid"`
	Roles  []string  `json:"roles"`
	jwt.RegisteredClaims
}

// SessionID parses the standard "jti" claim (RegisteredClaims.ID) back into
// the sessions.id it names — see internal/service.SessionService. Every token
// issued by Issue carries one; a token from before sessions existed, or one
// hand-crafted without it, has no session row to check against and is
// treated by AccountGuard the same as a revoked one.
func (c *Claims) SessionID() (uuid.UUID, error) {
	return uuid.Parse(c.ID)
}

type TokenService struct {
	secret []byte
	issuer string
}

func NewTokenService(secret, issuer string) *TokenService {
	return &TokenService{secret: []byte(secret), issuer: issuer}
}

// Issue mints a token bound to sessionID (see internal/service.SessionService
// — the caller creates that row first, then calls Issue with its id). The
// token has no independent validity outside that row: AccountGuard checks the
// row on every request, so revoking it invalidates the token before its own
// exp is reached.
func (t *TokenService) Issue(userID uuid.UUID, roles []string, sessionID uuid.UUID, ttl time.Duration) (string, error) {
	claims := Claims{
		UserID: userID,
		Roles:  roles,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        sessionID.String(),
			Issuer:    t.issuer,
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
}

func (t *TokenService) Parse(raw string) (*Claims, error) {
	tok, err := jwt.ParseWithClaims(raw, &Claims{}, func(tk *jwt.Token) (interface{}, error) {
		if _, ok := tk.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return t.secret, nil
	})
	if err != nil {
		return nil, err
	}
	c, ok := tok.Claims.(*Claims)
	if !ok || !tok.Valid {
		return nil, errors.New("invalid token")
	}
	return c, nil
}
