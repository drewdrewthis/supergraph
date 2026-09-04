package github

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// store is the github plugin's SQLite state, layered over the core.Store (which
// owns the events ring and the cursors table). It holds the node cache, the tag
// index that drives scope/kind purges, the created-hook registry, and the
// seen-delivery set for redelivery dedup.
type store struct{ core *core.Store }

// node is one cached object (or list result). JSON is the raw parsed body; ETag +
// FetchedAt drive conditional refetch; Pinned exempts immutable nodes.
type node struct {
	Key       string
	Typename  string
	JSON      []byte
	ETag      string
	Pinned    bool
	FetchedAt time.Time
	UpdatedAt time.Time
}

const nodeSchema = `
CREATE TABLE IF NOT EXISTS github_nodes (
	key        TEXT PRIMARY KEY,
	typename   TEXT,
	node_json  BLOB,
	etag       TEXT,
	pinned     INTEGER NOT NULL DEFAULT 0,
	fetched_at TEXT,
	updated_at TEXT
);
CREATE TABLE IF NOT EXISTS github_tags (
	tag TEXT,
	key TEXT,
	PRIMARY KEY (tag, key)
);
CREATE TABLE IF NOT EXISTS github_hooks (
	owner   TEXT,
	repo    TEXT,
	hook_id INTEGER,
	secret  TEXT,
	PRIMARY KEY (owner, repo)
);
CREATE TABLE IF NOT EXISTS github_deliveries (
	delivery_id TEXT PRIMARY KEY,
	seen_at     TEXT
);`

// migrate creates the four state tables, add-only and idempotent.
func (s *store) migrate(ctx context.Context) error {
	if _, err := s.core.DB().ExecContext(ctx, nodeSchema); err != nil {
		return fmt.Errorf("github: migrate: %w", err)
	}
	return nil
}

func (s *store) db() *sql.DB { return s.core.DB() }

const rfc = time.RFC3339Nano

// get returns the node at key, or nil when absent.
func (s *store) get(ctx context.Context, key string) (*node, error) {
	var (
		n            node
		fetched, upd string
		pinned       int
	)
	err := s.db().QueryRowContext(ctx,
		`SELECT key, typename, node_json, etag, pinned, fetched_at, updated_at
		 FROM github_nodes WHERE key = ?`, key,
	).Scan(&n.Key, &n.Typename, &n.JSON, &n.ETag, &pinned, &fetched, &upd)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("github: get %q: %w", key, err)
	}
	n.Pinned = pinned != 0
	n.FetchedAt, _ = time.Parse(rfc, fetched)
	n.UpdatedAt, _ = time.Parse(rfc, upd)
	return &n, nil
}

// upsert writes (replacing) one node row.
func (s *store) upsert(ctx context.Context, n *node) error {
	pinned := 0
	if n.Pinned {
		pinned = 1
	}
	_, err := s.db().ExecContext(ctx,
		`INSERT INTO github_nodes (key, typename, node_json, etag, pinned, fetched_at, updated_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(key) DO UPDATE SET
		   typename=excluded.typename, node_json=excluded.node_json, etag=excluded.etag,
		   pinned=excluded.pinned, fetched_at=excluded.fetched_at, updated_at=excluded.updated_at`,
		n.Key, n.Typename, n.JSON, n.ETag, pinned,
		n.FetchedAt.Format(rfc), n.UpdatedAt.Format(rfc),
	)
	if err != nil {
		return fmt.Errorf("github: upsert %q: %w", n.Key, err)
	}
	return nil
}

// bumpFetched records a 304 revalidation: the body is unchanged, only freshness
// advances (AC-GH-ETAG-304).
func (s *store) bumpFetched(ctx context.Context, key string, at time.Time) error {
	_, err := s.db().ExecContext(ctx,
		`UPDATE github_nodes SET fetched_at=? WHERE key=?`, at.Format(rfc), key)
	return err
}

// putList stores a list result under listKey tagged with its covering composite,
// so a purge of any contained object evicts the list (AC-GH-NAMEDOP-KEYS).
func (s *store) putList(ctx context.Context, lk string, body []byte, comp string, at time.Time) error {
	if err := s.upsert(ctx, &node{Key: lk, Typename: "_list", JSON: body, FetchedAt: at, UpdatedAt: at}); err != nil {
		return err
	}
	if comp == "" {
		return nil
	}
	_, err := s.db().ExecContext(ctx,
		`INSERT OR IGNORE INTO github_tags (tag, key) VALUES (?, ?)`, comp, lk)
	return err
}

