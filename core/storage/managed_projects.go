package storage

import "fmt"

type ManagedProject struct {
	Name        string
	DefaultFrom string
	KeyDigest   []byte
}

func (s *Store) ManagedProjects() ([]ManagedProject, error) {
	rows, err := s.db.Query(`SELECT name, default_from, key_digest FROM managed_projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManagedProject
	for rows.Next() {
		var p ManagedProject
		if err := rows.Scan(&p.Name, &p.DefaultFrom, &p.KeyDigest); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) CreateManagedProject(p ManagedProject) error {
	_, err := s.db.Exec(`INSERT INTO managed_projects (name, default_from, key_digest) VALUES (?, ?, ?)`, p.Name, p.DefaultFrom, p.KeyDigest)
	return err
}
func (s *Store) RotateManagedProject(name string, digest []byte) error {
	result, err := s.db.Exec(`UPDATE managed_projects SET key_digest = ? WHERE name = ?`, digest, name)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("managed project not found")
	}
	return nil
}
