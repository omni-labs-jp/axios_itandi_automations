package contractkicker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/omni-labs-jp/axios_itandi_automations/pkg/clock"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/itandi"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/ledger"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/webhook"
)

// EntryLister は ITANDI 申込一覧の取得を抽象化する（*itandi.Client が実装）。
//
// interface で切っているのは 2 つの理由から。
//   - テストで HTTP を立てずに済む（service_test.go の fakeLister）
//   - ITANDI の IP 許可制が通らず「Heroku 経由で一覧を取る」案に切り替える場合、
//     この interface の実装を差し替えるだけで Service 側は無変更で済む
type EntryLister interface {
	ListEntryHeads(ctx context.Context, p itandi.ListParams) ([]itandi.EntryHead, error)
}

// Service は Phase A / Phase B を実行する。
//
// 外部に触るものはすべて interface / 差し替え可能な形で保持している。
// Run 1 回が Cloud Run Job の 1 実行に対応し、状態はすべて store（台帳）側に
// あるのでインスタンスは使い捨てでよい。
type Service struct {
	cfg    Config
	lister EntryLister    // ITANDI 申込一覧の取得（Phase A）
	store  ledger.Store   // 処理台帳。実行間で状態を引き継ぐ唯一の場所
	sender webhook.Sender // Heroku への送信（Phase B）。DRY_RUN 時は DryRunSender
	clk    clock.Clock    // 取得窓・バックオフの基準時刻。テストで固定できる
	log    *slog.Logger
}

// Options は Service の任意依存。
type Options struct {
	// Clock は nil で clock.System()。
	Clock clock.Clock
	// Logger は nil で slog.Default()。
	Logger *slog.Logger
}

// New は Service を組み立てる。必須依存が nil ならエラー。
//
// nil チェックをここでまとめてやるのは、Cloud Run Job の 1 実行が短命で、
// 途中の nil panic だと「何が足りなかったか」がログに残りにくいため。
func New(cfg Config, lister EntryLister, store ledger.Store, sender webhook.Sender, opts Options) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if lister == nil {
		return nil, errors.New("contractkicker: lister is required")
	}
	if store == nil {
		return nil, errors.New("contractkicker: store is required")
	}
	if sender == nil {
		return nil, errors.New("contractkicker: sender is required")
	}

	// 任意依存はゼロ値で使えるように既定へ落とす。
	clk := opts.Clock
	if clk == nil {
		clk = clock.System()
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Service{cfg: cfg, lister: lister, store: store, sender: sender, clk: clk, log: log}, nil
}

// Summary は 1 実行の結果サマリ。
//
// 通知は Cloud Logging のみなので、この構造体がそのまま「その実行で何が
// 起きたか」の記録になる。ログを見たときに件数の内訳が分かるよう、
// 検知系（Detected/NewlyQueued）と送信系（Selected/Sent/Failed/GaveUp）を
// 分けて持つ。
type Summary struct {
	// Detected は ITANDI から返った対象ステータスの申込件数。
	Detected int
	// NewlyQueued は台帳へ新規 pending 登録した件数。
	NewlyQueued int
	// Selected は送信対象として選ばれた件数。
	Selected int
	// Sent は Heroku が受け付けた件数。
	Sent int
	// Failed はリトライ待ちにした件数。
	Failed int
	// GaveUp はリトライ上限に達した件数。
	GaveUp int
	// DryRun は DRY_RUN で送信を見送った件数（台帳は未更新）。
	DryRun int
	// ListTruncated は一覧が 500 件上限に当たった疑いがあることを示す。
	ListTruncated bool
}

// LogAttrs はサマリをログ属性へ変換する。
func (s Summary) LogAttrs() []any {
	return []any{
		"detected", s.Detected,
		"newlyQueued", s.NewlyQueued,
		"selected", s.Selected,
		"sent", s.Sent,
		"failed", s.Failed,
		"gaveUp", s.GaveUp,
		"dryRun", s.DryRun,
		"listTruncated", s.ListTruncated,
	}
}

