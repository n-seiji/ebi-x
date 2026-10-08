# ebi-x

ebi-x は、Slack の mention を受けて Codex が方針を検討し、必要な作業を別セッションで実行して同じスレッドへ結果を返す、ミニマムな連携ボットです。

## Slack App の準備

1. Slack App を作成し、Socket Mode を有効にします。
2. Bot Token Scopes に `app_mentions:read`、`chat:write`、`reactions:write`、`reactions:read`、`channels:history` を追加します。scopeを追加・変更した場合は、workspaceへアプリを再インストールしてください。
3. Event Subscriptions で `app_mention` と `message.channels` を購読します。`message.groups` は追加しません（プライベートチャンネルはサポートしません）。
4. Workspace に App をインストールして Bot Token (`xoxb-...`) を取得します。
5. Socket Mode 用の App Token (`xapp-...`) を取得します。

## 設定と起動

`.env.example` を `.env` にコピーし、Slack token、許可する user/channel ID などを設定します。

チャンネルは `SLACK_ALLOWED_CHANNEL_IDS` で個別に許可するか、`SLACK_ALLOW_ALL_PUBLIC_CHANNELS=true` ですべてのパブリックチャンネルを許可できます。この場合もDM・グループDM・プライベートチャンネルは拒否します。mentionイベントにはチャンネル種別が含まれないため、`conversations.info` でパブリックかどうかを確認します（Bot Token Scopes に `channels:read` が必要です）。確認に失敗したチャンネルは拒否します。`SLACK_ALLOWED_CHANNEL_IDS` との併用はできません。どちらも設定しない場合は起動しません（以前の「空なら全チャンネル許可」は廃止しました）。

Workflow Builder の「メッセージを送信」からの mention も受け付ける場合は、`SLACK_ALLOW_WORKFLOWS=true` にし、許可するWorkflowのIDを `SLACK_ALLOWED_WORKFLOW_IDS` に列挙します（必須）。通常のBot投稿は拒否し、Slackイベントの `workflow_id` が一覧に含まれるmentionだけを許可します。Workflowはワークスペースのメンバーなら誰でも作れるため、IDで限定しないとユーザー許可リストを迂回できてしまいます。人・Workflowのどちらもチャンネル制限の対象です。

許可されていないuser、channel、Botからmentionされた場合は、`SLACK_ADMIN_USER_ID` のユーザーへ確認するよう同じスレッドに返信します。未設定時は `@seiji` というテキストを使用します。

承認済みmentionのスレッドで、許可ユーザー（`SLACK_ALLOWED_USER_IDS`）の通常の返信も処理するには、スレッド親メッセージに `SLACK_THREAD_SUBSCRIPTION_REACTION` で指定したリアクションを付けます。既定値は `thread-subete`、有効期間の既定値は `SLACK_THREAD_SUBSCRIPTION_TTL=336h` です。リアクション設定を明示的に空にすると、この機能を無効にできます。有効な場合、TTLは正のdurationにしてください。許可ユーザー以外の返信は、購読中のスレッドでも無視します。

```sh
cp .env.example .env
mise run build   # または: go build -o ebi-x ./cmd/ebi-x
./ebi-x
```

