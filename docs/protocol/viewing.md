# PeerCast チャンネル視聴プロトコル技術仕様書

## 1. 概要

PeerCast における「チャンネル視聴」とは、メディアプレイヤーがローカルの PeerCast ノードに HTTP で接続し、PeerCast ノードがリレーネットワーク（PCP プロトコル）を通じてストリームを取得・転送するプロセスである。

視聴には2種類のプロトコル経路がある:

- **直接視聴 (T_DIRECT)**: プレイヤー → PeerCast ノード (HTTP) → チャンネル本体 (PCP relay)
- **PCP リレー視聴 (T_RELAY)**: PeerCast ノード同士が PCP で接続し、下流ノードへ転送

### 1.1 通信アーキテクチャ

```
メディアプレイヤー          PeerCastノード (ローカル)        上流リレーノード         Tracker/配信元
        |                          |                              |                     |
        |-- HTTP GET /stream/ -->  |                              |                     |
        |                          |-- HTTP GET /channel/ ------> |                     |
        |                          |   (X-Peercast-Pcp: 1)       |                     |
        |                          |                              |-- PCP HELO -------> |
        |                          |                              |<-- PCP OLEH ------- |
        |                          |<- HTTP 200 + OLEH ---------- |                     |
        |                          |<- chan (info/track/pkt) ---- |                     |
        |<- HTTP 200 + headers --- |                              |                     |
        |<- stream data ---------- |<- chan.pkt (data) ---------- |                     |
        |         ...              |         ...                  |                     |
        |                          |<------- bcst (host info) --> |  (リレー経路情報の伝播)
```

---

## 2. フェーズ1: プレイヤーからのリクエスト

### 2.1 HTTP リクエスト

メディアプレイヤーは PeerCast ノード (デフォルト: `localhost:7144`) に HTTP GET を発行する。

```
GET /stream/<channel-id> HTTP/1.0
Host: localhost:7144
icy-metadata: 1
x-peercast-pos: <再生位置>
User-Agent: <エージェント文字列>
```

| ヘッダー | 用途 |
|:---|:---|
| `/stream/<id>` | 直接視聴 (HTTP ストリーム) |
| `/channel/<id>` | PCP リレー接続 (他 PeerCast ノードから) |
| `x-peercast-pos` | ストリーム内の再生開始位置 (バイト) |
| `icy-metadata: 1` | ICY メタデータ付き配信を要求 (MP3) |

**ソース: `servhs.cpp`**
```cpp
else if (strncmp(fn, "/stream/", 8) == 0)
{
    if (!sock->host.isLocalhost())
        if (!isAllowed(ALLOW_DIRECT) || !isFiltered(ServFilter::F_DIRECT))
            throw HTTPException(HTTP_SC_UNAVAILABLE, 503);
    triggerChannel(fn+8, ChanInfo::SP_HTTP, isPrivate() || hasValidAuthToken(fn+8));
}
```

### 2.2 アクセス制御

`/stream/` エンドポイントはローカルホスト (`localhost`) または適切な認証トークンを持つ接続のみ許可される。

---

## 3. フェーズ2: チャンネルの探索とリレー確立

### 3.1 チャンネルの状態確認

PeerCast ノードは受信した要求に対して `handshakeStream()` を呼び出し、チャンネルが既にローカルで利用可能かを確認する。

**ソース: `servent.cpp`**
```cpp
bool Servent::handshakeStream(ChanInfo &chanInfo)
{
    // チャンネルが既にある場合はストリーム位置を決定
    auto ch = chanMgr->findChannelByID(chanInfo.id);
    if (ch)
    {
        if (reqPos)
            streamPos = ch->rawData.findOldestPos(reqPos);  // 指定位置から
        else
            streamPos = ch->rawData.getLatestPos();          // 最新データから

        chanReady = canStream(ch, &reason);
        if (chanReady) setStatus(S_CONNECTED);
    }
    ...
}
```

