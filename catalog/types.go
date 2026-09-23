// Package catalog 提供元数据目录的核心模型与存储能力。
package catalog

// FieldType 字段类型。
type FieldType string

const (
	FieldTypeString    FieldType = "string"
	FieldTypeInt       FieldType = "int"
	FieldTypeFloat     FieldType = "float"
	FieldTypeBool      FieldType = "bool"
	FieldTypeTimestamp FieldType = "timestamp"
	FieldTypeBytes     FieldType = "bytes"
)

// Field 实体结构中的一个字段。
type Field struct {
	Name     string    `json:"name"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required"`
}

// Schema 实体的结构定义。
type Schema struct {
	Fields []Field `json:"fields"`
}

// Entity 元数据实体。
type Entity struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Source     string   `json:"source"`
	Version    int64    `json:"version"`
	Schema     Schema   `json:"schema"`
	Tags       []string `json:"tags,omitempty"`
	Terms      []string `json:"terms,omitempty"`
	Owners     []string `json:"owners,omitempty"`
	MergedInto string   `json:"merged_into,omitempty"`
}

// RelationType 关系类型。
type RelationType string

const (
	// RelationLineage 表示 src -> dst 的血缘（src 是 dst 的上游）。
	RelationLineage RelationType = "lineage"
	// RelationReference 表示一般引用关系。
	RelationReference RelationType = "reference"
)

// Relation 实体间的有向关系。
type Relation struct {
	ID      string       `json:"id"`
	Type    RelationType `json:"type"`
	SrcID   string       `json:"src_id"`
	DstID   string       `json:"dst_id"`
	Source  string       `json:"source"`
	Version int64        `json:"version"`
}

// RelationID 生成确定性的关系 ID，保证重复摄入幂等。
func RelationID(t RelationType, src, dst string) string {
	return string(t) + "|" + src + "->" + dst
}