// Run は Phase A → Phase B を通して 1 回実行する。
// Cloud Scheduler から 15 分ごとに起動される想定で、これがジョブの本体。
//
// Phase A が失敗しても Phase B は実行する（ITANDI 側の一時障害で、既に台帳へ
// 積まれた分の送信まで止めないため）。エラーは両フェーズ分をまとめて返し、
// 呼び出し側（main）が終了コード 1 にする。
func (s *Service) Run(ctx context.Context) (Summary, error) {
	var summary Summary
	var errs []error

	// Phase A: 検知。失敗しても errs に積んで Phase B へ進む。
	detected, err := s.Detect(ctx)
	summary.Detected = detected.Detected
	summary.NewlyQueued = detected.NewlyQueued
	summary.ListTruncated = detected.ListTruncated
	if err != nil {
		s.log.ErrorContext(ctx, "detect phase failed", "error", err.Error())
		errs = append(errs, err)
	}

	// Phase B: 送信。台帳にあるものが対象なので Phase A の成否とは独立。
	dispatched, err := s.Dispatch(ctx)
	summary.Selected = dispatched.Selected
	summary.Sent = dispatched.Sent
	summary.Failed = dispatched.Failed
	summary.GaveUp = dispatched.GaveUp
	summary.DryRun = dispatched.DryRun
	if err != nil {
		s.log.ErrorContext(ctx, "dispatch phase failed", "error", err.Error())
		errs = append(errs, err)
	}

	// 実行ごとに 1 行、必ずサマリを残す（後追いの調査はこの行が起点になる）。
	s.log.InfoContext(ctx, "run finished", summary.LogAttrs()...)
	return summary, errors.Join(errs...)
}

// DetectResult は Phase A の結果。
type DetectResult struct {
	Detected      int
	NewlyQueued   int
	ListTruncated bool
}

// Detect は Phase A（検知）を実行する。
//
// 取得窓に入った対象ステータスの申込のうち、台帳に無いものを pending として
// 登録する。この時点では送信しない。
//
// 「検知したら即送信」にしていないのは、送信が 1 実行 1 件に絞られているため。
// 即送信だと 1 件目を送っている間に残りが ITANDI の取得窓（datetime）から
// 外れ、二度と拾えなくなる。いったん台帳へ積めば窓と無関係に処理できる。
func (s *Service) Detect(ctx context.Context) (DetectResult, error) {
	// 取得窓は「今」を基準にしたローリングウィンドウ。
	// ITANDI は datetime 未指定だと当日分しか返さないため必ず明示する。
	now := s.clk.Now()
	since := now.Add(-s.cfg.Lookback)

	entries, err := s.lister.ListEntryHeads(ctx, itandi.ListParams{
		UpdatedSince: since,
		StatusIDs:    []int{s.cfg.TargetStatusID},
	})
	if err != nil {
		return DetectResult{}, fmt.Errorf("detect: list entry heads: %w", err)
	}

	res := DetectResult{Detected: len(entries)}
	// ITANDI は 500 件で黙って打ち切る（エラーにならない）。ちょうど上限なら
	// 取りこぼしを疑い、LOOKBACK_HOURS を狭めて複数回流す運用判断が必要になる。
	if len(entries) >= itandi.MaxListResults {
		res.ListTruncated = true
		s.log.WarnContext(ctx, "entry head list hit the API result cap; some entries may be missing",
			"detected", len(entries),
			"cap", itandi.MaxListResults,
			"lookbackHours", s.cfg.Lookback.Hours(),
		)
	}

	// 1 件ごとの失敗で全体を止めない。台帳が一時的に読めなくても、
	// 次の実行で同じ申込がまた窓に入る（更新日時は変わらないが窓は広い）ので
	// 取りこぼしにはなりにくい。
	var errs []error
	for _, e := range entries {
		// 台帳のキーは申込 ID の文字列表現。
		id := strconv.FormatInt(e.ID, 10)
		log := s.log.With("entryHeadId", id)

		_, err := s.store.Get(ctx, id)
		switch {
		case err == nil:
			// 既知の申込。ステータスが巻き戻って再度 120 になっても再送はしない。
			log.DebugContext(ctx, "already in ledger; skipped")
			continue
		case !errors.Is(err, ledger.ErrNotFound):
			errs = append(errs, fmt.Errorf("detect: get %s: %w", id, err))
			continue
		}

		// 未登録なので pending として積む。送信はこの実行では行わない。
		queued := ledger.Entry{
			ID:         id,
			Status:     ledger.StatusPending,
			DetectedAt: now,
			UpdatedAt:  now,
		}
		if err := s.store.Put(ctx, queued); err != nil {
			errs = append(errs, fmt.Errorf("detect: put %s: %w", id, err))
			continue
		}
		res.NewlyQueued++
		log.InfoContext(ctx, "queued new contract entry", "entryStatusId", e.EntryStatusID)
	}

	s.log.InfoContext(ctx, "detect phase finished",
		"detected", res.Detected,
		"newlyQueued", res.NewlyQueued,
		"since", since.UTC().Format(time.RFC3339),
	)
	return res, errors.Join(errs...)
}

