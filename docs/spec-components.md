# peercast-mi コンポーネント仕様

[spec.md](spec.md) のシステム概要・ライフサイクル・並行処理設計を前提とする。

---

## 4.1 RTMPServer

### 使用ライブラリ

`github.com/yutopp/go-rtmp` を使用する。`gortmp.NewServer` でサーバーを立て、`gortmp.Handler` インターフェースを実装して FLV タグを受け取る。

### 責務

- ポート 1935 で TCP 待ち受け
- RTMP ハンドシェイク処理 (go-rtmp に委譲)
- `OnPublish` でストリームキーを検証し、未発行キーの接続を拒否
- FLV タグ (Video / Audio / ScriptData) のストリームへの変換
- チャンネルメタデータ (`onMetaData`) からの `ChannelInfo` 更新

### ストリームキー認証

`OnPublish(cmd *message.NetStreamPublish)` コールバックで認証を行う。

```
cmd.PublishingName = "sk_a1b2c3..."  ← OBS の「ストリームキー」欄
```

- `Manager.IsIssuedKey(key)` が `false` → エラーを返して接続拒否
- `true` → 接続を受け付け、`handler.streamKey` に記録

`broadcastChannel` が呼ばれるまでの間、データは `Manager.GetByStreamKey()` が `nil` を返すため静かにドロップされる。

### FLV タグ形式

RTMP メッセージを FLV タグ形式に変換して扱う。

| フィールド | サイズ | 内容 |
|:---|:---|:---|
| TagType | 1 byte | 8=Audio / 9=Video / 18=ScriptData |
| DataSize | 3 bytes | ボディのバイト数 (big-endian) |
| Timestamp | 3 bytes | ミリ秒タイムスタンプ (big-endian 下位 24 bit) |
| TimestampExt | 1 byte | タイムスタンプ上位 8 bit |
| StreamID | 3 bytes | 常に `0x000000` |
| Data | N bytes | RTMP メッセージボディ |
| BackPointer | 4 bytes | タグ全体サイズ `11 + N` (big-endian) |

### シーケンスヘッダーの検出

RTMP メッセージボディの先頭バイトで判定する。

| タグタイプ | 条件 | 意味 |
|:---|:---|:---|
| Video (9) | `body[0] == 0x17 && body[1] == 0x00` | AVC sequence header (AVCDecoderConfigurationRecord) |
| Audio (8) | `body[0] == 0xAF && body[1] == 0x00` | AAC sequence header (AudioSpecificConfig) |
| ScriptData (18) | AMF0 文字列 = `"onMetaData"` | メタデータ |

- `0x17`: FrameType=1 (keyframe) + CodecID=7 (AVC/H.264)
- `0xAF`: SoundFormat=10 (AAC) + SoundRate=3 + SoundSize=1 + SoundType=1

### head パケットの組み立て

AVC または AAC シーケンスヘッダーが揃った時点で組み立て、`Channel.SetHeader()` を呼ぶ。

```
[FLV ファイルヘッダー 13 bytes]
  "FLV" + version(0x01) + flags(0x05: hasVideo|hasAudio) + dataOffset(9) + PreviousTagSize0(0)

[onMetaData タグ]            ← 存在する場合のみ。タイムスタンプは 0 に書き換え
[AVC sequence header タグ]   ← 存在する場合のみ。タイムスタンプは 0 に書き換え
[AAC sequence header タグ]   ← 存在する場合のみ。タイムスタンプは 0 に書き換え
```

各タグには 4 バイトの BackPointer (タグサイズ) を後置する。

シーケンスヘッダーが再送されたとき (エンコーダー再接続等) は head パケットを更新し、接続中の全出力ストリームに `NotifyHeader()` で通知する。

### RTMP → Channel マッピング

| RTMP メッセージ | 処理 |
|:---|:---|
| ScriptData `onMetaData` | AMF0 をデコードして `Bitrate`、`Type/MIMEType/Ext` を更新。head パケットに含める |
| Video `body[0]==0x17 && body[1]==0x00` | AVC sequence header として保存。head を再組み立て |
| Audio `body[0]==0xAF && body[1]==0x00` | AAC sequence header として保存。head を再組み立て |
| Video keyframe `body[0]==0x17` (上記以外) | `Channel.Write(data, pos, 0x00)` |
| Video inter frame `body[0]==0x27` | `Channel.Write(data, pos, 0x02)` |
| Audio (sequence header 以外) | `Channel.Write(data, pos, 0x04)` |

ContFlags は PeerCastStation 互換のビットフラグ:
- `0x00`: キーフレーム (None)
- `0x02`: 映像非キーフレーム (InterFrame)
- `0x04`: 音声パケット (AudioFrame)

