package session

import (
	"context"
	"slices"
	"sync"
	"time"
)

var (
	_ TailLister = (*MemoryStore)(nil)
	_ Forker     = (*MemoryStore)(nil)
)

// MemoryStore is an in-memory Store, suited to tests and lightweight single-process use.
type MemoryStore struct {
	mu       sync.Mutex
	sessions map[string]Session
	messages map[string][]Message
}

// NewMemoryStore creates an in-memory session store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions: make(map[string]Session),
		messages: make(map[string][]Message),
	}
}

func (s *MemoryStore) EnsureSession(_ context.Context, session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now()
	}
	if _, ok := s.sessions[session.ID]; !ok {
		s.sessions[session.ID] = session
	}
	return nil
}

func (s *MemoryStore) GetSession(_ context.Context, sessionID string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return Session{}, ErrNotFound
	}
	return session, nil
}

func (s *MemoryStore) AppendMessages(_ context.Context, sessionID string, messages ...Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return ErrNotFound
	}
	for _, msg := range messages {
		s.messages[sessionID] = append(s.messages[sessionID], msg.Clone())
	}
	return nil
}

func (s *MemoryStore) ListMessages(_ context.Context, sessionID string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return nil, ErrNotFound
	}
	return CloneMessages(s.messages[sessionID]), nil
}

// ListTail implements TailLister.
func (s *MemoryStore) ListTail(_ context.Context, sessionID string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return nil, ErrNotFound
	}
	return CloneMessages(Tail(s.messages[sessionID])), nil
}

// ForkSession implements Forker.
func (s *MemoryStore) ForkSession(_ context.Context, dst Session, srcID, cutID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[srcID]; !ok {
		return ErrNotFound
	}
	if _, ok := s.sessions[dst.ID]; ok {
		return ErrExists
	}
	src := s.messages[srcID]
	end := len(src)
	if cutID != "" {
		end = slices.IndexFunc(src, func(m Message) bool { return m.ID == cutID })
		if end < 0 {
			return ErrNotFound
		}
	}
	messages := CloneMessages(src[:end])
	for i := range messages {
		messages[i].SessionID = dst.ID
	}
	if dst.CreatedAt.IsZero() {
		dst.CreatedAt = time.Now()
	}
	s.sessions[dst.ID] = dst
	s.messages[dst.ID] = messages
	return nil
}
