#!/usr/bin/env bash
# GoChat の DB を pg_dump でバックアップするスクリプト（本番サーバーの cron で毎日実行する）
#
# 置き場所：サーバーの ~/bin/gochat-backup.sh（このファイルをコピーして使う）
# 出力先  ：~/backups/gochat_YYYYMMDD_HHMM.sql.gz
# 取り出し：デスクトップPCの scripts/pull-backups.ps1 が D ドライブにコピーする

# -e: コマンドが失敗したらそこで止める
# -u: 未定義の変数を使ったらエラーにする（打ち間違いで空の変数を使う事故を防ぐ）
# -o pipefail: パイプ（|）の途中のコマンドが失敗しても、失敗として扱う
#              （これがないと pg_dump が失敗しても gzip が成功すれば「成功」になってしまう）
set -euo pipefail

# このスクリプトが作るファイルを、自分だけが読めるようにする（DB の中身は個人情報なので）
umask 077

COMPOSE_DIR="/opt/apps/gochat" # docker-compose.yml がある場所
BACKUP_DIR="$HOME/backups"     # バックアップの保存先
KEEP_DAYS=14                   # サーバーに残す日数（PC を数日つけなくても取りこぼさないように）

STAMP="$(date +%Y%m%d_%H%M)"
OUT="$BACKUP_DIR/gochat_${STAMP}.sql.gz"
TMP="${OUT}.tmp" # 書き込み中は .tmp にしておき、完成したら名前を変える

# 途中で失敗したら、書きかけの .tmp を消してから終わる
trap 'rm -f "$TMP"' EXIT

log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') $*"
}

mkdir -p "$BACKUP_DIR"
cd "$COMPOSE_DIR"

log "バックアップ開始"

# db コンテナの中で pg_dump を実行し、出力を gzip で圧縮してファイルに保存する
# -T: 端末を割り当てない（cron から実行するときに必要）
# ユーザー名と DB 名は、コンテナに渡してある環境変数をそのまま使う（パスワードをここに書かない）
# --no-owner: 復元先のユーザー名が違っても復元できるようにする
docker compose exec -T db sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner' | gzip > "$TMP"

# 圧縮ファイルが壊れていないか確認する
gzip -t "$TMP"

# 中身が空っぽでないか確認する（テーブル定義が入っていれば CREATE TABLE があるはず）
if ! gzip -dc "$TMP" | grep -q "CREATE TABLE"; then
  log "エラー：ダンプの中身が空です"
  exit 1
fi

# 完成したので本来の名前に変える（PC がコピーするのは .sql.gz だけなので、書きかけは取りに行かない）
mv "$TMP" "$OUT"
log "保存しました：$OUT（$(du -h "$OUT" | cut -f1)）"

# KEEP_DAYS より古いバックアップを消す（手動で取った .sql は対象外）
find "$BACKUP_DIR" -maxdepth 1 -name 'gochat_*.sql.gz' -mtime +"$KEEP_DAYS" -print -delete |
  while read -r f; do log "古いバックアップを削除：$f"; done

log "バックアップ完了"
