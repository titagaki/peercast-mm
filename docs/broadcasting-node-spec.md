# PeerCast Go 実装 — ブロードキャストノード実装仕様

## 1. 概要

RTMP で受け取ったストリームを PCP ネットワークに配信する、Go 製 PeerCast ブロードキャストノードの実装仕様。

### 1.1 スコープ

- RTMP サーバー (エンコーダーからの push 受信)
- PCP ブロードキャストノード (non-root, IsBroadcasting = true)
  - YP への COUT 接続・チャンネル登録
  - 下流ノードへの PCP リレー送信
  - 視聴クライアントへの HTTP 直接送信

スコープ外: リレーノード (上流から受け取って中継する機能)、Web UI

### 1.2 依存ライブラリ

- `github.com/titagaki/peercast-pcp` — PCP プロトコル層

---

## 2. システム構成

```
エンコーダー
    │ RTMP push
    ▼
┌─────────────────────────────────────────────────────┐
│ RTMPServer                                          │
│  ポート 1935 で待ち受け。FLV タグを解析し、         │
│  Channel にストリームデータを渡す。                 │
└─────────────────┬───────────────────────────────────┘
                  │ ContentSink
                  ▼
┌─────────────────────────────────────────────────────┐
│ Channel                                             │
│  ┌──────────────────┐  ┌──────────────────────────┐ │
│  │  ContentBuffer   │  │  ChannelInfo / TrackInfo │ │
│  │  (head + data)   │  │  (メタデータ)             │ │
│  └──────────────────┘  └──────────────────────────┘ │
└──┬───────────────────────────────────────────────┬──┘
   │                                               │
   │ fan-out                                       │ 定期 bcst
   ▼                                               ▼
┌───────────────────┐                   ┌──────────────────┐
│  OutputListener   │                   │  YPClient        │
│  ポート 7144      │                   │  (COUT 接続)     │
│  プロトコル識別   │                   │  YP に bcst 送信  │
└──────┬────────────┘                   └──────────────────┘
       │
       ├─ GET /channel/ ─→ PCPOutputStream × N  (下流リレーノード)
       ├─ GET /stream/  ─→ HTTPOutputStream × N (視聴プレイヤー)
       └─ pcp\n         ─→ (将来: PING 応答等)
```

---

## 3. 識別子

### 3.1 セッション ID (SessionID)

ノードの一意識別子。起動ごとにランダム生成する 16 バイトの GnuID。
PCP ハンドシェイクの `helo.sid` として送信する。

### 3.2 ブロードキャスト ID (BroadcastID)

配信セッションの識別子。起動ごとにランダム生成する 16 バイトの GnuID。
`helo.bcid`、`bcst` 内の `chan.bcid` として使用する。

### 3.3 チャンネル ID (ChannelID)

チャンネルを識別する 16 バイトの GnuID。次の入力から決定論的に生成する:

```
input = BroadcastID || NetworkType || ChannelName || Genre || SourceURL
ChannelID = MD5(SHA512(input))[:16]
```

NetworkType は固定値 `"ipv4"` とする。

---

## 4. コンポーネント詳細

### 4.1 RTMPServer

#### 使用ライブラリ

`github.com/yutopp/go-rtmp` を使用する。`rtmp.NewServer` でサーバーを立て、`rtmp.ConnHandler` インターフェースを実装して FLV タグを受け取る。

#### 責務

- ポート 1935 で TCP 待ち受け
- RTMP ハンドシェイク処理 (go-rtmp に委譲)
- FLV タグ (Video / Audio / Script Data) のストリームへの変換
- チャンネルメタデータ (`onMetaData`) からの `ChannelInfo` 生成

#### 出力インターフェース

```go
// ContentSink はデコードされたストリームデータを受け取る。
type ContentSink interface {
    // SetHeader はストリームヘッダー (head パケット) を設定する。
    // 上流が再接続した場合など、複数回呼ばれることがある。
    SetHeader(data []byte)

    // Write はストリームデータパケットを追記する。
    Write(data []byte, pos uint32, cont bool)

    // SetInfo はチャンネルメタデータを更新する。
    SetInfo(info ChannelInfo)

    // SetTrack はトラック情報を更新する。
    SetTrack(track TrackInfo)
}
```

