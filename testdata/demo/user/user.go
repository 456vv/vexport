// Package user is the fixture target package. It deliberately declares User
// itself, so that inlining model.User hits the name clash policy.
package user

import (
	"demo/internal/model"
	"demo/internal/store"
)

// User is the service facing view of a user.
type User struct {
	ID   model.ID
	Name string
}

// Svc serves users.
type Svc struct {
	mem *store.Memory
}

// NewSvc builds a service.
func NewSvc() *Svc { return &Svc{mem: store.NewMemory()} }

// Add stores a user.
func (s *Svc) Add(u User) error {
	m := model.NewUser(u.Name)
	if m.Kind == model.KindGuest {
		return model.ErrNotFound
	}
	s.mem.Put(m)
	return nil
}

// Get reads a user back.
func (s *Svc) Get(id model.ID) (*model.User, error) {
	return s.mem.Get(id)
}

// Count reports how many users are stored.
func (s *Svc) Count() int { return s.mem.Count() }
