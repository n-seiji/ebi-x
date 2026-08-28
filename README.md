# ebiii

ebiii は、Slack の mention を受けて Codex が方針を検討し、必要な作業を別セッションで実行して同じスレッドへ結果を返す、ミニマムな連携ボットです。

## Slack App の準備

1. Slack App を作成し、Socket Mode を有効にします。
2. Bot Token Scopes に `app_mentions:read`、`chat:write`、`reactions:write`、`reactions:read`、`channels:history` を追加します。scopeを追加・変更した場合は、workspaceへアプリを再インストールしてください。
3. Event Subscriptions で `app_mention` と `message.channels` を購読します。`message.groups` は追加しません（プライベートチャンネルはサポートしません）。
4. Workspace に App をインストールして Bot Token (`xoxb-...`) を取得します。
5. Socket Mode 用の App Token (`xapp-...`) を取得します。

## 設定と起動

`.env.example` を `.env` にコピーし、Slack token、許可する user/channel ID などを設定します。

Workflow Builder の「メッセージを送信」からの mention も受け付ける場合は、`SLACK_ALLOW_WORKFLOWS=true` にします。通常のBot投稿は拒否し、Slackイベントに `Wf` で始まる `workflow_id` が含まれるmentionだけを許可します。人・Workflowのどちらも `SLACK_ALLOWED_CHANNEL_IDS` の制限対象です。

許可されていないuser、channel、Botからmentionされた場合は、`SLACK_ADMIN_USER_ID` のユーザーへ確認するよう同じスレッドに返信します。未設定時は `@seiji` というテキストを使用します。

承認済みmentionのスレッドで通常の返信も処理するには、スレッド親メッセージに `SLACK_THREAD_SUBSCRIPTION_REACTION` で指定したリアクションを付けます。既定値は `thread-subete`、有効期間の既定値は `SLACK_THREAD_SUBSCRIPTION_TTL=336h` です。リアクション設定を明示的に空にすると、この機能を無効にできます。有効な場合、TTLは正のdurationにしてください。

```sh
cp .env.example .env
mise run build   # または: go build -o ebiii ./cmd/ebiii
./ebiii
```

Go のバージョンは [mise](https://mise.jdx.dev/) で管理しています(`mise install` で揃います)。テストは `mise run test`、lint は `mise run lint` で実行できます。mise なしでも `go build` / `go test -race ./...` / `go vet ./...` で同等です。メモリの読み取り分離には permission profile と `--ignore-user-config` を使うため、Codex CLI 0.149.0 以上が必要です。

ebiii は単一プロセスでの運用を前提としており、多重起動には対応していません。

## メモリ

ebiii は作業で得た長期的に有用な情報を、次の2スコープに分けて保存します。

```text
data/memory/MEMORY.md                     # 全ユーザー・全チャンネル共通
data/memory/channels/{Slack channel ID}/MEMORY.md
```

- 全体メモリ: 他のユーザーやチャンネルでも再利用できる技術的・運用上の知識
- チャンネルメモリ: そのチャンネルの参加者で共有してよい用語・目的・運用ルール

Codex はメモリファイルを直接読み書きしません。bot だけが全体メモリと現在のSlack channel IDに対応する内容を読み、プロンプトへ注入します。Codex の全 turn ではユーザー設定を読み込まない専用 permission profile を使い、`data/memory` の絶対パスを read/write ともに deny します。`EBIII_WRITABLE_ROOTS` がmemory本体・親・子と重なる場合も、シンボリックリンクを解決したうえで起動を拒否します。

Codex は最終応答で追記を提案し、bot が保存先を決定します。認証情報、秘密、一時的な依頼内容、推測したセンシティブ属性は保存対象外です。既存の `data/memory/MEMORY.md` はそのまま全体メモリとして使用されます。既存の `data/memory/users/{Slack user ID}/MEMORY.md` はbotが使用せず、削除もしません。分離導入前のCodexセッションは再利用せず、導入後の最初のmentionから新しいセッションになります。

## interrupted の運用

work turn は非冪等な副作用を持つ可能性があります。そのため、work 中のクラッシュや失敗は `interrupted` として記録し、自動再実行しません。ユーザーが作業状況を確認したうえで、新しい mention として依頼し直してください。
