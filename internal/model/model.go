// Package model defines the core metadata catalog data model: entities,
// relationships, schema definitions and durable decision records.
package model

import "time"

// FieldType enumerates the primitive field types supported by a Schema.
type FieldType string

// Supported primitive field types.
const (
	TypeString  FieldType = "string"
	TypeInt     FieldType = "int"
	TypeFloat   FieldType = "float"
	TypeBool    FieldType = "bool"
	TypeDate    FieldType = "date"
	TypeRef     FieldType = "ref" // reference to another entity ID
	TypeUnknown FieldType = ""
)

// Field describes a single field of an entity schema.
type Field struct {
	Name     string    `json:"name"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required"`
	// RenameOf marks this field as the renamed successor of another field.
	RenameOf string `json:"renameOf,omitempty"`
}

// Schema is the structural definition of an entity, versioned by Version.
type Schema struct {
	Version int     `json:"version"`
	Fields  []Field `json:"fields"`
}

// Entity is a versioned metadata record.
type Entity struct {
	ID        string            `json:"id"`
	Version   int               `json:"version"`
	TypeName  string            `json:"typeName"`
	Source    string            `json:"source"`
	Schema    Schema            `json:"schema"`
	Attrs     map[string]string `json:"attrs,omitempty"`
	Tags      []string          `json:"tags,omitempty"`
	Owner     string            `json:"owner,omitempty"`
	Terms     []string          `json:"terms,omitempty"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// RelKind enumerates lineage relationship kinds.
type RelKind string

const (
	RelDerivedFrom RelKind = "derived_from"
	RelDependsOn   RelKind = "depends_on"
	RelProduces    RelKind = "produces"
)

// Relationship is a directed lineage edge: Downstream depends on Upstream
// (data flows Upstream -> Downstream).
type Relationship struct {
	Key        string    `json:"key"`
	Upstream   string    `json:"upstream"`
	Downstream string    `json:"downstream"`
	Kind       RelKind   `json:"kind"`
	Source     string    `json:"source"`
	Version    int       `json:"version"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Decision is the machine-distinguishable, persisted outcome of a write attempt.
type Decision string

const (
	// Accepted decisions.
	DecisionAccepted     Decision = "accepted"
	DecisionDuplicate    Decision = "duplicate"
	DecisionAlreadyNewer Decision = "already_newer"

	// Rejected decisions.
	RejectStaleVersion    Decision = "reject_stale_version"
	RejectOutOfOrderGap   Decision = "reject_out_of_order_gap"
	RejectContentConflict Decision = "reject_content_conflict"
	RejectSchemaIncompat  Decision = "reject_schema_incompatible"
	RejectMergeConflict   Decision = "reject_merge_conflict"
	RejectCycle           Decision = "reject_cycle"
	RejectValidation      Decision = "reject_validation"
)

// Op is the kind of catalog operation a decision applies to.
type Op string

const (
	OpIngestEntity Op = "ingest_entity"
	OpIngestRel    Op = "ingest_rel"
	OpEvolve       Op = "evolve_schema"
	OpMerge        Op = "merge"
)

// DecisionRecord is the durable, replayable record of one attempted write.
type DecisionRecord struct {
	Seq       int64          `json:"seq"`
	Op        Op             `json:"op"`
	Decision  Decision       `json:"decision"`
	Accepted  bool           `json:"accepted"`
	Reason    string         `json:"reason"`
	RuleID    string         `json:"ruleId,omitempty"`
	Subject   string         `json:"subject"`
	Source    string         `json:"source,omitempty"`
	Version   int            `json:"version,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
	Submitted any            `json:"submitted,omitempty"`
	Time      time.Time      `json:"time"`
}
