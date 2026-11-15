// Package account is the caller-rewrite fixture. It declares its own User so
// that inlining model.User under -conflict prefix renames it to ModelUser, and
// it embeds model.User so that the rename also changes the embedded field name.
package account

import "demo/internal/model"

// User is the account facing view of a user.
type User struct {
	Name string
}

// Holder embeds model.User. The embedded field is named after the type, so
// renaming model.User also renames the field references to it.
type Holder struct {
	model.User
	Tag string
}

// Label reads the promoted field by its embedded name.
func (h Holder) Label() string { return h.User.Name + ":" + h.Tag }

// NewHolder builds a holder, setting the promoted field by name.
func NewHolder(u model.User) Holder { return Holder{User: u, Tag: "h"} }

// KindString calls a method of an inlined type.
func KindString(k model.Kind) string { return k.String() }