// purge evicts key and every list result whose covering composite includes it,
// returning the keys actually deleted (EDR §"Tag index").
func (s *store) purge(ctx context.Context, key string) ([]string, error) {
	deleted := []string{}
	comp := composite(key)
	if comp != "" {
		rows, err := s.db().QueryContext(ctx, `SELECT key FROM github_tags WHERE tag=?`, comp)
		if err != nil {
			return nil, fmt.Errorf("github: purge scan %q: %w", key, err)
		}
		var lks []string
		for rows.Next() {
			var lk string
			if err := rows.Scan(&lk); err != nil {
				_ = rows.Close()
				return nil, err
			}
			lks = append(lks, lk)
		}
		_ = rows.Close()
		for _, lk := range lks {
			if _, err := s.db().ExecContext(ctx, `DELETE FROM github_nodes WHERE key=?`, lk); err != nil {
				return nil, err
			}
			if _, err := s.db().ExecContext(ctx, `DELETE FROM github_tags WHERE key=?`, lk); err != nil {
				return nil, err
			}
			deleted = append(deleted, lk)
		}
	}
	res, err := s.db().ExecContext(ctx, `DELETE FROM github_nodes WHERE key=?`, key)
	if err != nil {
		return nil, fmt.Errorf("github: purge %q: %w", key, err)
	}
	if _, err := s.db().ExecContext(ctx, `DELETE FROM github_tags WHERE key=?`, key); err != nil {
		return nil, err
	}
	if aff, _ := res.RowsAffected(); aff > 0 {
		deleted = append(deleted, key)
	}
	return deleted, nil
}

// deliverySeen reports whether this X-GitHub-Delivery id was already ingested, so a
// redelivery replays a missed event exactly once (AC-GH-FORWARD).
func (s *store) deliverySeen(ctx context.Context, id string) bool {
	var one int
	err := s.db().QueryRowContext(ctx,
		`SELECT 1 FROM github_deliveries WHERE delivery_id=?`, id).Scan(&one)
	return err == nil
}

func (s *store) markDelivery(ctx context.Context, id string, at time.Time) error {
	_, err := s.db().ExecContext(ctx,
		`INSERT OR IGNORE INTO github_deliveries (delivery_id, seen_at) VALUES (?, ?)`,
		id, at.Format(rfc))
	return err
}

// hook returns the created hook id for a repo, or ok=false.
func (s *store) hook(ctx context.Context, owner, repo string) (int64, bool) {
	var id int64
	err := s.db().QueryRowContext(ctx,
		`SELECT hook_id FROM github_hooks WHERE owner=? AND repo=?`, owner, repo).Scan(&id)
	return id, err == nil
}

func (s *store) putHook(ctx context.Context, owner, repo string, id int64, secret string) error {
	_, err := s.db().ExecContext(ctx,
		`INSERT INTO github_hooks (owner, repo, hook_id, secret) VALUES (?,?,?,?)
		 ON CONFLICT(owner, repo) DO UPDATE SET hook_id=excluded.hook_id, secret=excluded.secret`,
		owner, repo, id, secret)
	return err
}

// hooks lists every created hook (owner/repo/id) for redelivery sweeps.
func (s *store) hooks(ctx context.Context) ([][3]string, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT owner, repo, hook_id FROM github_hooks`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out [][3]string
	for rows.Next() {
		var o, r, id string
		if err := rows.Scan(&o, &r, &id); err != nil {
			return nil, err
		}
		out = append(out, [3]string{o, r, id})
	}
	return out, rows.Err()
}

// cursor / setCursor delegate to core's cursors table (EDR §"SQLite state": since,
// notif, hook cursors all live there).
func (s *store) cursor(ctx context.Context, name string) string {
	v, _ := s.core.Cursor(ctx, name)
	return v
}

func (s *store) setCursor(ctx context.Context, name, value string) error {
	return s.core.SetCursor(ctx, name, value)
}
