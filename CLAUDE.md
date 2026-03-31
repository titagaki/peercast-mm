# peercast-mm

Go 製 PeerCast ノード。ブロードキャストノード（RTMP → PCP 配信）とリレーノード（上流から受け取って中継）の両方に対応する。

## リポジトリ構成

```
main.go                    エントリーポイント
internal/
  id/id.go                 SessionID / BroadcastID / ChannelID 生成
  channel/
    info.go                ChannelInfo / TrackInfo
    content.go             ContentBuffer (64件リングバッファ)
    channel.go             Channel (OutputStream ファンアウト、ブロードキャスト/リレー共通)
    manager.go             Manager (ストリームキー・チャンネル・リレークライアント管理)
  rtmp/server.go           RTMPServer (yutopp/go-rtmp ラッパー、FLV タグ変換)
  relay/client.go          RelayClient (上流 PCP ノードへの接続・ストリーム受信)
  yp/client.go             YPClient (COUT 接続・bcst ループ・指数バックオフ再接続)
  servent/
    listener.go            Listener (ポート 7144、プロトコル識別)
    pcp.go                 PCPOutputStream (下流リレーノードへの PCP 送信)
    http.go                HTTPOutputStream (メディアプレイヤーへの HTTP 送信)
docs/
  spec.md                  実装仕様書 (概要・アーキテクチャ・ライフサイクル)
  spec-components.md       実装仕様書 (コンポーネント詳細)
  TODO.md                  改善候補
  api/jsonrpc.md           JSON-RPC API 仕様
  protocol/
    PCP_SPEC.md            PCP プロトコル仕様
    broadcasting.md        配信プロトコルメモ
    viewing.md             視聴関連仕様メモ
    yp_channel_registration.md  YP チャンネル登録仕様
```

## 依存ライブラリ

- `github.com/titagaki/peercast-pcp` — PCP プロトコル層 (本人作)
- `github.com/yutopp/go-rtmp` — RTMP サーバー

## 実行方法

```sh
go run . [-config config.toml] [-yp <YP名>]
```

| フラグ | デフォルト | 説明 |
|---|---|---|
| `-config` | `config.toml` | 設定ファイルのパス |
| `-yp` | config の先頭エントリ | 使用する YP 名 |

チャンネルの作成・設定はすべて JSON-RPC API (`POST /api/1`) で行う。

## ポート

| ポート | 用途 |
|---|---|
| 1935 | RTMP (エンコーダーからの push 受信) |
| 7144 | PCP / HTTP (下流ノード・視聴プレイヤー・JSON-RPC API) |

## スコープ

- **対象:** ブロードキャストノード (RTMP → PCP、`IsBroadcasting = true`)
- **対象:** リレーノード (上流 PCP ノードから受け取って中継)
- **対象外:** Web UI、push 接続 (firewalled ノード向け)

## 未解決事項

- [ ] ChannelID 生成アルゴリズムの既存実装 (peercast-yt 等) との互換性確認
- [ ] `x-peercast-pos` による途中参加時のストリーム位置決定ロジック
- [ ] push 接続 (firewalled ノード向け) の対応範囲
- [ ] YP からの host アトム情報を使った上流ノードの自動探索

## 作業の進め方

- 仕様は `docs/spec.md` / `docs/spec-components.md` を参照
- JSON-RPC API 仕様は `docs/api/jsonrpc.md` を参照
- PCP プロトコルの詳細は `docs/protocol/PCP_SPEC.md` を参照
- peercast-pcp の API は `go doc github.com/titagaki/peercast-pcp/pcp` で確認
