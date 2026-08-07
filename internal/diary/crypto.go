package diary

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
)

// keyCheckSentinel is the plaintext encrypted and stored once per user in
// user_key_checks. Its only purpose is to let the server validate a key
// submission before any record is read or written, so a wrong key fails fast
// with a clear error rather than silently skipping encrypted rows or returning
// garbled content.
const keyCheckSentinel = "diary-key-check-v1"

// gcmNonceSize is the standard AES-GCM nonce length in bytes. Callers must
// check a stored nonce's length against this before calling decryptContent —
// cipher.AEAD.Open panics (rather than returning an error) on a wrong-length
// nonce, and content_nonce has no NOT NULL/length constraint at the schema
// level, so a corrupt or out-of-band-written row can carry a nil or
// wrong-length nonce.
const gcmNonceSize = 12

// encryptContent encrypts plaintext using AES-256-GCM with a random 12-byte
// nonce. Returns the ciphertext (which already includes the GCM authentication
// tag) and the nonce separately so both can be stored in the database.
func encryptContent(key []byte, plaintext string) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("diary: create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("diary: create GCM: %w", err)
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("diary: generate nonce: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return ciphertext, nonce, nil
}

// decryptContent decrypts a ciphertext produced by encryptContent. Returns an
// error (wrapping cipher.ErrOpen) when the key is wrong or the data has been
// tampered with.
func decryptContent(key, nonce, ciphertext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("diary: create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("diary: create GCM: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("diary: decrypt content: %w", err)
	}
	return string(plaintext), nil
}

// verifyOrInitKeyCheck validates key against a per-user sentinel stored in
// user_key_checks, creating the row on first use. This is called at the start
// of every operation that touches a user's records when encryption is enabled,
// so a wrong key fails fast before any expensive work (embedding calls, row
// scans) takes place.
//
// First call for a user: encrypts keyCheckSentinel with the provided key and
// inserts the result. Subsequent calls: decrypts the stored sentinel and checks
// that it matches — a mismatch means the wrong key was submitted and an error
// is returned.
//
// The SELECT and the first-use INSERT run inside one transaction so they hold
// the store's single pooled connection (see store.go's SetMaxOpenConns(1))
// for the whole check-then-act sequence — without that, two concurrent first
// calls for the same brand-new user_id could both observe sql.ErrNoRows
// before either INSERTs, and the second INSERT would fail on the user_id
// primary key instead of cleanly reporting a wrong key. This correctness
// argument depends on the pool staying capped at one connection.
func verifyOrInitKeyCheck(ctx context.Context, db *sql.DB, userID string, key []byte) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("diary: begin key check: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	var checkNonce, checkCipher []byte
	err = tx.QueryRowContext(ctx,
		`SELECT check_nonce, check_cipher FROM user_key_checks WHERE user_id = ?`,
		userID,
	).Scan(&checkNonce, &checkCipher)

	if errors.Is(err, sql.ErrNoRows) {
		// First use for this user: store a sentinel encrypted with the given key.
		ciphertext, nonce, err := encryptContent(key, keyCheckSentinel)
		if err != nil {
			return fmt.Errorf("diary: init key check: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO user_key_checks (user_id, check_nonce, check_cipher) VALUES (?, ?, ?)`,
			userID, nonce, ciphertext,
		)
		if err != nil {
			return fmt.Errorf("diary: store key check: %w", err)
		}
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("diary: load key check: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("diary: commit key check: %w", err)
	}

	// Validate: if decryption succeeds and the plaintext matches, the key is correct.
	// A wrong-length checkNonce (corrupt row, out-of-band write) would panic
	// gcm.Open rather than error, so it's treated the same as a wrong key.
	if len(checkNonce) != gcmNonceSize {
		return fmt.Errorf("wrong encryption key for user %q", userID)
	}
	plaintext, err := decryptContent(key, checkNonce, checkCipher)
	if err != nil || plaintext != keyCheckSentinel {
		return fmt.Errorf("wrong encryption key for user %q", userID)
	}
	return nil
}
