package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEnvelope(i int) Envelope {
	return Envelope{
		TS:      time.Now().UTC(),
		Source:  "template",
		Type:    "demo.ping",
		V:       2,
		Key:     "k",
		Payload: json.RawMessage(`{"i":` + itoa(i) + `}`),
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// AC-CORE-2: the event ring caps at capacity N — appending N+5 leaves exactly N
// rows, oldest pruned.
func TestAppendEventRingCap(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "ring.db")
	const capN = 10

	s, err := OpenStore(ctx, path, capN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	for i := 0; i < capN+5; i++ {
		if err := s.AppendEvent(ctx, testEnvelope(i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	evs, err := s.Events(ctx, 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(evs) != capN {
		t.Fatalf("rows=%d want %d", len(evs), capN)
	}

	// Newest-first: the most recent append (i=cap+4) is first; oldest kept is i=5.
	var newest, oldest struct {
		I int `json:"i"`
	}
	if err := json.Unmarshal(evs[0].Payload, &newest); err != nil {
		t.Fatalf("decode newest: %v", err)
	}
	if err := json.Unmarshal(evs[len(evs)-1].Payload, &oldest); err != nil {
		t.Fatalf("decode oldest: %v", err)
	}
	if newest.I != capN+4 {
		t.Errorf("newest i=%d want %d", newest.I, capN+4)
	}
	if oldest.I != 5 {
		t.Errorf("oldest i=%d want 5 (older pruned)", oldest.I)
	}
}

// AC-CORE-2: a cursor written before Close is readable after reopen; an absent
// cursor returns "" with a nil error.
func TestCursorPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cursor.db")

	s, err := OpenStore(ctx, path, 100)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if v, err := s.Cursor(ctx, "since"); err != nil || v != "" {
		t.Fatalf("absent cursor: got (%q,%v) want (\"\",nil)", v, err)
	}

	if err := s.SetCursor(ctx, "since", "X"); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	// upsert overwrites.
	if err := s.SetCursor(ctx, "since", "X"); err != nil {
		t.Fatalf("re-set cursor: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := OpenStore(ctx, path, 100)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.Cursor(ctx, "since")
	if err != nil {
		t.Fatalf("cursor after reopen: %v", err)
	}
	if got != "X" {
		t.Fatalf("cursor=%q want X", got)
	}
}

// Migrations are idempotent: reopening an existing db (which re-runs CREATE ...
// IF NOT EXISTS) must not error and must preserve data.
func TestMigrationsIdempotentOnReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "idem.db")

	s, err := OpenStore(ctx, path, 100)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.AppendEvent(ctx, testEnvelope(1)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := OpenStore(ctx, path, 100)
	if err != nil {
		t.Fatalf("reopen (re-migrate) failed: %v", err)
	}
	defer s2.Close()

	evs, err := s2.Events(ctx, 10)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("data lost across reopen: rows=%d want 1", len(evs))
	}
}

// AC-CORE-17: a reader goroutine calling Events while a writer loops AppendEvent
// 200 times must not hit "database is locked" (WAL + busy_timeout).
func TestConcurrentReadWriteNoLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.db")

	s, err := OpenStore(ctx, path, 50)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := s.AppendEvent(ctx, testEnvelope(i)); err != nil {
				errCh <- err
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if _, err := s.Events(ctx, 20); err != nil {
				errCh <- err
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			if strings.Contains(err.Error(), "database is locked") {
				t.Fatalf("got lock error: %v", err)
			}
			t.Fatalf("concurrent op error: %v", err)
		}
	}
}

// PRD "Rebuildable": deleting the db file and reopening rebuilds core's schema
// from scratch — core tables present and the schema_version row stamped at 1.
func TestDeleteDBAndRebuild(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rebuild.db")

	s, err := OpenStore(ctx, path, 100)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.AppendEvent(ctx, testEnvelope(1)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Remove the db and its WAL/SHM sidecars — a real "delete the db" (PRD §6).
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", path+suffix, err)
		}
	}

	s2, err := OpenStore(ctx, path, 100)
	if err != nil {
		t.Fatalf("reopen after delete: %v", err)
	}
	defer s2.Close()

	// Core tables rebuilt.
	for _, table := range []string{"events", "cursors", "schema_version"} {
		var name string
		err := s2.DB().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %q missing after rebuild: %v", table, err)
		}
	}

	// Schema stamped at version 1, exactly one row.
	var count, version int
	if err := s2.DB().QueryRowContext(ctx,
		`SELECT COUNT(1), COALESCE(MAX(version), 0) FROM schema_version`,
	).Scan(&count, &version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if count != 1 || version != 1 {
		t.Fatalf("schema_version rows=%d maxVersion=%d, want rows=1 version=1", count, version)
	}

	// Fresh db has no leftover data from before the delete.
	evs, err := s2.Events(ctx, 10)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("rebuilt db not empty: rows=%d want 0", len(evs))
	}
}
