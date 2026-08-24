// Package clock は時刻取得を差し替え可能にする最小の仕組み。
//
// キッカーは「取得窓 = now - LOOKBACK_HOURS」「nextRetryAt = now + backoff」と
// 時刻に依存した判断が多いため、テストでは Fake を差して時間を任意に進める。
package clock

import (
	"sync"
	"time"
)

// Clock は現在時刻の取得のみを抽象化する。
type Clock interface {
	Now() time.Time
}

// Func は関数を Clock として扱うためのアダプタ。
type Func func() time.Time

// Now implements Clock.
func (f Func) Now() time.Time { return f() }

// System は実時間を返す Clock。
func System() Clock { return Func(time.Now) }

// Fake はテスト用の固定 Clock。並行アクセスも安全。
// Advance / Set で「15 分後の実行」「リトライ期限ちょうど」などを再現する。
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake は指定時刻で止まった Fake を返す。
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

// Now implements Clock.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance は時刻を d だけ進める。
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set は時刻を上書きする。
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}
