package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/erfianugrah/discord-wipe-go/internal/discord"
	"github.com/erfianugrah/discord-wipe-go/internal/state"
)

// fakeDrainer records the call sequence for drain tests.
type fakeDrainer struct {
	calls      []string
	unarchive  discord.DeleteResult // returned for SetThreadArchived(ch, false)
	rearchive  discord.DeleteResult // returned for SetThreadArchived(ch, true)
	deleteSt   string               // returned for DeleteMessage
	deleteSeen []string
}

func (f *fakeDrainer) SetThreadArchived(ch string, archived bool) (discord.DeleteResult, error) {
	if archived {
		f.calls = append(f.calls, "rearchive:"+ch)
		return f.rearchive, nil
	}
	f.calls = append(f.calls, "unarchive:"+ch)
	return f.unarchive, nil
}

func (f *fakeDrainer) DeleteMessage(ch, msg string) (discord.DeleteResult, error) {
	f.calls = append(f.calls, "delete:"+ch+":"+msg)
	f.deleteSeen = append(f.deleteSeen, msg)
	return discord.DeleteResult{Status: f.deleteSt}, nil
}

// Bug13: messages in archived threads must actually get deleted via the
// unarchive -> delete -> re-archive sequence, then marked in state.
func TestDrainArchivedThreadsSequence(t *testing.T) {
	st, err := state.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDrainer{
		unarchive: discord.DeleteResult{Status: "ok"},
		rearchive: discord.DeleteResult{Status: "ok"},
		deleteSt:  "ok",
	}
	pending := map[string][]string{"T1": {"M1", "M2"}}
	var counts catchupCounts

	if err := drainArchivedThreads(f, st, pending, &counts, 0*time.Second); err != nil {
		t.Fatal(err)
	}
	want := []string{"unarchive:T1", "delete:T1:M1", "delete:T1:M2", "rearchive:T1"}
	if len(f.calls) != len(want) {
		t.Fatalf("calls=%v, want %v", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Fatalf("calls=%v, want %v", f.calls, want)
		}
	}
	if counts.ok != 2 {
		t.Fatalf("ok=%d, want 2", counts.ok)
	}
	if !st.IsDeleted("M1") || !st.IsDeleted("M2") {
		t.Fatal("deleted-in-thread messages must be marked in state")
	}
}

// Bug13 companion: when the thread cannot be unarchived (no permission /
// locked), the messages are counted forbidden but NOT marked done - a later
// pass must be able to retry them.
func TestDrainArchivedThreadsCannotUnarchiveLeavesUnmarked(t *testing.T) {
	st, err := state.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDrainer{
		unarchive: discord.DeleteResult{Status: "forbidden"},
		rearchive: discord.DeleteResult{Status: "ok"},
		deleteSt:  "ok",
	}
	pending := map[string][]string{"T1": {"M1"}}
	var counts catchupCounts

	if err := drainArchivedThreads(f, st, pending, &counts, 0*time.Second); err != nil {
		t.Fatal(err)
	}
	if counts.forbidden != 1 {
		t.Fatalf("forbidden=%d, want 1", counts.forbidden)
	}
	if st.IsDeleted("M1") {
		t.Fatal("message in un-unarchivable thread must NOT be marked done")
	}
	for _, c := range f.calls {
		if c == "rearchive:T1" {
			t.Fatal("must not re-archive a thread that was never unarchived")
		}
		if len(c) >= 6 && c[:6] == "delete" {
			t.Fatal("must not attempt deletes in a thread that stayed archived")
		}
	}
}