#### RTMP → FLV タグ変換

`yutopp/go-rtmp` は RTMP メッセージとして Video/Audio/Data を渡してくる。
各メッセージをそのまま FLV タグ形式に変換して扱う:

```
FLV タグ = 11バイトヘッダー + メッセージボディ + 4バイト後方ポインタ
```

| フィールド | サイズ | 内容 |
|:---|:---|:---|
| TagType | 1 byte | 8=Audio, 9=Video, 18=ScriptData |
| DataSize | 3 bytes | ボディのバイト数 (big-endian) |
| Timestamp | 3 bytes | ミリ秒タイムスタンプ (big-endian 下位 24 bit) |
| TimestampExt | 1 byte | タイムスタンプ上位 8 bit |
| StreamID | 3 bytes | 常に 0x000000 |
| Data | N bytes | RTMP メッセージボディそのまま |
| BackPointer | 4 bytes | タグ全体サイズ (11 + N) (big-endian) |

#### シーケンスヘッダーの検出

RTMP メッセージボディの先頭バイトで判定する。

| タグタイプ | 条件 | 意味 |
|:---|:---|:---|
| Video (9) | `body[0] == 0x17 && body[1] == 0x00` | AVC sequence header (AVCDecoderConfigurationRecord) |
| Audio (8) | `body[0] == 0xAF && body[1] == 0x00` | AAC sequence header (AudioSpecificConfig) |
| ScriptData (18) | AMF0 文字列 = `"onMetaData"` | メタデータ |

- `0x17`: FrameType=1 (keyframe) + CodecID=7 (AVC/H.264)
- `0xAF`: SoundFormat=10 (AAC) + SoundRate=3 + SoundSize=1 + SoundType=1

#### head パケットの組み立て

peercast-yt / PeerCastStation ともに同じ構造。シーケンスヘッダーが揃った時点で組み立て、`SetHeader` を呼ぶ:

```
[FLV ファイルヘッダー 13 bytes]
  "FLV" + version(1) + flags(0x05: hasVideo|hasAudio) + dataOffset(9)

[onMetaData タグ]   ← 存在する場合
  完全な FLV タグ形式。タイムスタンプは 0 に書き換える。

[AVC sequence header タグ]  ← 存在する場合
  完全な FLV タグ形式。タイムスタンプは 0 に書き換える。

[AAC sequence header タグ]  ← 存在する場合
  完全な FLV タグ形式。タイムスタンプは 0 に書き換える。
```

シーケンスヘッダーが再送されたとき (エンコーダー再接続等) は head パケットを更新し、既存の出力接続に再送する。

#### RTMP → PCP 変換

| RTMP メッセージ | 処理 |
|:---|:---|
| ScriptData `onMetaData` | `ChannelInfo.Bitrate` 等を更新。head パケットに含める |
| Video: `body[0]==0x17 && body[1]==0x00` | AVC sequence header として保存。head パケットを再組み立て |
| Audio: `body[0]==0xAF && body[1]==0x00` | AAC sequence header として保存。head パケットを再組み立て |
| Video keyframe (`body[0]==0x17` かつ sequence header 以外) | `Write(data, pos, cont=false)` |
| Video inter frame (`body[0]==0x27`) | `Write(data, pos, cont=true)` |
| Audio (sequence header 以外) | `Write(data, pos, cont=true)` |

`cont=false` (キーフレーム) を起点に新規視聴者へのストリーム配信を開始する。

---

### 4.2 ChannelInfo / TrackInfo

チャンネルのメタデータ。

```go
type ChannelInfo struct {
    Name    string // チャンネル名
    URL     string // コンタクト URL
    Desc    string // 説明
    Comment string // コメント
    Genre   string // ジャンル
    Type    string // コンテンツタイプ ("FLV", "MKV" など)
    MIMEType string // MIME タイプ ("video/x-flv" など)
    Ext     string // 拡張子 (".flv" など)
    Bitrate uint32 // ビットレート (kbps)
}

type TrackInfo struct {
    Title   string
    Creator string
    URL     string
    Album   string
}
```

---

### 4.3 ContentBuffer

ストリームデータを保持するバッファ。

#### データ構造

