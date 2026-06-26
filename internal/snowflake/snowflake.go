// Package snowflake provides Discord snowflake ID helpers.
//
// Discord snowflakes are 64-bit integers encoding a Unix millisecond
// timestamp, an internal worker ID, an internal process ID, and an
// increment. The timestamp is the first 42 bits after shifting right
// by 22. The epoch is the first second of 2015 UTC.
package snowflake

import "time"

// DiscordEpochMS is the start of Discord's snowflake epoch
// (2015-01-01T00:00:00Z) in milliseconds since Unix epoch.
const DiscordEpochMS int64 = 1420070400000

// At returns a snowflake whose timestamp is t.
// Used as a max_id or min_id bound for API calls.
func At(t time.Time) int64 {
	return (t.UnixMilli() - DiscordEpochMS) << 22
}

// Timestamp extracts the creation time from a snowflake.
func Timestamp(sf int64) time.Time {
	ms := (sf >> 22) + DiscordEpochMS
	return time.UnixMilli(ms).UTC()
}

// Before returns a snowflake bound for messages older than d.
func Before(d time.Duration) int64 {
	return At(time.Now().UTC().Add(-d))
}
