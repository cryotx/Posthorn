package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Recipient-scoped delivery projection (FR128, ADR-44).
//
// A submission may carry several recipients, and providers disagree
// about what an event covers: Postmark emits one event PER RECIPIENT
// with a Recipient field, while Resend emits one event PER MESSAGE
// carrying the whole recipient array. Letting either kind write a
// single submission-level state was wrong — one recipient's bounce
// would overwrite another's delivery.
//
// So state is projected per recipient, and the submission carries an
// AGGREGATE derived from those rows. The event log remains the record
// of fact and is never rewritten to maintain either.

// Aggregate delivery states on submissions.delivery_state.
const (
	// AggPending means at least one recipient has no terminal outcome
	// yet. Also the state of a message no provider has reported on.
	AggPending = "pending"

	// AggDelivered means every recipient reached a positive terminal
	// outcome.
	AggDelivered = "delivered"

	// AggPartial means recipients disagree: some delivered, some
	// failed. The case a single submission-level column could not
	// express at all.
	AggPartial = "partial"

	// AggFailed means every recipient reached a negative terminal
	// outcome.
	AggFailed = "failed"
)

// terminalStates are the unified event types that state an OUTCOME. A
// transient state never overrides one of these, whatever its timestamp
// claims (ADR-44).
var terminalStates = map[string]bool{
	"message.delivered":  true,
	"message.bounced":    true,
	"message.complained": true,
	"message.failed":     true,
}

// negativeStates are the terminal outcomes that mean the message did
// not usefully arrive.
var negativeStates = map[string]bool{
	"message.bounced":    true,
	"message.complained": true,
	"message.failed":     true,
}

// IsTerminalState reports whether a state is an outcome rather than a
// progress report.
func IsTerminalState(state string) bool { return terminalStates[state] }

// stateSeverity orders states WITHIN their class, for the only case
// where ordering is needed: two events carrying the same timestamp.
//
// Deliberately not a single global rank across all states — a linear
// rank cannot express "a transient never overrides a terminal", which
// is the rule that actually matters when a delayed `deferred` webhook
// lands after a `delivered` one.
func stateSeverity(state string) int {
	switch state {
	// Terminal, most significant last: a message that was delivered
	// and then complained about is, operationally, a complaint.
	case "message.delivered":
		return 1
	case "message.failed":
		return 2
	case "message.bounced":
		return 3
	case "message.complained":
		return 4
	// Non-terminal progress.
	case "message.accepted":
		return 1
	case "message.sent":
		return 2
	case "message.deferred":
		return 3
	default:
		return 0
	}
}

// Supersedes decides whether a new event replaces the current
// projected state (ADR-44). Exported so the rules are directly
// testable against the disorder cases they exist for.
//
// The rules, in order:
//
//  1. Nothing beats something: any event lands on an empty state.
//  2. A TERMINAL event always lands on a non-terminal state, whatever
//     the timestamps say. A bounce that arrives an hour late is still
//     the outcome.
//  3. A NON-TERMINAL event never overrides a terminal one. This is the
//     case a plain "latest event wins" rule gets wrong: a `deferred`
//     webhook delayed past a `delivered` one must not un-deliver the
//     message.
//  4. Between two TERMINAL states, severity wins and time only breaks
//     ties. Terminal outcomes escalate and never de-escalate: a
//     complaint is not undone by a delivery event that happens to
//     carry a later timestamp, which is exactly what an out-of-order
//     webhook produces. This is the rule a linear rank plus "latest
//     wins" could not express, and the reason ADR-44 replaced it.
//  5. Between two NON-TERMINAL states the later event wins, with
//     severity breaking ties. Progress reports legitimately move
//     around and every one of them aggregates to "pending" anyway.
//
// Duplicates are idempotent under both branches.
func Supersedes(current string, currentAt time.Time, next string, nextAt time.Time) bool {
	if current == "" {
		return true
	}
	curTerminal, nextTerminal := IsTerminalState(current), IsTerminalState(next)
	switch {
	case !curTerminal && nextTerminal:
		return true
	case curTerminal && !nextTerminal:
		return false
	case curTerminal && nextTerminal:
		curSev, nextSev := stateSeverity(current), stateSeverity(next)
		if nextSev != curSev {
			return nextSev > curSev
		}
		return nextAt.After(currentAt)
	}
	if nextAt.After(currentAt) {
		return true
	}
	if nextAt.Before(currentAt) {
		return false
	}
	return stateSeverity(next) > stateSeverity(current)
}

// RecipientState is one recipient's projected delivery outcome.
type RecipientState struct {
	Recipient string
	State     string
	At        time.Time
}

