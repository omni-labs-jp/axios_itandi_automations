// Package logging は Cloud Logging が解釈できる構造化 JSON ログを出す
// slog.Logger を組み立てる。
//
// Cloud Logging は JSON ログの "severity" / "message" フィールドを特別扱いする
// ため、slog 既定の "level" / "msg" をリネームしている。これをやらないと
// すべてのログが severity=DEFAULT 扱いになり、ERROR で絞り込めない。
//
// 通知は Cloud Logging のみ（Slack 通知なし）という方針なので、
// 将来アラートが必要になったら ERROR に Log-based Alert を張れば足りる。
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Options はロガーの組み立てオプション。
type Options struct {
	// Level はこのレベル未満のログを捨てる。ゼロ値は slog.LevelInfo。
	Level slog.Level
	// AddSource は呼び出し元のファイル/行を付ける。
	AddSource bool
}

// New は w へ Cloud Logging 互換の JSON を書く Logger を返す。
func New(w io.Writer, opts Options) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       opts.Level,
		AddSource:   opts.AddSource,
		ReplaceAttr: replaceAttr,
	})
	return slog.New(h)
}

// NewStdout は標準出力へ書く Logger を返す（Cloud Run Jobs の既定経路）。
func NewStdout(opts Options) *slog.Logger { return New(os.Stdout, opts) }

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		if t, ok := a.Value.Any().(time.Time); ok {
			return slog.String("time", t.UTC().Format(time.RFC3339Nano))
		}
	case slog.LevelKey:
		if lv, ok := a.Value.Any().(slog.Level); ok {
			return slog.String("severity", Severity(lv))
		}
	case slog.MessageKey:
		return slog.Attr{Key: "message", Value: a.Value}
	}
	return a
}

// Severity は slog.Level を Cloud Logging の severity 文字列へ変換する。
func Severity(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO"
	case l < slog.LevelError:
		return "WARNING"
	case l < slog.LevelError+4:
		return "ERROR"
	default:
		return "CRITICAL"
	}
}

// ParseLevel は "debug" / "info" / "warn"(warning) / "error" を解釈する。
//
// 未知の値は info 扱い。LOG_LEVEL のタイプミスでジョブが起動しないのは
// 割に合わないため、ここだけはエラーにしない。
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
