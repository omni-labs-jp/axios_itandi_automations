// Package env は環境変数の読み込みとバリデーションをまとめて行う。
//
// Loader はエラーを蓄積するため、設定不備を 1 回の起動で全部まとめて
// 報告できる（起動 → 落ちる → 直す のループを最小化する）。
package env

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// LookupFunc は環境変数 1 件の取得を抽象化する（テスト差し替え用）。
type LookupFunc func(key string) (string, bool)

// Loader は環境変数を読みつつエラーを蓄積する。
//
// 各メソッドはエラー時も「既定値を返して処理を続ける」。呼び出し側は最後に
// Err() を 1 回見るだけでよく、読み取りコードが if err の連鎖にならない。
// ゴルーチンセーフではない（起動時に 1 スレッドで使う想定）。
type Loader struct {
	lookup LookupFunc
	errs   []error
}

// New は os.LookupEnv を使う Loader を返す。
func New() *Loader { return &Loader{lookup: os.LookupEnv} }

// NewFromMap はマップを参照する Loader を返す（テスト用）。
func NewFromMap(m map[string]string) *Loader {
	return &Loader{lookup: func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}}
}

// NewWithLookup は任意の LookupFunc を使う Loader を返す。
func NewWithLookup(f LookupFunc) *Loader { return &Loader{lookup: f} }

// Err は蓄積されたエラーをまとめて返す。無ければ nil。
func (l *Loader) Err() error { return errors.Join(l.errs...) }

func (l *Loader) fail(key string, err error) {
	l.errs = append(l.errs, fmt.Errorf("env %s: %w", key, err))
}

// raw はトリム済みの値と、値が設定されているかを返す。
//
// 空文字は「未設定」として扱う。Terraform や Cloud Run のコンソールでは
// 変数を空文字で残してしまいがちで、それを 0 や false と解釈するより
// 既定値へ落ちた方が安全なため。
func (l *Loader) raw(key string) (string, bool) {
	v, ok := l.lookup(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

// String は未設定なら def を返す。
func (l *Loader) String(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

// Require は必須の文字列。未設定ならエラーを蓄積して空文字を返す。
func (l *Loader) Require(key string) string {
	v, ok := l.raw(key)
	if !ok {
		l.fail(key, errors.New("required but not set"))
		return ""
	}
	return v
}

// Int は整数。パースできなければエラーを蓄積して def を返す。
func (l *Loader) Int(key string, def int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, fmt.Errorf("invalid integer %q", v))
		return def
	}
	return n
}

// PositiveInt は 1 以上の整数を要求する。
// LOOKBACK_HOURS や SEND_LIMIT_PER_RUN が 0 だと何もしないジョブになるので、
// 既定値へ落とさず設定ミスとして報告する。
func (l *Loader) PositiveInt(key string, def int) int {
	n := l.Int(key, def)
	if n < 1 {
		l.fail(key, fmt.Errorf("must be >= 1, got %d", n))
		return def
	}
	return n
}

// Bool は true/false/1/0/yes/no を受け付ける。
func (l *Loader) Bool(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "t", "true", "yes", "y", "on":
		return true
	case "0", "f", "false", "no", "n", "off":
		return false
	default:
		l.fail(key, fmt.Errorf("invalid boolean %q", v))
		return def
	}
}

// Duration は "30s" 形式の期間。
func (l *Loader) Duration(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, fmt.Errorf("invalid duration %q", v))
		return def
	}
	if d <= 0 {
		l.fail(key, fmt.Errorf("must be positive, got %s", d))
		return def
	}
	return d
}

// Millis はミリ秒指定の整数を Duration として読む。
// REQUEST_TIMEOUT_MS のように単位が変数名に含まれている場合に使う。
func (l *Loader) Millis(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, fmt.Errorf("invalid milliseconds %q", v))
		return def
	}
	if n <= 0 {
		l.fail(key, fmt.Errorf("must be positive, got %d", n))
		return def
	}
	return time.Duration(n) * time.Millisecond
}

// DurationList は "15m,1h,4h" 形式のカンマ区切り期間リスト。
// リトライのバックオフ段階を環境変数で調整できるようにするためのもの。
func (l *Loader) DurationList(key string, def []time.Duration) []time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]time.Duration, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		d, err := time.ParseDuration(p)
		if err != nil {
			l.fail(key, fmt.Errorf("invalid duration %q in list", p))
			return def
		}
		if d < 0 {
			l.fail(key, fmt.Errorf("negative duration %q in list", p))
			return def
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		l.fail(key, errors.New("empty duration list"))
		return def
	}
	return out
}
