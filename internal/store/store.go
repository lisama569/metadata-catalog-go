// Package store provides an append-only JSONL journal with replay. Accepted
// mutations and rejected decisions are both durable, so retries after a
// failure converge to the same state and rejection causes survive restarts.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/lisama569/metadata-catalog-go/internal/model"
)

// EntryType identifies a journal entry kind.
type EntryType string

const (
	EntryEntity   EntryType = "entity"
	EntryRel      EntryType = "rel"
	EntryMerge    EntryType = "merge"
	EntryDecision EntryType = "decision"
)

// MergeEntry records an applied entity merge.
type MergeEntry struct {
	SurvivorID string        `json:"survivorId"`
	VictimID   string        `json:"victimId"`
	Survivor   model.Entity  `json:"survivor"`
	Redirects  []RelRedirect `json:"redirects"`
	Source     string        `json:"source"`
}

// RelRedirect is one repointed relationship endpoint.
type RelRedirect struct {
	RelKey     string `json:"relKey"`
	Upstream   string `json:"upstream"`
	Downstream string `json:"downstream"`
}

// Entry is one journal record with a monotonic sequence number.
type Entry struct {
	Seq      int64                 `json:"seq"`
	Type     EntryType             `json:"type"`
	Decision *model.DecisionRecord `json:"decision,omitempty"`
	Entity   *model.Entity         `json:"entity,omitempty"`
	Rel      *model.Relationship   `json:"rel,omitempty"`
	Merge    *MergeEntry           `json:"merge,omitempty"`
}

// Store is the durable append-only journal. A zero path selects in-memory mode.
type Store struct {
	mu   sync.Mutex
	path string
	f    *os.File
	w    *bufio.Writer
	seq  int64

	mem [][]byte // in-memory mode only
}

// Open opens or creates the journal at path and restores the seq counter.
// An empty path creates an in-memory journal (useful for tests).
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if path == "" {
		return s, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open journal %q: %w", path, err)
	}
	s.f = f
	s.w = bufio.NewWriter(f)

	var maxSeq int64 = -1
	err = ReplayFile(path, func(e Entry) error {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
		return nil
	})
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	s.seq = maxSeq + 1
	return s, nil
}

// Append writes one entry, assigning the next sequence number, and fsyncs.
// The caller never needs to set Seq itself.
func (s *Store) Append(e *Entry) (int64, error) {
	if e == nil {
		return 0, errors.New("nil entry")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e.Seq = s.seq
	s.seq++

	line, err := encodeEntry(e)
	if err != nil {
		return 0, err
	}
	line = append(line, '\n')

	if s.f != nil {
		if _, err := s.w.Write(line); err != nil {
			return 0, err
		}
		if err := s.w.Flush(); err != nil {
			return 0, err
		}
		if err := s.f.Sync(); err != nil {
			return 0, err
		}
	} else {
		buf := bytes.Clone(line)
		s.mem = append(s.mem, buf)
	}
	return e.Seq, nil
}

// LastSeq returns the next sequence number that will be assigned.
func (s *Store) LastSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Replay streams all entries through fn in sequence order.
func (s *Store) Replay(fn func(Entry) error) error {
	if s.f != nil {
		return ReplayFile(s.path, fn)
	}
	s.mu.Lock()
	lines := make([][]byte, len(s.mem))
	copy(lines, s.mem)
	s.mu.Unlock()
	for _, line := range lines {
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

// ReplayFile reads every JSON line from path in order.
func ReplayFile(path string, fn func(Entry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var e Entry
			if jerr := json.Unmarshal(bytes.TrimSpace(line), &e); jerr != nil {
				return fmt.Errorf("corrupt journal line: %w", jerr)
			}
			if ferr := fn(e); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Close flushes and closes the journal.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	return s.f.Close()
}

// encodeEntry marshals one canonical JSON object per line.
func encodeEntry(e *Entry) ([]byte, error) {
	return json.Marshal(e)
}
