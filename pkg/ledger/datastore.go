package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/datastore"
)

// DefaultKind は既定の Datastore Kind。
const DefaultKind = "contract_kicker_entries"

// DatastoreConfig は DatastoreStore の設定。
type DatastoreConfig struct {
	// ProjectID は GCP プロジェクト ID。空なら環境から自動検出
	// (datastore.DetectProjectID)。
	ProjectID string
	// DatabaseID は Firestore のデータベース ID。空なら "(default)"。
	DatabaseID string
	// Namespace は相乗り時の名前空間。空なら既定名前空間。
	Namespace string
	// Kind は空なら DefaultKind。
	Kind string
}

// DatastoreStore は Firestore (Datastore mode) を使う Store 実装。
//
// Firestore はプロジェクトに 1 つしか作れずモード変更もできないため、
// 既存プロジェクトに相乗りする場合は Namespace で他システムと分離する。
type DatastoreStore struct {
	client    *datastore.Client
	namespace string
	kind      string
	// ownsClient が true のとき Close() でクライアントを閉じる。
	ownsClient bool
}

// NewDatastoreStore は Datastore クライアントを生成して Store を返す。
// 生成したクライアントは Close() で閉じる。
func NewDatastoreStore(ctx context.Context, cfg DatastoreConfig) (*DatastoreStore, error) {
	projectID := strings.TrimSpace(cfg.ProjectID)
	if projectID == "" {
		projectID = datastore.DetectProjectID
	}

	var (
		client *datastore.Client
		err    error
	)
	if db := strings.TrimSpace(cfg.DatabaseID); db != "" {
		client, err = datastore.NewClientWithDatabase(ctx, projectID, db)
	} else {
		client, err = datastore.NewClient(ctx, projectID)
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: create datastore client: %w", err)
	}

	s := NewDatastoreStoreWithClient(client, cfg)
	s.ownsClient = true
	return s, nil
}

// NewDatastoreStoreWithClient は既存クライアントを使う Store を返す。
// クライアントの Close は呼び出し側の責務（Close() は何もしない）。
// エミュレータ相手のテストで 1 つのクライアントを共有するために使う。
func NewDatastoreStoreWithClient(client *datastore.Client, cfg DatastoreConfig) *DatastoreStore {
	kind := strings.TrimSpace(cfg.Kind)
	if kind == "" {
		kind = DefaultKind
	}
	return &DatastoreStore{
		client:    client,
		namespace: strings.TrimSpace(cfg.Namespace),
		kind:      kind,
	}
}

func (s *DatastoreStore) key(id string) *datastore.Key {
	k := datastore.NameKey(s.kind, id, nil)
	k.Namespace = s.namespace
	return k
}

