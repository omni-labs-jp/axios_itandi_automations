# contract-kicker 導入ログ

本番導入の経緯と現在地。時刻は JST。

## 現在地（2026-08-24 時点）

| # | 作業 | 状態 |
|---|---|---|
| 1 | アプリ実装・テスト | ✅ 完了（PR #1） |
| 2 | omni-infrastructure でインフラ構築 | ✅ 完了（PR #47 / タグ v1.9.0） |
| 3 | Cloud Run Job の作成 | ✅ 完了（`DRY_RUN=true`） |
| 4 | Datastore 複合インデックス投入 | ✅ 完了（2件 READY） |
| 5 | DRY_RUN での動作検証 | ✅ 完了（全経路の疎通確認済み） |
| 6 | DRY_RUN の副作用修正 | ✅ 完了（PR #3） |
| 7 | Heroku 側 ITANDI Webhook の二重起動確認 | ✅ 確認済み（冪等なので問題なし） |
| 8 | 本稼働（`DRY_RUN=false` + タグ） | 🔄 `DRY_RUN=false` を PR 中。タグ待ち |
| 9 | 実送信の手動確認（1件） | ⬜ 未対応 |
| 10 | Cloud Scheduler の再開 | ⬜ 未対応（現在 PAUSED） |

## 構築済みリソース（omni-property-prd）

| リソース | 値 |
|---|---|
| Cloud Run Job | `contract-kicker`（asia-northeast1 / `DRY_RUN=true`） |
| イメージ | `asia-northeast1-docker.pkg.dev/omni-property-prd/omni-images/contract-kicker` |
| 実行 SA | `contract-kicker-run-client@omni-property-prd.iam.gserviceaccount.com` |
| Datastore DB | `automations`（DATASTORE_MODE / asia-northeast1） |
| 名前空間 / Kind | `axios` / `contract_kicker_entries` |
| 複合インデックス | 2件（`status`+`detectedAt` / `status`+`nextRetryAt`）READY |
| Cloud Scheduler | `contract-kicker-schedule`（`*/15 * * * *` / **PAUSED**） |

## 事前調査の実測値（2026-08-24）

ITANDI API を直接叩いて確認した。

| 項目 | 値 |
|---|---|
| 契約(120) の総数 | **1,091 件**（最古 2022-10-06） |
| 直近 12 ヶ月 | 222 件 |
| 月別 | 13〜34 件（月 15〜30 件が平常） |
| 直近 30 日 | 11 件 |
| 直近 24 時間 | 0 件 |

判明した仕様:

- **IP 許可設定は Axios のテナントでは未使用**。送信元 IP の固定は不要で、
  VPC / Cloud NAT も要らない（Cloud Run から直接叩けることを実証済み）
- **`/entry_heads` の 500 件上限では `updated_at` が新しい側が落ちる**。
  期間指定なしで引くと 1,091 件のうち古い順 500 件（2022-10〜2024-05）が返った。
  上限に当たるといちばん拾いたい直近分からこぼれる
- 過去分（既に契約になっている約 1,100 件）は取得窓が 24 時間なので**自動的に対象外**。
  seed で処理済みにする作業は不要だった

## 経緯

### インフラ構築（omni-infrastructure）

タグ `v1.9.0` の apply が **1 回失敗**した。

```
Error: Error applying IAM policy for cloudrunv2 job ".../jobs/contract-kicker":
  Error 404: Resource 'contract-kicker' of kind 'JOB' ... does not exist.
```

`google_cloud_run_v2_job_iam_member` がまだ存在しない Cloud Run Job を参照していたため。
Cloud Run は Terraform 管理外（各サービスの CI/CD がデプロイする方針）なので、
**ジョブ作成 → Terraform apply の順序**が必要。`mail-ingestion-job` と同じ制約。

SA / DB / API / Scheduler は失敗前に作成されていたため、ジョブ作成後の再 apply で解消した。

### デプロイ（このリポジトリ）

イメージのビルドが **1 回失敗**した。

```
go: go.mod requires go >= 1.25.0 (running go 1.24.13; GOTOOLCHAIN=local)
```

`cloud.google.com/go/datastore` の追加で `go mod tidy` が go.mod を 1.25.0 に上げたのに
Dockerfile が `golang:1.24` のままだった。CI（Check）は `go-version-file: go.mod` で
toolchain を解決するため気付けなかった。PR #2 で修正。

### DRY_RUN 検証の結果

`LOOKBACK_HOURS=720`（30 日）の実行時オーバーライドで 1 回実行し、全経路を確認した。

```
detected=11 newlyQueued=11 selected=1 sent=0 dryRun=1
```

| 経路 | 結果 |
|---|---|
| Cloud Run → ITANDI API 一覧取得 | ✅ 疎通（IP 制限の懸念クリア） |
| Datastore 読み取り（複合インデックス） | ✅ 動作 |
| Datastore 書き込み（`pending` 登録） | ✅ 11 件書き込み確認 |
| 送信対象の選択ロジック | ✅ `detectedAt` 昇順 + ID タイブレークで `5619245` |
| DRY_RUN で送信しない | ✅ |
| Heroku への実送信 | ⏸ 未検証（DRY_RUN のため） |

### 台帳に残っている 11 件

上記の検証実行により、直近 30 日に契約になった 11 件が `pending` として台帳に入っている。

