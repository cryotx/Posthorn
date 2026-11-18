package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// ManagedProvider is an operator-configured upstream. Settings are encrypted
// at rest; callers must not expose them in admin responses or logs.
type ManagedProvider struct {
	Name, Type, Sender       string
	Settings                 map[string]string
	DailyLimit, MonthlyLimit int
	Enabled                  bool
}

// SaveManagedProvider atomically inserts or replaces a managed upstream.
func (s *Store) SaveManagedProvider(p ManagedProvider) error {
	if p.Name == "" || p.Type == "" || p.Sender == "" || p.DailyLimit < 0 || p.MonthlyLimit < 0 {
		return errors.New("storage: invalid managed provider")
	}
	s.providerKeyMu.Lock()
	defer s.providerKeyMu.Unlock()
	key, err := s.managedProviderKey(true)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(p.Settings)
	if err != nil {
		return fmt.Errorf("storage: encode provider settings: %w", err)
	}
	gcm, err := providerCipher(key)
	if err != nil {
		return err
	}
	// Do not overwrite existing ciphertext using a wrong key after an
	// incomplete restore. Check the existing database before any write.
	var existingName string
	var existingCiphertext []byte
	err = s.db.QueryRow(`SELECT name, settings FROM managed_providers LIMIT 1`).Scan(&existingName, &existingCiphertext)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("storage: verify managed provider key: %w", err)
	}
	if err == nil {
		if len(existingCiphertext) < gcm.NonceSize() {
			return errors.New("storage: invalid encrypted provider settings")
		}
		if _, err := gcm.Open(nil, existingCiphertext[:gcm.NonceSize()], existingCiphertext[gcm.NonceSize():], []byte(existingName)); err != nil {
			return errors.New("storage: cannot decrypt provider settings; verify the encryption key and database backup match")
		}
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("storage: generate encryption nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, plain, []byte(p.Name))
	_, err = s.db.Exec(`INSERT INTO managed_providers (name,type,sender,settings,daily_limit,monthly_limit,enabled)
		VALUES (?,?,?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET type=excluded.type,sender=excluded.sender,
		settings=excluded.settings,daily_limit=excluded.daily_limit,monthly_limit=excluded.monthly_limit,enabled=excluded.enabled`,
		p.Name, p.Type, p.Sender, ciphertext, p.DailyLimit, p.MonthlyLimit, p.Enabled)
	if err != nil {
		return fmt.Errorf("storage: save managed provider: %w", err)
	}
	return nil
}

// ListManagedProviders decrypts settings only for trusted server-side use.
func (s *Store) ListManagedProviders() ([]ManagedProvider, error) {
	s.providerKeyMu.Lock()
	defer s.providerKeyMu.Unlock()
	rows, err := s.db.Query(`SELECT name,type,sender,settings,daily_limit,monthly_limit,enabled FROM managed_providers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("storage: list managed providers: %w", err)
	}
	type encryptedRecord struct {
		provider ManagedProvider
		data     []byte
	}
	var records []encryptedRecord
	for rows.Next() {
		var e encryptedRecord
		if err = rows.Scan(&e.provider.Name, &e.provider.Type, &e.provider.Sender, &e.data, &e.provider.DailyLimit, &e.provider.MonthlyLimit, &e.provider.Enabled); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("storage: read managed provider: %w", err)
		}
		records = append(records, e)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("storage: read managed providers: %w", err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	key, err := s.managedProviderKey(false)
	if err != nil {
		return nil, err
	}
	gcm, err := providerCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]ManagedProvider, 0, len(records))
	for _, e := range records {
		if len(e.data) < gcm.NonceSize() {
			return nil, errors.New("storage: invalid encrypted provider settings")
		}
		plain, err := gcm.Open(nil, e.data[:gcm.NonceSize()], e.data[gcm.NonceSize():], []byte(e.provider.Name))
		if err != nil {
			return nil, errors.New("storage: cannot decrypt provider settings; verify the encryption key and database backup match")
		}
		if err := json.Unmarshal(plain, &e.provider.Settings); err != nil {
			return nil, errors.New("storage: invalid decrypted provider settings")
		}
		out = append(out, e.provider)
	}
	return out, nil
}

func providerCipher(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("storage: invalid provider encryption key")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("storage: initialize provider encryption: %w", err)
	}
	return gcm, nil
}

// managedProviderKey must run under providerKeyMu. A missing key is never
// replaced when ciphertext exists: that would make recovery impossible.
func (s *Store) managedProviderKey(create bool) ([]byte, error) {
	if s.providerKeyPath == "" {
		if s.providerMemoryKey == nil && create {
			s.providerMemoryKey = make([]byte, 32)
			if _, err := io.ReadFull(rand.Reader, s.providerMemoryKey); err != nil {
				s.providerMemoryKey = nil
				return nil, fmt.Errorf("storage: generate provider key: %w", err)
			}
		}
		if s.providerMemoryKey == nil {
			return nil, errors.New("storage: provider encryption key missing")
		}
		return s.providerMemoryKey, nil
	}
	info, err := os.Lstat(s.providerKeyPath)
	if errors.Is(err, os.ErrNotExist) && create {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM managed_providers`).Scan(&count); err != nil {
			return nil, fmt.Errorf("storage: check managed providers: %w", err)
		}
		if count != 0 {
			return nil, errors.New("storage: provider encryption key missing; restore the key from backup")
		}
		key := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("storage: generate provider key: %w", err)
		}
		f, err := os.OpenFile(s.providerKeyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, fmt.Errorf("storage: create provider encryption key: %w", err)
		}
		if _, err = f.Write(key); err == nil {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(s.providerKeyPath)
			return nil, fmt.Errorf("storage: write provider encryption key: %w", err)
		}
		return key, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: provider encryption key unavailable; restore the key from backup: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("storage: provider encryption key must be a regular file with owner-only permissions")
	}
	key, err := os.ReadFile(s.providerKeyPath)
	if err != nil {
		return nil, fmt.Errorf("storage: read provider encryption key: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("storage: invalid provider encryption key length")
	}
	return key, nil
}
