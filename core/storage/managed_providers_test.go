package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedProviderEncryptedPersistenceAndReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posthorn.db")
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	original := ManagedProvider{Name: "resend-1", Type: "resend", Sender: "sender@example.org", Settings: map[string]string{"api_key": "sensitive-token-456"}, DailyLimit: 100, MonthlyLimit: 3000, Enabled: true}
	if err := s.SaveManagedProvider(original); err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err := s.db.QueryRow(`SELECT settings FROM managed_providers WHERE name=?`, original.Name).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("sensitive-token-456")) {
		t.Fatal("plaintext credential in database")
	}
	stat, err := os.Stat(filepath.Join(dir, "provider-secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("key mode = %o", stat.Mode().Perm())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.ListManagedProviders()
	if err != nil || len(list) != 1 {
		t.Fatalf("list len %d err %v", len(list), err)
	}
	if list[0].Settings["api_key"] != "sensitive-token-456" || list[0].Sender != original.Sender || list[0].DailyLimit != 100 || !list[0].Enabled {
		t.Fatal("reopened record did not match")
	}
	original.Settings["api_key"] = "replacement-token"
	original.Enabled = false
	original.DailyLimit = 42
	if err := s.SaveManagedProvider(original); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListManagedProviders()
	if err != nil || len(list) != 1 || list[0].Settings["api_key"] != "replacement-token" || list[0].Enabled || list[0].DailyLimit != 42 {
		t.Fatal("replacement did not persist")
	}
}

func TestManagedProviderMissingOrMismatchedKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Config{Path: filepath.Join(dir, "db")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := ManagedProvider{Name: "p", Type: "resend", Sender: "sender@example.org", Settings: map[string]string{"api_key": "secret"}, Enabled: true}
	if err := s.SaveManagedProvider(p); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "provider-secrets.key")
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListManagedProviders(); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("missing key error: %v", err)
	}
	if err := s.SaveManagedProvider(p); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing key allowed rewrite: %v", err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("missing key regenerated: %v", err)
	}
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{1}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListManagedProviders(); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("wrong key accepted: %v", err)
	}
	if err := s.SaveManagedProvider(p); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("wrong key allowed overwriting encrypted credentials: %v", err)
	}
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListManagedProviders(); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("unsafe key permissions accepted: %v", err)
	}
}

func TestManagedProviderCiphertextBoundToName(t *testing.T) {
	s, err := Open(Config{InMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveManagedProvider(ManagedProvider{Name: "a", Type: "resend", Sender: "s@example.org", Settings: map[string]string{"api_key": "secret"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE managed_providers SET name='b' WHERE name='a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListManagedProviders(); err == nil {
		t.Fatal("altered record accepted")
	}
}
