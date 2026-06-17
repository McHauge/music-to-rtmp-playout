package services

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// Sha256Hex matches the client-side hashing the browser applies before sending
// a password, so server-initiated account creation (admin bootstrap) stores a
// comparable hash.
func Sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// AuthService manages the (small) set of operator accounts. The UI hashes the
// password with SHA-256 client-side before transmission; we bcrypt that here,
// mirroring the reference project's defense-in-depth pattern.
type AuthService struct {
	db *sql.DB
}

func NewAuthService(db *sql.DB) *AuthService { return &AuthService{db: db} }

// HasUsers reports whether any account exists. When false the UI shows a
// first-run setup screen instead of a login form.
func (s *AuthService) HasUsers() (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// CreateUser inserts a new account. clientHash is the SHA-256 hex the browser
// sends; it is bcrypt-hashed for storage.
func (s *AuthService) CreateUser(username, clientHash string) error {
	if username == "" || clientHash == "" {
		return errors.New("username and password are required")
	}
	bh, err := bcrypt.GenerateFromPassword([]byte(clientHash), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO users (username, password_hash) VALUES (?, ?)`, username, string(bh))
	return err
}

// EnsureAdmin creates the configured admin account on boot if no users exist.
// password is the plaintext from env; it is treated as the client hash input
// for simplicity (bootstrap path only).
func (s *AuthService) EnsureAdmin(username, password string) error {
	if username == "" || password == "" {
		return nil
	}
	has, err := s.HasUsers()
	if err != nil || has {
		return err
	}
	return s.CreateUser(username, Sha256Hex(password))
}

// Authenticate verifies a username + client hash. Returns the user id on success.
func (s *AuthService) Authenticate(username, clientHash string) (int64, error) {
	var id int64
	var stored string
	err := s.db.QueryRow(`SELECT id, password_hash FROM users WHERE username = ?`, username).Scan(&id, &stored)
	if err == sql.ErrNoRows {
		return 0, errors.New("invalid credentials")
	}
	if err != nil {
		return 0, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(clientHash)); err != nil {
		return 0, errors.New("invalid credentials")
	}
	return id, nil
}

// UserExists reports whether a user id is still valid (used by session checks).
func (s *AuthService) UserExists(id int64) bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, id).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
