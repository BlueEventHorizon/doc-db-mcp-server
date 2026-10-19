# doc-dbをMacへのログイン時に自動起動する方法

`/opt/homebrew/bin/doc-db` を、LaunchAgent（launchd）を使ってログイン時に自動起動します。Homebrewでインストール済みのdoc-db 0.3.3（Apple Silicon Mac）を対象とします。以下は本人のログイン中のターミナルで実行し、`sudo` は使いません。

## 0. doc-db本体と必須設定を確認する

```bash
/opt/homebrew/bin/doc-db --version
```

doc-dbは `~/.doc-db/doc-db.yaml` を必ず読み込みます。ファイルがなければ起動できません。初回だけ、次を実行して同梱サンプルをコピーします。`-n` により既存設定は上書きしません。

```bash
mkdir -p ~/.doc-db
cp -n /opt/homebrew/share/doc-db/doc-db.yaml.example ~/.doc-db/doc-db.yaml
nano ~/.doc-db/doc-db.yaml
```

既存のDBを使っている場合は、既存の `server.db_path`・`embedding.model`・`embedding.dim` を維持してください。モデルや次元数を変更する場合はDB再構築の検討が必要です。

サンプルの主な設定は次のとおりです。

| 項目 | サンプル値 |
| --- | --- |
| ポート | `58080` |
| DB | `~/.doc-db/docdb.sqlite` |
| Embeddingモデル | `text-embedding-3-large` |
| Embedding次元数 | `3072` |
| 通常ログ | `~/.doc-db/doc-db.log` |

サンプル全体を使ってください。`log` 以外のセクションは必須で、未知の設定キーや不正値も起動エラーになります。DBとログの保存先は、自分が書き込める場所を指定します。

サーバーを起動せずに設定を読み込み、保存先とポートを確認できます。APIキーは表示されません。

```bash
/opt/homebrew/bin/doc-db --show-config
```

続いて、待受ポートがすでに使われていないか確認します。ポートを変更している場合は `58080` を変更後の値に置き換えてください。

```bash
lsof -nP -iTCP:58080 -sTCP:LISTEN
```

何も表示されなければ、この確認時点では待受プロセスはありません。既存のdoc-dbが動いている場合は、その起動元の方法で停止してからLaunchAgentへ切り替えます。別アプリが使用中ならdoc-dbの `server.port` とクライアント側の接続URLを変更してください。同じDB・ポートで二重起動しないようにします。

## 1. 設定ファイルを作成する

ターミナルで次を実行します。

```bash
mkdir -p ~/Library/LaunchAgents
umask 077
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
        <key>OPENAI_API_DOCDB_KEY</key>
        <string>ここを実際のAPIキーに置き換える</string>
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

### APIキーの渡し方

上記の `EnvironmentVariables` にある `OPENAI_API_DOCDB_KEY` の値を、実際のOpenAI APIキーに置き換えてください。プレースホルダーのままでは利用できません。

インストール済みのdoc-db 0.3.3のREADMEと付属の設定例では、APIキーは環境変数 `OPENAI_API_DOCDB_KEY` または `OPENAI_API_KEY` で渡す仕様です。実装では `OPENAI_API_DOCDB_KEY` が優先され、未設定なら `OPENAI_API_KEY` が使用されます。

ターミナルや `.zshrc` で `export` しただけでは、このLaunchAgentに同じ値が渡るとは限りません。自動起動用の設定には、上記のように環境変数を明示します。APIキーはdoc-dbのYAML設定ファイルではなく、環境変数で渡します。

この方法ではAPIキーがplistに平文で保存されるため、保存後に読み書きを自分だけに制限します。キーを含むplistは共有したりGitへ登録したりしないでください。

```bash
chmod 600 ~/Library/LaunchAgents/com.local.doc-db.plist
```

XMLの文字として `&` や `<` がキーに含まれる場合は、それぞれ `&amp;`、`&lt;` と記述します。

キーを変更した後は、「4. 設定を変更した場合」の手順で再登録してください。

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
launchctl print gui/$(id -u)/com.local.doc-db | /usr/bin/grep -E '^[[:space:]]*(state|pid|last exit code) ='
```

常駐中なら、出力の `state = running` や `pid` が確認の目安になります。

`launchctl print` の出力全体には、環境変数に設定したAPIキーが含まれる場合があります。上記は状態に関する行だけを表示します。出力全体をチャットや共有ファイルへ貼り付けないでください。

