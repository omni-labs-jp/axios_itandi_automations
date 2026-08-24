// Package retry はリトライ間隔の算出を提供する。
//
// プロセス内で待つのではなく「次にいつ試すか」を時刻として返すだけなので、
// 定期実行ジョブ（実行のたびにプロセスが死ぬ）でも使える。
package retry

import (
	"errors"
	"fmt"
	"time"
)

// Backoff は「試行回数 → 次回までの待ち時間」を表す固定スケジュール。
//
// 計算式ではなく配列にしているのは、運用側が「何回目に何分待つか」を
// 環境変数（RETRY_SCHEDULE）でそのまま指定・確認できるようにするため。
//
// 例: Schedule = [15m, 1h, 4h] のとき
//
//	Delay(1) = 15m（1 回失敗した直後）
//	Delay(2) = 1h
//	Delay(3) = 4h
//	Delay(4) = 4h（最後の要素で飽和）
type Backoff struct {
	Schedule []time.Duration
}

// DefaultSchedule は実装手順書で定めた既定値。
var DefaultSchedule = []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour}

// Default は既定スケジュールの Backoff を返す。
func Default() Backoff {
	s := make([]time.Duration, len(DefaultSchedule))
	copy(s, DefaultSchedule)
	return Backoff{Schedule: s}
}

// New はスケジュールを検証して Backoff を作る。
func New(schedule []time.Duration) (Backoff, error) {
	if len(schedule) == 0 {
		return Backoff{}, errors.New("retry: schedule must not be empty")
	}
	for i, d := range schedule {
		if d < 0 {
			return Backoff{}, fmt.Errorf("retry: schedule[%d] is negative (%s)", i, d)
		}
	}
	s := make([]time.Duration, len(schedule))
	copy(s, schedule)
	return Backoff{Schedule: s}, nil
}

// Delay は attempt 回目の失敗後に待つ時間を返す。
// attempt は 1 以上。範囲外/未設定時はスケジュール端で飽和する。
func (b Backoff) Delay(attempt int) time.Duration {
	if len(b.Schedule) == 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(b.Schedule) {
		attempt = len(b.Schedule)
	}
	return b.Schedule[attempt-1]
}

// NextAt は attempt 回目の失敗後に再試行可能となる時刻を返す。
// 台帳の nextRetryAt に入れる値をこれで求める。
func (b Backoff) NextAt(now time.Time, attempt int) time.Time {
	return now.Add(b.Delay(attempt))
}
