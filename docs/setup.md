# 導入と設定

ebi-x を動かすまでの手順と、運用者が決める設定をまとめます。Slack での使い方は [usage.md](usage.md)、内部の仕組みは [design.md](design.md) を見てください。

## 1. Slack App

Slack App の作成画面で「From a manifest」を選び、`examples/slack-app-manifest.json` を貼り付けると、以下がまとめて入ります。manifest を使わない場合は、同じものを手で設定します。

| 項目 | 設定 | 用途 |
| --- | --- | --- |
| Socket Mode | 有効 | 公開 URL なしでイベントを受ける |
| Bot Token Scopes | `app_mentions:read` `chat:write` `reactions:read` `reactions:write` `channels:history` `files:write` `im:history` `assistant:write` `commands` | mention・投稿・リアクション、成果物の添付、DM とエージェント表示、ショートカット |
| Event Subscriptions | `app_mention` `message.channels` `message.im` `app_home_opened` | mention、購読スレッドの返信、DM、Home タブ |
| App Home | Home Tab と Messages Tab を有効、「Allow users to send Slash commands and messages from the messages tab」をオン | Home タブと DM |
| Interactivity & Shortcuts | Interactivity をオン、Message Shortcut「ebi-x に頼む」（Callback ID `ebix_request`） | メッセージから依頼する |
| Agents & AI Apps | Agent App に切り替え（manifest では `features.agent_view`） | サイドバーと Messages タブでのエージェント表示 |