待受ポートも確認します。

```bash
lsof -nP -iTCP:58080 -sTCP:LISTEN
```

表示されたPIDとLaunchAgentのPIDが一致することを確認します。起動直後は準備中のことがあるため、数秒後に再確認します。

通常ログ：

```bash
tail -f ~/.doc-db/doc-db.log
```

doc-db 0.3.3の通常ログは、既定では `~/.doc-db/doc-db.log` に書かれます。doc-dbのYAML設定で `log.path` を変更している場合は、その保存先を確認してください。plistの `StandardOutPath` で指定した `/tmp/doc-db.log` は、標準出力の保存先です。

標準エラー（設定・APIキー未設定などの起動失敗の確認）：

```bash
tail -f /tmp/doc-db.err
```

ログ表示は `Control + C` で終了します。ログファイルはdoc-dbの起動後に作成されます。`/tmp` 内のログは永続保存用ではありません。

doc-dbのログ初期化後のエラーは、通常ログ側にも記録されます。問題がある場合は通常ログと標準エラーの両方を確認してください。

### MCPクライアントから接続する

doc-dbはHTTPで常駐するMCPサーバーです。クライアントには、Streamable HTTPの接続先として次を設定します。

```text
http://localhost:58080/mcp
```

ポートを変更した場合はURLも合わせます。クライアントから別途 `doc-db` コマンドを起動する設定との併用は避けてください。

Claude Codeを使う場合の登録例（未登録の場合だけ実行）：

```bash
claude mcp add --transport http -s user doc-db http://localhost:58080/mcp
```

登録済みなら、既存接続先が正しいか確認してください。他のクライアントは、そのクライアントのHTTP MCP接続設定を使います。

接続後はツール一覧や `list_indexes` を確認します。これは接続確認であり、OpenAI APIキーの有効性の確認にはなりません。起動時はキーが空でないことを確認するだけなので、APIキーの有効性は既存インデックスに対して `mode=emb` の検索などを実行し、成功することを別途確認します。Embeddingによる検索にはOpenAI APIへのネットワーク接続が必要です。

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
- `/opt/homebrew/bin/doc-db --show-config` で必須設定を読み込めるか確認します。
- `/tmp/doc-db.err` と、設定された通常ログの両方を確認します。
- `address already in use` はポートの重複、APIキー未設定エラーはplistの環境変数設定を確認します。
- Homebrew版doc-dbは単体バイナリなので、この手順ではmiseの初期化やPython・Nodeの設定は不要です。
- DB・ログには絶対パスかdoc-dbが展開する `~/` を使います。plistのパス指定では `~` や `$HOME` のシェル展開を前提にせず、絶対パスを使います。
- すでに登録済みの場合、`bootstrap` が失敗することがあります。「4. 設定を変更した場合」の手順で再登録します。
- 過去に `launchctl disable` で無効化した場合は、`launchctl enable gui/$(id -u)/com.local.doc-db` で有効にしてから登録します。
- `KeepAlive` があるため、起動に失敗すると再試行が続きます。修正作業中は「5. 停止する・自動起動をやめる」の `bootout` で停止してください。
- `--help` はこのバージョンのソースではヘルプ表示用に処理されず、通常起動に進みます。確認用途には `--version` または `--show-config` を使ってください。

## 最後の確認

- plistの形式確認が `OK` になっている。
- LaunchAgentが `running` になり、待受ポートのPIDが一致している。
- MCPクライアントから接続し、必要ならEmbedding検索も成功している。
- 最後にMacからログアウト・再ログインし、同じ状態確認を行う。再ログイン後も起動すれば自動起動を確認できる。

## 起動するタイミングについて

この手順は、ユーザーがMacにログインした後に起動する設定です。ログイン前のOS起動直後から実行するには、LaunchDaemonとして別途設定する必要があります。

## 確認範囲（2026年10月19日）

インストール済みdoc-db 0.3.3のREADME・設定例・ローカルのソースコードと照合しました。実機で `--version` と `--show-config` が成功し、既存のYAML設定を読み込めること、ポートが58080であることを確認しています。文書内のplist例も抽出して `plutil -lint` が `OK` になることを確認しました。

この作業では実際のLaunchAgentの登録・変更、APIキーを使った検索、ログインし直しての自動起動確認は行っていません。最後の確認手順で実動作を確認してください。
