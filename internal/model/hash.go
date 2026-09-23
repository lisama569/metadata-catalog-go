package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// contentBody is the canonical, order-independent body used for duplicate
// detection. Version and UpdatedAt are deliberately excluded: two
// submissions with identical business content are duplicates even if their
// timestamps differ.
type contentBody struct {
	ID       string            `json:"id"`
	TypeName string            `json:"typeName"`
	Source   string            `json:"source"`
	Schema   Schema            `json:"schema"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	Tags     []string          `json:"tags,omitempty"`
	Owner    string            `json:"owner,omitempty"`
	Terms    []string          `json:"terms,omitempty"`
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func sortedAttrs(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// canonicalJSON marshals with sorted keys (Go encoding/json sorts map keys
// already) and sorted slices, yielding stable bytes.
func canonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// EntityFingerprint returns a stable SHA-256 of the entity's business content.
func EntityFingerprint(e Entity) string {
	body := contentBody{
		ID:       e.ID,
		TypeName: e.TypeName,
		Source:   e.Source,
		Schema:   NormalizeSchema(e.Schema),
		Attrs:    sortedAttrs(e.Attrs),
		Tags:     sortedCopy(e.Tags),
		Owner:    e.Owner,
		Terms:    sortedCopy(e.Terms),
	}
	sum := sha256.Sum256(canonicalJSON(body))
	return hex.EncodeToString(sum[:])
}

// NormalizeSchema returns a schema with fields in stable declaration order
// and rename references trimmed.
func NormalizeSchema(s Schema) Schema {
	out := Schema{Version: s.Version}
	out.Fields = append(out.Fields, s.Fields...)
	return out
}

// SchemaFingerprint hashes a schema's fields independent of its version.
func SchemaFingerprint(s Schema) string {
	type fieldBody struct {
		Name     string    `json:"name"`
		Type     FieldType `json:"type"`
		Required bool      `json:"required"`
		RenameOf string    `json:"renameOf,omitempty"`
	}
	bodies := make([]fieldBody, 0, len(s.Fields))
	for _, f := range s.Fields {
		bodies = append(bodies, fieldBody{
			Name: f.Name, Type: f.Type, Required: f.Required, RenameOf: f.RenameOf,
		})
	}
	sum := sha256.Sum256(canonicalJSON(bodies))
	return hex.EncodeToString(sum[:])
}

// RelFingerprint hashes a relationship's business content (excluding
// version/timestamp/key).
func RelFingerprint(r Relationship) string {
	body := struct {
		Upstream   string  `json:"upstream"`
		Downstream string  `json:"downstream"`
		Kind       RelKind `json:"kind"`
		Source     string  `json:"source"`
	}{r.Upstream, r.Downstream, r.Kind, r.Source}
	sum := sha256.Sum256(canonicalJSON(body))
	return hex.EncodeToString(sum[:])
}
