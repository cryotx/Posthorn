# Posthorn

[简体中文](README.zh-CN.md) · [English](README.md) · 日本語 · [한국어](README.ko.md) · [Español](README.es.md)

Posthorn は、セルフホストするアプリケーション向けのメール送信ゲートウェイです。Web フォーム、JSON API、SMTP を受け付け、Resend、Postmark、Mailgun、Amazon SES などの配信サービスへ中継します。配信サービスを複数登録して、ルーティングルールや障害時の切り替えを設定できます。

## Docker Compose で試す

Docker Compose v2、Bash、OpenSSL が必要です。最初にリポジトリを取得します。

```bash
git clone https://github.com/cryotx/Posthorn.git
cd Posthorn/deploy/compose
```

[Compose の手順](deploy/compose/README.md)に沿って、クライアント用 API キーを作成し、ローカルのイメージをビルドしてください。サンプルはループバックアドレスで待ち受け、初期状態では実際のメールを送信しません。認証付きで `POST /api/send` にリクエストすると、`"status":"dry_run"` と生成されたメッセージを確認できます。

実際の送信を始めるには、配信サービスの認証情報と認証済みの送信元アドレスを設定し、`POSTHORN_DRY_RUN=false` に変更します。

## プロジェクトと配信サービスの管理

管理画面を有効にするには、`.env` の `POSTHORN_ADMIN_TOKEN` に管理者専用のトークンを設定し、`docker compose up -d` で設定を反映してから `/admin/login` を開きます。ログイン画面と管理画面で表示言語を選べます。

管理画面では、プロジェクトごとの API キーの発行・更新、配信サービスの設定、再試行キューや配信履歴の確認ができます。プロジェクト用のキーは発行時に一度だけ表示されます。キーを更新すると、古いキーは直ちに無効になります。管理画面で設定した配信サービスの認証情報は、暗号化して保存されます。

## ソースからビルドする

リポジトリのルートで、Go 1.25 以降を使用します。

```bash
cd core
go build -o posthorn ./cmd/posthorn
./posthorn version
```

連携方法は[呼び出しガイド](docs/calling-posthorn.md)を参照してください。詳しい設定例は [posthorn.dev の上流ドキュメント](https://posthorn.dev)にあります。

## ライセンス

この版は [craigmccaskill/posthorn](https://github.com/craigmccaskill/posthorn) を基にしています。ライセンスは [Apache-2.0](LICENSE) です。
