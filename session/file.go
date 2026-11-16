package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var _ Forker = (*FileStore)(nil)

// maxFileSessionID is the longest session ID a FileStore takes, so the hex file names stay within 255 bytes.
const maxFileSessionID = 120

// FileStore is a Store keeping sessions as files in one directory, for local development and single-process use.
// Each session has a metadata file <hex ID>.json and a message file <hex ID>.jsonl with one Message per line, only
// ever appended to. File names are the hex of the session ID, so any ID up to 120 bytes is safe on any file system.
//
// A complete line is the unit of recovery: after a crash, a partly written last line is ignored and cut off by the next
// append, and of a batch only the messages written in full are kept. Every append is synced to disk.
// Sessions are locked within the process only; several processes must not share a directory.
// Message.Details goes through JSON, so it reads back as JSON values (map[string]any and so on), not as its original type.
type FileStore struct {
	dir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewFileStore creates a FileStore in dir, creating the directory when it does not exist.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create store directory: %w", err)
	}
	return &FileStore{dir: dir, locks: make(map[string]*sync.Mutex)}, nil
}

func (s *FileStore) EnsureSession(_ context.Context, session Session) error {
	base, err := s.base(session.ID)
	if err != nil {
		return err
	}
	unlock := s.lock(session.ID)
	defer unlock()
	if _, err := os.Stat(base + ".json"); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("session: %w", err)
	}
	// A message file without metadata is left by a fork that crashed before publishing; it must not become this session's history.
	if err := os.Remove(base + ".jsonl"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("session: %w", err)
	}
	if err := s.publish(base, session); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// ForkSession implements Forker. The prefix is copied into a temporary file under the source's lock, which is released
// before the new session is locked, so two forks never wait on each other; the metadata is published last.
func (s *FileStore) ForkSession(_ context.Context, dst Session, srcID, cutID string) error {
	dstBase, err := s.base(dst.ID)
	if err != nil {
		return err
	}
	srcBase, err := s.existing(srcID)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".fork-*.tmp")
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer os.Remove(tmp.Name())
	err = s.copyPrefix(tmp, srcBase, srcID, dst.ID, cutID)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}

	unlock := s.lock(dst.ID)
	defer unlock()
	if _, err := os.Stat(dstBase + ".json"); err == nil {
		return ErrExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("session: %w", err)
	}
	if err := os.Rename(tmp.Name(), dstBase+".jsonl"); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	err = s.publish(dstBase, dst)
	if errors.Is(err, fs.ErrExist) {
		return ErrExists
	}
	return err
}

// copyPrefix writes to w the messages of the session srcID that precede cutID, or all of them when cutID is empty, with
// SessionID set to dstID.
func (s *FileStore) copyPrefix(w io.Writer, srcBase, srcID, dstID, cutID string) error {
	unlock := s.lock(srcID)
	defer unlock()
	var werr error
	stopped, err := readMessages(srcBase, srcID, func(msg Message) bool {
		if cutID != "" && msg.ID == cutID {
			return false
		}
		msg.SessionID = dstID
		data, err := json.Marshal(msg)
		if err != nil {
			werr = fmt.Errorf("session: encode message %q: %w", msg.ID, err)
			return false
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			werr = fmt.Errorf("session: write messages: %w", err)
			return false
		}
		return true
	})
	switch {
	case err != nil:
		return err
	case werr != nil:
		return werr
	case cutID != "" && !stopped:
		return ErrNotFound
	}
	return nil
}

// readMessages passes the messages in a session's message file to yield in order, until yield returns false, and reports
// whether it did. A missing file holds no messages; a last line without its newline was cut short by a crash and is ignored.
func readMessages(base, sessionID string, yield func(Message) bool) (stopped bool, err error) {
	f, err := os.Open(base + ".jsonl")
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("session: %w", err)
	}
	defer f.Close()
	// bufio.Reader rather than bufio.Scanner: a line holding inline media can be far longer than a Scanner's limit.
	r := bufio.NewReader(f)
	for n := 1; ; n++ {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("session: read messages of %q: %w", sessionID, err)
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			return false, fmt.Errorf("session: messages of %q, line %d: %w", sessionID, n, err)
		}
		if !yield(msg) {
			return true, nil
		}
	}
}

// publish writes the metadata of a session, which makes it exist. The file appears under its name only when written in
// full, so a crash cannot leave a session that looks valid; an error wrapping fs.ErrExist means it already existed.
func (s *FileStore) publish(base string, session Session) error {
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now()
	}
	data, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("session: encode session: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".session-*.tmp")
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("session: write session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("session: write session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: write session: %w", err)
	}
	if err := os.Link(tmp.Name(), base+".json"); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}

func (s *FileStore) GetSession(_ context.Context, sessionID string) (Session, error) {
	base, err := s.base(sessionID)
	if err != nil {
		return Session{}, err
	}
	data, err := os.ReadFile(base + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return Session{}, ErrNotFound
	} else if err != nil {
		return Session{}, fmt.Errorf("session: %w", err)
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return Session{}, fmt.Errorf("session: decode session %q: %w", sessionID, err)
	}
	return session, nil
}

func (s *FileStore) AppendMessages(_ context.Context, sessionID string, messages ...Message) error {
	base, err := s.existing(sessionID)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, msg := range messages {
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("session: encode message %q: %w", msg.ID, err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	unlock := s.lock(sessionID)
	defer unlock()
	f, err := os.OpenFile(base+".jsonl", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer f.Close()
	if err := cutPartialLine(f); err != nil {
		return fmt.Errorf("session: repair messages of %q: %w", sessionID, err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("session: write messages: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("session: write messages: %w", err)
	}
	return nil
}

func (s *FileStore) ListMessages(_ context.Context, sessionID string) ([]Message, error) {
	base, err := s.existing(sessionID)
	if err != nil {
		return nil, err
	}
	unlock := s.lock(sessionID)
	defer unlock()
	var messages []Message
	_, err = readMessages(base, sessionID, func(msg Message) bool {
		messages = append(messages, msg)
		return true
	})
	if err != nil {
		return nil, err
	}
	return messages, nil
}

// base returns the path of a session's files without their extension.
func (s *FileStore) base(sessionID string) (string, error) {
	if sessionID == "" || len(sessionID) > maxFileSessionID {
		return "", fmt.Errorf("session: a FileStore takes session IDs of 1 to %d bytes, got %d", maxFileSessionID, len(sessionID))
	}
	return filepath.Join(s.dir, hex.EncodeToString([]byte(sessionID))), nil
}

// existing returns base of a session that exists, or ErrNotFound.
func (s *FileStore) existing(sessionID string) (string, error) {
	base, err := s.base(sessionID)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(base + ".json"); errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotFound
	} else if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	return base, nil
}

// lock locks one session's files and returns the unlock.
func (s *FileStore) lock(sessionID string) func() {
	s.mu.Lock()
	l, ok := s.locks[sessionID]
	if !ok {
		l = new(sync.Mutex)
		s.locks[sessionID] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// cutPartialLine truncates f after its last newline when it does not end with one, removing a line a crash cut short
// so the next one does not join it.
func cutPartialLine(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	end := info.Size()
	buf := make([]byte, 32<<10)
	for pos := end; pos > 0; {
		n := min(int64(len(buf)), pos)
		pos -= n
		if _, err := f.ReadAt(buf[:n], pos); err != nil {
			return err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			if keep := pos + int64(i) + 1; keep < end {
				return f.Truncate(keep)
			}
			return nil
		}
	}
	if end > 0 {
		return f.Truncate(0)
	}
	return nil
}
