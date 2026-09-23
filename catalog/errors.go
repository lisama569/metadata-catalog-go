package catalog

import "fmt"

// RejectReason 可区分、可持久化的拒绝原因。
type RejectReason string

const (
	// ReasonStaleVersion 乱序到达的较旧版本，不得覆盖较新版本。
	ReasonStaleVersion RejectReason = "STALE_VERSION"
	// ReasonVersionConflict 同版本但内容不同（并发更新冲突）。
	ReasonVersionConflict RejectReason = "VERSION_CONFLICT"
	// ReasonDuplicate 完全相同的重复提交（幂等，不算错误，但会记录）。
	ReasonDuplicate RejectReason = "DUPLICATE"
	// ReasonDanglingReference 引用了不存在的实体。
	ReasonDanglingReference RejectReason = "DANGLING_REFERENCE"
	// ReasonSchemaIncompatible 结构演进不兼容。
	ReasonSchemaIncompatible RejectReason = "SCHEMA_INCOMPATIBLE"
	// ReasonMergeConflict 合并时双方存在无法调和的冲突。
	ReasonMergeConflict RejectReason = "MERGE_CONFLICT"
	// ReasonEntityMerged 目标实体已被合并，拒绝直接写入。
	ReasonEntityMerged RejectReason = "ENTITY_MERGED"
	// ReasonEntityNotFound 实体不存在。
	ReasonEntityNotFound RejectReason = "ENTITY_NOT_FOUND"
)

// Rejection 一次被拒绝的写入及其持久化上下文。
type Rejection struct {
	Reason   RejectReason `json:"reason"`
	EntityID string       `json:"entity_id,omitempty"`
	Relation string       `json:"relation_id,omitempty"`
	Source   string       `json:"source"`
	Version  int64        `json:"version"`
	Detail   string       `json:"detail"`
	Seq      int64        `json:"seq"` // 目录内单调递增序号，保证可追溯顺序
}

// Error 实现 error 接口。
func (r *Rejection) Error() string {
	return fmt.Sprintf("rejected(%s): entity=%s relation=%s source=%s version=%d detail=%s",
		r.Reason, r.EntityID, r.Relation, r.Source, r.Version, r.Detail)
}
