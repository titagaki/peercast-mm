# peercast-mi 実装仕様

Go 製 PeerCast ノードの実装仕様。ブロードキャストノード（RTMP → PCP 配信）とリレーノード（上流 PCP ノードから受け取って中継）の両方に対応する。

コンポーネント別の詳細仕様は [spec-components.md](spec-components.md) を参照。

---

## 1. スコープ

### 対象

- RTMP サーバー (エンコーダーからの push 受信)
- PCP ブロードキャストノード (non-root、`IsBroadcasting = true`)
  - YP への COUT 接続・チャンネル登録
  - 下流 PeerCast ノードへの PCP リレー送信
  - 視聴クライアントへの HTTP 直接送信
- PCP リレーノード (上流 PeerCast ノードからストリームを受け取って中継)
  - 上流ノードへの PCP 接続・ストリーム受信
  - 受信したストリームを下流ノード・視聴クライアントに配信

### 対象外

- Web UI
- push 接続 (firewalled ノード向け)
- 上流ノードの自動探索 (YP からの host アトムを使った自動接続)

---

## 2. システム構成

```
エンコーダー                       上流 PeerCast ノード
    │ RTMP push (ポート 1935)          │ PCP (ポート 7144)
    │ stream key で認証                │ GET /channel/<id>
    ▼                                  ▼
┌──────────────┐          ┌───────────────────────┐
│ internal/rtmp│          │ internal/relay         │
│ RTMPServer   │          │ RelayClient            │
│ FLV タグ変換 │          │ 上流接続・ストリーム受信│
└──────┬───────┘          └───────────┬───────────┘
       │ Write/SetHeader/SetInfo       │ Write/SetHeader/SetInfo
       ▼                               ▼
┌──────────────────────────────────────────────────────────┐
│ internal/channel — Manager + Channel                     │
│  ┌────────────────────────────────────────────────────┐  │
│  │  Manager                                           │  │
│  │  keys          *StreamKeyStore     (ストリームキー) │  │
│  │  byID          map[GnuID]*Channel  (全アクティブch) │  │
│  │  byStreamKey   map[string]*Channel (broadcast のみ) │  │
│  │  streamKeyByID map[GnuID]string    (逆引き)       │  │
│  │  relays        map[GnuID]RelayHandle (relay のみ)  │  │
│  └──────────────────┬─────────────────────────────────┘  │
│                     │ 0..N                                │
│                     ▼                                     │
│  ┌─────────────────────┐  ┌─────────────────────────┐    │
│  │  ContentBuffer      │  │  ChannelInfo / TrackInfo │    │
│  │  (head + data ring) │  │  (メタデータ)             │    │
│  └─────────────────────┘  └─────────────────────────┘    │
└──┬────────────────────────────────────────────────────┬───┘
   │ fan-out (チャンネルID でルーティング)               │ 定期 bcst (全チャンネル)
   ▼                                                    ▼
┌────────────────────┐                     ┌──────────────────────┐
│ internal/servent   │                     │ internal/yp          │
│  Listener          │                     │  YPClient            │
│  ポート 7144       │                     │  (COUT 接続)          │
│  プロトコル識別    │                     │  YP に bcst 送信      │
└──────┬─────────────┘                     └──────────────────────┘
       │
       ├─ GET /channel/<id> → PCPOutputStream  × N  (下流リレーノード)
       ├─ GET /stream/<id>  → HTTPOutputStream × N  (視聴プレイヤー)
       ├─ pcp\n             → handlePing() (YP ファイアウォール疎通確認)
       └─ POST /api/1       → JSON-RPC API (internal/jsonrpc)
```

### パッケージ構成

| パッケージ | 役割 |
|:---|:---|
| `internal/rtmp` | RTMPServer |
| `internal/relay` | RelayClient (上流 PCP 接続・ストリーム受信) |
| `internal/channel` | Manager、StreamKeyStore、Channel、ContentBuffer、ChannelInfo/TrackInfo |
| `internal/servent` | Listener、PCPOutputStream、HTTPOutputStream |
| `internal/yp` | YPClient |
| `internal/jsonrpc` | JSON-RPC API サーバー (ChannelManager インターフェース経由で Manager に依存) |
| `internal/pcputil` | PCP Host アトム構築ユーティリティ (BuildHostAtom) |
| `internal/id` | SessionID / BroadcastID / ChannelID 生成 |
| `internal/version` | バージョン定数 |
| `internal/config` | 設定ファイル (TOML) 読み込み |

---

## 3. 識別子

### 3.1 セッション ID (SessionID)

