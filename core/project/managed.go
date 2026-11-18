package project

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/craigmccaskill/posthorn/storage"
	"net/mail"
	"regexp"
	"strings"
)

var (
	ErrInvalidName       = errors.New("project name must contain 1–64 letters, numbers, underscores or hyphens")
	ErrProjectExists     = errors.New("project already exists")
	ErrProjectNotManaged = errors.New("project is not managed by the admin")
	namePattern          = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
)

// Manager owns credentials created in the admin interface. Plaintext is returned once.
type Manager struct {
	registry    *Registry
	store       *storage.Store
	defaultFrom string
}

func NewManager(registry *Registry, store *storage.Store, defaultFrom string) (*Manager, error) {
	if registry == nil || store == nil {
		return nil, errors.New("project: registry and storage are required")
	}
	addr, err := mail.ParseAddress(defaultFrom)
	if err != nil || !strings.Contains(addr.Address, "@") {
		return nil, errors.New("project: valid default sender is required")
	}
	return &Manager{registry, store, addr.Address}, nil
}
func managedProject(name, from string, digest [sha256.Size]byte) (*Project, error) {
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(addr.Address, "@")
	if len(parts) != 2 {
		return nil, errors.New("invalid sender")
	}
	p, err := build(Config{Name: name, Enabled: true, APIKey: strings.Repeat("x", MinAPIKeyLength), DefaultFrom: addr.Address, AllowedFromDomains: []string{parts[1]}})
	if err != nil {
		return nil, err
	}
	p.fixedFrom = true
	p.keyDigest = digest
	p.keyDigests = [][sha256.Size]byte{digest}
	return p, nil
}
func (m *Manager) add(name, from string, digest [sha256.Size]byte, persist bool) error {
	if !namePattern.MatchString(name) {
		return ErrInvalidName
	}
	p, err := managedProject(name, from, digest)
	if err != nil {
		return err
	}
	r := m.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[name]; ok {
		return ErrProjectExists
	}
	if _, ok := r.byKey[digest]; ok {
		return errors.New("project: credential already in use")
	}
	if persist {
		if err := m.store.CreateManagedProject(storage.ManagedProject{Name: name, DefaultFrom: from, KeyDigest: digest[:]}); err != nil {
			return err
		}
	}
	r.byName[name] = p
	r.byKey[digest] = p
	r.ordered = append(r.ordered, p)
	r.enabled++
	r.managed[name] = true
	return nil
}

// Restore reconstitutes managed projects before the gateway begins serving.
func (m *Manager) Restore() error {
	records, err := m.store.ManagedProjects()
	if err != nil {
		return err
	}
	for _, rec := range records {
		if len(rec.KeyDigest) != sha256.Size {
			return fmt.Errorf("project %q: invalid stored digest", rec.Name)
		}
		var d [sha256.Size]byte
		copy(d[:], rec.KeyDigest)
		if err := m.add(rec.Name, rec.DefaultFrom, d, false); err != nil {
			return fmt.Errorf("restore project %q: %w", rec.Name, err)
		}
	}
	return nil
}
func newManagedKey() (string, [sha256.Size]byte, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", [sha256.Size]byte{}, err
	}
	key := "ph_live_" + base64.RawURLEncoding.EncodeToString(b[:])
	return key, sha256.Sum256([]byte(key)), nil
}
func (m *Manager) Create(name string) (string, error) {
	key, d, err := newManagedKey()
	if err != nil {
		return "", err
	}
	if err := m.add(name, m.defaultFrom, d, true); err != nil {
		return "", err
	}
	return key, nil
}

// Rotate immediately revokes the old key after the new digest is durable.
func (m *Manager) Rotate(name string) (string, error) {
	key, d, err := newManagedKey()
	if err != nil {
		return "", err
	}
	r := m.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.managed[name] {
		return "", ErrProjectNotManaged
	}
	if _, ok := r.byKey[d]; ok {
		return "", errors.New("project: credential already in use")
	}
	if err := m.store.RotateManagedProject(name, d[:]); err != nil {
		return "", err
	}
	p := r.byName[name]
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	for _, old := range p.keyDigests {
		delete(r.byKey, old)
	}
	p.keyDigest = d
	p.keyDigests = [][sha256.Size]byte{d}
	r.byKey[d] = p
	return key, nil
}
func (m *Manager) IsManaged(name string) bool {
	r := m.registry
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.managed[name]
}
