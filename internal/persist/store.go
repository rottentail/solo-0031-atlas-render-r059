package persist

import (
	"atlas-render/internal/model"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type Store struct {
	mu    sync.RWMutex
	path  string
	state model.State
	lock  *os.File
	write func(string, []byte) error
}

func Open(path string) (*Store, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	lock, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, fmt.Errorf("state is already opened by another process: %w", e)
	}
	s := &Store{path: path, state: model.NewState(), lock: lock, write: atomicWrite}
	data, e := os.ReadFile(path)
	if e != nil && !os.IsNotExist(e) {
		s.Close()
		return nil, e
	}
	if e == nil {
		if e = json.Unmarshal(data, &s.state); e != nil {
			s.Close()
			return nil, fmt.Errorf("invalid persisted JSON: %w", e)
		}
		if e = Validate(s.state); e != nil {
			s.Close()
			return nil, e
		}
	}
	if e = s.markBoot(); e != nil {
		s.Close()
		return nil, e
	}
	return s, nil
}

// markBoot stamps a fresh boot identity so previews recorded before a restart
// can never be executed afterwards. Pending previews are kept as audit history
// but marked stale; their tokens also carry the old boot identity.
func (s *Store) markBoot() error {
	boot := model.NewBootID()
	return s.Update(func(st *model.State) error {
		st.BootID = boot
		if st.RetentionPreviews == nil {
			st.RetentionPreviews = map[string]model.RetentionPreview{}
		}
		for id, p := range st.RetentionPreviews {
			if p.State == "pending" {
				p.State = "stale"
				st.RetentionPreviews[id] = p
			}
		}
		return nil
	})
}

func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	e := s.lock.Close()
	s.lock = nil
	return e
}
func (s *Store) Read() model.State { s.mu.RLock(); defer s.mu.RUnlock(); return model.Clone(s.state) }
func (s *Store) Update(fn func(*model.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := model.Clone(s.state)
	if next.RetentionPreviews == nil {
		next.RetentionPreviews = map[string]model.RetentionPreview{}
	}
	if e := fn(&next); e != nil {
		return e
	}
	if e := Validate(next); e != nil {
		return e
	}
	b, e := json.Marshal(next)
	if e != nil {
		return e
	}
	if e = s.write(s.path, b); e != nil {
		return fmt.Errorf("persist state: %w", e)
	}
	s.state = next
	return nil
}
func atomicWrite(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".atlas-write-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	return os.Rename(name, path)
}