```go
type Content struct {
    Pos  uint32 // ストリーム内バイト位置
    Data []byte
    Cont bool   // 継続パケットフラグ (= キーフレームでない)
}

type ContentBuffer struct {
    header  []byte    // 最新のストリームヘッダー
    packets []Content // リングバッファ (固定サイズ)
    mu      sync.RWMutex
}
```

#### 設計方針

- `header`: `SetHeader` で上書き。新規接続時に必ず最初に送る。
- `packets`: 固定長リングバッファ (デフォルト 64 件)。満杯時は最古から上書き。
- 新規接続に対しては `header` → `cont=false` の最初のパケット以降を送信する (キーフレームスタート)。

#### インターフェース

```go
// SetHeader はストリームヘッダーを更新する。ヘッダー変化時に既存接続にも再送が必要。
func (b *ContentBuffer) SetHeader(data []byte)

// Write はデータパケットを追記する。
func (b *ContentBuffer) Write(data []byte, pos uint32, cont bool)

// Header は最新のヘッダーを返す。
func (b *ContentBuffer) Header() (data []byte, pos uint32)

// Since は指定位置以降のパケットを返す。
// pos が古すぎる場合は最古パケットから返す。
func (b *ContentBuffer) Since(pos uint32) []Content
```

---

### 4.4 Channel

```go
type Channel struct {
    ID          pcp.GnuID
    BroadcastID pcp.GnuID
    Info        ChannelInfo
    Track       TrackInfo
    Buffer      *ContentBuffer
    StartTime   time.Time

    mu          sync.RWMutex
    outputs     []OutputStream // 接続中の出力ストリーム
}
```

`outputs` への追加・削除は `mu` で保護する。

---

### 4.5 YPClient (COUT 接続)

#### 責務

YP (root server) に PCP コントロール接続 (COUT) を確立し、チャンネル情報を定期ブロードキャストする。

#### 接続フロー

```
1. TCP 接続 (YP のホスト:ポート)
2. "pcp\n" + version(1) 送信  ← pcp.NewConn が自動送信
3. helo 送信
     agnt = "AgentName/Version"
     ver  = 1218
     sid  = SessionID
     port = 7144 (リスニングポート)
     bcid = BroadcastID
4. oleh 受信 → RemoteIP を取得してグローバル IP を把握
5. root atom 受信 (任意)
     root.uint: 更新間隔 (秒)。デフォルト 120 秒として扱う
     root.upd: 即時更新要求 → bcst を今すぐ送信
6. ok 受信
7. bcst ループ (UpdateInterval 秒ごと)
8. チャンネル終了時: quit 送信
```

#### bcst の構造

```
bcst
  ttl  = 7
  hops = 0
  from = SessionID
  grp  = 0x03  (ROOT | TRACKERS)
  cid  = ChannelID
  vers = 1218
  chan
    id   = ChannelID
    bcid = BroadcastID
    info
      name = ChannelInfo.Name
      url  = ChannelInfo.URL
      desc = ChannelInfo.Desc
      cmnt = ChannelInfo.Comment
      gnre = ChannelInfo.Genre
      type = ChannelInfo.Type
      bitr = ChannelInfo.Bitrate
    trck
      titl = TrackInfo.Title
      crea = TrackInfo.Creator
      url  = TrackInfo.URL
      albm = TrackInfo.Album
  host
    id   = SessionID
    ip   = グローバル IP (oleh.rip から取得)
    port = 7144
    numl = 直接視聴接続数
    numr = リレー接続数
    uptm = 稼働秒数
    oldp = Buffer.OldestPos()
    newp = Buffer.NewestPos()
    cid  = ChannelID
    flg1 = RELAY | DIRECT | RECV | CIN
```

#### 再接続

接続が切れた場合は指数バックオフ (初期 5 秒、最大 120 秒) で再接続する。

---

### 4.6 OutputListener

#### 責務

ポート 7144 で TCP 待ち受け。先頭バイト列でプロトコルを識別し、適切な出力ストリームを生成する。

#### プロトコル識別

```
先頭バイト列                   生成するストリーム
─────────────────────────────────────────────────
"GET /channel/<id> HTTP"    → PCPOutputStream
"GET /stream/<id> HTTP"     → HTTPOutputStream
"pcp\n" (0x70 0x63 0x70 0x0a) → PingHandler (将来拡張)
```

