# peercast-mm 実装仕様

RTMP で受け取ったストリームを PCP ネットワークに配信する、Go 製 PeerCast ブロードキャストノードの実装仕様。

コンポーネント別の詳細仕様は [spec-components.md](spec-components.md) を参照。

---

## 1. スコープ

### 対象

- RTMP サーバー (エンコーダーからの push 受信)
- PCP ブロードキャストノード (non-root、IsBroadcasting = true)
  - YP への COUT 接続・チャンネル登録
  - 下流 PeerCast ノードへの PCP リレー送信
  - 視聴クライアントへの HTTP 直接送信

### 対象外

- リレーノード (上流から受け取って中継する機能)
- Web UI
- push 接続 (firewalled ノード向け)

---

## 2. システム構成

```
エンコーダー
    │ RTMP push (ポート 1935)
    ▼
┌──────────────────────────────────────────────────────────┐
│ internal/rtmp — RTMPServer                               │
│  FLV タグを解析し Channel へ渡す                         │
└──────────────────┬───────────────────────────────────────┘
                   │ Channel.Write / SetHeader / SetInfo
                   ▼
┌──────────────────────────────────────────────────────────┐
│ internal/channel — Channel                               │
│  ┌─────────────────────┐  ┌─────────────────────────┐   │
│  │  ContentBuffer      │  │  ChannelInfo / TrackInfo │   │
│  │  (head + data 64件) │  │  (メタデータ)             │   │
│  └─────────────────────┘  └─────────────────────────┘   │
└──┬────────────────────────────────────────────────────┬──┘
   │ fan-out                                            │ 定期 bcst
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
       └─ pcp\n             → (将来: PING 応答等)
```

### パッケージ構成

| パッケージ | 役割 |
|:---|:---|
| `internal/rtmp` | RTMPServer |
| `internal/channel` | Channel、ContentBuffer、ChannelInfo/TrackInfo |
| `internal/servent` | Listener、PCPOutputStream、HTTPOutputStream |
| `internal/yp` | YPClient |
| `internal/id` | SessionID / BroadcastID / ChannelID 生成 |
| `internal/version` | バージョン定数 |
| `internal/config` | CLI フラグ・設定 |

---

## 3. 識別子

### 3.1 セッション ID (SessionID)

ノードの一意識別子。起動ごとにランダム生成する 16 バイトの GnuID。
PCP ハンドシェイクの `helo.sid` および `bcst.from`、`oleh.sid` として使用する。

### 3.2 ブロードキャスト ID (BroadcastID)

配信セッションの識別子。起動ごとにランダム生成する 16 バイトの GnuID。
`helo.bcid`、`bcst > chan.bcid` として使用する。

### 3.3 チャンネル ID (ChannelID)

チャンネルを識別する 16 バイトの GnuID。次の入力から決定論的に生成する:

```
input     = BroadcastID || NetworkType || ChannelName || Genre || SourceURL
ChannelID = MD5(SHA512(input))[:16]
```

NetworkType は固定値 `"ipv4"` とする。

