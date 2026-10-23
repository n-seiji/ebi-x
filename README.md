# ebi-x

ebi-x は、Slack で頼まれた作業を Codex に実行させ、結果を同じスレッドへ返す常駐ボットです。

```text
@tanaka  @ebi-x CI が落ちている原因を調べて
  └ ebi-x がコマンドを実行しています（go test）… 1/3 完了
  └ @ebi-x  原因はタイムゾーンの扱いでした。修正して PR を作成しました。…
            👀 次の PR を見守ります。CI の失敗やレビューがあれば、このスレッドで対応します。
```

## できること

- **依頼**：mention、ebi-x との DM、メッセージの「…」メニューから頼めます。スレッドに返信すれば、同じ Codex セッションで続きを頼めます。
- **作業の見える化**：作業中は Codex がいま何をしているかを表示し、「停止」で止められます。Home タブに自分の依頼の状況が並びます。
- **会話が止まった後の作業**：後で確認するフォローアップ、曜日と時刻を決めた定期実行、作った PR の CI やレビューへの対応ができます。
- **成果物**：PDF・画像・pptx・CSV をスレッドに添付します。スライドの作成から納品までを playbook で扱えます。
- **安全な実行**：使える人とチャンネルを許可制にし、Codex はスレッドごとの作業場所とサンドボックスの中で動きます。外部への送信など確認が必要な操作の前には、依頼者に聞きます。

使い方の詳細は [docs/usage.md](docs/usage.md) にあります。

## 仕組み

1. Slack の Socket Mode でイベントを受け、依頼した人とチャンネルが許可されているかを確かめます。
2. Slack スレッドごとに Codex セッションを1つ持ち、`codex exec` で新規作成または再開します。1回の実行で調査・作業・回答まで行います。
3. Codex の最終応答から決まった見出し（添付ファイル、フォローアップなど）を取り出して bot が処理し、残りの本文を Slack に投稿します。

設計の全体像は [docs/design.md](docs/design.md) にあります。

## はじめかた

必要なもの：Go（`mise install` で揃います）、Codex CLI 0.149.0 以上（ログイン済み）、Slack App を作れるワークスペース。

1. `examples/slack-app-manifest.json` から Slack App を作り、ワークスペースにインストールします。Bot Token（`xoxb-`）と Socket Mode 用の App Token（`xapp-`）を取得します。
2. `.env` を作り、トークンと許可する user・channel の ID を書きます。

   ```sh
   cp .env.example .env
   ```

3. ビルドして起動します。

   ```sh
   mise run build   # または go build -o ebi-x ./cmd/ebi-x
   TZ=Asia/Tokyo ./ebi-x
   ```

4. 許可したチャンネルに ebi-x を招待し、`@ebi-x 生きてる？` と mention して応答を確かめます。

Slack App の設定項目、`.env` の各項目、承認チャンネル、PR の見守りや MCP の設定は [docs/setup.md](docs/setup.md) にあります。

## ドキュメント

| ファイル | 内容 |
| --- | --- |
| [docs/setup.md](docs/setup.md) | 導入と設定（Slack App、`.env`、許可、運用） |
| [docs/usage.md](docs/usage.md) | Slack での使い方と、各機能の動き |
| [docs/design.md](docs/design.md) | 設計（処理の流れ、状態、出力契約、隔離と保護） |
| [examples/](examples/) | Slack App の manifest、操作ルールと playbook の例 |

## 開発

```sh
mise run test   # go test -race ./...
mise run lint   # go vet ./... と gofmt の確認
```

動いている ebi-x にコードの変更を反映するときは `mise run reload` を使います。実行中の作業を止めずに入れ替えます（[docs/setup.md](docs/setup.md#9-運用)）。