識別には最初の 4〜16 バイトを peek する (接続は消費しない)。

#### アクセス制御

| 接続元 | 許可する OutputStreamType |
|:---|:---|
| ループバック (`127.0.0.1`) | Relay, Play |
| サイトローカル (`192.168.x.x` 等) | Relay, Play |
| グローバル IP | Relay, Play (制限なし、初期実装) |

---

### 4.7 PCPOutputStream (PCP リレー)

下流の PeerCast ノードへ PCP でストリームを送信する。

#### 接続受け付けフロー

```
1. HTTP GET /channel/<channel-id> を受け取る
   ヘッダー: x-peercast-pcp: 1
             x-peercast-pos: <再生開始位置>

2. "pcp\n" + version を受け取る

3. helo 受信・バリデーション
   - sid が自分の SessionID と一致 → quit(LOOPBACK)
   - sid がゼロ → quit(NOTIDENTIFIED)
   - ver < 1200 → quit(BADAGENT)
   - ping が含まれる場合 → 非同期で PingHost を実行 (2 秒タイムアウト)

4. oleh 送信
     agnt = "AgentName/Version"
     sid  = SessionID
     ver  = 1218
     rip  = 接続元のグローバル IP
     port = 接続元から見えたポート

5. HTTP 200 OK レスポンス送信
   Content-Type: application/x-peercast-pcp

6. 初期チャンネル情報送信
   chan
     id   = ChannelID
     info = (ChannelInfo 全フィールド)
     trck = (TrackInfo 全フィールド)
     pkt
       type = "head"
       pos  = Buffer.HeaderPos()
       data = Buffer.Header()

7. ストリームデータ送信ループ
   - ContentBuffer から未送信パケットを取り出して送信
   - 送信待ちがない場合は短時間 (50ms) スリープ
   - オーバーフロー検出: 5 秒以上キューが詰まったら接続を切る
   - ChannelInfo が更新された場合: chan > info を送信
   - TrackInfo が更新された場合: chan > trck を送信
   - bcst を受け取った場合: TTL デクリメント後に転送

8. 終了: quit 送信
```

#### bcst の転送ルール

受信した `bcst` を他の出力ストリームに転送するときのルール:

- `ttl` が 0 なら転送しない
- 転送時に `ttl -= 1`、`hops += 1`
- `from` が自分の SessionID なら転送しない (ループ防止)
- `dest` が特定の SessionID を指している場合は、そのノードにのみ転送

---

### 4.8 HTTPOutputStream (HTTP 直接視聴)

メディアプレイヤーへ HTTP でストリームを送信する。

#### フロー

```
1. HTTP GET /stream/<channel-id> を受け取る

2. チャンネルが配信中かつバッファにデータがあるか確認
   なければ: 503 Service Unavailable

3. HTTP 200 OK レスポンスを送信
   Content-Type: video/x-flv  (ChannelInfo.MIMEType)
   icy-name: <channel name>
   icy-genre: <genre>
   icy-url: <url>
   icy-bitrate: <bitrate>

4. Buffer.Header() を送信

5. キーフレームを起点にストリームデータを連続送信
   - cont=false のパケット (キーフレーム) から開始
   - 送信タイムアウト: 60 秒
   - ポーリング間隔: 200ms
```

---

## 5. 状態管理とライフサイクル

### 5.1 配信開始フロー

```
1. RTMPServer が接続を受け付ける
2. onMetaData から ChannelInfo を構築
3. Channel を作成 (ChannelID 生成)
4. YPClient を起動 (COUT 接続)
5. OutputListener を起動 (7144 待ち受け)
6. RTMP データを ContentBuffer に流し始める
```

### 5.2 配信終了フロー

```
1. RTMP 接続が切れる (または手動停止)
2. YPClient: quit 送信 → 接続を閉じる
3. 既存の PCPOutputStream / HTTPOutputStream: quit 送信 → 接続を閉じる
4. OutputListener: 停止
5. Channel を破棄
```

### 5.3 RTMP 切断・再接続時