キーフレーム (`ContFlags == 0x00`) を起点に新規視聴者へのストリーム配信を開始する。

---

## 4.2 ChannelInfo / TrackInfo

チャンネルのメタデータ。

```go
type ChannelInfo struct {
    Name     string // チャンネル名
    URL      string // コンタクト URL
    Desc     string // 説明
    Comment  string // コメント
    Genre    string // ジャンル
    Type     string // コンテンツタイプ ("FLV" など)
    MIMEType string // MIME タイプ ("video/x-flv" など)
    Ext      string // 拡張子 (".flv" など)
    Bitrate  uint32 // ビットレート (kbps)
}

type TrackInfo struct {
    Title   string
    Creator string
    URL     string
    Album   string
}
```

---

## 4.3 ContentBuffer

ストリームデータを保持するバッファ。

### データ構造

```go
type Content struct {
    Pos       uint32 // ストリーム内バイト位置
    Data      []byte
    ContFlags byte   // PeerCastStation 互換ビットフラグ (0x00=None, 0x02=InterFrame, 0x04=AudioFrame)
}

type ContentBuffer struct {
    header    []byte                    // 最新のストリームヘッダー
    headerPos uint32                    // ヘッダーのストリーム位置
    packets   []Content                 // リングバッファ (サイズはビットレートから自動計算)
    count     int                       // 書き込み総数
    mu        sync.RWMutex
}
```

### 設計方針

- `header`: `SetHeader` で上書き。新規接続時に必ず最初に送る
- `packets`: リングバッファ。サイズはビットレートと `content_buffer_seconds` 設定 (デフォルト 8 秒) から自動計算。最小 64 パケット。満杯時は最古から上書き
- 新規接続は `header` を送信後、`ContFlags` に `InterFrame (0x02)` が含まれない最初のパケット (= キーフレーム) 以降を送る

### インターフェース

```go
// SetHeader はストリームヘッダーを更新する。
func (b *ContentBuffer) SetHeader(data []byte)

// Write はデータパケットを追記する。
func (b *ContentBuffer) Write(data []byte, pos uint32, contFlags byte)

// Header は最新のヘッダーとその位置を返す。
func (b *ContentBuffer) Header() (data []byte, pos uint32)

// OldestPos は最古パケットのストリーム位置を返す。バッファ空の場合は 0。
func (b *ContentBuffer) OldestPos() uint32

// NewestPos は最新パケットのストリーム位置を返す。バッファ空の場合は 0。
func (b *ContentBuffer) NewestPos() uint32

// Since は指定位置以降のパケットを返す。
// pos が古すぎる場合は最古パケットから返す。空の場合は nil。
func (b *ContentBuffer) Since(pos uint32) []Content

// HasData はバッファにパケットが 1 件以上あるか返す。
func (b *ContentBuffer) HasData() bool
```

---

## 4.4 Channel

```go
type OutputStreamType int

const (
    OutputStreamPCP  OutputStreamType = iota // PCPOutputStream (下流リレーノード)
    OutputStreamHTTP                         // HTTPOutputStream (視聴プレイヤー)
)

// OutputStream は PCPOutputStream と HTTPOutputStream の共通インターフェース。
type OutputStream interface {
    NotifyHeader() // ストリームヘッダー変化時に呼ばれる
    NotifyInfo()   // ChannelInfo 変化時に呼ばれる
    NotifyTrack()  // TrackInfo 変化時に呼ばれる
    Close()        // 接続を終了する
    Type() OutputStreamType // PCP / HTTP の種別
    ID() int                // Listener が払い出す接続ID
    RemoteAddr() string     // 接続元アドレス ("host:port")
    SendRate() int64        // 直近 1 秒間の送信バイト数
}

// BcstForwarder は PCP 出力ストリームのみが実装するインターフェース。
// bcst アトム転送とループ防止用のピア識別に使用する。
// HTTPOutputStream はこのインターフェースを実装しない。
type BcstForwarder interface {
    SendBcst(atom *pcp.Atom) // bcst アトムを下流に転送
    PeerID() pcp.GnuID      // リモートピアのセッション ID
}

// ConnectionInfo は接続中の出力ストリームのスナップショット。
type ConnectionInfo struct {
    ID         int
    Type       OutputStreamType
    RemoteAddr string
    SendRate   int64
}

type Channel struct {
    ID        pcp.GnuID
    buffer    *ContentBuffer // private: 委譲メソッド経由でアクセス
    StartTime time.Time

    mu             sync.RWMutex
    broadcastID    pcp.GnuID
    isBroadcasting bool   // true = RTMP ソース、false = リレー
    source         string // 表示用ソース文字列
    upstreamAddr   string // リレーチャンネルの上流 host:port
    info           ChannelInfo
    track          TrackInfo
    outputs        []OutputStream
    numListeners   int // HTTPOutputStream の数
    numRelays      int // PCPOutputStream の数
}
```

