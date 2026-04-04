# peercast-mi

Go 製 PeerCast ノード。**ブロードキャストノード**（RTMP → PCP）と**リレーノード**（上流 PeerCast ノードから受け取って中継）の両方に対応する。

複数チャンネルを同時に扱える。チャンネルは起動時ではなく JSON-RPC API で動的に作成する。

## 必要なもの

- Go 1.22 以上
- RTMP 対応エンコーダー (OBS Studio など)

## インストール

```sh
git clone https://github.com/titagaki/peercast-mi.git
cd peercast-mi
go build -o peercast-mi .
```

## 起動

あらかじめ `config.toml` で YP とポートを設定しておく。

```sh
./peercast-mi
```

### オプション

| フラグ | デフォルト | 説明 |
|---|---|---|
| `-yp` | config.toml の先頭エントリ | 使用する YP 名 |
| `-config` | `config.toml` | 設定ファイルのパス |

### config.toml

```toml
rtmp_port     = 1935
peercast_port = 7144

# ログレベル: "debug" | "info" | "warn" | "error"  (デフォルト: "info")
# log_level = "info"

# 直下に接続できるリレーノード数の上限 (デフォルト: 0 = 無制限)
# max_relays = 4

# 全チャンネル合計のリレーノード数の上限 (デフォルト: 0 = 無制限)
# max_relays_total = 16

# 直下に接続できる HTTP 視聴者数の上限 (デフォルト: 0 = 無制限)
# max_listeners = 4

# 全チャンネル合計の上り帯域上限 kbps (デフォルト: 0 = 無制限)
# max_upstream_kbps = 0

# コンテンツリングバッファが保持する秒数 (デフォルト: 0 = 8秒)
# ビットレートから必要なパケット数を自動計算する
# content_buffer_seconds = 8

[[yp]]
name = "moe"
addr = "pcp://yp.pcmoe.net/"

[[yp]]
name = "local"
addr = "pcp://localhost:7144/"
```

### ログ

ログは標準エラー出力 (stderr) に出力される。ファイルに保存したい場合はリダイレクトする。

```sh
./peercast-mi 2>> peercast.log
```

## チャンネルの作成と配信

`ui/peercast-mm-api-client.html` はブラウザから操作できる Web UI。`file://` で直接開くと CORS エラーになるため、Python の簡易 HTTP サーバーで配信する。

```sh
python3 -m http.server 8080 --directory ui/
# → http://localhost:8080/peercast-mi-api-client.html
```

localhost 以外のマシンからアクセスする場合は `config.toml` で Basic 認証を設定する。

```toml
admin_user = "admin"
admin_pass = "your-password"
```

チャンネルは JSON-RPC API を使って動的に作成する。

### 1. ストリームキーを発行する

```sh
curl -s -X POST http://localhost:7144/api/1 \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"issueStreamKey","params":[],"id":1}'
```

```json
{"jsonrpc":"2.0","id":1,"result":{"streamKey":"sk_a1b2c3d4e5f6..."}}
```

ストリームキーはプロセスが終了するまで有効。チャンネルを止めても失効しない。

### 2. エンコーダーを接続する

OBS Studio の設定:

- **サービス:** カスタム
- **サーバー:** `rtmp://localhost/live`
- **ストリームキー:** 手順 1 で取得したキー (`sk_a1b2c3d4e5f6...`)

エンコーダーの接続はこの時点で行ってもよいし、手順 3 の後でもよい。
ストリームキーが発行済みであれば RTMP 接続は受け付けられる。

### 3. チャンネルを開始する

```sh
curl -s -X POST http://localhost:7144/api/1 \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc":"2.0","method":"broadcastChannel","params":[{
      "streamKey": "sk_a1b2c3d4e5f6...",
      "info": {
        "name":    "テスト配信",
        "genre":   "ゲーム",
        "url":     "https://example.com",
        "desc":    "",
        "comment": "",
        "bitrate": 3000
      },
      "track": {"title":"","creator":"","album":"","url":""}
    }],"id":2}'
```

```json
{"jsonrpc":"2.0","id":2,"result":{"channelId":"0123456789abcdef0123456789abcdef"}}
```

同じパラメータで再度呼び出すと同じ `channelId` が返る（決定論的生成）。

### 4. チャンネルを停止する

```sh
curl -s -X POST http://localhost:7144/api/1 \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"stopChannel","params":["0123456789abcdef0123456789abcdef"],"id":3}'
```

チャンネルが停止してもストリームキーは残るため、手順 3 から繰り返せる。

## 視聴・リレー

peercast-mi はポート 7144 で待ち受ける。

| URL | 用途 |
|---|---|
| `http://localhost:7144/stream/<channelId>` | メディアプレイヤーで直接視聴 |
| `http://localhost:7144/channel/<channelId>` | PeerCast ノードからのリレー接続 |

`channelId` は `broadcastChannel` の返却値、または `getChannels` で確認できる。

## JSON-RPC API

詳細仕様は [docs/api/jsonrpc.md](docs/api/jsonrpc.md) を参照。

| メソッド | 説明 |
|---|---|
| `issueStreamKey` | ストリームキーを発行する |
| `broadcastChannel` | ブロードキャストチャンネルを開始する |
| `getChannels` | チャンネルの一覧 |
| `getChannelInfo` | チャンネル情報を取得 |
| `getChannelStatus` | チャンネルステータスを取得 |
| `setChannelInfo` | チャンネル情報を更新 |
| `stopChannel` | チャンネルを停止 |
| `bumpChannel` | YP への bcst を即時送信 |
| `getChannelConnections` | 接続一覧を取得 |
| `stopChannelConnection` | 特定の接続を切断 |
| `getYellowPages` | YP 一覧を取得 |
| `getChannelRelayTree` | リレーツリーを取得 |
| `getVersionInfo` | エージェント名を取得 |
| `getSettings` | ポート設定を取得 |
