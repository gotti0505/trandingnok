package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"quantsaas/internal/saas/config"
)

// Claims carries the minimal identity payload embedded in every JWT.
type Claims struct {
	UserID uint   `json:"user_id"`
	Role   string `json:"role"` // mirrors config AppRole: saas / lab / dev
	jwt.RegisteredClaims
}

// SignToken issues a signed HS256 JWT for the given userID and role.
func SignToken(cfg *config.Config, userID uint, role string) (string, error) {
	claims := Claims{
		UserID: userID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(cfg.JWT.TTL.Duration)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(cfg.JWT.Secret))
}

// ParseToken validates the token string and returns the embedded Claims.
func ParseToken(cfg *config.Config, tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(cfg.JWT.Secret), nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}
