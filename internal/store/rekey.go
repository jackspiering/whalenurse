package store

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
)

// RekeyMeta says which meta rows hold a state key: Keyed are prefixes followed by the key ("parents:<key>"), WithKey are
// prefixes whose value is a JSON object with the key in its "key" field ("tagmoved:<ref>"). The daemon owns both lists.
type RekeyMeta struct {
	Keyed   []string
	WithKey []string
}

// Rekey gives every persisted record keyed by a state key of another host the current host (plan v3.13 item 12). One
// store belongs to one installation; in a container the default host ID is the hostname, the container's ID, which
// changes whenever whalenurse is recreated (an upgrade), and the state under the old ID would otherwise never match a
// live container again.
//
// It covers the per-key state, the flap history, the incidents (open and closed), the meta rows of m, and the
// notification dedupe rows of every key it moves. When the current key already has state records (whalenurse ran under this host ID before), the key whose records
// show the later activity wins and every record of the other is dropped; on a tie the current one wins. The event
// history keeps the host it was written with. A key whose remainder is not <project>/<service>/<number> or <name> (a host
// ID with a "/" in it) is left alone. It runs in one transaction and returns how many keys were moved.
func (s *Store) Rekey(host string, m RekeyMeta) (int, error) {
	if host == "" {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	keys, err := persistedKeys(tx, m)
	if err != nil {
		return 0, err
	}
	// The keys of other hosts, grouped by the key they become.
	groups := map[string][]string{}
	for k := range keys {
		to, ok := rekeyed(k, host)
		if ok && to != k {
			groups[to] = append(groups[to], k)
		}
	}
	targets := make([]string, 0, len(groups))
	for to := range groups {
		targets = append(targets, to)
	}
	sort.Strings(targets)

	moved := 0
	for _, to := range targets {
		cands := groups[to]
		sort.Strings(cands)
		winner := ""
		var best int64 = -1
		if keys[to] {
			winner = to
			if best, err = activity(tx, to); err != nil {
				return 0, err
			}
		}
		for _, k := range cands {
			a, err := activity(tx, k)
			if err != nil {
				return 0, err
			}
			if a > best {
				winner, best = k, a
			}
		}
		for _, k := range append(cands, to) {
			if k != winner && keys[k] {
				if err := dropKey(tx, k, m); err != nil {
					return 0, err
				}
			}
		}
		if winner != to {
			if err := moveKey(tx, winner, to, m); err != nil {
				return 0, err
			}
			moved++
		}
	}
	if err := rekeyMetaValues(tx, host, m); err != nil {
		return 0, err
	}
	return moved, tx.Commit()
}

// rekeyed is k under host, and whether k has the shape of a state key: host/<project>/<service>/<number> or host/<name>.
func rekeyed(k, host string) (string, bool) {
	if strings.HasPrefix(k, host+"/") {
		return k, true
	}
	_, rest, ok := strings.Cut(k, "/")
	if !ok || rest == "" {
		return "", false
	}
	if n := strings.Count(rest, "/"); n != 0 && n != 2 {
		return "", false
	}
	return host + "/" + rest, true
}

func persistedKeys(tx *sql.Tx, m RekeyMeta) (map[string]bool, error) {
	keys := map[string]bool{}
	// Not the notification dedupe: it also holds global messages keyed host/<scope> (a pause), which are not a
	// container's state and must not decide a collision. Its rows move with their key (moveKey) or expire.
	for _, q := range []string{`SELECT key FROM key_state`, `SELECT DISTINCT key FROM flaps`, `SELECT DISTINCT key FROM incidents`} {
		if err := scanStrings(tx, q, func(k string) { keys[k] = true }); err != nil {
			return nil, err
		}
	}
	for _, p := range m.Keyed {
		err := scanStrings(tx, `SELECT key FROM meta WHERE substr(key, 1, ?) = ?`, func(k string) { keys[strings.TrimPrefix(k, p)] = true }, len(p), p)
		if err != nil {
			return nil, err
		}
	}
	delete(keys, "")
	return keys, nil
}

// activity is the latest time any state record of k shows. Meta rows have no time, and the dedupe is not state; they
// decide nothing.
func activity(tx *sql.Tx, k string) (int64, error) {
	var a sql.NullInt64
	err := tx.QueryRow(`SELECT MAX(t) FROM (
		SELECT updated_at AS t FROM key_state WHERE key = ?1
		UNION ALL SELECT at FROM flaps WHERE key = ?1
		UNION ALL SELECT MAX(opened_at, suspended_at, recovery_since, COALESCE(closed_at, 0)) FROM incidents WHERE key = ?1)`, k).Scan(&a)
	if err != nil {
		return 0, err
	}
	return a.Int64, nil
}

func dropKey(tx *sql.Tx, k string, m RekeyMeta) error {
	for _, q := range []string{`DELETE FROM key_state WHERE key = ?`, `DELETE FROM flaps WHERE key = ?`, `DELETE FROM incidents WHERE key = ?`, `DELETE FROM notified WHERE key = ?`} {
		if _, err := tx.Exec(q, k); err != nil {
			return err
		}
	}
	for _, p := range m.Keyed {
		if _, err := tx.Exec(`DELETE FROM meta WHERE key = ?`, p+k); err != nil {
			return err
		}
	}
	return nil
}

func moveKey(tx *sql.Tx, from, to string, m RekeyMeta) error {
	for _, q := range []string{`UPDATE key_state SET key = ?1 WHERE key = ?2`, `UPDATE flaps SET key = ?1 WHERE key = ?2`, `UPDATE incidents SET key = ?1 WHERE key = ?2`} {
		if _, err := tx.Exec(q, to, from); err != nil {
			return err
		}
	}
	// The dedupe identity is <type>|<key>|<discriminator>|<incident> (notify.render): the key in it moves too.
	var ids []string
	if err := scanStrings(tx, `SELECT id FROM notified WHERE key = ?`, func(id string) { ids = append(ids, id) }, from); err != nil {
		return err
	}
	for _, id := range ids {
		nid := strings.Replace(id, "|"+from+"|", "|"+to+"|", 1)
		// OR REPLACE: the same message may already be recorded under the new key.
		if _, err := tx.Exec(`UPDATE OR REPLACE notified SET id = ?, key = ? WHERE id = ?`, nid, to, id); err != nil {
			return err
		}
	}
	for _, p := range m.Keyed {
		if _, err := tx.Exec(`UPDATE meta SET key = ? WHERE key = ?`, p+to, p+from); err != nil {
			return err
		}
	}
	return nil
}

// rekeyMetaValues moves the key inside the JSON values of the WithKey rows (the container that moved a tag).
func rekeyMetaValues(tx *sql.Tx, host string, m RekeyMeta) error {
	for _, p := range m.WithKey {
		type row struct{ k, v string }
		var rows []row
		r, err := tx.Query(`SELECT key, value FROM meta WHERE substr(key, 1, ?) = ?`, len(p), p)
		if err != nil {
			return err
		}
		for r.Next() {
			var x row
			if err := r.Scan(&x.k, &x.v); err != nil {
				r.Close()
				return err
			}
			rows = append(rows, x)
		}
		if err := closeRows(r); err != nil {
			return err
		}
		for _, x := range rows {
			var obj map[string]json.RawMessage
			if json.Unmarshal([]byte(x.v), &obj) != nil {
				continue
			}
			var k string
			if json.Unmarshal(obj["key"], &k) != nil {
				continue
			}
			to, ok := rekeyed(k, host)
			if !ok || to == k {
				continue
			}
			obj["key"], _ = json.Marshal(to)
			b, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = ?`, string(b), x.k); err != nil {
				return err
			}
		}
	}
	return nil
}

func scanStrings(tx *sql.Tx, q string, f func(string), args ...any) error {
	r, err := tx.Query(q, args...)
	if err != nil {
		return err
	}
	for r.Next() {
		var s string
		if err := r.Scan(&s); err != nil {
			r.Close()
			return err
		}
		f(s)
	}
	return closeRows(r)
}