// entity は Datastore に保存する形。Entry とは別に持つ。
//
//   - 省略可能な時刻はポインタ。Go の time.Time ゼロ値（西暦 1 年）は
//     Datastore の扱える範囲外なので、未設定は「フィールドを書かない」で表す
//   - lastError / herokuJobId は検索しないので noindex（インデックス費用と
//     サイズ上限の節約）
//   - タグ名は README のスキーマ表と一致させる
type entity struct {
	// 物件名・部屋番号はログ表示専用なので検索対象にしない（noindex）。
	PropertyName string     `datastore:"propertyName,noindex,omitempty"`
	RoomNumber   string     `datastore:"roomNumber,noindex,omitempty"`
	Status       string     `datastore:"status"`
	Attempts     int        `datastore:"attempts"`
	DetectedAt   *time.Time `datastore:"detectedAt,omitempty"`
	SentAt       *time.Time `datastore:"sentAt,omitempty"`
	NextRetryAt  *time.Time `datastore:"nextRetryAt,omitempty"`
	LastError    string     `datastore:"lastError,noindex,omitempty"`
	JobID        string     `datastore:"herokuJobId,noindex,omitempty"`
	UpdatedAt    *time.Time `datastore:"updatedAt,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

func timeVal(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

func toEntity(e Entry) *entity {
	return &entity{
		PropertyName: e.PropertyName,
		RoomNumber:   e.RoomNumber,
		Status:       string(e.Status),
		Attempts:     e.Attempts,
		DetectedAt:   timePtr(e.DetectedAt),
		SentAt:       timePtr(e.SentAt),
		NextRetryAt:  timePtr(e.NextRetryAt),
		LastError:    e.LastError,
		JobID:        e.JobID,
		UpdatedAt:    timePtr(e.UpdatedAt),
	}
}

func fromEntity(id string, ent *entity) Entry {
	return Entry{
		ID:           id,
		PropertyName: ent.PropertyName,
		RoomNumber:   ent.RoomNumber,
		Status:       Status(ent.Status),
		Attempts:     ent.Attempts,
		DetectedAt:   timeVal(ent.DetectedAt),
		SentAt:       timeVal(ent.SentAt),
		NextRetryAt:  timeVal(ent.NextRetryAt),
		LastError:    ent.LastError,
		JobID:        ent.JobID,
		UpdatedAt:    timeVal(ent.UpdatedAt),
	}
}

// Get implements Store.
func (s *DatastoreStore) Get(ctx context.Context, id string) (Entry, error) {
	if strings.TrimSpace(id) == "" {
		return Entry{}, errors.New("ledger: id is required")
	}
	var ent entity
	if err := s.client.Get(ctx, s.key(id), &ent); err != nil {
		// 未登録は正常系（Phase A はこれを見て新規登録する）。
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return Entry{}, ErrNotFound
		}
		// 手で足したフィールドや旧スキーマが残っていても、読めた分で処理を続ける
		// （台帳が読めないと全件が再送対象になってしまうため）。
		if _, ok := err.(*datastore.ErrFieldMismatch); !ok {
			return Entry{}, fmt.Errorf("ledger: get %s: %w", id, err)
		}
	}
	return fromEntity(id, &ent), nil
}

// Put implements Store.
func (s *DatastoreStore) Put(ctx context.Context, e Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if _, err := s.client.Put(ctx, s.key(e.ID), toEntity(e)); err != nil {
		return fmt.Errorf("ledger: put %s: %w", e.ID, err)
	}
	return nil
}

// List implements Store.
//
// status の等価条件 + 別プロパティのソートを使うため、Datastore の複合
// インデックス（deploy/index.yaml）が必要。インデックスが無いと実行時に
// エラーになるので、初回デプロイ時に忘れず投入する。
//
// 並び順・件数制限はサーバ側に任せる（1 実行 1 件なので全件は読まない）。
func (s *DatastoreStore) List(ctx context.Context, q Query) ([]Entry, error) {
	order := q.OrderBy
	if order == "" {
		order = OrderDetectedAt
	}

	dq := datastore.NewQuery(s.kind).Namespace(s.namespace)
	if q.Status != "" {
		dq = dq.FilterField("status", "=", string(q.Status))
	}
	if !q.NextRetryBefore.IsZero() {
		dq = dq.FilterField("nextRetryAt", "<=", q.NextRetryBefore.UTC())
	}
	dq = dq.Order(string(order))
	if q.Limit > 0 {
		dq = dq.Limit(q.Limit)
	}

	var ents []*entity
	keys, err := s.client.GetAll(ctx, dq, &ents)
	if err != nil {
		return nil, fmt.Errorf("ledger: list %s: %w", q.Status, err)
	}

	out := make([]Entry, 0, len(ents))
	for i, ent := range ents {
		out = append(out, fromEntity(keys[i].Name, ent))
	}
	return out, nil
}

// Close implements Store.
func (s *DatastoreStore) Close() error {
	if s.ownsClient && s.client != nil {
		return s.client.Close()
	}
	return nil
}

var _ Store = (*DatastoreStore)(nil)