エンコーダーが再接続した場合:
- `ContentBuffer.SetHeader()` を再度呼び出す (新しいヘッダー)
- 既存の出力接続に対して `chan.pkt.type=head` を再送する
- ストリーム位置は継続 (リセットしない)

---

## 6. 並行処理設計

| goroutine | 役割 |
|:---|:---|
| `RTMPServer.accept` | TCP accept ループ |
| `RTMPServer.handle` | 接続ごと 1 goroutine: RTMP 受信・デコード |
| `YPClient.run` | COUT 接続・bcst 送信ループ |
| `OutputListener.accept` | TCP accept ループ |
| `PCPOutputStream.send` | 接続ごと 1 goroutine: PCP 送信ループ |
| `HTTPOutputStream.send` | 接続ごと 1 goroutine: HTTP 送信ループ |

### Channel を介したデータ共有

```
RTMPServer ──(ContentSink)──→ ContentBuffer ←──(read)── 出力 goroutine 群
```

`ContentBuffer` の読み書きは `sync.RWMutex` で保護する。
出力 goroutine は位置ポインタを自分で持ち、`Buffer.Since(pos)` でポーリングする。

### 通知

ヘッダー更新・メタデータ更新は出力ストリームへ `chan` で通知する。
各出力 goroutine は専用の `chan struct{}` で wake-up を受ける。

---

## 7. エラー処理

| 状況 | 対応 |
|:---|:---|
| RTMP 接続切断 | ストリーム停止。YP へ quit。出力接続を全て閉じる |
| YP 接続切断 | 指数バックオフで再接続。出力接続は維持 |
| 出力ストリーム切断 | 該当接続のみ終了。他は継続 |
| 出力キュー詰まり (5 秒) | 該当接続を強制切断 |
| helo バリデーション失敗 | quit atom を送信して接続を閉じる |

---

## 8. 定数

| 定数名 | 値 | 説明 |
|:---|:---|:---|
| `DefaultPCPPort` | 7144 | PCP リスニングポート |
| `DefaultRTMPPort` | 1935 | RTMP リスニングポート |
| `PCPVersion` | 1218 | PCP プロトコルバージョン |
| `ContentBufferSize` | 64 | コンテンツバッファのパケット数 |
| `BcstTTL` | 7 | ブロードキャスト TTL |
| `YPRetryInitial` | 5s | YP 再接続初期待機時間 |
| `YPRetryMax` | 120s | YP 再接続最大待機時間 |
| `OutputQueueTimeout` | 5s | 出力キュー詰まり検出時間 |
| `DirectWriteTimeout` | 60s | HTTP 直接送信タイムアウト |
| `PingTimeout` | 2s | helo.ping FW 確認タイムアウト |
| `HostExpiry` | 180s | ノード情報の有効期限 |

---

## 9. peercast-pcp との対応

`peercast-pcp` ライブラリが提供する機能とこの実装での使用箇所の対応。

| peercast-pcp | 使用箇所 |
|:---|:---|
| `pcp.Conn` / `pcp.Dial` | YPClient の COUT 接続 |
| `pcp.NewConn` | OutputListener で accept した接続のラップ |
| `pcp.ReadAtom` / `Atom.Write` | 全 PCP 通信 |
| `pcp.HeloPacket` / `pcp.NewParentAtom` | helo / oleh 構築 |
| `pcp.BcstPacket` / `pcp.ChanPacket` | bcst / chan 構築 |
| `pcp.HostPacket` | YP への host 情報送信 |
| `pcp.GnuID` | SessionID / BroadcastID / ChannelID |
| `pcp.PCPHostFlags1*` 定数 | flg1 ビット設定 |
| `pcp.PCPBcstGroup*` 定数 | grp ビット設定 |
| `pcp.PCPErrorQuit` 等 | quit atom のエラーコード |

---

## 10. 未解決事項 (要調査)

- [x] RTMP ライブラリの選定 → **yutopp/go-rtmp** を使用する
- [x] FLV タグ → `head` / `data` 分割の詳細 → セクション 4.1 参照
- [ ] `ChannelID` 生成アルゴリズムの既存実装との互換性確認
- [ ] `x-peercast-pos` による途中参加時のストリーム位置決定ロジック
- [ ] push 接続 (firewalled ノード向け) の対応範囲