- `:done:` の監視（[usage.md](usage.md#done-の監視)）で `@channel` を使うときは `channels:read` と `users:read`（プライベートチャンネルでは `groups:read` も）を追加します。manifest には入っています。
- プライベートチャンネルで使う場合は `groups:history` も追加します（スレッドを読むため）。`message.groups` は購読しません。mention なしの返信を処理する購読スレッドは、パブリックチャンネルだけが対象です。
- Agent App への切り替えは元に戻せません。Slack の有料プランが必要です。
- scope を変えたらワークスペースへ再インストールします。Bot Token が変わった場合は `.env` も更新します。

インストール後、Bot Token（`xoxb-`）と、Socket Mode 用の App Token（`xapp-`、`connections:write`）を取得します。

## 2. `.env`

`.env.example` を `.env` にコピーして書きます。主な項目は次のとおりです。各項目の説明は `.env.example` にもあります。

| 変数 | 既定 | 内容 |
| --- | --- | --- |
| `SLACK_BOT_TOKEN` / `SLACK_APP_TOKEN` | なし（必須） | Slack のトークン |
| `SLACK_ALLOWED_USER_IDS` / `SLACK_ALLOWED_CHANNEL_IDS` | なし | 常に許可する user と channel。承認チャンネルを使わないときは、それぞれ1件以上必須 |
| `SLACK_APPROVAL_CHANNEL_ID` / `SLACK_APPROVER_USER_IDS` | なし | 承認チャンネルと承認者（後述） |
| `SLACK_ALLOW_WORKFLOWS` / `SLACK_ALLOWED_WORKFLOW_IDS` | `false` / なし | Workflow Builder からの mention を受けるか、受ける Workflow |
| `SLACK_ADMIN_USER_ID` | なし | 権限がない人に「許可を頼む相手」として示す user。未設定なら `@seiji` という文字列 |
| `SLACK_THREAD_SUBSCRIPTION_REACTION` / `SLACK_THREAD_SUBSCRIPTION_TTL` | `thread-subete` / `336h` | mention なしの返信も処理するスレッド購読。リアクションを空にすると無効 |
| `CODEX_COMMAND` | `codex` | Codex の実行ファイル（引数は書けません） |
| `CODEX_MODEL` | Codex の既定 | 使うモデル |
| `CODEX_TIMEOUT` | `30m` | 1回の Codex 実行の上限時間 |
| `CODEX_MAX_PARALLEL_WORK` | `3` | 同時に動かすスレッドの数 |
| `EBIX_HOME` | `.` | `data/` と `.codex/` を置くディレクトリ |
| `EBIX_WRITABLE_ROOTS` | なし | Codex が書き込めるディレクトリ（作業対象のリポジトリなど） |
| `EBIX_DENIED_READ_PATHS` | なし | 既定の認証情報の置き場に加えて、Codex に読ませないパス |
| `EBIX_ALLOWED_READ_PATHS` | なし | 既定で読ませない認証情報の置き場のうち、作業に要るもの（`~/.config/gcloud` など、後述） |
| `EBIX_CODEX_ENV` | なし | 許可リストに加えて Codex に渡す環境変数の名前（後述） |
| `EBIX_SHARED_WRITE_CHANNEL_IDS` | なし | playbook と全体メモリを書き換えられるチャンネル。空ならどこからも書けない |
| `EBIX_CHECKOUT_IDLE_TTL` | `120h` | 使われなくなったスレッド専用クローンを消すまでの時間 |
| `EBIX_GITHUB_TOKEN` / `EBIX_GITHUB_API_URL` | なし | PR の見守りに使うトークンと API の URL（後述） |

時刻はプロセスのローカルタイムゾーンで扱うので、日本時間で運用するなら `TZ=Asia/Tokyo` で起動します。

### 廃止した設定

古い `.env` を使い回すときは、次の項目を消してください。

| 変数 | 扱い |
| --- | --- |
| `SLACK_ALLOW_ALL_PUBLIC_CHANNELS` | `true` だと起動しません。チャンネルは `SLACK_ALLOWED_CHANNEL_IDS` か承認チャンネルで1つずつ許可します |
| `CODEX_MAX_PARALLEL_PLAN` | 使いません。同時実行数は `CODEX_MAX_PARALLEL_WORK` です |
| `CODEX_WORK_MODEL` | 使いません。モデルは `CODEX_MODEL` です |

## 3. 使える人とチャンネル

ebi-x を使えるかは、`.env` の固定の許可と、承認チャンネルで承認者が許可したものの2つで決まります。人の許可とチャンネルの許可の両方が要ります。DM は本人と ebi-x だけの会話なので、人が許可されていればチャンネルの許可は要りません。

権限のない人・チャンネル・Workflow から mention されると、何に権限がないかと、`SLACK_ADMIN_USER_ID` に許可を頼むよう同じスレッドに返します。

```text
🔒 あなたには ebi-x を使う権限がありません。使いたい場合は @seiji に許可を頼んでください。
```

### 承認チャンネル

`SLACK_APPROVAL_CHANNEL_ID` と `SLACK_APPROVER_USER_IDS` を設定して Bot を招待すると、許可されていない相手からの依頼を Slack 上で許可できます。追加の scope は要りません。

```text
#sales
@tanaka   @ebi-x 先週の売上まとめて
  └ @ebi-x  🔒 あなたには ebi-x を使う権限がまだないため、承認者に許可を依頼しました（承認依頼）。
            そのままお待ちください！許可されたら、この依頼から自動で始めます。

#ebi-x-owner
@ebi-x    🔐 ebi-x の利用許可の依頼 / 対象: チャンネル #sales
  └ @seiji   @ebi-x 許可
  └ @ebi-x  ✅ チャンネル #sales: @seiji が許可しました。

#sales（元のスレッド）
  └ @ebi-x  ✅ チャンネル #sales が許可されました。依頼を始めます。
```

- 依頼は相手ごとに決まるまで1件だけ出します。人とチャンネルの両方が未許可なら2件です。元のスレッドには依頼へのリンクを返します（承認チャンネルがプライベートだと、メンバー以外は開けません）。
- 承認者は依頼のスレッドで `@ebi-x 許可` か `@ebi-x 拒否` と mention します。操作できるのは `SLACK_APPROVER_USER_IDS` の人だけで、最初の決定が優先されます。拒否した相手は次から確認せずに断ります。
- 必要な許可が揃うと、待っていた依頼を自動で始めます。同じ相手の依頼が複数待っていれば最後の1件だけです。24時間より古い依頼、本文のない依頼、Workflow からの依頼は自動では始めず、もう一度 mention するよう伝えます。
- 承認チャンネルでは許可・拒否と利用枠の確認だけを受け付け、作業の依頼は受けません。
- 決定は誰がいつ決めたかと一緒に `data/state/approvals.json` に残ります。取り消すときは ebi-x を止めて、このファイルから該当する項目を消します。Codex はこのファイルを読み書きできません。

### Workflow からの mention

`SLACK_ALLOW_WORKFLOWS=true` にすると、Workflow Builder の「メッセージを送信」からの mention を受けます。受けるのは、Slack イベントの `workflow_id` が `SLACK_ALLOWED_WORKFLOW_IDS` にあるか承認チャンネルで許可されたものだけです。Workflow はワークスペースの誰でも作れるので、ID で限定しないと人の許可を迂回できてしまうためです。Workflow もチャンネルの許可の対象です。それ以外の Bot の投稿には反応しません。

## 4. 作業対象のリポジトリ

Codex に書き込ませたいディレクトリを `EBIX_WRITABLE_ROOTS` に並べます。そのうち Git リポジトリの最上位ディレクトリは、スレッドごとのクローンで作業するので元のリポジトリは書き換わりません（[design.md](design.md#作業場所の隔離)）。クローンの `origin` は元のリポジトリの `origin` なので、作業用ブランチを push できます。

`~/.ssh`・`~/.aws`・`~/.config/gh`・`~/.netrc` などの既知の認証情報の置き場は、既定で Codex に読ませません（一覧は [design.md](design.md#codex-から守るもの)）。ただし push と署名付きコミットに要るものは、起動時にリポジトリごとに調べて読めるようにし、どのパスを外したかを理由と一緒に起動ログに出します。

- push 先が SSH なら `~/.ssh`、HTTPS なら git の credential helper に応じて `~/.git-credentials`（`store`）、`~/.config/gh`（`gh`）、helper なしなら `~/.netrc` を外します。`osxkeychain` などはパスが要らないので外しません。
- `commit.gpgsign=true` なら、`gpg.format` に応じて `~/.ssh` か `~/.gnupg` を外します。
- 調べるのはグローバルとシステムの git 設定だけです。元のリポジトリの `.git/config` にだけ書いた helper や `pushInsteadOf` は、スレッドのクローンに引き継がれないので見ません。git は Codex に渡すのと同じ環境変数で動かします。
- `EBIX_WRITABLE_ROOTS` にリポジトリの親ディレクトリを書いた場合、配下のリポジトリは調べません。そこから SSH で push するなら `EBIX_ALLOWED_READ_PATHS=~/.ssh` を書きます。
- `gcloud`・`bq`・`kubectl`・プライベート npm registry などを作業で使うときは、`EBIX_ALLOWED_READ_PATHS=~/.config/gcloud,~/.kube,~/.npmrc` のように書きます。書けるのは既定の一覧にあるものだけで、それ以外は起動エラーです。

push の認証情報は `EBIX_DENIED_READ_PATHS` に入れないでください。push させたくない場合は、元のリポジトリの `origin` の認証情報を絞ります。

`EBIX_WRITABLE_ROOTS` が ebi-x の管理するパス（`data/` の各ディレクトリ、`.env`、`EBIX_HOME/.codex`、`CODEX_HOME`、`EBIX_DENIED_READ_PATHS`）と重なると、起動を拒否します。

## 5. 操作ルール

Codex がどの操作の前に依頼者へ確認するかは、既定で次のとおりです。

- 先に確認する：リモートへの push、PR やイシューの作成・マージ・クローズ、Slack・メール・外部サービスへの送信、ファイルやリソースの削除・公開、費用が発生する操作、本番環境の変更
- 確認せずに行う：調査と読み取り、スレッド用の作業コピーでの編集・コミット、テストやビルド、成果物の作成

変えたいときは `$EBIX_HOME/data/rules.md` に Markdown で書きます（例は `examples/rules.md`）。ファイルがあれば既定の代わりに使い、新しいスレッドから再起動なしで反映します。16KB までで、Codex からは書き換えられません。

ルールはモデルへの指示です。書き込める範囲や読めないパスは、サンドボックスで別に強制しています。

## 6. playbook

playbook は、特定の作業の手順を書いた Markdown です。`data/playbooks/` の直下に置くと、スレッドの最初の依頼で一覧が Codex に渡り、該当する作業のときに読まれます。

```markdown
---
name: slides
description: どんな依頼のときに使うかを具体的に書く
---

# 手順 …
```

- frontmatter の `name` と `description` が必要で、1ファイル 64KB までです。`README.md` は一覧に含めません。
- 置き換えるときは一時ファイルを書き終えてから rename します。
- `examples/playbooks/` に例があります。

| 例 | 内容 |
| --- | --- |
| `slides.md` | [miive-project/slides](https://github.com/miive-project/slides) でスライドを作成・修正・納品する |
| `playbook-from-thread.md` | スレッドの対応を playbook にする、既存の playbook を直す |
| `request-discovery.md` | 改善要望を1問ずつ聞き取ってまとめる |

`slides.md` を使うには、slides を clone して `EBIX_WRITABLE_ROOTS` に加え、ビルドに必要な Node・pnpm・Python・Chrome・LibreOffice・日本語フォントを用意して `pnpm install` を済ませておきます。

Slack から playbook を書き換えられるのは `EBIX_SHARED_WRITE_CHANNEL_IDS` のチャンネルだけです。全チャンネルの依頼が playbook を読むためです。`data/` は Git の管理外なので、playbook は運用環境で保管します。

## 7. PR の見守り

追加の設定なしで有効になります。bot は起動時に、`EBIX_WRITABLE_ROOTS` のうち GitHub にある https の origin について、git の credential helper に保存されたトークンを `git credential fill` で読み、それで PR を読みます。`gh auth` は使いません。origin が SSH の場合や認証情報がない場合は無効になります。

- 別のトークンを使うときだけ `EBIX_GITHUB_TOKEN` に書きます。PR・チェック・コミットステータスの読み取り権限で足ります。Codex には渡しません。見守るリポジトリだけに絞った fine-grained token をおすすめします。
- GitHub Enterprise Server では `EBIX_GITHUB_API_URL` に `https://<host>/api/v3` を書きます。
- 1つの PR につき1時間に約180回 API を呼びます。上限が1時間に5,000回のトークンなら、同時に見守る PR は20件程度までにします。
- トークンの持ち主自身のコメントやレビューでは作業を再開しません。

## 8. MCP（BigQuery など）

Codex の MCP サーバーは `$EBIX_HOME/.codex/config.toml` に書きます。`~/.codex/config.toml` は読みません。

[MCP Toolbox](https://github.com/googleapis/genai-toolbox) の prebuilt BigQuery サーバーで SQL を使うときは、`enabled_tools` を書かずに全ツールを公開します。

```toml
[mcp_servers.bigquery]
# command / args / env はそのまま。enabled_tools は書かない
```

Toolbox の `execute_sql` はどんな文でも実行できるため、ebi-x は Toolbox の BigQuery サーバー（`env` に `BIGQUERY_PROJECT` がある、または `--prebuilt bigquery` で起動するもの）に `BIGQUERY_WRITE_MODE` も `BIGQUERY_READONLY` もなければ、`BIGQUERY_WRITE_MODE="blocked"` を付けて起動します。この状態では SELECT だけが実行できます。ラッパースクリプトで起動していてこの条件に当たらない場合は、自分で `env` に書きます。

Google Cloud のリモート BigQuery MCP（`https://bigquery.googleapis.com/mcp`）では、`disabled_tools = ["execute_sql"]` で更新できるツールを外し、`execute_sql_readonly` を使います。

必要なツールが公開されていないとき、Codex は「接続できない」ではなく「公開されていない」と依頼者に伝えます。

### 環境変数

Codex に渡す環境変数は許可リストで決まっていて、`.env` やシェルのそれ以外の変数は渡りません（一覧は [design.md](design.md#codex-から守るもの)）。渡さなかった変数の名前は起動ログに1行出ます。MCP サーバーやモデルプロバイダーが環境変数を読むときは、次の両方が要ります。

1. `EBIX_CODEX_ENV` に変数名を書いて Codex に渡す
2. そのサーバーの `env_vars` に名前を書く（または `env` に値を直接書く）。stdio の MCP サーバーは Codex の環境をそのまま引き継がないためです

```toml
[mcp_servers.example]
command = "example-mcp"
env_vars = ["EXAMPLE_API_TOKEN"]   # EBIX_CODEX_ENV=EXAMPLE_API_TOKEN も必要
env = { EXAMPLE_REGION = "asia-northeast1" }
```

`SLACK_BOT_TOKEN`・`SLACK_APP_TOKEN`・`EBIX_GITHUB_TOKEN` は書けません（起動エラー）。名前に `KEY`・`SECRET`・`TOKEN` を含む変数は Codex 本体には届きますが、Codex が実行するコマンドには渡りません。そのため `GH_TOKEN` などで `git push` や `gh` を認証する使い方はできないので、credential helper を使います。

## 9. 運用

- **1台1プロセス**：起動時に `data/state/ebi-x.lock` をロックし、同じ `data/` で2つ目を起動するとエラーにします。ロックはプロセスの終了で外れるので、ファイルが残っていても問題ありません。エラーが出たら、メッセージの pid のプロセスを止めてから起動します。ロックファイルは消さないでください。
- **リロード**：`mise run reload`（ビルドしてから、動いている ebi-x に `SIGHUP` を送る）で、実行中の作業を止めずに新しいバイナリに入れ替えます。mise なしなら `go build -o ebi-x ./cmd/ebi-x && kill -HUP "$(cat data/state/ebi-x.lock)"` です。
  - 先に新しいバイナリを `-check-config` で起動してみて、失敗したら入れ替えずにそのまま動き続けます。`mise run reload` はビルドに失敗したら `SIGHUP` を送りません。
  - 動いている作業はそのまま続け、その間に届いた依頼と順番待ちの依頼は `data/state/deferred.json` に保存して「🔄 ebi-x を更新しています」と返し、再起動後に届いた順で自動で始めます。取り出してから実行するので、直後に落ちた依頼は実行されません（二重には実行しません）。
  - 作業がすべて終わると Slack から切断し、起動したときのバイナリを `exec` し直します（PID は同じ、`.env` も読み直す）。待ち時間は最長 `CODEX_TIMEOUT` で、1分ごとに残りの件数をログに出します。待っている間の2回目の `SIGHUP` は無視し、`SIGTERM` は通常の停止になります。
  - 切り替えの数秒間に届いた mention は取りこぼすことがあります。ロックを外してから取り直すまでのごく短い間に、手で2つ目を起動しないでください。`go run` で起動している場合と Windows ではリロードできません。
  - systemd では `ExecReload=/bin/kill -HUP $MAINPID` を設定すると `systemctl reload ebi-x` で同じことができます。
- **中断した依頼**：作業中に ebi-x が落ちた依頼は `interrupted` になり、自動ではやり直しません。作業は外部に副作用を残している可能性があるためです。状況を確かめてから、新しい mention で依頼し直します。
- **ディスク**：スレッド専用クローンは `EBIX_CHECKOUT_IDLE_TTL` で自動で消えますが、`data/workspace/` のスレッドごとの作業ディレクトリは消えません。数千スレッドを超えると Codex への引数が上限（約120KB）を超えて作業を始められなくなるので、古いものを消します。
- **Codex のログイン**：ログインが切れると承認チャンネルに知らせが届きます。`codex login` でログインし直します。
- **データの保管**：`data/` は `.gitignore` で除外しています。状態・メモリ・個人設定・playbook はここにあり、コミットされません。
