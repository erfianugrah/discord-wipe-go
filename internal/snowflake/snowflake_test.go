package snowflake

import (
	"testing"
	"time"
)

func TestAtTimestampRoundTrip(t *testing.T) {
	want := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	got := Timestamp(At(want))
	if !got.Equal(want) {
		t.Fatalf("round-trip: got %s, want %s", got, want)
	}
}

// At is the retention bound used as max_id: an older cutoff time must yield a
// smaller snowflake (Discord returns messages with id <= max_id).
func TestAtIsMonotonic(t *testing.T) {
	older := At(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := At(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if !(older < newer) {
		t.Fatalf("expected older(%d) < newer(%d)", older, newer)
	}
}

func TestEpochZeroPoint(t *testing.T) {
	epoch := time.UnixMilli(DiscordEpochMS).UTC()
	if At(epoch) != 0 {
		t.Fatalf("snowflake at Discord epoch = %d, want 0", At(epoch))
	}
}
