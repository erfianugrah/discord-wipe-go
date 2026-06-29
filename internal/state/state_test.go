package state

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Mark("100")
	s.Mark("200")
	s.SetExportConsumed(true)
	s.LastPassAt = "2026-06-29T00:00:00Z"
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s2, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Len() != 2 || !s2.IsDeleted("100") || !s2.IsDeleted("200") {
		t.Fatalf("deleted set not round-tripped: len=%d", s2.Len())
	}
	if !s2.IsExportConsumed() {
		t.Fatal("export_consumed not round-tripped")
	}
	if s2.LastPassAt != "2026-06-29T00:00:00Z" {
		t.Fatalf("last_pass_at=%q", s2.LastPassAt)
	}
}

// Anti-GC guard (the v0.3.0 Python footgun): IDs we just deleted have OLD
// snowflake timestamps by definition (we only deleted them because they were
// past the retention cutoff). Any snowflake-age-based GC would sweep the
// just-deleted set and re-attempt 100% of them next pass. State must keep
// every ID regardless of age, and must expose NO gc method.
func TestOldSnowflakeIDsSurviveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	s, _ := New(p)
	// 53313060000000000 ~ a 2018-era snowflake (very old).
	s.Mark("53313060000000000")
	s.Save() //nolint:errcheck
	s2, _ := New(p)
	if !s2.IsDeleted("53313060000000000") {
		t.Fatal("old-snowflake ID was dropped (snowflake-age GC regression)")
	}
}

func TestStateHasNoGCMethod(t *testing.T) {
	typ := reflect.TypeOf(&State{})
	for _, name := range []string{"GC", "Gc", "Prune", "Compact", "Trim"} {
		if _, ok := typ.MethodByName(name); ok {
			t.Fatalf("State exposes a %q method \u2014 snowflake-age GC must not be reintroduced", name)
		}
	}
}

// Bug12 durability: a 0-byte state.json (torn write / SIGKILL inside the
// writeback window on Unraid shfs) must NOT erase a completed wipe. load()
// falls back to state.json.bak.
func TestZeroByteStateFallsBackToBak(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")

	s, _ := New(p)
	s.Mark("777")
	s.SetExportConsumed(true)
	s.Save() //nolint:errcheck // writes state.json
	s.Mark("888")
	s.Save() //nolint:errcheck // rotates state.json -> .bak, writes new state.json

	// Simulate a torn write: truncate state.json to 0 bytes.
	if err := os.WriteFile(p, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	recovered, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	// .bak held the previous good copy ({777}); the torn write loses at most
	// the last delta (888), never the whole set.
	if !recovered.IsDeleted("777") {
		t.Fatal("0-byte state.json wiped the deleted set; .bak fallback failed")
	}
	if !recovered.IsExportConsumed() {
		t.Fatal("export_consumed lost after 0-byte truncation")
	}
}

func TestCorruptStateIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte(`{ broken`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(p); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "state.json.corrupt-*"))
	if len(matches) == 0 {
		t.Fatal("corrupt state.json was not quarantined to a .corrupt-* backup")
	}
}

// Guards the data race fixed by the RWMutex: the /metrics goroutine reads
// Len()/IsExportConsumed() while the wipe loop calls Mark()/SetExportConsumed().
// Run with -race (CI does).
func TestConcurrentMarkAndReadNoRace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	s, _ := New(p)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			s.Mark(string(rune(i)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = s.Len()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			s.SetExportConsumed(i%2 == 0)
			_ = s.IsExportConsumed()
		}
	}()
	wg.Wait()
}