> **未解決**: 既存実装 (peercast-yt 等) との互換性は未確認。→ [§10](#10-未解決事項)

---

## 4. ライフサイクル

### 4.1 配信開始フロー

```
1. エンコーダーが RTMP 接続を確立
2. onMetaData から ChannelInfo を構築 → Channel.SetInfo()
3. Channel を生成 (ChannelID 生成、ContentBuffer 初期化)
4. YPClient を起動 (COUT 接続・bcst ループ開始)
5. Listener を起動 (ポート 7144 待ち受け)
6. RTMP データを ContentBuffer に流し始める
```

### 4.2 配信終了フロー

```
1. RTMP 接続が切れる (または手動停止)
2. YPClient: quit 送信 → 接続を閉じる
3. 既存の PCPOutputStream: quit 送信 → 接続を閉じる
4. 既存の HTTPOutputStream: 接続を閉じる
5. Listener: 停止
6. Channel を破棄
```

### 4.3 RTMP 再接続時

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
| `YPClient.Run` | COUT 接続・bcst 送信ループ |
| `Listener` accept | TCP accept ループ |
| `PCPOutputStream.run` | 接続ごと: PCP 送信ループ |
| `HTTPOutputStream.run` | 接続ごと: HTTP 送信ループ |

### データ共有

```
RTMPServer ──(SetHeader/Write)──→ ContentBuffer ←──(Since/Header)── 出力 goroutine 群
```

- `ContentBuffer` の読み書きは `sync.RWMutex` で保護する
- 出力 goroutine は自前の `pos uint32` を持ち、`Buffer.Since(pos)` でポーリングする

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
| `ContentBufferSize` | 64 | コンテンツバッファのパケット数 |
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

---

## 9. 依存ライブラリ

| ライブラリ | 用途 |
|:---|:---|
| `github.com/titagaki/peercast-pcp` | PCP プロトコル層 |
| `github.com/yutopp/go-rtmp` | RTMP サーバー |
| `github.com/yutopp/go-amf0` | AMF0 デコード (onMetaData) |

---

## 10. 未解決事項と実装指示

peercast-yt (`_ref/peercast-yt/core/common/`) のコードリーディング結果を踏まえた実装方針。

---

### 10.1 ChannelID 生成アルゴリズムの互換性

**調査結果: 現在の実装 (SHA512+MD5) は peercast-yt と互換性がない。**

peercast-yt の実装 (`gnuid.cpp:24`, `servhs.cpp:2440`):

```
// id の初期値 = broadcastID
// 1. 各バイトに bitrate(uint8) を XOR
// 2. name と genre の文字を循環しながら XOR
//    (ループ回数 = ceil(max(len(name), len(genre)) / 16) * 16)
//    文字列末尾 (NUL) に達したらインデックスを 0 にリセットし、その回はXORしない
```

peercast-mm の現在の実装 (`id/id.go`):

```
input = BroadcastID || "ipv4" || Name || Genre || SourceURL
ChannelID = MD5(SHA512(input))[:16]
```

差異のまとめ:

| 項目 | peercast-yt | peercast-mm (現在) |
|:---|:---|:---|
| アルゴリズム | XOR ベース | SHA512 + MD5 |
| 入力パラメータ | BroadcastID, Name, Genre, Bitrate(uint8) | BroadcastID, "ipv4", Name, Genre, SourceURL |
| 再現性 | あり (同条件で同一ID) | あり |

**実装指示:**

`internal/id/id.go` の `ChannelID` 関数を peercast-yt 互換のアルゴリズムに書き換える。

```go
// ChannelID は peercast-yt と互換のアルゴリズムでチャンネル ID を生成する。
//
//   id の初期値 = broadcastID
//   1. 全バイトに bitrate(uint8) を XOR
//   2. name と genre の文字を循環しながら XOR
//      ループ回数 = (max(len(name), len(genre))/16 + 1) * 16
//      各文字列は末尾に達したらインデックスを 0 にリセットし、その回はXORしない
func ChannelID(broadcastID pcp.GnuID, name, genre string, bitrate uint32) pcp.GnuID {
    id := broadcastID

    // step 1: XOR with bitrate (lower 8 bits)
    b := byte(bitrate)
    for i := range id {
        id[i] ^= b
    }

    // step 2: XOR with name and genre cycling
    maxLen := len(name)
    if len(genre) > maxLen {
        maxLen = len(genre)
    }
    n := (maxLen/16 + 1) * 16
    s1, s2 := 0, 0
    for i := 0; i < n; i++ {
        ipb := id[i%16]
        if s1 < len(name) {
            ipb ^= name[s1]
            s1++
        } else {
            s1 = 0
        }
        if s2 < len(genre) {
            ipb ^= genre[s2]
            s2++
        } else {
            s2 = 0
        }
        id[i%16] = ipb
    }
    return id
}
```

呼び出し元 (`main.go` 等) では `id.ChannelID(broadcastID, info.Name, info.Genre, info.Bitrate)` のようにシグネチャを変更する。`SourceURL` は入力から除外し、`"ipv4"` も除外する。

> **補足**: peercast-yt には `randomizeBroadcastingChannelID` フラグがあり、セットされているとランダム ID を使う。peercast-mm では決定論的生成のみとし、このフラグは不要。

---

### 10.2 `x-peercast-pos` による途中参加時のストリーム位置決定ロジック

**調査結果: peercast-yt では `x-peercast-pos: <uint32>` をリクエストヘッダーで受け取り、送信開始位置を決定する。**

参照: `channel.cpp:443` (送信側)、`servent.cpp:560` (受信側)

現在の peercast-mm (`servent/pcp.go`) は HTTP リクエストを `req.Body.Close()` で読み捨てており、ヘッダーを全く参照していない。

**実装指示:**

`PCPOutputStream.handshake()` でリクエストヘッダーの `x-peercast-pos` を読み取り、`startPos` として返す (または構造体フィールドに保存する)。`streamLoop()` の初期 `pos` をこの値から設定する。

```go
// handshake の戻り値として startPos を追加するか、
// PCPOutputStream に startPos uint32 フィールドを追加する

func (o *PCPOutputStream) handshake() (startPos uint32, err error) {
    req, err := http.ReadRequest(o.br)
    if err != nil {
        return 0, fmt.Errorf("read HTTP request: %w", err)
    }
    _ = req.Body.Close()

    // x-peercast-pos ヘッダーを読み取る
    if v := req.Header.Get("x-peercast-pos"); v != "" {
        if n, err := strconv.ParseUint(v, 10, 32); err == nil {
            startPos = uint32(n)
        }
    }

    // ... 以降の helo/oleh 処理は変更なし
    return startPos, nil
}
```

`streamLoop()` での使用:

```go
func (o *PCPOutputStream) run() {
    startPos, err := o.handshake()
    // ...
    o.streamLoop(startPos)
}

func (o *PCPOutputStream) streamLoop(reqPos uint32) {
    // reqPos が Buffer の範囲内にあれば使用する。
    // 古すぎる場合 (OldestPos > reqPos) は OldestPos から開始。
    // 0 の場合はヘッダー位置から開始 (現在と同じ挙動)。
    _, hpos := o.ch.Buffer.Header()
    pos := hpos
    if reqPos > 0 {
        oldest := o.ch.Buffer.OldestPos()
        if reqPos >= oldest {
            pos = reqPos
        } else {
            pos = oldest
        }
    }
    // ... 以降のループは変更なし
}
```

> **注意**: `sendInitial()` で送る `chan > pkt(type=head)` の `pos` フィールドは `Buffer.HeaderPos()` のままでよい。これはストリームヘッダーの位置であり、データ送信開始位置とは独立している。

---

### 10.3 push 接続 (firewalled ノード向け) の対応範囲

**調査結果: peercast-yt の GIV プロトコル (`servent.cpp:418`, `980`) は「firewalled リレーノードへのアウトバウンド接続」であり、peercast-mm のスコープ外。**

peercast-mm はブロードキャストノード専用であり、下流のリレーノードは常に peercast-mm に TCP 接続してくる (peercast-mm がサーバー側)。そのため、下流へのアウトバウンド push 接続 (GIV) は不要。

**結論: 対象外で確定。対応不要。**

ただし peercast-mm 自身が NAT/ファイアウォール内にある場合の外部到達性確認は §10.4 の `helo.ping` で対処する。

---

### 10.4 `helo.ping` によるファイアウォール疎通確認の実装

**調査結果: peercast-yt では HELO に `ping` フィールド (= listen port) を含め、YP からの接続確認を受ける。**

参照: `servent.cpp:1020` (`writeHeloAtom`), `servent.cpp:1028-1029`

フロー:
1. YP への HELO に `ping = listenPort` を含める
2. YP は指定ポートへ `pcp\n` + HELO で接続してくる
3. peercast-mm は OLEH (sid = sessionID) を返して quit する
4. YP はこの OLEH を受けて oleh.rip が正しいかを確認

**実装指示:**

#### ① `buildHelo()` に `ping` フィールドを追加 (`yp/client.go`)

```go
func (c *Client) buildHelo() *pcp.Atom {
    return pcp.NewParentAtom(pcp.PCPHelo,
        pcp.NewStringAtom(pcp.PCPHeloAgent, version.AgentName),
        pcp.NewIntAtom(pcp.PCPHeloVersion, version.PCPVersion),
        pcp.NewIDAtom(pcp.PCPHeloSessionID, c.sessionID),
        pcp.NewShortAtom(pcp.PCPHeloPort, c.listenPort),
        pcp.NewShortAtom(pcp.PCPHeloPing, c.listenPort), // 追加
        pcp.NewIDAtom(pcp.PCPHeloBCID, c.broadcastID),
    )
}
```

`pcp.PCPHeloPing` 定数は `peercast-pcp` ライブラリで `"ping"` として定義されていることを確認すること。定義がない場合は `pcp.NewID4("ping")` で代替する。

#### ② `Listener` で `pcp\n` 接続を処理 (`servent/listener.go`)

現在は `pcp\n` 接続を即切断している。YP からのファイアウォール確認を受け付けるよう変更する。

```go
case startsWith(peek, "pcp\n"):
    log.Printf("servent: ping connection from %s", conn.RemoteAddr())
    go handlePing(conn, br, l.sessionID)
```

```go
// handlePing は YP からのファイアウォール疎通確認接続を処理する。
// "pcp\n" + helo を受け取り、oleh を返して quit する。
func handlePing(conn net.Conn, br *bufio.Reader, sessionID pcp.GnuID) {
    defer conn.Close()

    // "pcp\n" の 12 バイト (tag + length + version) を読み捨てる
    magic := make([]byte, 12)
    if _, err := io.ReadFull(br, magic); err != nil {
        return
    }

    // helo を読む
    heloAtom, err := pcp.ReadAtom(br)
    if err != nil || heloAtom.Tag != pcp.PCPHelo {
        return
    }

    // oleh を送信
    oleh := pcp.NewParentAtom(pcp.PCPOleh,
        pcp.NewStringAtom(pcp.PCPHeloAgent, version.AgentName),
        pcp.NewIDAtom(pcp.PCPHeloSessionID, sessionID),
        pcp.NewIntAtom(pcp.PCPHeloVersion, version.PCPVersion),
    )
    if err := oleh.Write(conn); err != nil {
        return
    }

    // quit を送信
    pcp.NewIntAtom(pcp.PCPQuit, pcp.PCPErrorQuit+pcp.PCPErrorShutdown).Write(conn)
}
```

> **注意**: `helo.ping` への応答では `rip` (remote IP) は含めなくてよい。YP は sessionID が一致することだけを確認する。ファイアウォール状態の管理 (FW_UNKNOWN / FW_OFF / FW_ON の状態機械) は今回のスコープ外とし、`flg1` のフラグ更新も将来課題とする。

---

### 10.5 リスナー数・リレー数の正確なカウント (`numl` / `numr`)

**調査結果: peercast-yt では `T_DIRECT` 型 (HTTP 視聴) = numl、`T_RELAY` 型 (PCP リレー) = numr としてカウントする。**

参照: `channel.cpp:256-262` (`localRelays`, `localListeners`), `servmgr.cpp:1689` (`numStreams`)

現在の peercast-mm は `Channel.outputs []OutputStream` で型の区別がなく、`buildBcst()` で `0` をハードコードしている。

**実装指示:**

#### ① `Channel` に numl/numr カウンタを追加 (`channel/channel.go`)

```go
type Channel struct {
    // ... 既存フィールド
    numListeners int // HTTPOutputStream の数
    numRelays    int // PCPOutputStream の数
}

func (c *Channel) NumListeners() int {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.numListeners
}

func (c *Channel) NumRelays() int {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.numRelays
}
```

#### ② `OutputStream` インターフェースに種別メソッドを追加

```go
type OutputStreamType int

const (
    OutputStreamPCP  OutputStreamType = iota // PCPOutputStream (リレーノード)
    OutputStreamHTTP                         // HTTPOutputStream (視聴プレイヤー)
)

type OutputStream interface {
    NotifyHeader()
    NotifyInfo()
    NotifyTrack()
    Close()
    Type() OutputStreamType // 追加
}
```

`PCPOutputStream.Type()` は `OutputStreamPCP` を返す。`HTTPOutputStream.Type()` は `OutputStreamHTTP` を返す。

#### ③ `AddOutput` / `RemoveOutput` でカウンタを更新

```go
func (c *Channel) AddOutput(o OutputStream) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.outputs = append(c.outputs, o)
    switch o.Type() {
    case OutputStreamPCP:
        c.numRelays++
    case OutputStreamHTTP:
        c.numListeners++
    }
}

func (c *Channel) RemoveOutput(o OutputStream) {
    c.mu.Lock()
    defer c.mu.Unlock()
    for i, out := range c.outputs {
        if out == o {
            c.outputs = append(c.outputs[:i], c.outputs[i+1:]...)
            switch o.Type() {
            case OutputStreamPCP:
                c.numRelays--
            case OutputStreamHTTP:
                c.numListeners--
            }
            return
        }
    }
}
```

#### ④ `buildBcst()` でカウンタを参照 (`yp/client.go`)

```go
pcp.NewIntAtom(pcp.PCPHostNumListeners, uint32(c.ch.NumListeners())),
pcp.NewIntAtom(pcp.PCPHostNumRelays,    uint32(c.ch.NumRelays())),
```
