# peercast-mm

RTMP で受け取ったストリームを PCP ネットワークに配信する、Go 製 PeerCast ブロードキャストノード。

## リポジトリ構成

```
main.go                    エントリーポイント
internal/
  id/id.go                 SessionID / BroadcastID / ChannelID 生成
  channel/
    info.go                ChannelInfo / TrackInfo
    content.go             ContentBuffer (64件リングバッファ)
    channel.go             Channel (OutputStream ファンアウト)
  rtmp/server.go           RTMPServer (yutopp/go-rtmp ラッパー、FLV タグ変換)
  yp/client.go             YPClient (COUT 接続・bcst ループ・指数バックオフ再接続)
  output/
    listener.go            OutputListener (ポート 7144、プロトコル識別)
    pcp.go                 PCPOutputStream (下流リレーノードへの PCP 送信)
    http.go                HTTPOutputStream (メディアプレイヤーへの HTTP 送信)
docs/
  broadcasting-node-spec.md  実装仕様書
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
go run . -name "チャンネル名" -genre "ジャンル" -yp yp.example.com:7144
```

| フラグ | デフォルト | 説明 |
|---|---|---|
| `-name` | `test` | チャンネル名 |
| `-genre` | — | ジャンル |
| `-url` | — | コンタクト URL |
| `-desc` | — | チャンネル説明 |
| `-yp` | — | YP アドレス (host:port) |

エンコーダーから `rtmp://localhost/live/<任意のストリームキー>` に push して使う。

## ポート

| ポート | 用途 |
|---|---|
| 1935 | RTMP (エンコーダーからの push 受信) |
| 7144 | PCP / HTTP (下流ノード・視聴プレイヤー) |

## スコープ

- **対象:** ブロードキャストノード (IsBroadcasting = true、non-root)
- **対象外:** リレーノード (上流から受け取って中継する機能)、Web UI、push 接続 (firewalled ノード向け)

## 未解決事項

- [ ] ChannelID 生成アルゴリズムの既存実装 (peercast-yt 等) との互換性確認
- [ ] `x-peercast-pos` による途中参加時のストリーム位置決定ロジック
- [ ] push 接続 (firewalled ノード向け) の対応範囲

## 作業の進め方

- 仕様は `docs/broadcasting-node-spec.md` を参照
- PCP プロトコルの詳細は `docs/protocol/PCP_SPEC.md` を参照
- peercast-pcp の API は `go doc github.com/titagaki/peercast-pcp/pcp` で確認
