# axios_itandi_automations

Axios 向けの ITANDI 自動化対応ツール（Go）。

現在の実装対象は **契約系書類格納キッカー**（`cmd/contract-kicker`）。
ITANDI に Webhook が無く「契約」ステータスへ切り替わった瞬間を検知できないため、
GCP 上で定期実行し、処理済み管理を自前で持つ。

## 構成

```
cmd/contract-kicker/     エントリポイント（Cloud Run Jobs で実行）
internal/contractkicker/ 本体ロジック（Phase A: 検知 / Phase B: 送信）
pkg/itandi/              ITANDI API v2 クライアント
pkg/webhook/             汎用 JSON Webhook クライアント + DRY_RUN 用 Sender
pkg/ledger/              処理台帳（Store インタフェース / Firestore / インメモリ）
pkg/ledger/ledgertest/   Store 実装向け共通テストスイート
pkg/retry/               リトライ間隔（固定スケジュール）
pkg/logging/             Cloud Logging 互換の構造化ログ
pkg/env/                 環境変数の読み込み・検証（エラーをまとめて報告）
pkg/clock/               テスト可能な時刻取得
deploy/index.yaml        Datastore 複合インデックス
.github/workflows/       CI とデプロイ（Cloud Run Job の作成・更新）
```

`pkg/` 以下は他の自動化からもそのまま使い回せるように、
アプリ固有の判断（ステータス 120 の扱い、送信優先順位など）を含めていない。

## 処理フロー

1 実行 = Phase A → Phase B。両者を分けているのは、1 実行 1 件の流量制御下で
処理待ち申込が ITANDI の取得窓（`datetime`）から外れて取りこぼされるのを防ぐため。

```
Phase A（検知）
  GET /entry_heads?datetime={now-LOOKBACK_HOURS}&entry_status_ids[]=120
    → 台帳に無い申込を status=pending で登録（この時点では送らない）
    → 取得件数が 500（API 上限）に達したら WARNING

Phase B（送信・1 実行 SEND_LIMIT_PER_RUN 件）
  1. pending を detectedAt 昇順で選ぶ
  2. 足りなければ failed かつ nextRetryAt <= now を nextRetryAt 昇順で選ぶ
  3. POST {HEROKU_WEBHOOK_URL} {"id": <申込ID>}
       2xx        → done（skipped も 200 なので done 扱い）
       それ以外   → attempts++ / failed（nextRetryAt = バックオフ）
                    attempts >= MAX_ATTEMPTS → gaveup（ERROR ログ）
```

`DRY_RUN=true` のときは送信せず、**台帳も更新しない**（pending のまま残す）。
ここで done を書いてしまうと DRY_RUN を外した時に「台帳上は処理済みなので
誰も送らない」状態になり、検証したはずの申込がまるごと落ちるため。

`pending` を `failed` より優先するのが肝。逆にすると壊れた 1 件で後続が全部詰まる。

Phase A が失敗しても Phase B は実行する（ITANDI 側の一時障害で、既に台帳へ
積まれた分の送信まで止めないため）。終了コードはどちらかが失敗すれば 1。

## 台帳（Firestore / Datastore mode）

`omni-property-prd` の named database `automations` に載せる。
Axios 分は名前空間とエンティティで分離する。

| 階層 | 値 |
|---|---|
| データベース | `automations`（Datastore mode。他案件の自動化も相乗り可） |
| 名前空間 | `axios` |
| Kind | `contract_kicker_entries` |
| Key | ITANDI 申込 ID（文字列） |

| フィールド | 型 | 説明 |
|---|---|---|
| `status` | string | `pending` / `done` / `failed` / `gaveup` |
| `attempts` | number | 送信試行回数 |
| `detectedAt` | timestamp | Phase A で最初に検知した日時 |
| `sentAt` | timestamp | 最終送信日時 |
| `nextRetryAt` | timestamp | リトライ可能になる日時（`failed` のみ） |
| `lastError` | string | 最終エラー要約 |
| `herokuJobId` | string | Heroku が受付時に返す jobId |
| `updatedAt` | timestamp | 更新日時 |

- 送信対象から外れるのは `done` と `gaveup`
- 台帳の意味は「**Heroku が受け付けた**」であって「Drive へ格納された」ではない
  （Heroku 側が非同期）。実際の格納は Heroku 側のログ／エラー通知で担保する
- ステータス巻き戻りで再度 120 になっても、台帳に居れば再送しない

複合インデックスが必要:

```bash
gcloud datastore indexes create deploy/index.yaml
```

## 環境変数

