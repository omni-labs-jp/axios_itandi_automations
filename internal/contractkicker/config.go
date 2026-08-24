// Package contractkicker は「契約系書類格納キッカー」の本体ロジック。
//
// ITANDI に Webhook が無いため、Cloud Scheduler から定期起動され、
//
//	Phase A: 契約(120)になった申込を検知して台帳へ pending 登録
//	Phase B: 台帳から 1 件選んで Heroku へ送信し、結果を書き戻す
//
// の 2 段構成で動く。検知と送信を分けているのは、1 実行 1 件という流量制御の
// 下で ITANDI の取得窓（datetime）から処理待ち申込が外れて取りこぼすのを
// 防ぐため。
package contractkicker

import (
	"fmt"
	"time"

	"github.com/omni-labs-jp/axios_itandi_automations/pkg/env"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/itandi"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/ledger"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/retry"
)

// Config は実行時設定。環境変数から LoadConfig で組み立てる。
//
// Cloud Run Job には Terraform（別リポジトリ）から環境変数として渡され、
// 機密（ITANDI トークン / Heroku URL）だけ Secret Manager 経由で注入される。
// 値の意味と既定値は README の「環境変数」表と対応している。
type Config struct {
	ITANDIBaseURL string
	ITANDIToken   string

	HerokuWebhookURL string
	// HerokuAuthHeader / HerokuAuthToken は Heroku 側に共有シークレット検証を
	// 足した場合に使う。空なら送らない。
	HerokuAuthHeader string
	HerokuAuthToken  string

	// TargetStatusID は検知対象の申込ステータス（既定 120 = 契約）。
	TargetStatusID int
	// Lookback は Phase A の取得窓（now - Lookback 以降の更新分）。
	Lookback time.Duration
	// MaxAttempts は送信リトライ上限。到達で gaveup。
	MaxAttempts int
	// SendLimitPerRun は 1 実行あたりの送信件数（当面 1）。
	SendLimitPerRun int
	// RequestTimeout は ITANDI / Heroku への HTTP タイムアウト。
	RequestTimeout time.Duration
	// DryRun は true で Heroku へ送らずログのみ。
	DryRun bool
	// Backoff は失敗時の nextRetryAt 算出に使う。
	Backoff retry.Backoff

	// GCPProjectID は空なら実行環境から自動検出。
	GCPProjectID string
	// FirestoreDatabaseID は空なら "(default)"。
	FirestoreDatabaseID string
	// FirestoreNamespace は相乗り時の名前空間。
	FirestoreNamespace string
	// FirestoreKind は台帳の Kind。
	FirestoreKind string

	// LogLevel は "debug"/"info"/"warn"/"error"。
	LogLevel string

	// LedgerBackend は台帳の実装。本番は datastore。
	// memory はローカル確認用で、DryRun と併用しないと弾かれる。
	LedgerBackend LedgerBackend
}

// 環境変数名。Terraform 側（別リポジトリ）の定義と揃える。
// 定数にしているのは、テストとインフラ定義で同じ名前を参照するため。
const (
	EnvITANDIBaseURL       = "ITANDI_BASE_URL"
	EnvITANDIToken         = "ITANDI_API_TOKEN"
	EnvHerokuWebhookURL    = "HEROKU_WEBHOOK_URL"
	EnvHerokuAuthHeader    = "HEROKU_AUTH_HEADER"
	EnvHerokuAuthToken     = "HEROKU_AUTH_TOKEN"
	EnvTargetStatusID      = "TARGET_STATUS_ID"
	EnvLookbackHours       = "LOOKBACK_HOURS"
	EnvMaxAttempts         = "MAX_ATTEMPTS"
	EnvSendLimitPerRun     = "SEND_LIMIT_PER_RUN"
	EnvRequestTimeoutMS    = "REQUEST_TIMEOUT_MS"
	EnvDryRun              = "DRY_RUN"
	EnvRetrySchedule       = "RETRY_SCHEDULE"
	EnvGCPProject          = "GOOGLE_CLOUD_PROJECT"
	EnvFirestoreDatabaseID = "FIRESTORE_DATABASE_ID"
	EnvFirestoreNamespace  = "FIRESTORE_NAMESPACE"
	EnvFirestoreKind       = "FIRESTORE_KIND"
	EnvLogLevel            = "LOG_LEVEL"
	EnvLedgerBackend       = "LEDGER_BACKEND"
)

// LedgerBackend は台帳の実装の選択肢。
type LedgerBackend string

const (
	// LedgerDatastore は Firestore (Datastore mode)。本番の既定。
	LedgerDatastore LedgerBackend = "datastore"
	// LedgerMemory はプロセス内のインメモリ台帳。
	//
	// GCP 認証もエミュレータも無しにローカルで Phase A の挙動（ITANDI から何件
	// 拾えるか、どの申込が対象になるか）を確認するためのもの。実行が終われば
	// 消えるので、本番で使うと毎回同じ申込を送り続けてしまう。
	// そのため DryRun と併用しない限り起動時に弾く。
	LedgerMemory LedgerBackend = "memory"
)

