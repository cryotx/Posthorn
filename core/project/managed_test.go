package project

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"

	"github.com/craigmccaskill/posthorn/storage"
)

func TestManagedProjectLifecycle(t *testing.T) {
	store, err := storage.Open(storage.Config{Path: t.TempDir() + "/mail.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry, err := Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(registry, store, "sender@example.com")
	if err != nil {
		t.Fatal(err)
	}
	key, err := manager.Create("app-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Authenticate(key); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ManagedProjects()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(key))
	if len(rows) != 1 || !bytes.Equal(rows[0].KeyDigest, digest[:]) {
		t.Fatal("credential must be stored as a digest")
	}
	if _, err := manager.Create("app-one"); !errors.Is(err, ErrProjectExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := manager.Create("invalid/name"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("invalid: %v", err)
	}
	newKey, err := manager.Rotate("app-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Authenticate(key); err == nil {
		t.Fatal("old key remains valid")
	}
	if _, err := registry.Authenticate(newKey); err != nil {
		t.Fatal(err)
	}
	restarted, _ := Build(nil)
	restored, err := NewManager(restarted, store, "sender@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.Restore(); err != nil {
		t.Fatal(err)
	}
	if !restored.IsManaged("app-one") {
		t.Fatal("project not restored")
	}
	p, err := restarted.Authenticate(newKey)
	if err != nil {
		t.Fatal(err)
	}
	if from, err := p.ResolveFrom("", ""); err != nil || from != "sender@example.com" {
		t.Fatalf("sender %q: %v", from, err)
	}
	if _, err := p.ResolveFrom("other@example.com", ""); err == nil {
		t.Fatal("managed project accepted another mailbox on the same domain")
	}
	if from, err := p.ResolveFrom("sender@example.com", ""); err != nil || from != "sender@example.com" {
		t.Fatalf("explicit fixed sender %q: %v", from, err)
	}

}

func TestManagedConcurrentReadAndRotate(t *testing.T) {
	store, err := storage.Open(storage.Config{InMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r, _ := Build(nil)
	m, _ := NewManager(r, store, "sender@example.com")
	key, err := m.Create("app")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.Authenticate(key)
				r.Names()
				for _, p := range r.All() {
					p.KeyFingerprint()
					p.KeyCount()
					p.KeySetDigest()
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		if _, err := m.Rotate("app"); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
