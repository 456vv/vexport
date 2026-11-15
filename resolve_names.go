package main

import (
	"fmt"
	"go/types"
)

// unitNames returns every package level name defined by one emission unit:
// the selected declaration itself plus all sibling names of a multi-name spec
// or an iota block. The order is stable and the primary name comes first.
func (s *Selection) unitNames(d *Decl) []string {
	names := make([]string, 0, 1+len(d.siblingNames()))
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" || name == "_" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	add(d.Name)
	for _, name := range d.siblingNames() {
		add(name)
	}
	return names
}

func (s *Selection) sameUnit(a, b *Decl) bool {
	if a == nil || b == nil {
		return false
	}
	return a == b || a.key() == b.key()
}

// unitNameFree is nameFree extended to an entire emission unit. A sibling
// declaration belongs to the same unit, so the output name it already owns is
// not a conflict.
func (s *Selection) unitNameFree(name string, d *Decl) bool {
	if o, ok := s.owner[name]; ok {
		return s.sameUnit(o, d)
	}
	if s.Prog.Root != nil {
		if td, ok := s.Prog.Root.Idx.byName[name]; ok && !s.sameUnit(td, d) {
			return false
		}
	}
	return true
}

// siblingObject maps a sibling name back to its package level object, which is
// required when -conflict prefix has to rename a sibling inside the copied
// spec/block.
func (s *Selection) siblingObject(d *Decl, name string) types.Object {
	if d == nil || d.Idx == nil {
		return nil
	}
	if sd, ok := d.Idx.byName[name]; ok && s.sameUnit(sd, d) {
		return sd.Obj
	}
	return nil
}

// assignUnitNames resolves every name in one emission unit before anything is
// committed. This is the core fix for duplicate definitions: a multi-name spec
// or iota block cannot be admitted partially.
func (s *Selection) assignUnitNames(d *Decl) (primary string, names map[string]string, keep bool, err error) {
	if d.Kind == "method" {
		// Methods are not package level names; their receiver type is handled
		// separately.
		return d.Name, nil, false, nil
	}

	all := s.unitNames(d)
	chosen := make(map[string]string, len(all))
	reserved := map[string]bool{}

	for _, old := range all {
		cand := old
		if !s.unitNameFree(cand, d) || reserved[cand] {
			switch s.cfg.Conflict {
			case ConflictError:
				return "", nil, false, fmt.Errorf("name %q from %s clashes with %s",
					old, d.pkg().Path, s.ownerName(old))
			case ConflictPrefix:
				prefix := capitalize(d.pkg().Name)
				cand = prefix + old
				for i := 2; !s.unitNameFree(cand, d) || reserved[cand]; i++ {
					cand = prefix + old + itoa(i)
				}
				s.Notes = append(s.Notes, fmt.Sprintf(
					"renamed %s.%s to %s", d.pkg().Name, old, cand))
			default:
				// The whole unit has to stay an import. Returning early avoids
				// registering a partial set of names before the decision.
				return "", nil, true, nil
			}
		}
		chosen[old] = cand
		reserved[cand] = true
	}

	// A sibling rename needs an object so callerRename can rewrite both the
	// declaration and all references to it. Missing objects are an internal
	// inconsistency, not a partial inline.
	for old, newName := range chosen {
		if old == d.Name || old == newName {
			continue
		}
		if s.siblingObject(d, old) == nil {
			return "", nil, false, fmt.Errorf(
				"cannot rename %s.%s: sibling object is not indexed", d.pkg().Path, old)
		}
	}
	return chosen[d.Name], chosen, false, nil
}