| 変数 | 既定値 | 説明 |
|---|---|---|
| `ITANDI_API_TOKEN` | （必須） | ITANDI API トークン |
| `ITANDI_BASE_URL` | 本番 API | ITANDI API ベース URL |
| `HEROKU_WEBHOOK_URL` | （必須） | `https://{app}.herokuapp.com/webhook/itandi/contract` |
| `HEROKU_AUTH_HEADER` / `HEROKU_AUTH_TOKEN` | 空 | Heroku 側に共有シークレット検証を足した場合に使う（両方セット） |
| `TARGET_STATUS_ID` | `120` | 検知対象ステータス（契約） |
| `LOOKBACK_HOURS` | `24` | Phase A の取得窓 |
| `MAX_ATTEMPTS` | `3` | 送信リトライ上限 |
| `RETRY_SCHEDULE` | `15m,1h,4h` | バックオフ（カンマ区切り） |
| `SEND_LIMIT_PER_RUN` | `1` | 1 実行あたりの送信件数 |
| `REQUEST_TIMEOUT_MS` | `30000` | HTTP タイムアウト |
| `DRY_RUN` | `false` | true で Heroku へ送らずログのみ |
| `GOOGLE_CLOUD_PROJECT` | 自動検出 | GCP プロジェクト ID |
| `FIRESTORE_DATABASE_ID` | `(default)` | Firestore データベース ID（本番は `automations`） |
| `FIRESTORE_NAMESPACE` | `contract-kicker` | 相乗り時の名前空間分離（本番は `axios`） |
| `FIRESTORE_KIND` | `contract_kicker_entries` | 台帳の Kind |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

設定不備は起動時にまとめて ERROR ログへ出して終了コード 1。

すべて Cloud Run Job の環境変数として渡す（Secret Manager は使わない）。
`ITANDI_API_TOKEN` は GitHub Secrets → Cloud Run のリビジョン設定に入るため、
プロジェクトの Cloud Run 閲覧権限がある人には見える。ローテーションは
GitHub Secrets を更新してタグを打ち直す運用になる。

## 開発

ツールは mise で揃える（`system-prpty-omni` と同じ構成）。

```bash
mise install                    # go / golangci-lint

mise run fmt                    # フォーマット適用（gofumpt + goimports）
mise run lint                   # golangci-lint run
mise run test                   # go test ./...
mise run test-race              # レース検出付き
mise run cover                  # カバレッジ
mise run build-contract-kicker  # bin/contract-kicker
mise run dev-contract-kicker    # DRY_RUN でローカル実行（GCP 認証が必要）
```

lint 設定（`.golangci.yml`）も `system-prpty-omni` と揃えている
（errcheck / govet / staticcheck + gofumpt / goimports）。

台帳の Firestore 実装は Datastore エミュレータがある場合のみ実 API に対して
共通スイートを走らせる（無い場合は自動 skip）:

```bash
gcloud emulators datastore start --project=test-project --consistency=1.0
$(gcloud beta emulators datastore env-init)
go test ./pkg/ledger/... -run Emulator -v
```

## デプロイ

`system-prpty-omni` の `mail-ingestion-job` と同じ作法に揃えている。

- Cloud Run は Terraform 管理外（omni-infrastructure `modules/property/cloud_run.tf`
  の方針）。**Cloud Run Job 本体と環境変数はこのリポジトリの GitHub Actions が作る**
- Cloud Scheduler は Terraform（omni-infrastructure）側
- Secret Manager は使わない。ITANDI トークンと Heroku URL は GitHub Secrets から
  Cloud Run Job の環境変数として渡す

| ワークフロー | トリガ | 内容 |
|---|---|---|
| `ci.yml` → `ci-check.yml` | PR | fmt / lint / test（PR にカバレッジをコメント） |
| `deploy-contract-kicker-prd.yml` | `contract-kicker-v*` タグ / 手動 | check → ビルド → Artifact Registry へ push → `gcloud run jobs deploy` |

実行環境は本番のみなので dev 用ワークフローは無い。リリースはタグで行う:

```bash
git tag contract-kicker-v1.0.0
git push origin contract-kicker-v1.0.0
```

手動実行（`workflow_dispatch`）では `execute` を on にするとデプロイ後にジョブを
1 回走らせる。初回検証はこれを使う（`DRY_RUN=true` の間）。

ジョブは `--max-retries=0`、tasks / parallelism は既定の 1（多重起動の防止）。
個々の申込のリトライは台帳の `failed` / `nextRetryAt` が担当する。

### 設定値の置き場所

