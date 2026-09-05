package single

import (
	"sync"
	"testing"
)

func TestPtr(t *testing.T) {
	t.Run("unset yields nil", func(t *testing.T) {
		var s Ptr[int]
		if got := s.Get(); got != nil {
			t.Fatalf("Get() = %v, want nil", got)
		}
	})

	t.Run("set then get returns the same pointer", func(t *testing.T) {
		var s Ptr[int]
		v := 42
		s.Set(&v)
		if got := s.Get(); got != &v {
			t.Fatalf("Get() = %p, want %p", got, &v)
		}
	})

	t.Run("overwrite replaces the prior value", func(t *testing.T) {
		var s Ptr[int]
		a, b := 1, 2
		s.Set(&a)
		s.Set(&b)
		if got := s.Get(); got != &b {
			t.Fatalf("Get() = %p, want %p", got, &b)
		}
	})
}

// TestPtr_ConcurrentSetGet exercises concurrent Set/Get under -race: atomic.Pointer
// guarantees no data race and every Get either sees nil or a fully-formed *int.
func TestPtr_ConcurrentSetGet(t *testing.T) {
	var s Ptr[int]
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		v := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.Set(&v)
		}()
		go func() {
			defer wg.Done()
			_ = s.Get()
		}()
	}
	wg.Wait()
}