// LoadConfig は Loader から設定を読み、検証する。
//
// env.Loader がエラーを蓄積するので、設定不備は 1 度にまとめて報告される。
// 15 分間隔のジョブで「直す → 次の実行で別の不備が出る」を繰り返さないため。
func LoadConfig(l *env.Loader) (Config, error) {
	cfg := Config{
		ITANDIBaseURL:    l.String(EnvITANDIBaseURL, itandi.ProductionBaseURL),
		ITANDIToken:      l.Require(EnvITANDIToken),
		HerokuWebhookURL: l.Require(EnvHerokuWebhookURL),
		HerokuAuthHeader: l.String(EnvHerokuAuthHeader, ""),
		HerokuAuthToken:  l.String(EnvHerokuAuthToken, ""),

		TargetStatusID:  l.PositiveInt(EnvTargetStatusID, itandi.StatusContract),
		Lookback:        time.Duration(l.PositiveInt(EnvLookbackHours, 24)) * time.Hour,
		MaxAttempts:     l.PositiveInt(EnvMaxAttempts, 3),
		SendLimitPerRun: l.PositiveInt(EnvSendLimitPerRun, 1),
		RequestTimeout:  l.Millis(EnvRequestTimeoutMS, 30*time.Second),
		DryRun:          l.Bool(EnvDryRun, false),

		GCPProjectID:        l.String(EnvGCPProject, ""),
		FirestoreDatabaseID: l.String(EnvFirestoreDatabaseID, ""),
		FirestoreNamespace:  l.String(EnvFirestoreNamespace, "contract-kicker"),
		FirestoreKind:       l.String(EnvFirestoreKind, ledger.DefaultKind),

		LogLevel:      l.String(EnvLogLevel, "info"),
		LedgerBackend: LedgerBackend(l.String(EnvLedgerBackend, string(LedgerDatastore))),
	}

	schedule := l.DurationList(EnvRetrySchedule, retry.DefaultSchedule)

	// ここまでで蓄積した読み取りエラーを一括で返す。
	if err := l.Err(); err != nil {
		return Config{}, err
	}

	backoff, err := retry.New(schedule)
	if err != nil {
		return Config{}, err
	}
	cfg.Backoff = backoff

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate は組み立て済み Config の整合性を検証する。
//
// LoadConfig 経由でなく手で組んだ Config（テストなど）も Service.New で
// ここを通るため、不正な設定のまま動き出すことはない。
func (c Config) Validate() error {
	if c.ITANDIBaseURL == "" {
		return fmt.Errorf("config: %s is required", EnvITANDIBaseURL)
	}
	if c.ITANDIToken == "" {
		return fmt.Errorf("config: %s is required", EnvITANDIToken)
	}
	if c.HerokuWebhookURL == "" {
		return fmt.Errorf("config: %s is required", EnvHerokuWebhookURL)
	}
	if c.Lookback <= 0 {
		return fmt.Errorf("config: %s must be positive", EnvLookbackHours)
	}
	if c.MaxAttempts < 1 {
		return fmt.Errorf("config: %s must be >= 1", EnvMaxAttempts)
	}
	if c.SendLimitPerRun < 1 {
		return fmt.Errorf("config: %s must be >= 1", EnvSendLimitPerRun)
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("config: %s must be positive", EnvRequestTimeoutMS)
	}
	if len(c.Backoff.Schedule) == 0 {
		return fmt.Errorf("config: %s must not be empty", EnvRetrySchedule)
	}
	// ヘッダ名だけ / 値だけ設定されていると認証が黙って効かないので弾く。
	if (c.HerokuAuthHeader == "") != (c.HerokuAuthToken == "") {
		return fmt.Errorf("config: %s and %s must be set together", EnvHerokuAuthHeader, EnvHerokuAuthToken)
	}

	switch c.LedgerBackend {
	case LedgerDatastore:
	case LedgerMemory:
		// インメモリ台帳は実行のたびに消えるため、実送信と組み合わせると
		// 同じ申込を何度も Heroku へ送ってしまう。
		if !c.DryRun {
			return fmt.Errorf("config: %s=%s requires %s=true", EnvLedgerBackend, LedgerMemory, EnvDryRun)
		}
	default:
		return fmt.Errorf("config: %s must be %q or %q, got %q",
			EnvLedgerBackend, LedgerDatastore, LedgerMemory, c.LedgerBackend)
	}
	return nil
}

// LogAttrs は起動時に出す設定サマリ。
//
// ITANDI トークンや Heroku の URL・共有シークレットは**含めない**。
// Cloud Logging は閲覧範囲が広いので、機密はログに出さない方針。
func (c Config) LogAttrs() []any {
	return []any{
		"itandiBaseURL", c.ITANDIBaseURL,
		"targetStatusId", c.TargetStatusID,
		"lookbackHours", c.Lookback.Hours(),
		"maxAttempts", c.MaxAttempts,
		"sendLimitPerRun", c.SendLimitPerRun,
		"requestTimeoutMs", c.RequestTimeout.Milliseconds(),
		"dryRun", c.DryRun,
		"firestoreNamespace", c.FirestoreNamespace,
		"firestoreKind", c.FirestoreKind,
		"retrySchedule", fmt.Sprint(c.Backoff.Schedule),
		"ledgerBackend", string(c.LedgerBackend),
	}
}
