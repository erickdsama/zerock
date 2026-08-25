package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Scopes a token can hold.
const (
	// ScopeTunnel allows opening tunnels and managing one's own reservations.
	ScopeTunnel = "tunnel"
	// ScopeAdmin allows managing tokens, reservations and tunnels of any owner.
	ScopeAdmin = "admin"
)

// tokenAlphabet is unpadded, lowercase base32 without vowels-heavy confusion;
// standard base32 is fine and keeps tokens copy-pasteable.
var tokenAlphabet = base32.StdEncoding.WithPadding(base32.NoPadding)

// Token is a credential record. The secret itself is never stored: only its
// SHA-256. ID is embedded in the presented token so lookup is a single get
// rather than a scan.
type Token struct {
	ID        string     `json:"id"`
	Hash      string     `json:"hash"`
	Label     string     `json:"label"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	LastUsed  *time.Time `json:"last_used_at,omitempty"`

	// MaxTunnels caps concurrent tunnels for this token. Zero means unlimited.
	MaxTunnels int `json:"max_tunnels"`
	// MaxReservations caps how many subdomains this token may hold. Zero means
	// the server default applies.
	MaxReservations int `json:"max_reservations"`
}

// HasScope reports whether t carries scope s. Admin implies every scope.
func (t *Token) HasScope(s string) bool {
	for _, have := range t.Scopes {
		if have == s || have == ScopeAdmin {
			return true
		}
	}
	return false
}

// Active reports whether the token may be used right now.
func (t *Token) Active(now time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	if t.ExpiresAt != nil && now.After(*t.ExpiresAt) {
		return false
	}
	return true
}

// Status renders the token state for listings.
func (t *Token) Status(now time.Time) string {
	switch {
	case t.RevokedAt != nil:
		return "revoked"
	case t.ExpiresAt != nil && now.After(*t.ExpiresAt):
		return "expired"
	default:
		return "active"
	}
}

// ErrMalformedToken is returned when a presented string is not a zerock token.
var ErrMalformedToken = errors.New("malformed token")

// NewSecret mints an id and a full presentable token of the form
// zk_<id>_<secret>. The secret carries 160 bits of entropy; only its hash is
// persisted, so the returned string is the single chance to record it.
func NewSecret() (id, full, hash string, err error) {
	idBytes := make([]byte, 5) // 8 base32 chars
	secretBytes := make([]byte, 20)
	if _, err = rand.Read(idBytes); err != nil {
		return "", "", "", err
	}
	if _, err = rand.Read(secretBytes); err != nil {
		return "", "", "", err
	}
	id = strings.ToLower(tokenAlphabet.EncodeToString(idBytes))
	secret := strings.ToLower(tokenAlphabet.EncodeToString(secretBytes))
	full = "zk_" + id + "_" + secret
	return id, full, HashSecret(secret), nil
}

// HashSecret hashes the secret half of a token.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ParseToken splits a presented token into its id and secret halves.
func ParseToken(full string) (id, secret string, err error) {
	parts := strings.Split(strings.TrimSpace(full), "_")
	if len(parts) != 3 || parts[0] != "zk" || parts[1] == "" || parts[2] == "" {
		return "", "", ErrMalformedToken
	}
	return parts[1], parts[2], nil
}

// matches compares a presented secret against the stored hash in constant time.
func (t *Token) matches(secret string) bool {
	return subtle.ConstantTimeCompare([]byte(t.Hash), []byte(HashSecret(secret))) == 1
}