Go のバージョンは [mise](https://mise.jdx.dev/) で管理しています(`mise install` で揃います)。テストは `mise run test`、lint は `mise run lint` で実行できます。mise なしでも `go build` / `go test -race ./...` / `go vet ./...` で同等です。メモリの読み取り分離には permission profile と `--ignore-user-config` を使うため、Codex CLI 0.149.0 以上が必要です。

ebi-x は単一プロセスでの運用を前提としており、多重起動には対応していません。

## 並列作業とスレッドごとのクローン

作業ターンは、Slackスレッドが異なれば `CODEX_MAX_PARALLEL_WORK`（既定 3）件まで並列に実行します。同じスレッド内の作業は依頼順に1件ずつ実行します。上限に達している間、そのスレッドには順番待ちのステータスを表示します。方針検討ターンは従来どおりスレッドごとに直列で、他スレッドの作業を待ちません。方針検討ターン全体の同時実行数は `CODEX_MAX_PARALLEL_PLAN`（既定 3）件までです。

`CODEX_WORK_MODEL` を設定すると、作業ターンだけそのモデルで実行します（未設定時は `CODEX_MODEL`）。

スレッドが並列に動いてもファイルが衝突しないよう、作業場所をスレッドごとに分けます。

```text
data/workspace/{channel ID}-{thread ts}/                    # スレッドごとのcwd（方針・作業とも）
data/checkouts/{channel ID}-{thread ts}/{repo}-{hash}/        # スレッドごとのクローン
data/checkouts/{channel ID}-{thread ts}/{repo}-{hash}.gitdir/ # そのgitディレクトリ
```

- `EBIX_WRITABLE_ROOTS` のうちGitリポジトリの最上位ディレクトリは、スレッドごとのローカルクローン（ブランチ `ebi-x/{channel ID}-{thread ts}`、作成時点の `HEAD` から分岐）で作業します。元のリポジトリは書き込み不可のままで、ブランチも元のリポジトリには作られません。クローンの `origin` は元のリポジトリの `origin` を指すため、作業用ブランチをpushできます。
- Codexは `.git` という名前のディレクトリを書き込み可能に指定しても読み取り専用にするため、git worktree ではコミットできません。そのため、gitディレクトリを `.gitdir` という別名で分けたクローンを使っています。ローカルクローンはオブジェクトをハードリンクで共有するため軽量ですが、`EBIX_HOME` が元のリポジトリと別のファイルシステムにあるとスレッドごとに全オブジェクトがコピーされます。
- Gitリポジトリでない（またはリポジトリのサブディレクトリを指す）writable rootと `data/playbooks` は、全スレッドで共有のままです。同時に同じファイルを書き換えると後勝ちになります。
- 最後の作業から `EBIX_CHECKOUT_IDLE_TTL`（既定 `120h` = 5日）経ったスレッドは作業が終わったものとみなし、起動時と1時間ごとにクローンを作業用ブランチごと削除します。未コミットの変更やpushしていないコミットも削除されるため、残したい成果はそれまでにpushしてください。実行中の作業ターンが使っているクローンは、その作業が終わるまで削除しません。
- スレッドごとのcwd（`data/workspace/...`）は自動削除しません。各ターンは他スレッドのcwdとクローンを読み書き禁止にするため、その一覧をCodexへの引数1つで渡します。スレッド数が数千に達して引数が上限（約120KB）を超えるとターンを開始できなくなるので、不要になった古いディレクトリは削除してください。

この変更より前のCodexセッションは共有workspaceをcwdにしているため再利用せず、導入後の最初のmentionから新しいセッションになります。

## Slackへの投稿

Codexの応答はMarkdownのまま、Block Kit の markdown ブロックとして投稿します。Slackが見出し・太字・箇条書き・表・リンクをそのまま描画するため、bot側でmrkdwnへの変換は行いません。追加のscopeは不要です。

- 通知プレビューとブロックを描画できないクライアントのために、Markdown記法を除いたテキストを `text` にも添えます。ブロックが描画される場合、このテキストは本文としては表示されません。
- Slackがブロックを拒否した場合（`invalid_blocks` / `msg_too_long`）は、同じ内容をテキストのみで1回だけ投稿し直します。書式の問題で回答そのものを失わないためです。
- 1メッセージは8,000文字で分割します（markdownブロックの上限は12,000文字）。分割は行単位で行い、コードブロックの途中で切れる場合は閉じてから次のメッセージで開き直します。
- 読みやすさのため、Slackに投稿される本文の書式規約（結論を先頭に3行、全体1800字以内、表は3列以内、リンクは末尾にまとめる）をプロンプトで指定しています。

## Slackからplaybookを作成・更新する

`examples/playbooks/playbook-from-thread.md` を `data/playbooks/` にコピーすると、Slackで `@ebi この対応をplaybookにして` や `@ebi このplaybookを修正して。質問は1問ずつにして` と依頼できます。作成用playbookでは、既知情報を聞き直さず、必要な質問を1問ずつ行い、新規・更新とも完成案を提示して「この内容でいいですか？」と確認し、依頼者の承認後に保存・反映するよう定めます。修正が入れば最新版を再提示して承認を待ちます。「案だけ」の場合はworkspaceに下書きを保存します。

改善要望のヒアリング用には `examples/playbooks/request-discovery.md` も配置できます。既存ファイルがある場合は差分を確認して反映してください。

playbookは全チャンネルの依頼で参照されるため、書き込めるのは `EBIX_SHARED_WRITE_CHANNEL_IDS` に列挙したチャンネルの作業ターンだけです（未設定ならどのチャンネルからも書き込めません）。一覧は依頼のたびに読み直すため、保存したplaybookは次の依頼から再起動なしで使われます。方針検討ターンは引き続き読み取り専用です。playbook変更にも既存のSlack利用権限が適用されます。

playbookは直下のMarkdownファイルに `name` と `description` のfrontmatterを付け、64KB以内にします。書き込み途中の読み込みを避けるため、一時ファイルを完成させてからrenameで置き換えてください。`data/` はGit管理外なので、実際のplaybookは運用環境で保持してください。この機能を含むバイナリへの更新時のみ再起動が必要です。

## メモリ

ebi-x は作業で得た長期的に有用な情報を、次の2スコープに分けて保存します。

```text
data/memory/MEMORY.md                     # 全ユーザー・全チャンネル共通
data/memory/channels/{Slack channel ID}/MEMORY.md
```

- 全体メモリ: 他のユーザーやチャンネルでも再利用できる技術的・運用上の知識
- チャンネルメモリ: そのチャンネルの参加者で共有してよい用語・目的・運用ルール

全体メモリへの追記も全チャンネルに影響するため、`EBIX_SHARED_WRITE_CHANNEL_IDS` のチャンネルからの提案だけを保存します。それ以外のチャンネルではチャンネルメモリだけを提案できます。

Codex はメモリファイルを直接読み書きしません。bot だけが全体メモリと現在のSlack channel IDに対応する内容を読み、プロンプトへ注入します。Codex の全 turn ではユーザー設定を読み込まない専用 permission profile を使い、`data/memory` の絶対パスを read/write ともに deny します。`EBIX_WRITABLE_ROOTS` がmemory本体・親・子と重なる場合も、シンボリックリンクを解決したうえで起動を拒否します。

Codex は最終応答で追記を提案し、bot が保存先を決定します。認証情報、秘密、一時的な依頼内容、推測したセンシティブ属性は保存対象外です。既存の `data/memory/MEMORY.md` はそのまま全体メモリとして使用されます。既存の `data/memory/users/{Slack user ID}/MEMORY.md` はbotが使用せず、削除もしません。分離導入前のCodexセッションは再利用せず、導入後の最初のmentionから新しいセッションになります。

## Codexから保護するパス

方針検討ターンの読み取り専用サンドボックスでも、Codexが実行するコマンドはファイルシステム全体を読めます。そのため、次のパスはすべてのターンで読み書きともに deny します。

- `data/memory`、`data/state`
- `.env`（起動ディレクトリと `EBIX_HOME`）と `EBIX_HOME/.codex`
- `CODEX_HOME`（既定 `~/.codex`）の `auth.json`、`sessions`、`archived_sessions`、`history.jsonl`
- 他スレッドの `data/workspace/...` と `data/checkouts/...`
- `EBIX_DENIED_READ_PATHS` に列挙したパス（`~/.aws` など。git push に使う認証情報は作業ターンで必要なので含めないでください）

`EBIX_WRITABLE_ROOTS` がこれらと重なる場合は起動を拒否します。また、`SLACK_BOT_TOKEN` と `SLACK_APP_TOKEN` はCodexのプロセス環境から除きます。

## interrupted の運用

work turn は非冪等な副作用を持つ可能性があります。そのため、work 中のクラッシュや失敗は `interrupted` として記録し、自動再実行しません。ユーザーが作業状況を確認したうえで、新しい mention として依頼し直してください。
