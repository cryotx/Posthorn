package storage

import (
	"database/sql"
	"fmt"

	"github.com/craigmccaskill/posthorn/transport"
)

// Provider-usage persistence (FR100). Together these two methods
// satisfy transport.UsageStore, which is how the quota tracker survives
// a restart without the transport package learning about SQLite.
//
// The interface is asserted here rather than in transport so the
// dependency arrow keeps pointing storage → transport.
var _ transport.UsageStore = (*Store)(nil)

// LoadProviderUsage returns every provider's counters for the given day
// and month period keys. Providers with no rows are simply absent —
// the tracker treats that as zero, which is what it means.
func (s *Store) LoadProviderUsage(day, month string) ([]transport.ProviderUsage, error) {
	rows, err := s.db.Query(`
		SELECT provider, window, used FROM provider_usage
		WHERE (window = 'day' AND period = ?) OR (window = 'month' AND period = ?)`,
		day, month)
	if err != nil {
		return nil, fmt.Errorf("storage: load provider usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byProvider := map[string]*transport.ProviderUsage{}
	for rows.Next() {
		var provider, window string
		var used int64
		if err := rows.Scan(&provider, &window, &used); err != nil {
			return nil, fmt.Errorf("storage: scan provider usage: %w", err)
		}
		u, ok := byProvider[provider]
		if !ok {
			u = &transport.ProviderUsage{Provider: provider}
			byProvider[provider] = u
		}
		if window == "day" {
			u.UsedToday = used
		} else {
			u.UsedMonth = used
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read provider usage: %w", err)
	}

	out := make([]transport.ProviderUsage, 0, len(byProvider))
	for _, u := range byProvider {
		out = append(out, *u)
	}
	return out, nil
}

// AddProviderUsage adds n to both the day and month counters for one
// provider, in a single transaction so a crash cannot leave the two
// windows disagreeing about the same send.
func (s *Store) AddProviderUsage(provider, day, month string, n int64) error {
	return s.inTx(func(tx *sql.Tx) error {
		const upsert = `
			INSERT INTO provider_usage (provider, window, period, used)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(provider, window, period) DO UPDATE SET used = used + excluded.used`
		if _, err := tx.Exec(upsert, provider, "day", day, n); err != nil {
			return fmt.Errorf("add day usage: %w", err)
		}
		if _, err := tx.Exec(upsert, provider, "month", month, n); err != nil {
			return fmt.Errorf("add month usage: %w", err)
		}
		return nil
	})
}

// ProviderUsageAt reads one provider's stored counters for the given
// periods, bypassing any in-memory cache. Used by tests and by the
// restart-recovery path.
func (s *Store) ProviderUsageAt(provider, day, month string) (transport.ProviderUsage, error) {
	u := transport.ProviderUsage{Provider: provider}
	err := s.db.QueryRow(
		`SELECT COALESCE((SELECT used FROM provider_usage
		                  WHERE provider = ? AND window = 'day' AND period = ?), 0),
		        COALESCE((SELECT used FROM provider_usage
		                  WHERE provider = ? AND window = 'month' AND period = ?), 0)`,
		provider, day, provider, month).Scan(&u.UsedToday, &u.UsedMonth)
	if err != nil {
		return u, fmt.Errorf("storage: read provider usage: %w", err)
	}
	return u, nil
}