// DispatchResult は Phase B の結果。
type DispatchResult struct {
	Selected int
	Sent     int
	Failed   int
	GaveUp   int
	// DryRun は DRY_RUN で送信を見送った件数。台帳は更新していない。
	DryRun int
}

// Dispatch は Phase B（送信）を実行する。1 実行あたり SendLimitPerRun 件。
//
// 件数を絞るのは Heroku 側（Playwright + Drive アップロード）への流量制御。
// 15 分間隔 × 1 件なので、バックログ N 件の消化には N/96 日かかる点に注意。
func (s *Service) Dispatch(ctx context.Context) (DispatchResult, error) {
	targets, err := s.selectTargets(ctx, s.cfg.SendLimitPerRun)
	if err != nil {
		return DispatchResult{}, err
	}

	res := DispatchResult{Selected: len(targets)}
	// 送るものが無いのは正常系（多くの実行はここで終わる）。
	if len(targets) == 0 {
		s.log.InfoContext(ctx, "no entry to send")
		return res, nil
	}

	var errs []error
	for _, target := range targets {
		// send は台帳への書き戻しまで済ませ、確定したステータスを返す。
		outcome, err := s.send(ctx, target)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch outcome {
		case ledger.StatusDone:
			res.Sent++
		case ledger.StatusFailed:
			res.Failed++
		case ledger.StatusGaveUp:
			res.GaveUp++
		case statusDryRun:
			res.DryRun++
		}
	}
	return res, errors.Join(errs...)
}

