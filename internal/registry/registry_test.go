package registry

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestUpdateLoadRoundtrip(t *testing.T) {
	s := &Store{Home: filepath.Join(t.TempDir(), "home")}
	err := s.Update(func(r *Registry) error {
		r.Envs["pr-1"] = &EnvRec{Name: "pr-1", Index: 0, BlockBase: 20000, CreatedAt: time.Now()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := reg.Get("pr-1")
	if !ok || rec.BlockBase != 20000 {
		t.Fatalf("roundtrip failed: %+v", rec)
	}
	if got := reg.FreeIndex(); got != 1 {
		t.Fatalf("FreeIndex = %d, want 1", got)
	}
}

func TestConcurrentUpdatesSerialize(t *testing.T) {
	s := &Store{Home: filepath.Join(t.TempDir(), "home")}
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a'+i%26)) + string(rune('0'+i/26))
			_ = s.Update(func(r *Registry) error {
				r.Tombstones = append(r.Tombstones, Tombstone{Name: name})
				return nil
			})
		}(i)
	}
	wg.Wait()
	reg, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Tombstones) != n {
		t.Fatalf("lost updates under concurrency: got %d tombstones, want %d", len(reg.Tombstones), n)
	}
}
