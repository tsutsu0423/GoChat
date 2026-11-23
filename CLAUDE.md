# GoChat 開発メモ（Claude Code 向け）

このファイルは、別の PC の Claude Code に作業を引き継ぐためのメモです。返答は日本語で行います。

## プロジェクト概要

- Go 製のリアルタイムチャット。就職作品（ポートフォリオ）として改善中です
- 技術：Gin、gorilla/websocket、GORM、PostgreSQL、bcrypt と Cookie セッション（gin-contrib/sessions）
- 主なファイル
  - `chat.go`：main 関数とルーティング。ポートは 4001
  - `hub.go`：部屋ごとの Hub。clients マップは Hub の goroutine だけが触り、他はチャネルで依頼する設計
  - `auth.go`：登録、ログイン、ログアウト、認証ミドルウェア
  - `models.go`：DB モデル。init() で DB に接続して AutoMigrate する
  - `templates/`、`static/css/`：画面
- 環境変数（`.env`。Git 管理外）：`DSN`、`session_secret`、`POSTGRES_USER`、`POSTGRES_PASSWORD`、`POSTGRES_DB`

## 決まった方向性

- **ターゲット**：ハッカソンや、仕事ではない友人とのチーム開発（短期・少人数のグループ開発）
- **コンセプト**：期間中は GoChat だけで完結する「期間限定の作業部屋」。Discord や Slack との差別化は、ハッカソン向けの機能を設定なしで一つの部屋にまとめていること
- **核の機能（4つ）**
  1. 状況ステータス（作業中、詰まり中、休憩、仮眠中と、いまやっていることの一行）
  2. 詰まり（ブロッカー）ボード（未解決一覧に残り、解決したら閉じる）
  3. 締切カウントダウンとフェーズ表示（企画、実装、デモ準備）
  4. 戻ったら要約（不在中の会話を Amazon Bedrock で要約）
- **GitHub 連携は採用**：Webhook（`/webhooks/github` など）で push、PR、CI を部屋に流して一覧表示する
  - `X-Hub-Signature-256` で署名を検証する
  - GitHub は10秒ほどしか待たないので、すぐ 200 を返す
  - DB 保存は Hub の外で行う
  - GitHub は失敗した通知を自動で再送しないので、常時稼働の環境が前提
- **余力があれば**：運営・メンター用の全チーム一覧（部屋一覧のリアルタイム化を発展させる）、終了後の振り返りレポート、招待リンクでの即参加
- **技術面のアピール**：並行処理の設計（Hub per room、mutex ではなくチャネル、ping/pong、遅いクライアントの切断、レート制限）を説明できること

## デプロイ方針（確定）

1. **常時稼働は友人のオンプレサーバー**で行う。Docker Compose で動かし、公開は Cloudflare Tunnel（ポートを開けない、HTTPS が自動で付く、WebSocket と Webhook も通る）にする予定
   - 決めておくこと：専用ユーザーまたは VM で友人の環境と切り離す、`pg_dump` で毎日バックアップしてサーバーの外にも保存する、停止時の取り決めを友人とする
2. **AWS は後から経験用に検証する**：無料プランで Fargate、ALB、RDS を Terraform で構築し、apply と destroy を確認する
   - 無料プランは登録から6か月で終わるので、アカウント登録はアプリが固まってからにする
   - AWS Budgets を最初に設定する。NAT Gateway は使わない
   - コストの目安：常時稼働なら月約1万円、必要なときだけ動かせば月1,000円前後

本人はコストをとても気にしています。インフラを提案するときは、月額の目安を必ず示してください。

## 進捗（2026-11-24 時点）

- Docker 化が完了し、ローカルで動作確認済み（`Dockerfile`、`docker-compose.yml`、`.dockerignore`）
  - `docker compose up --build` で起動し、http://localhost:4001/register で登録、ログイン、チャットができた
  - 再起動してもデータが残ること（volume `pgdata`）も確認済み
- 起動前に Docker Desktop を起動しておく必要がある。起動していないと pipe のエラーになる

## 次にやること

1. 友人のサーバーへの載せ替え（Docker Compose と Cloudflare Tunnel。独自ドメインが必要で、年1,000〜2,000円程度）
2. README の更新（ルーム機能、レート制限、Docker での起動方法、アーキ図、設計判断3つ）
3. 小さな穴の修正
   - ログインのエラーが "User not found" と "Invalid password" で分かれていて、ユーザー名の存在が推測できる（`auth.go`）
   - メッセージの長さ上限がない
   - `session_secret` が空でも起動してしまう
   - WebSocket の Origin チェックがない
4. 部屋一覧の WebSocket 化（今は `templates/home.html` で5秒ごとにポーリングしている）
5. 核の機能4つと GitHub 連携の実装
6. 負荷試験、`go test -race` での Hub のテスト、`handleChat` 内の DB 書き込みを Hub から切り離す、履歴のページネーション
7. AWS での Terraform 検証

## 未完了の資料

- 企画書（コンセプトシート）は Claude Docs にある。ターゲット、GitHub 連携、ポジショニングマップの縦軸（「利用期間：常設 ↔ 期間限定」に変える案）、インフラとコストを反映して作り直す必要がある
- 企画書のリンクはこのリポジトリにはないので、作り直すときは本人にリンクを聞く
- 企画書の提出物：企画書、ポジショニングマップ、コンセプトツリー、アピールポイント、制作スケジュール

## 本人の進め方の好み

- Docker やインフラは初心者です。学びながら自分で書きたいので、説明は初心者向けに1行ずつ、意味と理由を添えて行う
- 自分で書いたファイルは、Claude が読んで間違いを指摘する（例：`restart: 10` を `unless-stopped` に直した）
- コミットは1ファイルずつ、日本語の「〜しました」調のメッセージで行う。コミットや push は本人に確認してから行う
