// Package store dot-imports model, which forces the extractor to rewrite bare
// identifiers into qualified ones.
package store

import (
	. "demo/internal/model"
)

// Memory is an in-memory user store.
type Memory struct {
	users map[ID]*User
}

// NewMemory builds an empty store.
func NewMemory() *Memory { return &Memory{users: map[ID]*User{}} }

// Put stores a user.
func (m *Memory) Put(u *User) { m.users[u.ID] = u }

// Get looks a user up.
func (m *Memory) Get(id ID) (*User, error) {
	if u, ok := m.users[id]; ok {
		return u, nil
	}
	return nil, ErrNotFound
}

// Count returns the number of users.
func (m *Memory) Count() int { return len(m.users) }
