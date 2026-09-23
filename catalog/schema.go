package catalog

import "sort"

// ChangeKind 结构变更类别。
type ChangeKind string

const (
	ChangeFieldAdded    ChangeKind = "FIELD_ADDED"
	ChangeFieldRemoved  ChangeKind = "FIELD_REMOVED"
	ChangeFieldRenamed  ChangeKind = "FIELD_RENAMED"
	ChangeTypeChanged   ChangeKind = "TYPE_CHANGED"
	ChangeRequiredAdded ChangeKind = "REQUIRED_FIELD_ADDED"
)

// Change 单条结构变更及兼容性判定依据。
type Change struct {
	Kind       ChangeKind `json:"kind"`
	Field      string     `json:"field"`
	OldType    FieldType  `json:"old_type,omitempty"`
	NewType    FieldType  `json:"new_type,omitempty"`
	RenamedTo  string     `json:"renamed_to,omitempty"`
	Compatible bool       `json:"compatible"`
	Reason     string     `json:"reason"`
}

// Verdict 兼容性结论；规则不变时对相同输入可复现。
type Verdict struct {
	Compatible bool     `json:"compatible"`
	Changes    []Change `json:"changes"`
}

// RenameHint 显式字段改名声明（old -> new）。
type RenameHint struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// typeWidening 允许的类型加宽（旧 -> 新），其余类型变化均不兼容。
var typeWidening = map[FieldType][]FieldType{
	FieldTypeInt:   {FieldTypeFloat, FieldTypeString},
	FieldTypeFloat: {FieldTypeString},
	FieldTypeBool:  {FieldTypeString},
}

func wideningAllowed(from, to FieldType) bool {
	for _, t := range typeWidening[from] {
		if t == to {
			return true
		}
	}
	return false
}

// CheckCompatibility 比较新旧结构，输出稳定的兼容性结论。
// renames 为调用方显式声明的改名；未声明的"删除+新增"按删字段处理。
// 判定规则（固定，保证可复现）：
//   - 新增可选字段：兼容
//   - 新增必填字段：不兼容（下游无法保证提供）
//   - 删除字段：不兼容（下游可能仍读取）
//   - 显式改名：兼容（视为可追溯的重命名）
//   - 类型加宽（int->float 等）：兼容；其余类型变化：不兼容
func CheckCompatibility(oldSchema, newSchema Schema, renames []RenameHint) Verdict {
	oldByName := map[string]Field{}
	for _, f := range oldSchema.Fields {
		oldByName[f.Name] = f
	}
	newByName := map[string]Field{}
	for _, f := range newSchema.Fields {
		newByName[f.Name] = f
	}
	renamedOld := map[string]string{} // old -> new
	renamedNew := map[string]string{} // new -> old
	for _, r := range renames {
		renamedOld[r.Old] = r.New
		renamedNew[r.New] = r.Old
	}

	var changes []Change

	// 旧字段：删除 / 改名 / 类型变化。
	for _, of := range oldSchema.Fields {
		if newName, ok := renamedOld[of.Name]; ok {
			nf, exists := newByName[newName]
			if !exists {
				changes = append(changes, Change{
					Kind: ChangeFieldRemoved, Field: of.Name, OldType: of.Type,
					Compatible: false,
					Reason:     "rename target missing in new schema; treated as removal",
				})
				continue
			}
			c := Change{Kind: ChangeFieldRenamed, Field: of.Name, RenamedTo: newName,
				OldType: of.Type, NewType: nf.Type, Compatible: true,
				Reason: "explicit rename declared"}
			changes = append(changes, c)
			if of.Type != nf.Type {
				changes = append(changes, typeChange(of.Name, of.Type, nf.Type))
			}
			continue
		}
		nf, exists := newByName[of.Name]
		if !exists {
			changes = append(changes, Change{
				Kind: ChangeFieldRemoved, Field: of.Name, OldType: of.Type,
				Compatible: false,
				Reason:     "field removed without rename declaration",
			})
			continue
		}
		if of.Type != nf.Type {
			changes = append(changes, typeChange(of.Name, of.Type, nf.Type))
		}
	}

	// 新字段：新增（跳过改名的目标）。
	for _, nf := range newSchema.Fields {
		if _, isRenameTarget := renamedNew[nf.Name]; isRenameTarget {
			continue
		}
		if _, exists := oldByName[nf.Name]; !exists {
			if nf.Required {
				changes = append(changes, Change{
					Kind: ChangeRequiredAdded, Field: nf.Name, NewType: nf.Type,
					Compatible: false,
					Reason:     "required field added; downstream may not provide it",
				})
			} else {
				changes = append(changes, Change{
					Kind: ChangeFieldAdded, Field: nf.Name, NewType: nf.Type,
					Compatible: true,
					Reason:     "optional field added",
				})
			}
		}
	}

	// 确定性排序：先按字段名，再按变更类别。
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Field != changes[j].Field {
			return changes[i].Field < changes[j].Field
		}
		return changes[i].Kind < changes[j].Kind
	})

	v := Verdict{Compatible: true, Changes: changes}
	for _, c := range changes {
		if !c.Compatible {
			v.Compatible = false
			break
		}
	}
	return v
}

func typeChange(field string, from, to FieldType) Change {
	if wideningAllowed(from, to) {
		return Change{Kind: ChangeTypeChanged, Field: field, OldType: from, NewType: to,
			Compatible: true, Reason: "widening type change allowed"}
	}
	return Change{Kind: ChangeTypeChanged, Field: field, OldType: from, NewType: to,
		Compatible: false, Reason: "narrowing or unrelated type change"}
}
