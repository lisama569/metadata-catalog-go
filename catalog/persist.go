package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// persistedState 持久化到磁盘的完整目录状态。
// 采用整体快照 + 原子替换：每次状态变更后全量落盘，
// 重启时一次性恢复，保证恢复结果与重启前完全一致。
type persistedState struct {
	Entities   []Entity    `json:"entities"`
	Relations  []Relation  `json:"relations"`
	Rejections []Rejection `json:"rejections"`
	Seq        int64       `json:"seq"`
}

// OpenStore 打开（不存在则创建）path 对应的持久化存储。
// 已提交成功的实体、关系与全部拒绝记录在重启后完整恢复，
// 恢复后的单调序号 Seq 接续重启前，不会重复计数。
func OpenStore(path string) (*Store, error) {
	s := NewStore()
	s.path = path
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil // 首次启动：空状态，首次变更时落盘
	}
	if err != nil {
		return nil, fmt.Errorf("open state file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("decode state file %s: %w", path, err)
	}
	for _, e := range st.Entities {
		cp := e
		s.entities[e.ID] = &cp
	}
	for _, r := range st.Relations {
		cp := r
		s.relations[r.ID] = &cp
	}
	s.rejections = append(s.rejections, st.Rejections...)
	s.seq = st.Seq
	return s, nil
}

// persistLocked 将当前状态原子写入磁盘（调用方需持有锁）。
// path 为空（纯内存存储）时直接成功。先写临时文件再 rename，
// 崩溃时最多丢失最后一次变更，不会留下半截文件。
func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	st := persistedState{
		Entities:   make([]Entity, 0, len(s.entities)),
		Relations:  make([]Relation, 0, len(s.relations)),
		Rejections: s.rejections,
		Seq:        s.seq,
	}
	for _, e := range s.entities {
		st.Entities = append(st.Entities, *e)
	}
	for _, r := range s.relations {
		st.Relations = append(st.Relations, *r)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	// 尽力而为地落盘目录项，保证重启后 rename 结果可见。
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
