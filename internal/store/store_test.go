package store

import (
	"path/filepath"
	"testing"

	"github.com/lisama569/metadata-catalog-go/internal/model"
)

func TestInMemoryAppendAndReplay(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 3; i++ {
		seq, err := s.Append(&Entry{Type: EntryDecision, Decision: &model.DecisionRecord{Subject: "x"}})
		if err != nil {
			t.Fatal(err)
		}
		if seq != int64(i) {
			t.Fatalf("seq=%d want %d", seq, i)
		}
	}
	count := 0
	if err := s.Replay(func(e Entry) error {
		if e.Seq != int64(count) {
			t.Fatalf("replay order seq=%d want %d", e.Seq, count)
		}
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("replayed %d want 3", count)
	}
	t.Logf("in-memory replay %d entries, next seq=%d", count, s.LastSeq())
}

func TestFileReplayRestoresSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	s1, _ := Open(path)
	s1.Append(&Entry{Type: EntryEntity, Entity: &model.Entity{ID: "a", Version: 1}})
	s1.Append(&Entry{Type: EntryRel, Rel: &model.Relationship{Key: "k", Upstream: "a", Downstream: "b"}})
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.LastSeq() != 2 {
		t.Fatalf("after reopen lastSeq=%d want 2", s2.LastSeq())
	}
	var types []EntryType
	s2.Replay(func(e Entry) error { types = append(types, e.Type); return nil })
	if len(types) != 2 || types[0] != EntryEntity || types[1] != EntryRel {
		t.Fatalf("replayed types=%v", types)
	}
	seq, _ := s2.Append(&Entry{Type: EntryDecision, Decision: &model.DecisionRecord{}})
	if seq != 2 {
		t.Fatalf("post-reopen append seq=%d want 2", seq)
	}
	t.Logf("file replay restored seq counter, new entry seq=%d", seq)
}