`outputs` への追加・削除は `mu` で保護する。`AddOutput` / `RemoveOutput` 時に `numListeners` / `numRelays` を更新する。

`broadcastID` はブロードキャストチャンネルでは Manager の broadcastID を使用する。リレーチャンネルでは初期値ゼロで生成し、上流から受け取った `chan.bcid` で `SetBroadcastID()` により上書きされる。

`ContentBuffer` は private フィールド `buffer` として保持する。外部からは `Channel` の委譲メソッド (`HasData`, `Header`, `Signal`, `Since`, `OldestPos`, `NewestPos`, `Write`, `SetHeader`) 経由でアクセスする。

`Broadcast` メソッドは `OutputStream` を `BcstForwarder` に型アサーションし、bcst アトム転送を行う。`BcstForwarder` を実装しない HTTPOutputStream はスキップされる。同一ピア（同じ `PeerID()`）の別接続にも転送しない（ループ防止）。

### メソッド

```go
func (c *Channel) BroadcastID() pcp.GnuID
func (c *Channel) SetBroadcastID(id pcp.GnuID)
func (c *Channel) IsBroadcasting() bool
func (c *Channel) Source() string
func (c *Channel) SetSource(s string)
func (c *Channel) UpstreamAddr() string
func (c *Channel) SetUpstreamAddr(addr string)
func (c *Channel) Info() ChannelInfo
func (c *Channel) Track() TrackInfo
func (c *Channel) SetInfo(info ChannelInfo)   // 更新後に全 outputs へ NotifyInfo()
func (c *Channel) SetTrack(track TrackInfo)   // 更新後に全 outputs へ NotifyTrack()
func (c *Channel) SetHeader(data []byte)      // buffer 更新後に全 outputs へ NotifyHeader()
func (c *Channel) Write(data []byte, pos uint32, contFlags byte)
// ContentBuffer 委譲メソッド
func (c *Channel) HasData() bool
func (c *Channel) Header() ([]byte, uint32)
func (c *Channel) Signal() <-chan struct{}
func (c *Channel) Since(pos uint32) []Content
func (c *Channel) OldestPos() uint32
func (c *Channel) NewestPos() uint32
// OutputStream 管理
func (c *Channel) AddOutput(o OutputStream)
func (c *Channel) TryAddOutput(o OutputStream, maxRelays, maxListeners int) bool
func (c *Channel) RemoveOutput(o OutputStream)
func (c *Channel) NumListeners() int
func (c *Channel) NumRelays() int
func (c *Channel) IsRelayFull(maxRelays int) bool
func (c *Channel) IsDirectFull(maxListeners int) bool
func (c *Channel) CloseAll()                  // 全接続に Close() を呼ぶ
func (c *Channel) UptimeSeconds() uint32
func (c *Channel) Connections() []ConnectionInfo
func (c *Channel) CloseConnection(id int) bool
func (c *Channel) Broadcast(from OutputStream, atom *pcp.Atom) // BcstForwarder 型アサーションで転送
```

---

## 4.5 RelayClient (上流 PCP 接続)

上流 PeerCast ノードへ PCP でストリームを受信し、Channel に書き込む。

### 接続フロー

`connect()` が TCP 接続を確立し、`handshake()` がプロトコルハンドシェイクを処理する。

```
connect():
  1. TCP 接続 (upstreamAddr = "host:port")
  2. handshake() を呼び出し

handshake():
  1. HTTP GET /channel/<channelIdHex> HTTP/1.0 を送信
  2. helo アトム送信
       agnt = "peercast-mi/<version>"
       sid  = SessionID
       ver  = 1218
       port = listenPort
     ※ pcp\n magic は HTTP-upgraded /channel/ リクエストでは送信しない
  3. HTTP/1.0 200 OK レスポンス (ヘッダー部のみ) を読み捨て
  4. oleh アトム受信

connect() (続き):
  3. bcstHostLoop goroutine を起動 (定期的に BCST HOST アトムを上流に送信)
  4. receiveLoop() でストリームデータ受信
     - chan > pkt(type="head") → Channel.SetHeader()
     - chan > pkt(type="data") → Channel.Write()
     - chan > info / trck → Channel.SetInfo() / Channel.SetTrack()
     - host アトム → リレーホスト候補として収集 (503 再接続用)
     - quit アトム受信 → 接続切断・再接続
  5. タイムアウト (60 秒) で quit なく無音 → 接続切断・再接続
```

