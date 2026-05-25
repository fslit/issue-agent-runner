package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"issue-agent-runner/internal/domain"
)

type Store struct {
	statePath string
	lockPath  string
}

func New(stateDir string) *Store {
	return &Store{
		statePath: filepath.Join(stateDir, "state.json"),
		lockPath:  filepath.Join(stateDir, "runner.lock"),
	}
}

func (s *Store) Load() (*domain.State, error) {
	raw, err := os.ReadFile(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		st := &domain.State{}
		st.Normalize()
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}

	var st domain.State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	st.Normalize()
	return &st, nil
}

func (s *Store) Save(st *domain.State) error {
	st.Normalize()
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	raw = append(raw, '\n')

	tmpPath := filepath.Join(filepath.Dir(s.statePath), "state.tmp")
	if err := os.WriteFile(tmpPath, raw, 0o644); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmpPath, s.statePath); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func (s *Store) WithLock(ctx context.Context, fn func(*domain.State) error) error {
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o755); err != nil {
		return fmt.Errorf("create lock dir: %w", err)
	}

	lock := flock.New(s.lockPath)
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	if !locked {
		return ctx.Err()
	}
	defer lock.Unlock()

	st, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return s.Save(st)
}