// selectTargets は送信対象を優先順位に従って選ぶ。
//
//  1. pending を detectedAt 昇順（＝申込の検知が古いものから）
//  2. 足りなければ failed かつ nextRetryAt <= now を nextRetryAt 昇順
//
// pending を先に見るのが肝。逆にすると、恒久的に失敗する 1 件が毎回選ばれ続けて
// 後続の未送信分が全部詰まる（1 実行 1 件なので枠を 1 件に奪われる）。
// done / gaveup は対象外（Status で絞るので自然に外れる）。
func (s *Service) selectTargets(ctx context.Context, limit int) ([]ledger.Entry, error) {
	if limit < 1 {
		return nil, nil
	}

	pending, err := s.store.List(ctx, ledger.Query{
		Status:  ledger.StatusPending,
		OrderBy: ledger.OrderDetectedAt,
		Limit:   limit,
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch: list pending: %w", err)
	}
	// pending で枠が埋まったらリトライ分は見ない（次回以降に回す）。
	if len(pending) >= limit {
		return pending[:limit], nil
	}

	// 余った枠だけリトライへ回す。nextRetryAt が未来のものは対象外。
	retryable, err := s.store.List(ctx, ledger.Query{
		Status:          ledger.StatusFailed,
		NextRetryBefore: s.clk.Now(),
		OrderBy:         ledger.OrderNextRetryAt,
		Limit:           limit - len(pending),
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch: list retryable: %w", err)
	}
	return append(pending, retryable...), nil
}

// statusDryRun は send() の戻り値専用のマーカー。
//
// ledger.Status として保存されることはない（台帳へは書かないため）。
// 「送信も台帳更新もしなかった」を done/failed と区別するために使う。
const statusDryRun ledger.Status = "dryrun"

// contractPayload は Heroku POST /webhook/itandi/contract のボディ。
//
// 送るのは申込 ID だけ。ステータスが本当に 120 かは Heroku 側が申込詳細を
// 再取得して再判定するので、キッカー側で二重にチェックしない
// （巻き戻り分は Heroku が status=skipped の 200 で安全に落とす）。
type contractPayload struct {
	ID int64 `json:"id"`
}

// send は 1 件を送信し、結果を台帳へ書き戻す。返り値は確定したステータス。
//
// 「送信 → 台帳更新」の順なので、間で落ちると送信済みだが未記録の状態が残り、
// 次回同じ申込を再送する可能性がある。Heroku 側が同名スキップで冪等なため
// 実害は小さいが、記録失敗は ERROR で目立たせる（下部参照）。
func (s *Service) send(ctx context.Context, e ledger.Entry) (ledger.Status, error) {
	log := s.log.With("entryHeadId", e.ID)

	// 台帳のキーは数値 ID の文字列表現。ここで壊れているのはデータ不整合なので
	// 送信せずエラーにする（何度リトライしても直らない）。
	entryID, err := strconv.ParseInt(e.ID, 10, 64)
	if err != nil {
		log.ErrorContext(ctx, "ledger entry has a non-numeric id; skipped", "error", err.Error())
		return "", fmt.Errorf("dispatch: invalid ledger id %q: %w", e.ID, err)
	}

	// Heroku 側は非同期（受付だけ返す）なので、待つのは受付レスポンスのみ。
	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()

	res, sendErr := s.sender.Send(sendCtx, contractPayload{ID: entryID})

	// DRY_RUN では台帳を更新しない。
	// ここで done を書いてしまうと、DRY_RUN を外したときに「台帳上は処理済み
	// なので誰も送らない」状態になり、検証したはずの申込がまるごと落ちる。
	// 台帳を触らないので、対象は次の実行でも pending のまま残る。
	if sendErr == nil && res.DryRun {
		log.InfoContext(ctx, "dry run: entry not sent and ledger not updated",
			"attempts", e.Attempts,
			"status", string(e.Status),
		)
		return statusDryRun, nil
	}

	// 成否にかかわらず「試行した」ので attempts は必ず進める。
	// これが無いと恒久障害時に gaveup へ到達せず無限にリトライし続ける。
	now := s.clk.Now()
	updated := e
	updated.Attempts = e.Attempts + 1
	updated.SentAt = now
	updated.UpdatedAt = now

	switch {
	// 2xx（accepted / skipped どちらも 200）と DRY_RUN は受付成功扱い。
	// ここでの done は「Heroku が受け付けた」であって「Drive へ格納された」
	// ではない点に注意（実際の格納は Heroku 側のログで担保する）。
	case sendErr == nil && res.Accepted():
		updated.Status = ledger.StatusDone
		updated.JobID = res.JobID
		updated.LastError = ""
		updated.NextRetryAt = time.Time{}
	// 2xx 以外、タイムアウト、接続失敗はすべてここ。
	default:
		updated.LastError = failureReason(res, sendErr)
		// 上限到達なら gaveup。以降 selectTargets の対象から外れる。
		if updated.Attempts >= s.cfg.MaxAttempts {
			updated.Status = ledger.StatusGaveUp
			updated.NextRetryAt = time.Time{}
		} else {
			// まだ余地があるので指数的に間隔を空けて再送対象に残す。
			updated.Status = ledger.StatusFailed
			updated.NextRetryAt = s.cfg.Backoff.NextAt(now, updated.Attempts)
		}
	}

	if err := s.store.Put(ctx, updated); err != nil {
		// 送信済みなのに台帳へ書けないのが最悪のケース（二重送信の元）。
		// 明示的に ERROR で残す。
		log.ErrorContext(ctx, "failed to record send result; entry may be sent twice",
			"status", string(updated.Status),
			"error", err.Error(),
		)
		return "", fmt.Errorf("dispatch: put %s: %w", e.ID, err)
	}

	attrs := []any{
		"status", string(updated.Status),
		"attempts", updated.Attempts,
		"result", res.Summary(),
	}
	if updated.JobID != "" {
		attrs = append(attrs, "herokuJobId", updated.JobID)
	}
	if !updated.NextRetryAt.IsZero() {
		attrs = append(attrs, "nextRetryAt", updated.NextRetryAt.UTC().Format(time.RFC3339))
	}

	// severity の使い分け: 受付成功=INFO / リトライ継続=WARNING / 上限到達=ERROR。
	// 将来アラートを付けるときは ERROR に対する Log-based Alert を張れば足りる。
	switch updated.Status {
	case ledger.StatusDone:
		log.InfoContext(ctx, "entry accepted by heroku", attrs...)
	case ledger.StatusFailed:
		log.WarnContext(ctx, "send failed; will retry", append(attrs, "error", updated.LastError)...)
	case ledger.StatusGaveUp:
		log.ErrorContext(ctx, "send failed and reached max attempts; giving up",
			append(attrs, "error", updated.LastError)...)
	}
	return updated.Status, nil
}

// failureReason は台帳へ残す失敗理由を組み立てる。
//
// HTTP まで到達したか（res）、そこに至らなかったか（err）で情報が違うため、
// 両方ある場合は連結する。lastError は運用者が原因を切り分ける手掛かりなので
// 「HTTP 502: read: eof」のように両方残す。
func failureReason(res webhook.Result, err error) string {
	if err != nil {
		if res.StatusCode != 0 {
			return fmt.Sprintf("%s: %v", res.Summary(), err)
		}
		return err.Error()
	}
	return res.Summary()
}