### 再接続

接続失敗・切断時は指数バックオフ (初期 5 秒、最大 120 秒) で再接続する。`Stop()` が呼ばれると再接続ループを終了する。

Channel オブジェクトは再接続をまたいで維持されるため、下流の PCP リレー接続・HTTP 視聴接続は継続する。ただし、ヘッダーが再送されるまでの間は下流ノードは待機状態になる。

### API

```go
func New(upstreamAddr string, channelID, sessionID pcp.GnuID, listenPort uint16, ch *channel.Channel) *Client
func (c *Client) Run()            // 再接続ループ。goroutine として呼ぶ
func (c *Client) Stop()           // 停止シグナルを送り、Run() の終了を待つ
func (c *Client) SetGlobalIP(ip uint32) // YP から取得した globalIP を設定
```

---

## 4.6 YPClient (COUT 接続)

YP (root server) に PCP コントロール接続 (COUT) を確立し、チャンネル情報を定期ブロードキャストする。

### 接続フロー

```
1. TCP 接続 (YP のホスト:ポート)
2. "pcp\n" アトム + バージョン送信  ← pcp.Dial が自動処理
3. helo 送信
     agnt = "peercast-mi/<version>"
     ver  = 1218
     sid  = SessionID
     port = 7144
     bcid = BroadcastID
4. oleh 受信 → rip から globalIP を取得
5. root アトム受信 (任意)
     root.uint: 更新間隔 (秒)。受信した値で updateInterval を上書き
     root.upd : 即時更新要求 → 次の bcst 送信を前倒し
6. ok 受信 → ハンドシェイク完了
7. 初回 bcst 送信
8. updateInterval ごとに bcst を繰り返し送信
9. 配信終了時: quit(QUIT+SHUTDOWN) 送信
```

### bcst の構造

```
bcst
  ttl  = 7
  hops = 0
  from = SessionID
  grp  = 0x01  (ROOT のみ)
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
    ip   = globalIP  (oleh.rip から取得)
    port = 7144
    numl = Channel.NumListeners()
    numr = Channel.NumRelays()
    uptm = 稼働秒数
    oldp = Channel.OldestPos()
    newp = Channel.NewestPos()
    cid  = ChannelID
    flg1 = TRACKER | RELAY | DIRECT | RECV | CIN  (0x37)
    trkr = 1
    ver  = 1218
    vevp = 27
    vexp = "MM"
    vexn = <バージョン番号>
```

### 再接続

接続が切れた場合は指数バックオフ (初期 5 秒、最大 120 秒) で再接続する。

---

## 4.7 Listener

### 責務

ポート 7144 で TCP 待ち受け。先頭バイト列でプロトコルを識別し、適切な出力ストリームを生成する。

### プロトコル識別

先頭 16 バイトを peek して判定する (接続は消費しない)。

| 先頭バイト列 | 処理 |
|:---|:---|
| `"GET /channel/"` | PCPOutputStream を生成 |
| `"GET /stream/"` | HTTPOutputStream を生成 |
| `"pcp\n"` (0x70 0x63 0x70 0x0a) | `handlePing()` — YP ファイアウォール疎通確認 |
| `"POST /api"` | JSON-RPC API ハンドラーへ転送 |
| その他 | 不明プロトコル → 切断 |

---

## 4.8 PCPOutputStream (PCP リレー)

下流 PeerCast ノードへ PCP でストリームを送信する。

### 接続受け付けフロー

`run()` が全体フローを制御し、`handshake()` / `sendInitial()` / `streamLoop()` に分離されている。

