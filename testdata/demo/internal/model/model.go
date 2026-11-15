// Package model is the fixture source package: it has an iota const block,
// methods, a constructor, a package level error and platform specific files.
package model

import (
	"fmt"
	"strings"
	"time"
)

// Kind classifies users.
type Kind int

const (
	// KindAdmin can do everything.
	KindAdmin Kind = iota + 1
	// KindMember is a normal account.
	KindMember
	// KindGuest is read only.
	KindGuest
)

func (k Kind) String() string {
	switch k {
	case KindAdmin:
		return "admin"
	case KindMember:
		return "member"
	case KindGuest:
		return "guest"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// ID is a user identifier.
type ID string

// Key normalises the id.
func (i ID) Key() string { return strings.ToLower(string(i)) }

// User is a stored user.
type User struct {
	ID   ID
	Name string
	Kind Kind
	Seen time.Time
}

// Touch records activity.
func (u *User) Touch() { u.Seen = time.Now() }

// Display renders the user.
func (u User) Display() string { return u.Name + " <" + u.Kind.String() + ">" }

// NewUser builds a member.
func NewUser(name string) *User {
	return &User{ID: ID(strings.ToLower(name)), Name: name, Kind: KindMember}
}

// ErrNotFound is returned by lookups.
var ErrNotFound = fmt.Errorf("model: not found")
