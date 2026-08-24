// Package webhook は JSON ボディを POST する汎用 Webhook クライアント。
//
// 本ツールでは Heroku 側 itandi-automation の
// POST /webhook/itandi/contract 送信に使うが、URL とペイロードを差し替えれば
// 他の自動化からもそのまま使える。
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout = 30 * time.Second
	// maxBodySnippet はログ・エラーに残すレスポンス本文の最大長。
	maxBodySnippet = 512
)

// Result は 1 回の送信結果。HTTP レイヤまで到達した場合に返る。
//
// Heroku 側 /webhook/itandi/contract は成功時に
//
//	{"status":"accepted","jobId":"..."} または
//	{"status":"skipped","reason":"status_not_target"}
//
// を返す（どちらも 200）。前者は処理キュー投入、後者はステータスが対象外で
// 何もしなかったケース。キッカーから見ればどちらも「受け付けられた」。
type Result struct {
	// StatusCode は HTTP ステータス。DryRun 時は 0。
	StatusCode int
	// Body はレスポンス本文（maxBodySnippet で切り詰め）。
	Body string
	// Status / JobID / Reason は Heroku 側レスポンスの既知フィールド。
	// 相手が JSON を返さない場合は空。
	Status string
	JobID  string
	Reason string
	// DryRun は実際には送信していないことを示す。
	DryRun bool
}

// Accepted は相手に受け付けられた（2xx、または DryRun）かを返す。
// リトライすべきかの判断はこの 1 箇所に集約している。
func (r Result) Accepted() bool {
	return r.DryRun || (r.StatusCode >= 200 && r.StatusCode <= 299)
}

// Summary はログ・台帳に残す 1 行要約。
// JSON を返す相手なら status/reason、そうでなければ本文をそのまま添える。
func (r Result) Summary() string {
	if r.DryRun {
		return "dry-run: not sent"
	}
	s := fmt.Sprintf("HTTP %d", r.StatusCode)
	switch {
	case r.Status != "":
		s += " status=" + r.Status
		if r.Reason != "" {
			s += " reason=" + r.Reason
		}
	case r.Body != "":
		s += " body=" + r.Body
	}
	return s
}

// Sender は Webhook 送信を抽象化する。Client と DryRunSender が実装する。
//
// DRY_RUN の切り替えを「呼び出し側の if 分岐」ではなく実装の差し替えで
// 表現するための interface。呼び出し側（Service）は DRY_RUN を知らない。
type Sender interface {
	Send(ctx context.Context, payload any) (Result, error)
}

// Client は単一の URL へ JSON を POST する Sender。ゴルーチンセーフ。
type Client struct {
	url        string
	httpClient *http.Client
	headers    map[string]string
}

// Option は Client の設定を変更する。
type Option func(*Client)

// WithHTTPClient は使用する *http.Client を差し替える。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithTimeout は既定 http.Client のタイムアウトを設定する。
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.httpClient = &http.Client{Timeout: d}
		}
	}
}

// WithHeader は追加ヘッダを設定する。
// Heroku 側エンドポイントは現状ノーガードだが、後から共有シークレット
// （X-Kicker-Token 等）を足したときにこれで対応できる。
func WithHeader(key, value string) Option {
	return func(c *Client) {
		if key == "" || value == "" {
			return
		}
		if c.headers == nil {
			c.headers = map[string]string{}
		}
		c.headers[key] = value
	}
}

// NewClient は送信先 URL を検証してクライアントを生成する。
//
// URL は Secret Manager 経由で渡ってくるため、値の取り違え（空・スキーム欠落）を
// 起動時に検出できるようここで検証しておく。
func NewClient(endpoint string, opts ...Option) (*Client, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("webhook: endpoint is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("webhook: invalid endpoint: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("webhook: endpoint must be http(s), got %q", endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("webhook: endpoint has no host: %q", endpoint)
	}

	c := &Client{url: endpoint, httpClient: &http.Client{Timeout: defaultTimeout}}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// URL は送信先を返す（ログ用）。
func (c *Client) URL() string { return c.url }

// Send は payload を JSON として POST する。
//
// 返る error はリクエストを送れなかった/応答を読めなかった場合のみ。
// 2xx 以外は error ではなく Result.Accepted() == false で表現する。
// こう分けているのは、呼び出し側が「送れなかった」と「送れたが断られた」を
// 同じ経路で（attempts++ とバックオフで）扱いたいのに対し、台帳へ残す
// lastError の内容は両者で変えたいため。
func (c *Client) Send(ctx context.Context, payload any) (Result, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Result{}, fmt.Errorf("webhook: encode payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("webhook: POST %s: %w", c.url, err)
	}
	// 読み切った後の Close エラーは扱いようがないので明示的に捨てる。
	defer func() { _ = res.Body.Close() }()

	// 相手が巨大なエラーページを返しても引きずられないよう上限を切る。
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Result{StatusCode: res.StatusCode}, fmt.Errorf("webhook: POST %s: read body: %w", c.url, err)
	}

	out := Result{StatusCode: res.StatusCode, Body: snippet(raw)}
	var parsed struct {
		Status string `json:"status"`
		JobID  string `json:"jobId"`
		Reason string `json:"reason"`
	}
	// JSON でない相手（プロキシのエラーページ等）でも失敗させない。
	// 取れたら埋める、取れなければ Body に生テキストが残る、という扱い。
	if err := json.Unmarshal(raw, &parsed); err == nil {
		out.Status, out.JobID, out.Reason = parsed.Status, parsed.JobID, parsed.Reason
	}
	return out, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > maxBodySnippet {
		return s[:maxBodySnippet] + "…(truncated)"
	}
	return s
}

// DryRunSender は送信せずログだけ出す Sender。DRY_RUN=true で使う。
//
// 検証環境が無く本番のみの構成なので、初回デプロイでは必ずこれを通して
// 「どの申込が対象になるか」をログで確認してから実送信へ切り替える。
//
// Accepted() は true を返すが Result.DryRun も立てる。呼び出し側は DryRun を見て
// 「送信していない」ことを区別できる（キッカーはこの場合に台帳を更新しない）。
type DryRunSender struct {
	// Logger は nil でも安全（その場合は何も出さない）。
	Logger *slog.Logger
	// Target はログに出す想定送信先。
	Target string
}

// Send implements Sender. 常に受付成功（DryRun）を返す。
func (d DryRunSender) Send(_ context.Context, payload any) (Result, error) {
	if d.Logger != nil {
		attrs := []any{"target", d.Target}
		if b, err := json.Marshal(payload); err == nil {
			attrs = append(attrs, "payload", string(b))
		}
		d.Logger.Info("dry run: webhook not sent", attrs...)
	}
	return Result{DryRun: true}, nil
}
