package core

import (
	"encoding/json"
	"time"
)

// Envelope is the single ingest unit every plugin emits. It is immutable once
// emitted: core persists and fans it out but never rewrites it. Schema evolution
// is add-only via V + upcasters (PRD §6), which is why Payload stays opaque here
// instead of being decoded into a typed struct the core would have to know about.
type Envelope struct {
	// TS is the event time: source-assigned when the source carries one, else the
	// ingest time. Kept distinct from row insert order so late-arriving events sort
	// correctly by their real occurrence time.
	TS time.Time `json:"ts"`
	// Source is the emitting plugin's stable name (e.g. "github", "template"). It
	// doubles as the plugin's SQLite filename and health key, so it must be stable.
	Source string `json:"source"`
	// Type is the event type within a source (e.g. "issue.opened"). It lets
	// consumers route without decoding Payload.
	Type string `json:"type"`
	// V is the Payload schema version. Upcasters key off it; core never inspects it
	// beyond carrying it, so a bump never forces a core change.
	V int `json:"v"`
	// Key is the idempotency / entity key (e.g. "issue:org/repo#12@host"). It scopes
	// dedup and per-entity ordering, and carries the @host suffix for later peer/mesh
	// disambiguation.
	Key string `json:"key"`
	// Payload is the versioned event body, opaque to core. It is stored and returned
	// byte-identical so no core change is needed when a plugin evolves its schema.
	Payload json.RawMessage `json:"payload"`
}
