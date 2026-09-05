// Package pluginconfig holds the TOML-config coercion helpers shared by the
// github, claude, peer and tmux plugins. TOML decodes a plugin's `[plugins.X]`
// table into a map[string]any whose scalars are string/int64/float64/bool and
// whose arrays are []any; these helpers pull a typed value for a key, falling
// back to a caller default when the key is absent or the stored value has the
// wrong type. Each plugin previously carried a verbatim copy (strOr/intOr/…);
// this package is the single source of truth for that coercion.
package pluginconfig

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Str returns raw[k] when it holds a non-empty string, else def. A nil map or a
// wrong-typed / empty value yields def.
func Str(raw map[string]any, k, def string) string {
	if raw != nil {
		if v, ok := raw[k].(string); ok && v != "" {
			return v
		}
	}
	return def
}

// Int returns raw[k] coerced to int, else def. It accepts int, int64, float64
// and a numeric string; a nil map, an absent key, or a non-numeric value yields
// def. A stored numeric 0 returns 0 (not def) — the canonical type-switch
// semantics. This intentionally fixes the earlier github copy, whose `n != 0`
// guard mapped a legitimate 0-valued config back to the default.
func Int(raw map[string]any, k string, def int) int {
	if raw == nil {
		return def
	}
	switch n := raw[k].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return def
}

// Num returns raw[k] coerced to float64, else def. It accepts float64, int64 and
// int; a nil map, an absent key, or a wrong-typed value yields def. Used for the
// peer plugin's fractional-second thresholds.
func Num(raw map[string]any, k string, def float64) float64 {
	if raw == nil {
		return def
	}
	switch n := raw[k].(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return def
}

// Bool returns raw[k] when it holds a bool, else def.
func Bool(raw map[string]any, k string, def bool) bool {
	if raw != nil {
		if v, ok := raw[k].(bool); ok {
			return v
		}
	}
	return def
}

// Strs returns raw[k] as a []string, keeping only the string elements of the
// stored []any. A nil map, an absent key, or a non-array value yields nil.
func Strs(raw map[string]any, k string) []string {
	if raw == nil {
		return nil
	}
	xs, ok := raw[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(xs))
	for _, v := range xs {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ToInt coerces a bare TOML/JSON scalar to int, returning 0 for a nil or
// non-numeric value. Unlike Int it takes the value directly (no key/default) and
// does not parse strings — it mirrors the numeric-node coercion the github
// plugin applies to rate-limit fields and per-kind TTL values.
func ToInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// ReadErrStatus maps a request-body read/decode error to an HTTP status: 413
// when a MaxBytesReader cap tripped (the body exceeded the limit), else 400 for
// an ordinary malformed body.
func ReadErrStatus(err error) int {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}
