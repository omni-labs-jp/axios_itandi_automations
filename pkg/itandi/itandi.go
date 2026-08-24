// Package itandi は ITANDI「申込受付くん」管理会社向け API v2 のクライアント。
//
// 認証はトークン方式（Authorization: Token {token}）で、送信元 IP の許可設定が
// 必要な点に注意（許可外からは 401/403 で弾かれる）。ローカルや Cloud Run から
// 叩いて 401/403 が返る場合、まず実装バグではなく IP 制限を疑う。
//
// この API の癖（実装に影響する順）:
//   - ページング用パラメータが無い。件数は datetime / end_datetime の窓幅でしか
//     制御できない
//   - 1 回で最大 500 件（MaxListResults）。超過分はエラーではなく黙って切り捨て
//   - datetime を省略すると「当日中に更新があった分」しか返らない
//   - 更新日時はステータス変更でも更新される。つまり「契約」に変わった瞬間に
//     窓へ入るので、定期実行で拾える
package itandi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// ProductionBaseURL は本番 API のベース URL。
	ProductionBaseURL = "https://moushikomi-uketsukekun.com/maintenance_company/api/v2"
	// StagingBaseURL は検証環境の API ベース URL。
	StagingBaseURL = "https://staging.moushikomi-uketsukekun.com/maintenance_company/api/v2"

	// StatusContract は申込ステータス「契約」の ID。
	StatusContract = 120

	// MaxListResults は /entry_heads が 1 回で返す最大件数。
	// 超過分はエラーにならず黙って切り捨てられるため、レスポンスが
	// ちょうどこの件数だった場合は取りこぼしを疑う必要がある。
	MaxListResults = 500

	// datetimeLayout は datetime / end_datetime クエリの書式。
	// API 仕様の例にタイムゾーンが無いため、オフセット無しで送る。
	datetimeLayout = "2006-01-02T15:04:05"

	defaultTimeout = 30 * time.Second
)

// EntryHead は申込一覧/詳細のレスポンス 1 件分。
//
// 実際のレスポンスには properties[] や entry_user など多数のフィールドがあるが、
// キッカーが使うのは id / entry_status_id / updated_at だけ。ログや将来の
// 突き合わせで役に立つ範囲だけ型付けし、それ以外は意図的に落としている
// （項目追加で壊れないようにするため）。
type EntryHead struct {
	ID                 int64     `json:"id"`
	CompanyMasterID    int64     `json:"company_master_id"`
	EntryStatusID      int       `json:"entry_status_id"`
	Paper              bool      `json:"paper"`
	Corp               bool      `json:"corp"`
	ForeignContractKey *string   `json:"foreign_contract_key"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	Priority           int       `json:"priority"`
}

// APIError は 2xx 以外のレスポンスを表す。
// errors.As で取り出してステータスコードごとの判断ができるようにしている。
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

// Error implements error.
func (e *APIError) Error() string {
	body := e.Body
	const maxBody = 512
	if len(body) > maxBody {
		body = body[:maxBody] + "…(truncated)"
	}
	return fmt.Sprintf("itandi: %s %s: unexpected status %d: %s", e.Method, e.Path, e.StatusCode, body)
}

// IsUnauthorized は認証・IP 許可の問題らしいかを返す。
// ITANDI は IP 制限違反も 401/403 で返すため、両者は区別できない。
func (e *APIError) IsUnauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// Client は ITANDI API クライアント。ゴルーチンセーフ。
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	location   *time.Location
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
//
// 内部で http.Client を作り直すので、WithHTTPClient と併用する場合は
// 後に渡した方が勝つ。テストでは WithHTTPClient を先に渡している。
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.httpClient = &http.Client{Timeout: d}
		}
	}
}

// WithLocation は datetime クエリを組み立てる際のタイムゾーンを指定する。既定は UTC。
//
// API 仕様の例にオフセットが無く、サーバ側の解釈（JST か UTC か）が確定できない。
// 既定の UTC で送ると、サーバが JST と解釈した場合は窓が 9 時間ぶん過去へ
// ずれるだけで、取りこぼしにはならないため安全側に倒している。
func WithLocation(loc *time.Location) Option {
	return func(c *Client) {
		if loc != nil {
			c.location = loc
		}
	}
}

// NewClient はクライアントを生成する。
// 設定不備（URL・トークン欠落）は起動時に落としたいのでここで検証する。
func NewClient(baseURL, token string, opts ...Option) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("itandi: baseURL is required")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("itandi: invalid baseURL: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("itandi: token is required")
	}

	c := &Client{
		baseURL:    baseURL,
		token:      strings.TrimSpace(token),
		httpClient: &http.Client{Timeout: defaultTimeout},
		location:   time.UTC,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// ListParams は GET /entry_heads のクエリ条件。
// ゼロ値のフィールドはクエリに含めない（＝API 側の既定動作に任せる）。
type ListParams struct {
	// UpdatedSince は datetime に対応。ゼロ値なら送らない
	// （＝当日中に更新があった申込のみが返る）。
	UpdatedSince time.Time
	// UpdatedUntil は end_datetime に対応。ゼロ値なら送らない。
	UpdatedUntil time.Time
	// StatusIDs は entry_status_ids[] に対応。空なら絞り込みなし。
	StatusIDs []int
}

func (c *Client) query(p ListParams) url.Values {
	q := url.Values{}
	if !p.UpdatedSince.IsZero() {
		q.Set("datetime", p.UpdatedSince.In(c.location).Format(datetimeLayout))
	}
	if !p.UpdatedUntil.IsZero() {
		q.Set("end_datetime", p.UpdatedUntil.In(c.location).Format(datetimeLayout))
	}
	for _, id := range p.StatusIDs {
		q.Add("entry_status_ids[]", strconv.Itoa(id))
	}
	return q
}

// ListEntryHeads は申込一覧を取得する。
//
// ページング用パラメータは API に存在せず、1 回で最大 MaxListResults 件しか
// 返らない。しかも超過は黙って切り捨てられるため、
// len(結果) == MaxListResults なら「取りこぼしている」と考えて期間を分割して
// 取り直す必要がある（呼び出し側の責務。Service.Detect が WARNING を出す）。
func (c *Client) ListEntryHeads(ctx context.Context, p ListParams) ([]EntryHead, error) {
	var out []EntryHead
	if err := c.do(ctx, http.MethodGet, "/entry_heads", c.query(p), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetEntryHead は申込詳細を取得する。
func (c *Client) GetEntryHead(ctx context.Context, id int64) (*EntryHead, error) {
	if id <= 0 {
		return nil, fmt.Errorf("itandi: invalid entry head id %d", id)
	}
	var out EntryHead
	path := "/entry_heads/" + strconv.FormatInt(id, 10)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// do は GET リクエストの共通処理（URL 組み立て → 認証ヘッダ → 実行 →
// ステータス判定 → JSON デコード）。レスポンスは 500 件でも数百 KB 程度なので
// 全部読んでから扱う（エラー時に本文をそのままログへ残せる利点もある）。
func (c *Client) do(ctx context.Context, method, path string, q url.Values, dst any) error {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return fmt.Errorf("itandi: build request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("itandi: %s %s: %w", method, path, err)
	}
	// 読み切った後の Close エラーは扱いようがないので明示的に捨てる。
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("itandi: %s %s: read body: %w", method, path, err)
	}
	// 2xx 以外は本文込みで APIError にする（原因調査は本文を見ないと分からない）。
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &APIError{StatusCode: res.StatusCode, Method: method, Path: path, Body: string(body)}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("itandi: %s %s: decode body: %w", method, path, err)
	}
	return nil
}
