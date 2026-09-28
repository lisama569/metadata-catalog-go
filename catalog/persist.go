package catalog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// 持久化设计：预写日志（WAL）。
// 每一次真正改变状态的写入（实体/关系应用、关系删除、拒绝记录）都先追加到
// <dir>/wal.jsonl 并 fsync，再对内存状态生效的等价位置落盘；重启时按序重放，
// 恢复出与重启前一致的状态（包括拒绝日志与其 Seq）。
//
// 重放是确定性的：日志里记录的是"应用后的状态"（实体/关系的完整快照），
// 而不是需要重新判定的操作，因此规则演进不会改变已恢复的历史结论。

// walEventType 日志事件类别。
type walEventType string

const (
	walEntity         walEventType = "entity"          // 实体状态被应用
	walRelation       walEventType = "relation"        // 关系状态被应用
	walRelationDelete walEventType = "relation_delete" // 关系被删除（合并重定向）
	walRejection      walEventType = "rejection"       // 拒绝记录
)

// walEvent 一条 WAL 记录（JSON Lines 中的一行）。
type walEvent struct {
	Type       walEventType `json:"type"`
	Entity     *Entity      `json:"entity,omitempty"`
	Relation   *Relation    `json:"relation,omitempty"`
	RelationID string       `json:"relation_id,omitempty"`
	Rejection  *Rejection   `json:"rejection,omitempty"`
}

// wal 预写日志：追加写 + fsync，保证已确认的写入重启后可恢复。
type wal struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// walFileName 数据目录中的日志文件名。
const walFileName = "wal.jsonl"

// openWAL 打开（不存在则创建）数据目录下的 WAL 文件，返回句柄与重放读取器。
func openWAL(dir string) (*wal, []walEvent, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dir, walFileName)

	// 先读全部历史事件。
	events, err := readWALEvents(path)
	if err != nil {
		return nil, nil, err
	}

	// 再以追加模式打开，供后续写入。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open wal: %w", err)
	}
	return &wal{f: f, enc: json.NewEncoder(f)}, events, nil
}

// readWALEvents 读取日志中的全部事件；容忍最后一条记录不完整（进程在
// fsync 前崩溃），其余损坏返回错误。
func readWALEvents(path string) ([]walEvent, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open wal for replay: %w", err)
	}
	defer f.Close()

	var events []walEvent
	r := bufio.NewReader(f)
	line := 0
	for {
		b, err := r.ReadBytes('\n')
		if len(b) > 0 {
			line++
			var ev walEvent
			if jerr := json.Unmarshal(b, &ev); jerr != nil {
				if err == io.EOF {
					// 末尾半行：崩溃截断，丢弃。
					break
				}
				return nil, fmt.Errorf("wal line %d corrupt: %w", line, jerr)
			}
			events = append(events, ev)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read wal: %w", err)
		}
	}
	return events, nil
}

// append 追加一条事件并 fsync；失败时返回错误（调用方按致命错误处理）。
func (w *wal) append(ev walEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(ev); err != nil {
		return fmt.Errorf("encode wal event: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("fsync wal: %w", err)
	}
	return nil
}

// close 关闭日志文件。
func (w *wal) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
