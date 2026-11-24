# GoChat の本番サーバーから、DB のバックアップをこの PC に取ってくるスクリプト
#
# 置き場所：D:\Backups\gochat\pull-backups.ps1（このファイルをコピーして使う）
# 実行    ：Windows のタスクスケジューラで毎日（PC の電源が切れていた日は、次に起動したとき）
# 取り元  ：サーバーの ~/backups/gochat_*.sql.gz（サーバーの cron が毎日 3:00 に作る）
#
# 接続先のアドレスは公開リポジトリに書かないため、SSH の設定ファイル（~/.ssh/config）の
# 別名「gochat-server」を使う。

# エラーが起きたら、その場で止める
$ErrorActionPreference = 'Stop'

$Server  = 'gochat-server'         # ~/.ssh/config に登録した別名
$Dest    = 'D:\Backups\gochat'     # 保存先
$LogFile = Join-Path $Dest 'pull.log'
$Ssh     = 'C:\Windows\System32\OpenSSH\ssh.exe'
$Scp     = 'C:\Windows\System32\OpenSSH\scp.exe'

function Write-Log($Message) {
    $line = '{0} {1}' -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $Message
    Add-Content -Path $LogFile -Value $line -Encoding UTF8
    Write-Output $line
}

New-Item -ItemType Directory -Force -Path $Dest | Out-Null

# サーバーにあるバックアップの一覧を取る
# PC の起動直後は Tailscale がまだつながっていないことがあるので、最大3回、1分おきに試す
$remoteFiles = $null
for ($try = 1; $try -le 3; $try++) {
    # -n：入力を待たない（タスクスケジューラから動かしたときに止まらないように）
    # BatchMode=yes：パスワードなどを聞かれる状況になったら、待たずに失敗する
    $output = & $Ssh -n -o BatchMode=yes $Server 'ls -1 backups/gochat_*.sql.gz 2>/dev/null; true'
    if ($LASTEXITCODE -eq 0) {
        $remoteFiles = @($output | Where-Object { $_ -match '^backups/gochat_\d{8}_\d{4}\.sql\.gz$' })
        break
    }
    Write-Log "サーバーにつながりませんでした（$try 回目）"
    if ($try -lt 3) { Start-Sleep -Seconds 60 }
}

if ($null -eq $remoteFiles) {
    Write-Log 'エラー：サーバーにつながらないため、終了します'
    exit 1
}

# まだこの PC にないファイルだけをコピーする
$copied = 0
foreach ($remote in $remoteFiles) {
    $name  = Split-Path $remote -Leaf
    $local = Join-Path $Dest $name
    if (Test-Path $local) { continue }

    # 書き込み中は .tmp にしておき、コピーが終わってから名前を変える
    # （途中で失敗したときに、壊れたファイルを「取得済み」と勘違いしないように）
    $tmp = "$local.tmp"
    & $Scp -q -o BatchMode=yes "${Server}:$remote" $tmp
    if ($LASTEXITCODE -ne 0 -or (Get-Item $tmp).Length -eq 0) {
        Remove-Item $tmp -ErrorAction SilentlyContinue
        Write-Log "エラー：$name のコピーに失敗しました"
        exit 1
    }
    Move-Item $tmp $local
    Write-Log "取得しました：$name"
    $copied++
}

Write-Log "完了（新しく取得：$copied 件、サーバー上：$($remoteFiles.Count) 件）"
