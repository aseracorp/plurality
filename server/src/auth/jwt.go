package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

const tokenLifetime = 30 * 24 * time.Hour

type claims struct {
	jwt.RegisteredClaims
}

// IssueToken produces a signed JWT carrying the username as the subject.
func IssueToken(username string) (string, error) {
	if username == "" {
		return "", errors.New("username required")
	}
	now := time.Now()
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenLifetime)),
			Issuer:    "plurality",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	return tok.SignedString([]byte(GetConfig().JWTSecret))
}

// VerifyToken parses and validates a JWT, returning the username on success.
func VerifyToken(tokenStr string) (string, error) {
	parsed, err := jwt.ParseWithClaims(tokenStr, &claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(GetConfig().JWTSecret), nil
	})
	if err != nil {
		return "", err
	}
	c, ok := parsed.Claims.(*claims)
	if !ok || !parsed.Valid {
		return "", errors.New("invalid token")
	}
	if c.Subject == "" {
		return "", errors.New("token missing subject")
	}
	return c.Subject, nil
}
