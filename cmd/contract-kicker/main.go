// Command contract-kicker は ITANDI で「契約」になった申込を検知し、
// Heroku(itandi-automation) の書類格納 Webhook へ 1 件ずつ流す。
//
// Cloud Run Jobs から Cloud Scheduler 経由で 15 分ごとに起動される想定。
// 1 実行 1 件（SEND_LIMIT_PER_RUN）で流量制御し、処理状況は Firestore
// (Datastore mode) の台帳で管理する。
//
// ここでやることは配線だけ:
//
//	環境変数 → Config → 各クライアント生成 → Service.Run → 終了コード
//
// 判断ロジックは internal/contractkicker にある。
//
// インフラ（Job / Scheduler / Secret / Firestore）は別リポジトリの Terraform、
// イメージのビルドとデプロイは .github/workflows/deploy.yml が担当する。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/omni-labs-jp/axios_itandi_automations/internal/contractkicker"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/env"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/itandi"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/ledger"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/logging"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/webhook"
)

func main() {
	// 設定を読む前のログは INFO 固定（LOG_LEVEL 自体が読めない可能性がある）。
	bootLog := logging.NewStdout(logging.Options{})

	cfg, err := contractkicker.LoadConfig(env.New())
	if err != nil {
		bootLog.Error("invalid configuration", "error", err.Error())
		os.Exit(1)
	}

	log := logging.NewStdout(logging.Options{Level: logging.ParseLevel(cfg.LogLevel)})
	log.Info("contract-kicker starting", cfg.LogAttrs()...)

	// Cloud Run Jobs の停止シグナルで打ち切れるようにする。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 失敗時は終了コード 1。Cloud Run Jobs の実行が失敗として記録され、
	// コンソール／ログから気付ける（max_retries=0 なので自動再実行はされない。
	// 個々の申込のリトライは台帳側の failed / nextRetryAt が担当する）。
	if err := run(ctx, cfg, log); err != nil {
		log.Error("contract-kicker failed", "error", err.Error())
		os.Exit(1)
	}
	log.Info("contract-kicker finished")
}

// run は依存を組み立てて 1 回実行する。
// 生成に失敗したものは即 return（設定・権限の不備を黙って続行しない）。
func run(ctx context.Context, cfg contractkicker.Config, log *slog.Logger) error {
	client, err := itandi.NewClient(cfg.ITANDIBaseURL, cfg.ITANDIToken, itandi.WithTimeout(cfg.RequestTimeout))
	if err != nil {
		return fmt.Errorf("itandi client: %w", err)
	}

	store, err := newStore(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Warn("failed to close ledger store", "error", err.Error())
		}
	}()

	sender, err := newSender(cfg, log)
	if err != nil {
		return err
	}

	svc, err := contractkicker.New(cfg, client, store, sender, contractkicker.Options{Logger: log})
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}

	if _, err := svc.Run(ctx); err != nil {
		return err
	}
	return nil
}

// newStore は LEDGER_BACKEND に応じた台帳を返す。
//
// memory は Config.Validate が DRY_RUN との併用を強制しているので、
// ここに来る時点で送信は行われない（＝実行のたびに消えても害がない）。
func newStore(ctx context.Context, cfg contractkicker.Config, log *slog.Logger) (ledger.Store, error) {
	if cfg.LedgerBackend == contractkicker.LedgerMemory {
		log.Warn("using in-memory ledger; state is discarded when this run ends")
		return ledger.NewMemoryStore(), nil
	}

	// プロジェクト ID は Cloud Run 上では自動検出される。
	store, err := ledger.NewDatastoreStore(ctx, ledger.DatastoreConfig{
		ProjectID:  cfg.GCPProjectID,
		DatabaseID: cfg.FirestoreDatabaseID,
		Namespace:  cfg.FirestoreNamespace,
		Kind:       cfg.FirestoreKind,
	})
	if err != nil {
		return nil, fmt.Errorf("ledger store: %w", err)
	}
	return store, nil
}

// newSender は DRY_RUN に応じて実送信 / ログのみの Sender を返す。
//
// 分岐をここ 1 箇所に閉じ込めているので、Service 側は DRY_RUN を知らない。
// DRY_RUN 時は台帳も更新されないため、対象申込は次の実行でも残る。
func newSender(cfg contractkicker.Config, log *slog.Logger) (webhook.Sender, error) {
	if cfg.DryRun {
		// 本番のみの構成なので、DRY_RUN の付け外しはログで明確に分かるようにする。
		log.Warn("DRY_RUN is enabled; no request will be sent to heroku",
			"target", cfg.HerokuWebhookURL)
		return webhook.DryRunSender{Logger: log, Target: cfg.HerokuWebhookURL}, nil
	}

	opts := []webhook.Option{webhook.WithTimeout(cfg.RequestTimeout)}
	if cfg.HerokuAuthHeader != "" {
		opts = append(opts, webhook.WithHeader(cfg.HerokuAuthHeader, cfg.HerokuAuthToken))
	}
	client, err := webhook.NewClient(cfg.HerokuWebhookURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("heroku webhook client: %w", err)
	}
	return client, nil
}
