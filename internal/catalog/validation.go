package catalog

import (
	"fmt"
	"sort"

	"github.com/lisama569/metadata-catalog-go/internal/model"
)

// validateEntity returns stable, sorted validation error messages.
func validateEntity(e model.Entity) []string {
	errs := []string{}
	if e.ID == "" {
		errs = append(errs, "entity id is required")
	}
	if e.Version < 1 {
		errs = append(errs, "entity version must be >= 1")
	}
	if e.TypeName == "" {
		errs = append(errs, "entity typeName is required")
	}
	seen := map[string]bool{}
	for _, f := range e.Schema.Fields {
		if f.Name == "" {
			errs = append(errs, "schema field name is required")
			continue
		}
		if seen[f.Name] {
			errs = append(errs, fmt.Sprintf("duplicate schema field %q", f.Name))
		}
		seen[f.Name] = true
		if !validFieldType(f.Type) {
			errs = append(errs, fmt.Sprintf("field %q has unknown type %q", f.Name, f.Type))
		}
	}
	sort.Strings(errs)
	return errs
}

// validateRel returns stable validation error messages.
func validateRel(r model.Relationship) []string {
	errs := []string{}
	if r.Upstream == "" {
		errs = append(errs, "relationship upstream is required")
	}
	if r.Downstream == "" {
		errs = append(errs, "relationship downstream is required")
	}
	if r.Kind != model.RelDerivedFrom && r.Kind != model.RelDependsOn && r.Kind != model.RelProduces {
		errs = append(errs, fmt.Sprintf("unknown relationship kind %q", r.Kind))
	}
	if r.Version < 1 {
		errs = append(errs, "relationship version must be >= 1")
	}
	sort.Strings(errs)
	return errs
}

func validFieldType(t model.FieldType) bool {
	switch t {
	case model.TypeString, model.TypeInt, model.TypeFloat, model.TypeBool, model.TypeDate, model.TypeRef:
		return true
	default:
		return false
	}
}