```
5619245 5655946 5657694 5660472 5676350 5688999
5701281 5708877 5718765 5757021 5760036
```

**そのまま流す方針**で合意済み（本来格納すべき申込であり、Heroku 側がステータスを
再チェックするので巻き戻り分は `skipped` で安全に落ちる）。本稼働後、15 分ごとに
1 件ずつ、**約 2 時間 45 分**で消化される。

なおこの検証をきっかけに、DRY_RUN が台帳へ書き込むのは副作用として不適切と判断し、
PR #3 で「DRY_RUN は台帳を一切変更しない」に変更した。

## 二重起動の検討結果（2026-08-24）

ITANDI 側の Webhook 設定（「申込データ更新時」→ `/webhook/itandi/contract`）が
有効なままだとキッカーと二重で飛ぶが、**Heroku 側の Drive アップロードが冪等**なので
問題にならない。001 のドキュメントに明記がある。

> 同一ステータスで複数回Webhookが飛んでも、同名ファイルは `uploadFile()` で
> スキップされるため多重格納は起きない

> | 4 | 重複防止 | 同じ申込で2回叩く | 2回目は「File already exists on Drive, skipping upload」でスキップ |

したがって ITANDI 側の Webhook 登録を外す作業は本稼働の前提条件にしない。
（ITANDI に契約検知専用の Webhook 枠は無く、実際に登録されているかは管理画面でしか
確認できないが、どちらでも安全）

## 残作業

1. **タグ `contract-kicker-v1.0.0` を打つ** — `DRY_RUN=false` のイメージがデプロイされる。
   この時点では Scheduler が PAUSED なので自動では動かない
2. **実送信を手動で 1 回確認** — `gcloud run jobs execute`。1 件だけ Heroku に飛ぶので、
   Heroku 側のログと Drive の格納先（`押印済み` / `契約書類`）を目視確認する。
   問題があればここで止められる
3. **Scheduler 再開** — omni-infrastructure の `contract_kicker_schedule_paused = false`
   にして apply。**この時点から 15 分ごとに動き出し、台帳の 11 件が約 2 時間 45 分で
   Heroku へ流れる**
4. **数日間の監視** — `gaveup` が出ていないかを日次で確認

## 運用コマンド

### ログを見る

```bash
# 直近の実行サマリ
gcloud logging read \
  'resource.type="cloud_run_job" AND resource.labels.job_name="contract-kicker"' \
  --project omni-property-prd --limit 30 --freshness=1d \
  --format="value(timestamp,jsonPayload.severity,jsonPayload.message,jsonPayload.entryHeadId)"

# 異常のみ
gcloud logging read \
  'resource.type="cloud_run_job" AND resource.labels.job_name="contract-kicker"
   AND severity>=WARNING' \
  --project omni-property-prd --limit 50 --freshness=7d
```

`gaveup`（リトライ上限到達）は severity ERROR で出る。ここを見れば取りこぼしが分かる。

### 台帳の中身を見る

Datastore mode の named database なので `x-goog-request-params` ヘッダが必要。
これが無いと `(default)` DB を見に行って「存在しない」と言われる。

```bash
TOKEN=$(gcloud auth print-access-token)
curl -sS -X POST "https://datastore.googleapis.com/v1/projects/omni-property-prd:runQuery" \
  -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
  -H 'x-goog-request-params: project_id=omni-property-prd&database_id=automations' \
  -d '{"databaseId":"automations",
       "partitionId":{"projectId":"omni-property-prd","databaseId":"automations","namespaceId":"axios"},
       "query":{"kind":[{"name":"contract_kicker_entries"}],
                "order":[{"property":{"name":"detectedAt"},"direction":"ASCENDING"}],
                "limit":50}}' | jq '.batch.entityResults[] | {id: .entity.key.path[0].name, status: .entity.properties.status.stringValue, attempts: .entity.properties.attempts.integerValue}'
```

`status` で絞りたい場合は `query.filter` に `propertyFilter` を追加する。

### 手動で 1 回実行する

```bash
# 通常
gcloud run jobs execute contract-kicker --project omni-property-prd \
  --region asia-northeast1 --wait

# 取得窓だけ変えて 1 回試す（ジョブ設定は変わらない）
gcloud run jobs execute contract-kicker --project omni-property-prd \
  --region asia-northeast1 --update-env-vars LOOKBACK_HOURS=720 --wait
```

### インデックスの状態

```bash
gcloud firestore indexes composite list --project omni-property-prd --database automations
```

### ITANDI を直接叩いて件数を確認する

```bash
export ITANDI_BASE='https://moushikomi-uketsukekun.com/maintenance_company/api/v2'
curl -sS -G "${ITANDI_BASE}/entry_heads" \
  -H "Authorization: Token ${ITANDI_API_TOKEN}" -H 'Accept: application/json' \
  --data-urlencode "datetime=$(date -u -v-24H '+%Y-%m-%dT%H:%M:%S')" \
  --data-urlencode 'entry_status_ids[]=120' | jq 'length'
```

`length` が 500 なら上限に当たっている（新しい側が落ちているので窓を狭める）。

### 障害復旧時の注意

長期間ジョブが止まった後は取得窓が広がり、500 件上限に当たる可能性がある。
その場合は `LOOKBACK_HOURS` を狭めて手動で複数回実行し、期間を分けて埋める
（自動の期間分割は未実装）。上限到達時は WARNING が出る。