```
run():
  1. handshake() — ハンドシェイク処理
  2. sendInitial() — 初期チャンネル情報送信
  3. readLoop() goroutine を起動 (下流からの bcst/quit 読み取り)
  4. streamLoop() — ストリームデータ送信ループ

handshake():
  1. HTTP GET /channel/<channel-id> を受け取る
     (x-peercast-pcp: 1, x-peercast-pos: <位置> などのヘッダーを含む場合がある)
  2. HTTP/1.0 200 OK レスポンス送信
     Content-Type: application/x-peercast-pcp
     ※ PCP over HTTP では pcp\n マジックは送受信しない
  3. helo アトム受信・バリデーション
     - sid == 自分の SessionID → quit(QUIT+LOOPBACK)
     - sid == ゼロ             → quit(QUIT+NOTIDENTIFIED)
     - ver < 1200             → quit(QUIT+BADAGENT)
  4. ping (ファイアウォール疎通確認) — helo に ping フィールドがあれば実行
  5. oleh アトム送信
       agnt = "peercast-mi/<version>"
       sid  = SessionID
       ver  = 1218
       rip  = 接続元の IPv4 アドレス (uint32, big-endian で組み立て)
       port = ping 成功時のポート番号

sendInitial():
  1. chan アトム送信
       id   = ChannelID
       bcid = BroadcastID
       info / trck / pkt(type="head")
  2. host アトム送信 (pcputil.BuildHostAtom で構築)

streamLoop():
  1. drainNotifications() — 非ブロッキングで info/track/header/bcst 通知を処理
  2. sendDataPackets() — Channel.Since(pos) で未送信パケットを送信
  3. 送信待ちがなければ Signal() / 通知チャネル / stallTimer を select
     - 5 秒以上データが送れない場合 → 接続を切断
     - infoCh 通知時: chan > info を送信
     - trackCh 通知時: chan > trck を送信
     - headerCh 通知時: chan > pkt(type=head) を送信
     - bcstCh: bcst アトムを下流に転送
  4. 終了: quit(QUIT+SHUTDOWN) 送信
```

### data パケットの形式

```
chan
  id  = ChannelID
  pkt
    type = "data"
    pos  = <バイト位置>
    data = <FLV タグバイト列>
    cont = PeerCastStation 互換ビットフラグ (0x00=キーフレーム, 0x02=InterFrame, 0x04=AudioFrame)
```

### bcst 転送ルール

受信した `bcst` を転送する際のルール:

- `ttl` が 0 なら転送しない
- 転送時に `ttl -= 1`、`hops += 1`
- `from` が自分の SessionID なら転送しない (ループ防止)
- `dest` が特定の SessionID を指す場合はそのノードにのみ転送

---

## 4.9 HTTPOutputStream (HTTP 直接視聴)

メディアプレイヤーへ HTTP でストリームを送信する。

### フロー

```
1. HTTP GET /stream/<channel-id> を受け取る

2. Channel.HasData() を確認
   データなし → Signal() で最大 30 秒待機、タイムアウトなら終了

3. HTTP/1.0 200 OK レスポンス送信
   Content-Type: <ChannelInfo.MIMEType> (デフォルト "video/x-flv")
   icy-name:    sanitizeHeaderValue(ChannelInfo.Name)
   icy-genre:   sanitizeHeaderValue(ChannelInfo.Genre)
   icy-url:     sanitizeHeaderValue(ChannelInfo.URL)
   icy-bitrate: <ChannelInfo.Bitrate>
   ※ sanitizeHeaderValue() で CR/LF を除去し HTTP ヘッダーインジェクションを防止

4. Channel.Header() を送信

5. キーフレームを起点にストリームデータを連続送信
   - ContFlags != 0 のパケット (= 非キーフレーム) をスキップ
   - キーフレーム以降は全パケットを順次送信
   - 書き込みタイムアウト: 60 秒 (パケットごとに更新)
   - Channel.Signal() でデータ到着を待機
```

> **注意**: HTTPOutputStream は `BcstForwarder` インターフェースを実装しない。
> メタデータ通知 (`infoCh`、`trackCh`、`headerCh`) も受け取らない。
> ICY メタデータ (`icy-metaint`) は現在未実装。

---

## 4.10 JSON-RPC API (`internal/jsonrpc`)

`POST /api/1` で JSON-RPC 2.0 リクエストを受け付ける管理 API。
`Listener` が `POST /api` 接続を検出して `jsonrpc.Server.Handler()` に転送する。

`jsonrpc.Server` は具象型 `*channel.Manager` ではなく `ChannelManager` インターフェースに依存する。これによりテスト時のモックが容易になる。

```go
type ChannelManager interface {
    IssueStreamKey(accountName, streamKey string) error
    RevokeStreamKey(accountName string) bool
    Broadcast(streamKey string, info channel.ChannelInfo, track channel.TrackInfo) (*channel.Channel, error)
    Stop(channelID pcp.GnuID) bool
    GetByID(channelID pcp.GnuID) (*channel.Channel, bool)
    StreamKeyByID(channelID pcp.GnuID) (string, bool)
    List() []*channel.Channel
}
```

### アクセス制御

`isLocalhost()` でリモートアドレスを検査し、ループバックアドレス以外からのリクエストには Basic 認証 (`admin_user` / `admin_pass`) を要求する。

API の詳細仕様 (メソッド一覧・リクエスト/レスポンス形式・フィールド説明) は [docs/api/jsonrpc.md](api/jsonrpc.md) を参照。