| 種類 | 置き場所 |
|---|---|
| プロジェクト・リージョン・SA・イメージ名 | ワークフローの `env:`（`mail-ingestion-job` と同じく直書き） |
| `DRY_RUN` / `FIRESTORE_DATABASE_ID` / `FIRESTORE_NAMESPACE` / `LOG_LEVEL` | ワークフローの `env:` → `--set-env-vars` |
| `ITANDI_API_TOKEN` / `HEROKU_WEBHOOK_URL` | リポジトリシークレット |
| デプロイ用 SA キー | シークレット `GCP_SA_KEY_FOR_GITHUB_ACTIONS_PRD` |

`DRY_RUN` をワークフローに直書きしているのは、本稼働への切り替えが
「PR レビューを通ったコミット + タグ」として残るようにするため。

その他の環境変数（`LOOKBACK_HOURS` / `MAX_ATTEMPTS` / `RETRY_SCHEDULE` など）は
アプリ側の既定値を使う。変える場合はワークフローの `--set-env-vars` に追記する。
`RETRY_SCHEDULE` のようにカンマを含む値を渡すときは
`--set-env-vars="^@^A=1@RETRY_SCHEDULE=15m,1h,4h"` の区切り指定が必要。

### omni-infrastructure 側に必要なもの

アプリが GCP の API として叩くのは **Datastore だけ**。それ以外は動かすための足場。

1. **Firestore (Datastore mode) の named database `automations`** — 新規追加が必要。
   `omni-property-prd` には Firestore が未作成（Firestore API 自体が未有効）なので
   既存モードとの衝突は無い。既定 DB ではなく named database にするのは、
   既定枠を Datastore mode で潰すと property 系が将来 Native mode を使えなくなるため。

   ```hcl
   resource "google_firestore_database" "automations" {
     project     = var.project_id
     name        = "automations"
     location_id = "asia-northeast1"
     type        = "DATASTORE_MODE"

     # 消えると処理済み台帳を失い、全件が再送対象になる
     deletion_policy = "ABANDON"
   }
   ```

   併せて `firestore.googleapis.com` / `datastore.googleapis.com` の有効化も必要
   （`modules/property/apis.tf` に追加）
2. **Artifact Registry** — 既存の `omni-images`（asia-northeast1・DOCKER）を流用。
   確認済みなので**新規作成は不要**
3. **SA `contract-kicker-run-client@`** — ADR-0010 と同じくジョブ実行と
   Scheduler からの呼び出しを 1 つの SA で兼ねる。必要なロールは
   `roles/datastore.user`、`roles/logging.logWriter`、
   ジョブ単位の `roles/run.invoker`（自己参照バインディング）
   （Secret Manager 未使用なので `secretAccessor` は不要）
4. **既存のデプロイ用 SA**（`GCP_SA_KEY_FOR_GITHUB_ACTIONS_PRD` の実体）への権限追加 —
   `contract-kicker` ジョブに対する `roles/run.developer`、
   `contract-kicker-run-client@` に対する `roles/iam.serviceAccountUser`
   （Artifact Registry への writer は既存分で足りる見込み）
5. **Cloud Scheduler** — ADR-0010 の方式（`http_target` で Cloud Run Admin API の
   `jobs.run` を POST + `oauth_token`）、`retry_count = 0`、`*/15 * * * *`、
   呼び出し SA にジョブ単位の `roles/run.invoker`

初回の順序に注意: Scheduler の `google_cloud_run_v2_job_iam_member` はジョブ名を
参照するため、**先に GitHub Actions でジョブを作ってから** Terraform を apply する
（`mail-ingestion-job` と同じ順序）。

### 一度だけ手動で行う作業

```bash
# 複合インデックス（status + detectedAt / status + nextRetryAt）
gcloud datastore indexes create deploy/index.yaml \
  --project omni-property-prd \
  --database automations
```

## 未対応・注意点

- **ITANDI API は IP 許可制**。VPC / Cloud NAT は使わない構成なので Cloud Run の
  送信元 IP は固定できない。ITANDI 側で IP 制限が有効だと一覧取得が 401/403 で
  弾かれるため、事前に確認が必要（弾かれる場合は Heroku 側に一覧取得
  エンドポイントを足して経由させる案へ切り替える）
- **`/entry_heads` は 1 回で最大 500 件**。ページングパラメータは無いため、
  窓が広くなる初回バックフィルや長期障害復旧時は `LOOKBACK_HOURS` を狭めて
  手動で複数回実行する（自動の期間分割は未実装。上限到達時は WARNING が出る）
- **インフラの Terraform は別リポジトリ**（`omni-infrastructure`）。
  Datastore・Artifact Registry・SA/IAM・Scheduler はそちらに追加が必要
  （Cloud Run Job 本体はこのリポジトリの GitHub Actions が作る）
- 初回リリース時は `DRY_RUN=true` + Scheduler 停止でデプロイし、
  検知件数と中身を確認してから本稼働へ切り替える
