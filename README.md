# peercast-mm

RTMP で受け取ったストリームを PCP ネットワークに配信する Go 製 PeerCast ブロードキャストノード。

## 必要なもの

- Go 1.22 以上
- RTMP 対応エンコーダー (OBS Studio など)

## インストール

```sh
git clone https://github.com/titagaki/peercast-mm.git
cd peercast-mm
go build -o peercast-mm .
```

## 起動

あらかじめ `config.toml` で YP とポートを設定しておく。

```sh
./peercast-mm -name "チャンネル名" -genre "ジャンル"
```

### オプション

| フラグ | デフォルト | 説明 |
|---|---|---|
| `-name` | **(必須)** | チャンネル名 |
| `-genre` | — | ジャンル |
| `-url` | — | コンタクト URL |
| `-desc` | — | チャンネル説明 |
| `-yp` | config.toml の先頭エントリ | 使用する YP 名 |
| `-config` | `config.toml` | 設定ファイルのパス |

### config.toml

```toml
rtmp_port     = 1935
peercast_port = 7144

# ログレベル: "debug" | "info" | "warn" | "error"  (デフォルト: "info")
# log_level = "info"

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
./peercast-mm -name "テスト配信" 2>> peercast.log
```

`log_level = "debug"` にするとプロトコルの詳細 (YP への bcst 送信、PCP ハンドシェイク、下流ノードからの bcst 受信など) も出力される。

### YP を指定して起動する場合

```sh
./peercast-mm -name "テスト配信" -yp moe
```

## エンコーダーの設定

peercast-mm が起動したら、エンコーダーから以下の URL に RTMP push する。

- **サーバー:** `rtmp://localhost/live`
- **ストリームキー:** 任意 (例: `stream`)

OBS Studio の場合: 設定 → 配信 → サービス「カスタム」→ サーバーに上記 URL を入力。

## 視聴・リレー

peercast-mm はポート 7144 で待ち受ける。

| URL | 用途 |
|---|---|
| `http://localhost:7144/stream/<channel-id>` | メディアプレイヤーで直接視聴 |
| `http://localhost:7144/channel/<channel-id>` | PeerCast ノードからのリレー接続 |

ChannelID は起動時のログに表示される。

```
time=2026-01-01T00:00:00.000Z level=INFO msg=startup session_id=... broadcast_id=... channel_id=a1b2c3d4e5f6...
```
