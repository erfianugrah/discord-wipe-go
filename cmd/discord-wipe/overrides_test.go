package main

import (
	"testing"
	"time"

	"github.com/erfianugrah/discord-wipe-go/internal/snowflake"
)

func TestParseRetentionOverrides(t *testing.T) {
	valid := []struct {
		name    string
		entries []string
		wantKey string
		wantDay float64
	}{
		{"guild", []string{"guild:1234567890123456789:2"}, "guild:1234567890123456789", 2},
		{"channel", []string{"channel:213906555051966475:30"}, "channel:213906555051966475", 30},
		{"fractional", []string{"guild:123:0.5"}, "guild:123", 0.5},
		{"zero", []string{"guild:123:0"}, "guild:123", 0},
		{"whitespace", []string{"  guild:123:7  "}, "guild:123", 7},
		{"empty entries skipped", []string{"", " ", "guild:123:7"}, "guild:123", 7},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseRetentionOverrides(tc.entries)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got, ok := m[tc.wantKey]; !ok || got != tc.wantDay {
				t.Fatalf("m[%q] = %v,%v; want %v,true (map=%v)", tc.wantKey, got, ok, tc.wantDay, m)
			}
		})
	}

	m, err := parseRetentionOverrides([]string{"guild:1:2", "channel:2:3", "guild:4:5"})
	if err != nil || len(m) != 3 {
		t.Fatalf("multi: got %v, err %v; want 3 entries", m, err)
	}

	invalid := []string{
		"guild:123",                       // missing days
		"guild:123:2:extra",               // too many parts
		"server:123:2",                    // bad scope
		"guild::2",                        // empty id
		"guild:abc:2",                     // non-numeric id
		"guild:123:two",                   // non-numeric days
		"guild:123:-1",                    // negative days
		"guild:12345678901234567890123:2", // overflows int64
	}
	for _, e := range invalid {
		if _, err := parseRetentionOverrides([]string{e}); err == nil {
			t.Fatalf("entry %q: expected error, got nil", e)
		}
	}
}

func TestTargetCutoffSF(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	overrides := map[string]float64{"guild:111": 2, "channel:222": 30}

	// Override applies on exact scope:id match.
	sf, days, ok := targetCutoffSF(now, 7, "guild", "111", overrides)
	if !ok || days != 2 {
		t.Fatalf("guild override: days=%v ok=%v, want 2,true", days, ok)
	}
	if want := snowflake.At(now.Add(-48 * time.Hour)); sf != want {
		t.Fatalf("guild override cutoffSF = %d, want %d (2d ago)", sf, want)
	}

	// No match falls back to the global window.
	sf, days, ok = targetCutoffSF(now, 7, "guild", "999", overrides)
	if ok || days != 7 {
		t.Fatalf("non-matching guild: days=%v ok=%v, want 7,false", days, ok)
	}
	if want := snowflake.At(now.Add(-7 * 24 * time.Hour)); sf != want {
		t.Fatalf("global cutoffSF = %d, want %d (7d ago)", sf, want)
	}

	// Same ID under a different scope must NOT match.
	if _, _, ok = targetCutoffSF(now, 7, "channel", "111", overrides); ok {
		t.Fatal("channel:111 matched guild:111 override - scope must be part of the key")
	}

	// Longer-than-global override yields an OLDER cutoff (larger window).
	sf7, _, _ := targetCutoffSF(now, 7, "guild", "999", overrides)
	sf30, _, _ := targetCutoffSF(now, 7, "channel", "222", overrides)
	if sf30 >= sf7 {
		t.Fatalf("30d override cutoff %d should be older (smaller) than 7d cutoff %d", sf30, sf7)
	}
}

// TestResolveSliceEnv mirrors the shared-global guard tests: with the flag
// unset, the value must come from RETENTION_OVERRIDES (comma-separated).
func TestResolveSliceEnv(t *testing.T) {
	t.Setenv("RETENTION_OVERRIDES", "")
	if got := resolveSlice(cmdRun, "retention-override", "RETENTION_OVERRIDES"); got != nil {
		t.Fatalf("empty env: got %v, want nil", got)
	}
	t.Setenv("RETENTION_OVERRIDES", "guild:1:2, channel:2:3,,")
	got := resolveSlice(cmdRun, "retention-override", "RETENTION_OVERRIDES")
	if len(got) != 2 || got[0] != "guild:1:2" || got[1] != "channel:2:3" {
		t.Fatalf("env split: got %v, want [guild:1:2 channel:2:3]", got)
	}
}
