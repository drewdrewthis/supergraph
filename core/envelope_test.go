package core

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// AC-CORE-1: an Envelope round-trips through JSON to an equal value, and its
// versioned Payload survives byte-identical (core never re-parses it).
func TestEnvelopeRoundTrip(t *testing.T) {
	payload := json.RawMessage(`{"title":"hi","nested":{"n":1},"arr":[1,2,3]}`)
	orig := Envelope{
		TS:      time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
		Source:  "template",
		Type:    "demo.ping",
		V:       2,
		Key:     "issue:org/repo#12@host",
		Payload: payload,
	}

	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Envelope
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !got.TS.Equal(orig.TS) {
		t.Errorf("TS: got %v want %v", got.TS, orig.TS)
	}
	if got.Source != orig.Source || got.Type != orig.Type || got.Key != orig.Key {
		t.Errorf("string fields mismatch: %+v", got)
	}
	if got.V != 2 {
		t.Errorf("V: got %d want 2", got.V)
	}
	// Payload must survive as opaque bytes, semantically identical.
	if !jsonEqual(t, got.Payload, orig.Payload) {
		t.Errorf("payload changed: got %s want %s", got.Payload, orig.Payload)
	}
}

// A V:2 payload embedded in an envelope is not rewritten on a marshal/unmarshal
// cycle: the exact bytes we handed in come back out.
func TestEnvelopePayloadOpaque(t *testing.T) {
	raw := json.RawMessage(`{"v2only":true,"keep":"exact"}`)
	e := Envelope{V: 2, Payload: raw}

	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Envelope
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !bytes.Equal(got.Payload, raw) {
		t.Fatalf("payload not byte-identical: got %s want %s", got.Payload, raw)
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return bytes.Equal(ab, bb)
}
