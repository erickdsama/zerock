// Package store persists zerock's credentials and subdomain reservations in an
// embedded bbolt database. The data model is small and key-addressed, so a
// full SQL engine would only add build weight; management happens through the
// admin API rather than by hand-editing the file.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bktTokens       = []byte("tokens")
	bktReservations = []byte("reservations")
	bktMeta         = []byte("meta")
)

// Errors returned by the store.
var (
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("already exists")
	ErrBadRequest = errors.New("invalid request")
)

// Store is the persistent state of a zerock server.
type Store struct {
	db *bolt.DB
}

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bktTokens, bktReservations, bktMeta} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the database file.
func (s *Store) Close() error { return s.db.Close() }

// --- tokens ---

// CreateTokenOpts describes a token to mint.
type CreateTokenOpts struct {
	Label           string
	Scopes          []string
	TTL             time.Duration
	MaxTunnels      int
	MaxReservations int
}

// CreateToken mints a token and returns the record along with the only copy of
// the presentable secret.
func (s *Store) CreateToken(opts CreateTokenOpts) (*Token, string, error) {
	label := strings.TrimSpace(opts.Label)
	if label == "" {
		return nil, "", fmt.Errorf("%w: label is required", ErrBadRequest)
	}
	scopes := normalizeScopes(opts.Scopes)
	if len(scopes) == 0 {
		return nil, "", fmt.Errorf("%w: at least one valid scope is required", ErrBadRequest)
	}
	id, full, hash, err := NewSecret()
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	tok := &Token{
		ID:              id,
		Hash:            hash,
		Label:           label,
		Scopes:          scopes,
		CreatedAt:       now,
		MaxTunnels:      opts.MaxTunnels,
		MaxReservations: opts.MaxReservations,
	}
	if opts.TTL > 0 {
		exp := now.Add(opts.TTL)
		tok.ExpiresAt = &exp
	}
	if err := s.putToken(tok); err != nil {
		return nil, "", err
	}
	return tok, full, nil
}

func normalizeScopes(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != ScopeTunnel && s != ScopeAdmin {
			continue
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Store) putToken(tok *Token) error {
	b, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktTokens).Put([]byte(tok.ID), b)
	})
}

// GetToken loads a token by id.
func (s *Store) GetToken(id string) (*Token, error) {
	var tok Token
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bktTokens).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &tok)
	})
	if err != nil {
		return nil, err
	}
	return &tok, nil
}

// Authenticate validates a presented token string. It returns ErrNotFound for
// every failure mode that could reveal whether an id exists, so callers can
// answer "unauthorized" without leaking which half was wrong.
func (s *Store) Authenticate(full string) (*Token, error) {
	id, secret, err := ParseToken(full)
	if err != nil {
		return nil, ErrNotFound
	}
	tok, err := s.GetToken(id)
	if err != nil {
		return nil, ErrNotFound
	}
	if !tok.matches(secret) {
		return nil, ErrNotFound
	}
	if !tok.Active(time.Now().UTC()) {
		return nil, ErrNotFound
	}
	return tok, nil
}

// TouchToken records that a token was just used. Failures are not fatal to the
// caller's request, so the error is advisory.
func (s *Store) TouchToken(id string) error {
	tok, err := s.GetToken(id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tok.LastUsed = &now
	return s.putToken(tok)
}

// ListTokens returns every token, newest first.
func (s *Store) ListTokens() ([]*Token, error) {
	var out []*Token
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bktTokens).ForEach(func(_, raw []byte) error {
			var tok Token
			if err := json.Unmarshal(raw, &tok); err != nil {
				return err
			}
			out = append(out, &tok)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// RevokeToken marks a token unusable. It is idempotent.
func (s *Store) RevokeToken(id string) error {
	tok, err := s.GetToken(id)
	if err != nil {
		return err
	}
	if tok.RevokedAt != nil {
		return nil
	}
	now := time.Now().UTC()
	tok.RevokedAt = &now
	return s.putToken(tok)
}

// DeleteToken removes a token and releases every subdomain it reserved.
func (s *Store) DeleteToken(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		tokens := tx.Bucket(bktTokens)
		if tokens.Get([]byte(id)) == nil {
			return ErrNotFound
		}
		if err := tokens.Delete([]byte(id)); err != nil {
			return err
		}
		res := tx.Bucket(bktReservations)
		var orphans [][]byte
		err := res.ForEach(func(k, raw []byte) error {
			var r Reservation
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			if r.TokenID == id {
				orphans = append(orphans, append([]byte(nil), k...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range orphans {
			if err := res.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// CountAdmins reports how many usable admin tokens exist, which the server uses
// to decide whether it must bootstrap one on startup.
func (s *Store) CountAdmins() (int, error) {
	tokens, err := s.ListTokens()
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	n := 0
	for _, t := range tokens {
		if t.Active(now) && t.HasScope(ScopeAdmin) {
			n++
		}
	}
	return n, nil
}

// --- reservations ---

// Reservation binds a subdomain to a token so nobody else can claim it.
type Reservation struct {
	Subdomain string    `json:"sub"`
	TokenID   string    `json:"token_id"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Reserve binds sub to tokenID. It returns ErrConflict if another token holds
// it, and is a no-op when the same token re-reserves.
func (s *Store) Reserve(sub, tokenID, note string, max int) (*Reservation, error) {
	sub = strings.ToLower(strings.TrimSpace(sub))
	r := &Reservation{Subdomain: sub, TokenID: tokenID, Note: note, CreatedAt: time.Now().UTC()}
	err := s.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket(bktReservations)
		if raw := bkt.Get([]byte(sub)); raw != nil {
			var existing Reservation
			if err := json.Unmarshal(raw, &existing); err != nil {
				return err
			}
			if existing.TokenID != tokenID {
				return fmt.Errorf("%w: %s is reserved by another token", ErrConflict, sub)
			}
			r = &existing
			return nil
		}
		if max > 0 {
			held := 0
			if err := bkt.ForEach(func(_, raw []byte) error {
				var existing Reservation
				if err := json.Unmarshal(raw, &existing); err != nil {
					return err
				}
				if existing.TokenID == tokenID {
					held++
				}
				return nil
			}); err != nil {
				return err
			}
			if held >= max {
				return fmt.Errorf("%w: reservation limit of %d reached", ErrBadRequest, max)
			}
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return bkt.Put([]byte(sub), b)
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Reservation looks up who holds sub. A nil result with a nil error means the
// subdomain is unreserved.
func (s *Store) Reservation(sub string) (*Reservation, error) {
	sub = strings.ToLower(strings.TrimSpace(sub))
	var r *Reservation
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bktReservations).Get([]byte(sub))
		if raw == nil {
			return nil
		}
		var found Reservation
		if err := json.Unmarshal(raw, &found); err != nil {
			return err
		}
		r = &found
		return nil
	})
	return r, err
}

// ListReservations returns reservations sorted by subdomain. When tokenID is
// non-empty only that token's reservations are returned.
func (s *Store) ListReservations(tokenID string) ([]*Reservation, error) {
	var out []*Reservation
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bktReservations).ForEach(func(_, raw []byte) error {
			var r Reservation
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			if tokenID == "" || r.TokenID == tokenID {
				out = append(out, &r)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subdomain < out[j].Subdomain })
	return out, nil
}

// Release drops a reservation.
func (s *Store) Release(sub string) error {
	sub = strings.ToLower(strings.TrimSpace(sub))
	return s.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket(bktReservations)
		if bkt.Get([]byte(sub)) == nil {
			return ErrNotFound
		}
		return bkt.Delete([]byte(sub))
	})
}
