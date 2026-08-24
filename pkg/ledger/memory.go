package ledger

import (
	"context"
	"sync"
)

// MemoryStore はインメモリの Store 実装。テストとローカル実行用。
//
// Fail* に error を差すと任意の操作を失敗させられるので、Firestore 障害時の
// 振る舞い（1 件失敗しても続行する、送信後に書けなかったら ERROR を出す等）を
// テストで再現できる。
type MemoryStore struct {
	mu      sync.RWMutex
	entries map[string]Entry
	// FailGet / FailPut / FailList が非 nil ならその error を返す（異常系テスト用）。
	// テスト専用フィールドなので本番コードからは触らない。
	FailGet  error
	FailPut  error
	FailList error
}

// NewMemoryStore は空の MemoryStore を返す。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: map[string]Entry{}}
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id string) (Entry, error) {
	if s.FailGet != nil {
		return Entry{}, s.FailGet
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[id]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return e, nil
}

// Put implements Store.
func (s *MemoryStore) Put(_ context.Context, e Entry) error {
	if s.FailPut != nil {
		return s.FailPut
	}
	if err := e.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]Entry{}
	}
	s.entries[e.ID] = e
	return nil
}

// List implements Store.
// Datastore 実装と同じ結果になるよう、絞り込み → 整列 → 件数制限の順で処理する。
func (s *MemoryStore) List(_ context.Context, q Query) ([]Entry, error) {
	if s.FailList != nil {
		return nil, s.FailList
	}
	s.mu.RLock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if matches(e, q) {
			out = append(out, e)
		}
	}
	s.mu.RUnlock()

	// ロックを外してから整列する（保持時間を最小にする）。
	sortEntries(out, q.OrderBy)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// Close implements Store.
func (s *MemoryStore) Close() error { return nil }

// Len は保持件数を返す（テスト用）。
func (s *MemoryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Seed は初期データを一括投入する（テスト用）。Validate は通さない。
func (s *MemoryStore) Seed(entries ...Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]Entry{}
	}
	for _, e := range entries {
		s.entries[e.ID] = e
	}
}

var _ Store = (*MemoryStore)(nil)
