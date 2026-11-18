package storage

import "testing"

func TestManagedProjectTablePreservesSchemaCompatibility(t *testing.T) {
	s, err := Open(Config{Path: t.TempDir() + "/database.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 6 {
		t.Fatalf("schema version = %d, old version 6 binaries must be able to open this database", version)
	}
	if err := s.CreateManagedProject(ManagedProject{Name: "test", DefaultFrom: "sender@example.com", KeyDigest: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ManagedProjects()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows: %v, error: %v", rows, err)
	}
}
