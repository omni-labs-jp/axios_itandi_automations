// Package ledger は「処理済み管理台帳」の抽象と実装を提供する。
//
// ITANDI に Webhook が無いため、どの申込を Heroku へ送信済みかを外部ストアで
// 管理する必要がある。Cloud Run Job は毎回プロセスが死ぬので、実行をまたいで
// 残る状態はここだけ。
//
// Store インタフェースを介すことで、本番は Firestore (Datastore mode)、
// テストは MemoryStore と差し替えられる。実装を足したときは
// ledgertest.RunStoreTests で共通仕様を満たしているか確認する。
package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrNotFound は該当エントリが存在しないことを示す。
var ErrNotFound = errors.New("ledger: entry not found")

// Status はエントリの処理状態。
type Status string

// ステータスの遷移:
//
//	(未登録) --Phase A--> pending --送信成功--> done
//	                        |
//	                        +---送信失敗--> failed --リトライ成功--> done
//	                                          |
//	                                          +--上限到達--> gaveup
//
// done / gaveup が終端（Terminal）で、以後は送信対象に選ばれない。
const (
	// StatusPending は検知済み・未送信。
	StatusPending Status = "pending"
	// StatusDone は Heroku が受け付けた（2xx）。以後送らない。
	StatusDone Status = "done"
	// StatusFailed は送信失敗・リトライ待ち。
	StatusFailed Status = "failed"
	// StatusGaveUp はリトライ上限到達。以後送らない。
	StatusGaveUp Status = "gaveup"
)

// Valid は既知のステータスかを返す。
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusDone, StatusFailed, StatusGaveUp:
		return true
	}
	return false
}

// Terminal は以後送信対象にならないステータスかを返す。
func (s Status) Terminal() bool { return s == StatusDone || s == StatusGaveUp }

// Entry は台帳 1 件。ID は ITANDI 申込 ID の文字列表現。
//
// キーに申込 ID をそのまま使うことで、Phase A の存在確認がクエリではなく
// 直接 get で済む（インデックス不要・強整合）。
type Entry struct {
	ID string
	// PropertyName / RoomNumber は物件名・部屋番号。ログで申込を人が識別できる
	// ようにするためだけに持つ（検索条件には使わない）。Phase A で申込詳細から
	// 取得できなかった場合は空になる。
	PropertyName string
	RoomNumber   string
	Status       Status
	Attempts     int
	DetectedAt   time.Time
	SentAt       time.Time
	NextRetryAt  time.Time
	LastError    string
	// JobID は Heroku が受付時に返す jobId（追跡用）。
	JobID     string
	UpdatedAt time.Time
}

// Validate は保存前の最小チェック。
// 不正なステータスが台帳に入ると選択ロジックが静かに壊れるので、
// 書き込み側（各 Store 実装の Put）で必ず通す。
func (e Entry) Validate() error {
	if strings.TrimSpace(e.ID) == "" {
		return errors.New("ledger: entry ID is required")
	}
	if !e.Status.Valid() {
		return fmt.Errorf("ledger: unknown status %q", e.Status)
	}
	if e.Attempts < 0 {
		return fmt.Errorf("ledger: negative attempts %d", e.Attempts)
	}
	return nil
}

// Order は List の並び順。
type Order string

const (
	// OrderDetectedAt は検知日時の昇順（古い申込を先に処理する）。
	OrderDetectedAt Order = "detectedAt"
	// OrderNextRetryAt はリトライ可能時刻の昇順。
	OrderNextRetryAt Order = "nextRetryAt"
)

// Query は List の検索条件。
// Phase B の 2 段階の絞り込み（pending / 期限が来た failed）を表現できる
// だけの最小構成にしている。
type Query struct {
	// Status は必須。この状態のエントリのみを返す。
	Status Status
	// NextRetryBefore が非ゼロなら NextRetryAt <= この時刻のものに絞る。
	NextRetryBefore time.Time
	// OrderBy はゼロ値で OrderDetectedAt 扱い。
	OrderBy Order
	// Limit は最大件数。0 以下は無制限。
	Limit int
}

// Store は台帳の読み書き。実装はゴルーチンセーフであること。
//
// Phase A は Get/Put のみ、Phase B は List/Put のみを使う。
type Store interface {
	// Get は ID でエントリを取得する。無ければ ErrNotFound。
	Get(ctx context.Context, id string) (Entry, error)
	// Put はエントリを保存する（upsert）。
	Put(ctx context.Context, e Entry) error
	// List は条件に合うエントリを返す。
	List(ctx context.Context, q Query) ([]Entry, error)
	// Close は下位リソースを解放する。
	Close() error
}

// sortEntries は Query の並び順でスライスを整列する。
//
// 同キー（同時刻）の場合は ID で決定的に並べる。これが無いと「1 実行 1 件」で
// どれが選ばれるかが実行ごとに変わり、テストも運用上の追跡も不安定になる。
//
// Datastore 実装はサーバ側で並べるためこの関数を使わない（MemoryStore 専用）。
func sortEntries(entries []Entry, order Order) {
	key := func(e Entry) time.Time {
		if order == OrderNextRetryAt {
			return e.NextRetryAt
		}
		return e.DetectedAt
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := key(entries[i]), key(entries[j])
		if a.Equal(b) {
			return entries[i].ID < entries[j].ID
		}
		return a.Before(b)
	})
}

// matches は Query の条件に合致するかを判定する（MemoryStore 用）。
// ゼロ値の条件は「絞り込まない」を意味する。
func matches(e Entry, q Query) bool {
	if q.Status != "" && e.Status != q.Status {
		return false
	}
	if !q.NextRetryBefore.IsZero() && e.NextRetryAt.After(q.NextRetryBefore) {
		return false
	}
	return true
}