// ProjectRecipientEvent applies one normalized event to a message's
// recipients and refreshes the submission's aggregate.
//
// recipients empty means the event is message-scoped (Resend): it
// applies to every recipient of the submission. A named recipient
// (Postmark) applies to that one only; a name that matches none of the
// submission's recipients is reported so the caller can log it — the
// event itself is already recorded, and inventing a recipient row for
// it would corrupt the aggregate.
func (s *Store) ProjectRecipientEvent(messageID, recipient, state string, at time.Time) (matched bool, err error) {
	if messageID == "" || state == "" {
		return false, nil
	}
	err = s.inTx(func(tx *sql.Tx) error {
		known, terr := submissionRecipients(tx, messageID)
		if terr != nil {
			return terr
		}
		if len(known) == 0 {
			return nil
		}

		targets := known
		if recipient != "" {
			// Providers normalise case; match the same way rather than
			// silently dropping a real event over an uppercase letter.
			wanted := strings.ToLower(strings.TrimSpace(recipient))
			targets = nil
			for _, r := range known {
				if strings.ToLower(r) == wanted {
					targets = append(targets, r)
					break
				}
			}
			if len(targets) == 0 {
				return nil
			}
		}
		matched = true

		for _, r := range targets {
			if aerr := applyRecipientState(tx, messageID, r, state, at); aerr != nil {
				return aerr
			}
		}
		return refreshAggregate(tx, messageID, known)
	})
	return matched, err
}

// applyRecipientState upserts one recipient's state under the
// supersession rules.
func applyRecipientState(tx *sql.Tx, messageID, recipient, state string, at time.Time) error {
	var current string
	var currentAt int64
	row := tx.QueryRow(
		`SELECT state, state_at FROM recipient_delivery WHERE message_id = ? AND recipient = ?`,
		messageID, recipient)
	switch err := row.Scan(&current, &currentAt); {
	case err == sql.ErrNoRows:
		current, currentAt = "", 0
	case err != nil:
		return fmt.Errorf("read recipient state: %w", err)
	}

	if !Supersedes(current, time.Unix(currentAt, 0).UTC(), state, at) {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO recipient_delivery (message_id, recipient, state, state_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(message_id, recipient) DO UPDATE SET
		  state = excluded.state, state_at = excluded.state_at`,
		messageID, recipient, state, at.Unix())
	if err != nil {
		return fmt.Errorf("write recipient state: %w", err)
	}
	return nil
}

// refreshAggregate recomputes the submission-level summary from the
// recipient rows. A recipient with no row yet counts as pending, which
// is what keeps a half-reported multi-recipient message honest.
func refreshAggregate(tx *sql.Tx, messageID string, known []string) error {
	states := map[string]string{}
	rows, err := tx.Query(
		`SELECT recipient, state FROM recipient_delivery WHERE message_id = ?`, messageID)
	if err != nil {
		return fmt.Errorf("read recipient states: %w", err)
	}
	for rows.Next() {
		var r, st string
		if err := rows.Scan(&r, &st); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan recipient state: %w", err)
		}
		states[r] = st
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read recipient states: %w", err)
	}
	_ = rows.Close()

	var positive, negative, pending int
	var latest int64
	for _, r := range known {
		st, ok := states[r]
		switch {
		case !ok || !IsTerminalState(st):
			pending++
		case negativeStates[st]:
			negative++
		default:
			positive++
		}
	}
	agg := AggPending
	switch {
	case pending > 0:
		agg = AggPending
	case negative == 0:
		agg = AggDelivered
	case positive == 0:
		agg = AggFailed
	default:
		agg = AggPartial
	}

	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(state_at), 0) FROM recipient_delivery WHERE message_id = ?`,
		messageID).Scan(&latest); err != nil {
		return fmt.Errorf("read aggregate timestamp: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE submissions SET delivery_state = ?, delivery_state_at = ? WHERE id = ?`,
		agg, latest, messageID); err != nil {
		return fmt.Errorf("write aggregate state: %w", err)
	}
	return nil
}

// submissionRecipients reads a submission's recipient list.
func submissionRecipients(tx *sql.Tx, messageID string) ([]string, error) {
	var toJSON string
	err := tx.QueryRow(`SELECT to_addrs FROM submissions WHERE id = ?`, messageID).Scan(&toJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read submission recipients: %w", err)
	}
	var out []string
	if err := json.Unmarshal([]byte(toJSON), &out); err != nil {
		return nil, fmt.Errorf("decode submission recipients: %w", err)
	}
	return out, nil
}

// RecipientStates returns one message's per-recipient outcomes.
func (s *Store) RecipientStates(messageID string) ([]RecipientState, error) {
	rows, err := s.db.Query(`
		SELECT recipient, state, state_at FROM recipient_delivery
		WHERE message_id = ? ORDER BY recipient`, messageID)
	if err != nil {
		return nil, fmt.Errorf("storage: list recipient states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RecipientState
	for rows.Next() {
		var rs RecipientState
		var at int64
		if err := rows.Scan(&rs.Recipient, &rs.State, &at); err != nil {
			return nil, fmt.Errorf("storage: scan recipient state: %w", err)
		}
		rs.At = time.Unix(at, 0).UTC()
		out = append(out, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read recipient states: %w", err)
	}
	return out, nil
}

// PruneRecipientStates drops projection rows for pruned messages.
func (s *Store) PruneRecipientStates() error {
	_, err := s.db.Exec(`
		DELETE FROM recipient_delivery
		WHERE message_id NOT IN (SELECT id FROM submissions)`)
	if err != nil {
		return fmt.Errorf("storage: prune recipient states: %w", err)
	}
	return nil
}
