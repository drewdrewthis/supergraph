package pluginconfig

import (
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestStr(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		key  string
		def  string
		want string
	}{
		{"present", map[string]any{"k": "v"}, "k", "d", "v"},
		{"empty-falls-back", map[string]any{"k": ""}, "k", "d", "d"},
		{"absent", map[string]any{"other": "v"}, "k", "d", "d"},
		{"wrong-type", map[string]any{"k": 5}, "k", "d", "d"},
		{"nil-map", nil, "k", "d", "d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Str(c.raw, c.key, c.def); got != c.want {
				t.Fatalf("Str = %q, want %q", got, c.want)
			}
		})
	}
}

func TestInt(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		key  string
		def  int
		want int
	}{
		{"int", map[string]any{"k": 5}, "k", 7, 5},
		{"int64", map[string]any{"k": int64(5)}, "k", 7, 5},
		{"float64", map[string]any{"k": float64(5)}, "k", 7, 5},
		{"numeric-string", map[string]any{"k": "5"}, "k", 7, 5},
		{"padded-string", map[string]any{"k": " 5 "}, "k", 7, 5},
		{"non-numeric-string", map[string]any{"k": "x"}, "k", 7, 7},
		// AC-PCFG-INT0: a stored 0 returns 0, not the default (fixes the github
		// copy's `n != 0` fallback bug).
		{"zero-returns-zero", map[string]any{"k": 0}, "k", 7, 0},
		{"absent", map[string]any{}, "k", 7, 7},
		{"wrong-type", map[string]any{"k": true}, "k", 7, 7},
		{"nil-map", nil, "k", 7, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Int(c.raw, c.key, c.def); got != c.want {
				t.Fatalf("Int = %d, want %d", got, c.want)
			}
		})
	}
}

// TestIntCoerceParity pins that the three numeric encodings coerce identically.
func TestIntCoerceParity(t *testing.T) {
	for _, v := range []any{int(5), int64(5), float64(5)} {
		if got := Int(map[string]any{"k": v}, "k", 0); got != 5 {
			t.Fatalf("Int(%T) = %d, want 5", v, got)
		}
	}
}

func TestNum(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		key  string
		def  float64
		want float64
	}{
		{"float64", map[string]any{"k": 2.5}, "k", 1, 2.5},
		{"int64", map[string]any{"k": int64(5)}, "k", 1, 5},
		{"int", map[string]any{"k": 5}, "k", 1, 5},
		{"absent", map[string]any{}, "k", 1.5, 1.5},
		{"wrong-type", map[string]any{"k": "5"}, "k", 1.5, 1.5},
		{"nil-map", nil, "k", 1.5, 1.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Num(c.raw, c.key, c.def); got != c.want {
				t.Fatalf("Num = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNumCoerceParity pins that int/int64/float64 all coerce to the same 5.0.
func TestNumCoerceParity(t *testing.T) {
	for _, v := range []any{int(5), int64(5), float64(5)} {
		if got := Num(map[string]any{"k": v}, "k", 0); got != 5.0 {
			t.Fatalf("Num(%T) = %v, want 5", v, got)
		}
	}
}

func TestBool(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		key  string
		def  bool
		want bool
	}{
		{"true", map[string]any{"k": true}, "k", false, true},
		{"false-not-default", map[string]any{"k": false}, "k", true, false},
		{"absent", map[string]any{}, "k", true, true},
		{"wrong-type", map[string]any{"k": "true"}, "k", true, true},
		{"nil-map", nil, "k", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Bool(c.raw, c.key, c.def); got != c.want {
				t.Fatalf("Bool = %v, want %v", got, c.want)
			}
		})
	}
}

func TestStrs(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		key  string
		want []string
	}{
		{"all-strings", map[string]any{"k": []any{"a", "b"}}, "k", []string{"a", "b"}},
		{"drops-non-strings", map[string]any{"k": []any{"a", 1, "b"}}, "k", []string{"a", "b"}},
		{"absent", map[string]any{}, "k", nil},
		{"wrong-type", map[string]any{"k": "a"}, "k", nil},
		{"nil-map", nil, "k", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Strs(c.raw, c.key)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Strs = %v, want %v", got, c.want)
			}
		})
	}
}

func TestToInt(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"int", 5, 5},
		{"int64", int64(5), 5},
		{"float64", float64(5), 5},
		{"string-not-parsed", "5", 0},
		{"nil", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ToInt(c.in); got != c.want {
				t.Fatalf("ToInt = %d, want %d", got, c.want)
			}
		})
	}
}

func TestReadErrStatus(t *testing.T) {
	if got := ReadErrStatus(&http.MaxBytesError{}); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("MaxBytesError -> %d, want 413", got)
	}
	if got := ReadErrStatus(errors.New("boom")); got != http.StatusBadRequest {
		t.Fatalf("plain error -> %d, want 400", got)
	}
}