ノードの一意識別子。起動ごとにランダム生成する 16 バイトの GnuID。
PCP ハンドシェイクの `helo.sid` および `bcst.from`、`oleh.sid` として使用する。

### 3.2 ブロードキャスト ID (BroadcastID)

配信セッションの識別子。起動ごとにランダム生成する 16 バイトの GnuID。
`helo.bcid`、`bcst > chan.bcid` として使用する。

### 3.3 ストリームキー (StreamKey)

RTMP エンコーダーの認証に使用するトークン。`sk_` プレフィックスに 16 バイトのランダム hex を続けた形式 (`sk_a1b2c3...`)。

`issueStreamKey` で発行し、プロセスが終了するまで有効。チャンネルのライフサイクルに依存しない。

### 3.4 チャンネル ID (ChannelID)

チャンネルを識別する 16 バイトの GnuID。次の入力から決定論的に生成する:

```
input     = BroadcastID, Name + "\x00" + StreamKey, Genre, Bitrate
ChannelID = peercast-yt 互換 XOR アルゴリズム
```

同じパラメータで `broadcastChannel` を再呼び出しすると同じ ChannelID が返る。

> アルゴリズム詳細は [§10.1](#101-channelid-生成アルゴリズム-実装済み) を参照。

---

## 4. ライフサイクル

### 4.1 プロセス起動フロー

```
1. 設定ファイル (config.toml) を読み込む
2. シグナルハンドラ (SIGINT/SIGTERM) を設定
3. SessionID / BroadcastID (ノードレベル) を生成
4. ChannelManager を生成
5. Listener を起動 (ポート 7144 待ち受け)
6. YPClient を起動 (COUT 接続・bcst ループ開始) ← YP が設定されている場合のみ
7. JSON-RPC API ハンドラーを Listener に登録
8. RTMP サーバーを起動 (ポート 1935 待ち受け)
9. シャットダウンシグナルを待機
```

### 4.2 ブロードキャストチャンネル開始フロー (API 経由)

```
1. クライアントが issueStreamKey を呼ぶ → streamKey 発行
2. エンコーダーが rtmp://host:1935/live/<streamKey> に RTMP push
   ↳ RTMPServer.OnPublish でストリームキーを検証（未発行なら拒否）
3. クライアントが broadcastChannel を呼ぶ
   ↳ streamKey パラメータからストリームキーを取得
   ↳ Channel (IsBroadcasting=true) を生成、Manager に登録、channelId を返す
4. RTMP データが Channel.ContentBuffer に流れ始める
5. YPClient が次の bcst サイクルで新チャンネルを YP に通知
```

> エンコーダー接続 (手順 2) と broadcastChannel 呼び出し (手順 3) は順不同。
> broadcastChannel より先に RTMP が来た場合、データはチャンネルが作成されるまで静かにドロップされる。

### 4.3 リレーチャンネル開始フロー (API 経由)

```
1. クライアントが relayChannel({upstreamAddr, channelId}) を呼ぶ
2. Channel (IsBroadcasting=false) を生成、Manager に登録
3. RelayClient を生成し、ゴルーチンとして Run() を起動
4. RelayClient が upstreamAddr に TCP 接続
5. HTTP GET /channel/<channelId> + pcp\n magic + helo を送信
6. 上流から HTTP 200 + oleh + 初期 chan アトム (info/track/header) を受信
7. Channel に info/track/header をセット
8. 上流からの pkt アトムを Channel.ContentBuffer に継続的に書き込む
9. YPClient が次の bcst サイクルで新チャンネルを YP に通知
```

> 接続失敗・切断時は RelayClient が指数バックオフ (5 秒〜120 秒) で自動再接続する。
> Channel オブジェクトは再接続をまたいで維持され、下流ノードの接続も維持される。

### 4.4 チャンネル停止フロー

```
1. クライアントが stopChannel(channelId) を呼ぶ
2. リレーチャンネルの場合: RelayClient.Stop() → 上流接続を閉じ、再接続ループを終了
3. Channel.CloseAll() → 全 PCPOutputStream に quit 送信、全接続を閉じる
4. Manager から Channel を削除
5. ブロードキャストチャンネルの場合: streamKey は残る（再度 broadcastChannel で再開可能）
```

### 4.5 プロセス終了フロー

```
1. SIGINT / SIGTERM シグナル受信
2. RTMPServer.Close() → RTMP 受信停止
3. Listener.Close() → 新規接続受け付け停止
4. Manager.StopAll() → 全 RelayClient.Stop() + 全チャンネルの CloseAll()
5. YPClient.Stop() (defer) → quit(QUIT+SHUTDOWN) 送信 → YP 接続を閉じる
```

### 4.6 RTMP 再接続時

エンコーダーが再接続した場合:

1. `Channel.SetHeader()` を再呼び出し (新しいシーケンスヘッダー)
2. 既存の PCPOutputStream に `chan > pkt(type=head)` を再送
3. ストリームバイト位置は継続 (リセットしない)

---

## 5. 並行処理設計

### Goroutine 一覧

| goroutine | 役割 |
|:---|:---|
| `RTMPServer` accept | TCP accept ループ |
| `RTMPServer` per-conn | 接続ごと: RTMP 受信・デコード |
| `relay.Client.Run` | チャンネルごと: 上流接続・ストリーム受信ループ (再接続含む) |
| `YPClient.Run` | COUT 接続・bcst 送信ループ |
| `Listener` accept | TCP accept ループ |
| `PCPOutputStream.run` | 接続ごと: PCP 送信ループ |
| `HTTPOutputStream.run` | 接続ごと: HTTP 送信ループ |

### データ共有

```
RTMPServer ──(SetHeader/Write)──→ ContentBuffer ←──(Since/Header)── 出力 goroutine 群
```

- `ContentBuffer` の読み書きは `sync.RWMutex` で保護する
- 出力 goroutine は自前の `pos uint32` を持ち、`Channel.Since(pos)` でポーリングする
  （`ContentBuffer` は `Channel` の private フィールドで、委譲メソッド経由でアクセスする）

### 通知チャネル

ヘッダー・メタデータ更新は各出力 goroutine の専用チャネルで通知する。

```go
type PCPOutputStream struct {
    headerCh chan struct{} // SetHeader 時に通知
    infoCh   chan struct{} // SetInfo 時に通知
    trackCh  chan struct{} // SetTrack 時に通知
    closeCh  chan struct{} // 終了通知
}
```

各チャネルはバッファサイズ 1 でノンブロッキング送信 (`select { case ch <- struct{}{}: default: }`)。
これにより通知の取りこぼし防止と goroutine ブロック回避を両立する。

---

## 6. エラー処理

| 状況 | 対応 |
|:---|:---|
| RTMP 接続切断 | ストリーム停止。YP へ quit 送信。出力接続を全て閉じる |
| 上流 PCP 接続切断 | 指数バックオフ (5〜120 秒) で自動再接続。Channel・下流接続は維持 |
| YP 接続切断 | 指数バックオフで再接続。出力接続は維持 |
| 出力ストリーム切断 | 該当接続のみ終了。他は継続 |
| 出力キュー詰まり (5 秒) | 該当接続を強制切断 |
| helo バリデーション失敗 | quit atom を送信して接続を閉じる |

---

## 7. 定数

| 定数 | 値 | 説明 |
|:---|:---|:---|
| `defaultPCPPort` | 7144 | PCP リスニングポート |
| `defaultRTMPPort` | 1935 | RTMP リスニングポート |
| `PCPVersion` | 1218 | PCP プロトコルバージョン |
| `PCPVersionVP` | 27 | VP 拡張バージョン |
| `DefaultContentBufferSize` | 64 | コンテンツバッファの最小パケット数 |
| `DefaultContentBufferSeconds` | 8.0 | バッファが保持するデフォルト秒数 |
| `bcstTTL` | 7 | bcst TTL |
| `retryInitial` | 5s | YP 再接続初期待機時間 |
| `retryMax` | 120s | YP 再接続最大待機時間 |
| `defaultInterval` | 120s | YP bcst 送信間隔 (root.uint で上書きされる) |
| `outputQueueTimeout` | 5s | PCP 出力キュー詰まり検出時間 |
| `directWriteTimeout` | 60s | HTTP 直接送信タイムアウト |
| `httpPollInterval` | 200ms | HTTP 出力ポーリング間隔 |
| `pollInterval` | 50ms | PCP 出力ポーリング間隔 |

---

## 8. peercast-pcp ライブラリ対応表

`github.com/titagaki/peercast-pcp/pcp` が提供する API とこの実装での使用箇所。

| API | 使用箇所 |
|:---|:---|
| `pcp.Dial` | `YPClient.run` — YP への COUT 接続 |
| `pcp.ReadAtom` / `Atom.Write` | 全 PCP 通信 |
| `pcp.NewParentAtom` / `pcp.New*Atom` | 全アトム構築 |
| `pcp.GnuID` | SessionID / BroadcastID / ChannelID |
| `pcp.PCPHostFlags1*` | `flg1` ビット定数 |
| `pcp.PCPBcstGroup*` | `grp` ビット定数 |
| `pcp.PCPError*` | quit アトムのエラーコード |

> **注意:** `relay.Client` は `pcp.Dial` を使わず `net.DialTimeout` で TCP 接続したあと
> 手動で HTTP GET + pcp\n magic + helo を送信する。これは PCPOutputStream の受け付けフローに
> 合わせるためで、`pcp.Dial` が送る "pcp\n" atom と長さ・ペイロードが異なるため。

---

## 9. 依存ライブラリ

| ライブラリ | 用途 |
|:---|:---|
| `github.com/titagaki/peercast-pcp` | PCP プロトコル層 |
| `github.com/yutopp/go-rtmp` | RTMP サーバー |
| `github.com/yutopp/go-amf0` | AMF0 デコード (onMetaData) |

---

## 10. 設計決定メモ

peercast-yt (`_ref/peercast-yt/core/common/`) のコードリーディングに基づく設計決定の記録。

---

### 10.1 ChannelID 生成アルゴリズム (実装済み)

peercast-yt (`gnuid.cpp:24`, `servhs.cpp:2440`) と互換の XOR ベースアルゴリズムを採用。
`internal/id/id.go` の `ChannelID(broadcastID, name, genre, bitrate)` として実装。

- 入力: `BroadcastID`, `Name`, `Genre`, `Bitrate(uint8)`
- SHA512+MD5 方式は非互換のため不採用
- peercast-yt の `randomizeBroadcastingChannelID` フラグは peercast-mi では実装しない

---

### 10.2 `x-peercast-pos` による途中参加 (実装済み)

`PCPOutputStream.handshake()` で `x-peercast-pos` ヘッダーを読み取り、`streamLoop()` の初期位置として使用。
- `reqPos` がバッファ範囲内: その位置から開始
- `reqPos` が古すぎる: `OldestPos()` から開始
- `reqPos == 0`: ヘッダー位置から開始

---

### 10.3 push 接続 (firewalled ノード向け)

peercast-yt の GIV プロトコルは「firewalled リレーノードへのアウトバウンド接続」であり、**対象外**。
peercast-mi はブロードキャストノード専用で、下流ノードは常に peercast-mi 側に接続してくる。

---

### 10.4 `helo.ping` によるファイアウォール疎通確認 (実装済み)

- `yp/client.go` の `buildHelo()` に `ping = listenPort` を含める
- `servent/listener.go` の `handlePing()` で `pcp\n` + helo を受け取り、oleh + quit を返す
- `servent/pcp.go` のハンドシェイクで `PCPHeloPing` フィールドを確認し、`pingHost()` によるアクティブ ping（TCP 接続 + helo/oleh 交換）を実行。ping 成功時のみ下流ノードの port を設定する

---

### 10.5 リスナー数・リレー数の正確なカウント (実装済み)

`Channel` が `numListeners` / `numRelays` カウンタを保持し、`AddOutput` / `RemoveOutput` 時に更新。
`buildBcst()` では `ch.NumListeners()` / `ch.NumRelays()` の実際の値を使用。

---

### 10.6 インターフェース分離 (実装済み)

- **`OutputStream` / `BcstForwarder`**: `OutputStream` インターフェースから PCP 固有の `SendBcst(atom)` / `PeerID()` を `BcstForwarder` インターフェースに分離。`Channel.Broadcast()` は型アサーションで `BcstForwarder` を使用。HTTPOutputStream は `BcstForwarder` を実装しない。
- **`jsonrpc.ChannelManager`**: JSON-RPC サーバーは具象型 `*channel.Manager` ではなくインターフェース `ChannelManager` に依存する。テスト時にモックが容易。

### 10.7 StreamKeyStore の分離 (実装済み)

ストリームキーの管理 (発行・失効・永続化) を `Manager` から `StreamKeyStore` に分離。`Manager` は `*StreamKeyStore` を保持し、`IssueStreamKey` / `RevokeStreamKey` / `IsIssuedKey` を委譲する。`StreamKeyStore` は独自の `sync.RWMutex` を持ち、キャッシュファイルへの永続化を担当する。

### 10.8 Host アトム構築の共通化 (実装済み)

`servent/pcp.go`、`relay/client.go`、`yp/client.go` の 3 箇所にあった Host アトム構築ロジックを `internal/pcputil.BuildHostAtom` に統合。`HostAtomParams` 構造体で YP 固有フィールド (`TrackerAtom`) やリレー固有フィールド (`UphostIP/Port/Hops`) を表現する。