チャンネルが利用不可の場合、`503 Service Unavailable` を返して代替リレーの `HOST` atom リストを送信する（→ [3.2 代替リレーの案内](#32-代替リレーの案内)）。

### 3.2 代替リレーの案内 (チャンネル不在時)

チャンネルがない場合、ノードは既知のリレー情報を返す。

```
HTTP/1.0 503 Service Unavailable
Content-Type: application/x-peercast-pcp

[PCP: oleh]          ← PCP ハンドシェイク応答
[PCP: host] × N      ← 代替リレーノードのリスト
[PCP: quit]          ← 接続終了
```

リクエスト元は受け取った `HOST` atom を使い、別ノードに接続を試みる。

### 3.3 上流リレーへの接続 (チャンネル取得)

ローカルノードがチャンネルを持っていない場合、`ChanHitList` から最適なリレーを探し、PCP で接続する。

```
[ローカルノード]                    [上流リレーノード]
        |                                    |
        |-- HTTP GET /channel/<id> --------> |
        |   Host: <upstream-ip>:7144         |
        |   x-peercast-pcp: 1               |
        |                                    |
        |-- [4バイト] "pcp\n"             -> |  ← PCP 識別子
        |-- [4バイト] データ長 (4)        -> |
        |-- [4バイト] バージョン (1)      -> |
        |                                    |
```

---

## 4. フェーズ3: PCP ハンドシェイク

### 4.1 HELO (接続開始)

接続ノードが上流ノードに送信する識別パケット。

**ソース: `servent.cpp` - `writeHeloAtom()`**
```
[PCP: helo]                       親atom (子atom数: 3〜6)
  [agnt] = "PeerCast/0.1218 ..."  エージェント文字列
  [ver]  = 1218                   プロトコルバージョン (int32)
  [sid]  = <16バイト>             セッションID (GnuID, ユニーク識別子)
  [port] = 7144                   リスニングポート (送信可能な場合)
  [ping] = 7144                   ファイアウォールテスト要求ポート (FW状態不明時)
  [bcid] = <16バイト>             ブロードキャストID (信頼済み・配信中の場合)
```

| atom | 型 | 説明 |
|:---|:---|:---|
| `agnt` | string | クライアントソフト名とバージョン |
| `ver` | int32 | PCPプロトコルバージョン番号 |
| `sid` | bytes(16) | このノードのセッションID（起動毎にランダム生成） |
| `port` | int16 | 自身のリスニングポート（ファイアウォール内の場合は省略） |
| `ping` | int16 | ファイアウォール疎通確認用ポート |
| `bcid` | bytes(16) | 配信中チャンネルのブロードキャストID |

### 4.2 OLEH (接続応答)

上流ノードが HELO に対して返す応答。

**ソース: `servent.cpp` - `handshakeIncomingPCP()`**
```
[PCP: oleh]                       親atom (子atom数: 5)
  [agnt] = "PeerCast/0.1218 ..."  サーバのエージェント文字列
  [sid]  = <16バイト>             サーバのセッションID
  [ver]  = 1218                   サーバのプロトコルバージョン
  [rip]  = <4バイト>              接続元の外部IPアドレス（NAT越え確認用）
  [port] = 7144                   接続元から見えたポート番号
```

`rip` (remote IP) により、接続元ノードは自身のグローバルIPを知ることができる。

### 4.3 ルートノードからの追加情報

上流ノードがルートノード（Tracker）の場合、OLEH の後に追加 atom を送信することがある。

```
[PCP: root]          ← ルートノード情報
  [next] = <ホスト>  ← 次の接続先候補
  [update]           ← 更新通知
```

### 4.4 バリデーション

| 条件 | 応答 |
|:---|:---|
| バージョンが `PCP_CLIENT_MINVERSION` 未満 | `quit` + `PCP_ERROR_BADAGENT` |
| セッションID が自分自身と一致 | `StreamException("Servent loopback")` |
| セッションID 未設定 | `quit` + `PCP_ERROR_NOTIDENTIFIED` |
| 継続パケット非対応エージェント（設定による） | `quit` + `PCP_ERROR_BADAGENT` |

---

## 5. フェーズ4: ストリームデータの転送

### 5.1 HTTP 直接ストリーム (T_DIRECT)

メディアプレイヤーへの応答ヘッダー:

```
HTTP/1.0 200 OK
Content-Type: audio/mpeg    ← コンテンツタイプ（形式による）
icy-name: <チャンネル名>
icy-genre: <ジャンル>
icy-url: <URL>
icy-bitrate: <ビットレート>
icy-metaint: 8192           ← ICYメタデータ挿入間隔（有効時）
```

その後、フォーマットヘッダー（codec 固有）→ ストリームデータを連続送信する。

**ソース: `servent.cpp` - `sendRawChannel()`**

```
[ストリームヘッダー]          ch->headPack のバイト列（フォーマット固有）
[データパケット 1]            rawData から順次読み出し
[データパケット 2]
...
```

- ポーリング間隔: 200ms
- 書き込みタイムアウト: 60秒 (`DIRECT_WRITE_TIMEOUT`)
- `startPlayingFromKeyFrame` が有効な場合、キーフレームまで continuation パケットをスキップ

### 5.2 PCP リレーストリーム (T_RELAY)

他の PeerCast ノードへのリレーは PCP atom 形式で送信する。

**初期チャンネル情報の送信:**

```
[PCP: chan]                     チャンネルデータコンテナ
  [id]   = <16バイト>          チャンネルID
  [info]                       チャンネルメタデータ
    [name] = "チャンネル名"
    [bitr] = 128                ビットレート (kbps)
    [type] = "MP3"              コンテンツタイプ
    [gnre] = "ジャンル"
    [url]  = "http://..."
    [desc] = "説明"
  [trck]                       楽曲情報
    [titl] = "曲タイトル"
    [crea] = "アーティスト"
    [albm] = "アルバム"
    [url]  = "曲URL"
  [pkt]                        ヘッダーパケット
    [type] = "head"
    [pos]  = <バイト位置>
    [data] = <ヘッダーバイト列>
```

**ストリームデータパケット（繰り返し）:**

```
[PCP: chan]
  [id]  = <16バイト>
  [pkt]
    [type] = "data"
    [pos]  = <バイト位置>       ストリーム内の絶対位置
    [cont] = 1                  継続パケットフラグ（キーフレーム間のデータ）
    [data] = <データバイト列>

[PCP: chan]                     ヘッダー更新時
  [id]  = <16バイト>
  [pkt]
    [type] = "head"
    [pos]  = <バイト位置>
    [data] = <新ヘッダーバイト列>
```

| `pkt.type` | 意味 |
|:---|:---|
| `head` | ストリームフォーマットヘッダー（codec 初期化情報） |
| `data` | ストリームデータ本体 |
| `meta` | メタデータ更新 |

---

## 6. フェーズ5: リレーメッシュの維持 (BCST)

### 6.1 ブロードキャストパケット

PeerCast のリレー網ではノード情報を `bcst` (broadcast) atom で伝播させる。TTL が 0 になるまで各ノードが転送する。

**ソース: `pcp.cpp` - `readBroadcastAtoms()`**

```
[PCP: bcst]
  [ttl]  = 7              残り転送ホップ数（受信時に -1）
  [hops] = 1              経路ホップ数（受信時に +1）
  [from] = <16バイト>     発信元セッションID
  [dest] = <16バイト>     宛先セッションID（省略時は全体向け）
  [grp]  = 0x07           配信グループ (ROOT=0x01, TRACKERS=0x02, RELAYS=0x04)
  [cid]  = <16バイト>     対象チャンネルID
  [host]                  ノード情報（以下参照）
    ...
```

### 6.2 ループ防止

- `routeList` に `from` ID を追加し、同一ノードからのパケットを再処理しない
- 自分自身の sessionID と `from` が一致したら `PCP_ERROR_LOOPBACK` でドロップ

### 6.3 転送先グループ

| グループフラグ | 転送先 |
|:---|:---|
| `ROOT (0x01)` | COUT (Control-Out) 接続 |
| `TRACKERS (0x02)` | COUT / CIN (Control-In) 接続 |
| `RELAYS (0x04)` | T_RELAY 接続, CIN 接続 |

---

## 7. HOST atom: ノード情報

リレー探索・代替案内で使用されるノード情報 atom。

```
[PCP: host]
  [id]   = <16バイト>    セッションID
  [ip]   = <4バイト>     IPアドレス
  [port] = <16ビット>    ポート番号
  [numl] = <int>         自分に接続しているリスナー数
  [numr] = <int>         自分がリレーしている数
  [uptm] = <int>         稼働時間（秒）
  [cid]  = <16バイト>    対象チャンネルID
  [trkr] = 1             Tracker フラグ
  [flg1] = <byte>        機能フラグ（下記参照）
  [oldp] = <int>         保持している最古パケット位置
  [newp] = <int>         保持している最新パケット位置
  [upip] = <4バイト>     上流リレーのIPアドレス
  [uppt] = <16ビット>    上流リレーのポート
  [uphp] = <byte>        上流リレーまでのホップ数
```

**`flg1` ビットフラグ:**

| ビット | 定数 | 意味 |
|:---|:---|:---|
| 0x01 | `FLAGS1_TRACKER` | Tracker ノード |
| 0x02 | `FLAGS1_RELAY` | リレー可能 |
| 0x04 | `FLAGS1_DIRECT` | 直接視聴可能 |
| 0x08 | `FLAGS1_PUSH` | ファイアウォール状態（ポート未開放） |
| 0x10 | `FLAGS1_RECV` | 受信中 |
| 0x20 | `FLAGS1_CIN` | Control-In 接続あり |
| 0x40 | `FLAGS1_PRIVATE` | プライベートチャンネル |

---

## 8. エラーコードと終了処理

### 8.1 QUIT atom

接続終了時に送信する。

```
[PCP: quit] = <エラーコード>
```

| コード | 意味 |
|:---|:---|
| `1002` | すでに接続済み (`QUIT + ALREADYCONNECTED`) |
| `1003` | 受付不可 (`QUIT + UNAVAILABLE`) |
| `1004` | ループバック検出 (`QUIT + LOOPBACK`) |
| `1005` | 未識別 (`QUIT + NOTIDENTIFIED`) |
| `1006` | 不正応答 (`QUIT + BADRESPONSE`) |
| `1007` | 非対応エージェント (`QUIT + BADAGENT`) |
| `1008` | オフエア (`QUIT + OFFAIR`) |
| `1009` | サーバシャットダウン (`QUIT + SHUTDOWN`) |
| `1010` | ルートなし (`QUIT + NOROOT`) |
| `1011` | BAN済み (`QUIT + BANNED`) |

### 8.2 タイムアウト

| 種別 | 値 |
|:---|:---|
| 直接ストリーム書き込みタイムアウト | 60秒 (`DIRECT_WRITE_TIMEOUT`) |
| パケットポーリング間隔 | 200ms |

---

## 9. 全体フローまとめ

```
[プレイヤー]       [ローカルPeerCastノード]      [上流リレー]
     |                        |                       |
     |-- GET /stream/<id> --> |                       |
     |                        | (チャンネル未保持の場合)  |
     |                        |-- GET /channel/<id> -> |
     |                        |   "pcp\n" + ver        |
     |                        |-- helo --------------> |
     |                        |<-- oleh -------------- |
     |                        |<-- chan(info/trck/pkt) |
     |                        |   (HTTP 200 確立)      |
     |<-- HTTP 200 + headers -|                       |
     |<-- stream header ------ |<-- chan.pkt(head) --- |
     |<-- stream data -------- |<-- chan.pkt(data) --- |
     |          ...            |          ...          |
     |                        |<------- bcst -------->|  (リレー情報の伝播)
     |                        |-- bcst ------------->  |
     |                        |                       |
     |                        |-- quit ------------->  |  (視聴終了時)
     |<-- connection closed -- |                       |
```

---

## 10. 参照ソースファイル (peercast-yt)

| ファイル | 内容 |
|:---|:---|
| `core/common/servhs.cpp` | HTTP ハンドシェイク、エンドポイントルーティング |
| `core/common/servent.cpp` | ストリーム処理、PCP ハンドシェイク、データ送信 |
| `core/common/pcp.cpp` | PCPStream、BCST パケット処理 |
| `core/common/pcp.h` | PCP atom 定数定義 |
| `core/common/http.cpp` | HTTP リクエスト/レスポンス解析 |
| `core/common/channel.cpp` | チャンネル管理、上流接続 |
| `core/common/chanmgr.cpp` | チャンネルリスト、ヒットリスト管理 |
