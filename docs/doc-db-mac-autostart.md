# doc-dbをMacへのログイン時に自動起動する方法

`/opt/homebrew/bin/doc-db` を、LaunchAgent（launchd）を使ってログイン時に自動起動します。この手順は、doc-dbが常駐するコマンドであることを前提としています。

## 1. 設定ファイルを作成する

ターミナルで次を実行します。

```bash
mkdir -p ~/Library/LaunchAgents
nano ~/Library/LaunchAgents/com.local.doc-db.plist
```

次の内容を貼り付け、保存します。nanoでは `Control + O` → Enterで保存し、`Control + X` で終了できます。

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.local.doc-db</string>

    <key>ProgramArguments</key>
    <array>
        <string>/opt/homebrew/bin/doc-db</string>
    </array>

    <key>RunAtLoad</key>
    <true/>

    <key>KeepAlive</key>
    <true/>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>

    <key>StandardOutPath</key>
    <string>/tmp/doc-db.log</string>

    <key>StandardErrorPath</key>
    <string>/tmp/doc-db.err</string>
</dict>
</plist>
```

- `RunAtLoad`：登録時と、その後のログイン時に起動します。
- `KeepAlive`：終了した場合に再起動します。単発処理のコマンドなら、このキーと直後の `<true/>` を削除してください。
- `PATH`：launchdは通常のシェル設定（`.zshrc`など）を読まないため、実行ファイルの検索先を明示します。

## 2. 設定を確認して登録する

設定ファイルの形式を確認します。

```bash
plutil -lint ~/Library/LaunchAgents/com.local.doc-db.plist
```

`OK` と表示されたら、登録します。この操作で起動し、次回以降もログイン時に起動します。

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.local.doc-db.plist
```

## 3. 起動状態とログを確認する

```bash
launchctl print gui/$(id -u)/com.local.doc-db
```

常駐中なら、出力の `state = running` や `pid` が確認の目安になります。

通常ログ：

```bash
tail -f /tmp/doc-db.log
```

エラーログ：

```bash
tail -f /tmp/doc-db.err
```

ログ表示は `Control + C` で終了します。ログファイルはdoc-dbの起動後に作成されます。`/tmp` 内のログは永続保存用ではありません。

## 4. 設定を変更した場合

登録済みの設定を解除してから、再登録します。

```bash
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.local.doc-db.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.local.doc-db.plist
```

## 5. 停止する・自動起動をやめる

停止して現在の登録を解除します。

```bash
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.local.doc-db.plist
```

次回ログイン時の自動起動もやめるには、設定ファイルをLaunchAgentsの外へ移動します。

```bash
mkdir -p ~/Library/LaunchAgents-disabled
mv ~/Library/LaunchAgents/com.local.doc-db.plist ~/Library/LaunchAgents-disabled/
```

## 起動しない場合の確認点

- `/opt/homebrew/bin/doc-db` をターミナルから直接実行できるか確認します。
- まず `/tmp/doc-db.err` の内容を確認します。
- mise管理のPythonやNodeなどに依存する場合、上記のPATHだけでは不足することがあります。必要な実行ファイルや環境変数は、doc-dbの構成に合わせて設定してください。
- 相対パスに依存する場合は、plistに `WorkingDirectory` を追加して作業フォルダを絶対パスで指定します。
- すでに登録済みの場合、`bootstrap` が失敗することがあります。「4. 設定を変更した場合」の手順で再登録します。

## 起動するタイミングについて

この手順は、ユーザーがMacにログインした後に起動する設定です。ログイン前のOS起動直後から実行するには、LaunchDaemonとして別途設定する必要があります。

この文書は以前のチャットで案内した手順をまとめたものです。Mac上の実際の設定や、doc-dbの動作状態は確認していません。
