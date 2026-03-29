# PeerCast チャンネル配信プロトコル技術仕様書

## 1. 概要

PeerCast における「配信」とは、配信ソフトウェア（エンコーダ）が PeerCast ノードにメディアストリームを送り込み、PeerCast ノードがそのストリームを受け取って リレーネットワークへ配信するプロセスである。

### 1.1 ソース接続方式

| 方式 | プロトコル | エンドポイント | 代表的なソフト |
|:---|:---|:---|:---|
| ShoutCast (ICY) | HTTP/1.0 変形 | パスワード文字列 | Winamp DSP プラグイン等 |
| Icecast (SOURCE) | HTTP/1.0 | `SOURCE /<マウントポイント>` | Icecast 対応エンコーダ |
| HTTP Push | HTTP POST | `POST /` | ブラウザ、スクリプト等 |
| RTMP (経由) | RTMP → HTTP Push | 外部プロセス経由 | OBS Studio 等 |

### 1.2 通信アーキテクチャ

```
[エンコーダ]               [PeerCastノード]          [YP/トラッカー]   [リレーノード]
     |                            |                         |                |
     | ShoutCast/Icecast/         |                         |                |
     | HTTP Push                  |                         |                |
     |--- ストリーム送信 -------> |                         |                |
     |                            |-- bcst (tracker upd) -->|                |
     |                            |   (30秒ごと)            |                |
     |<--- 継続受信 ------------- |                         |                |
     |                            |--- chan.pkt(data) ----> |                |
     |                            |   (broadcastPacket)                      |
     |                            |--- chan.pkt(data) -----------------------------> |
```

---

## 2. ソース接続の確立

### 2.1 ShoutCast (ICY) 接続

ShoutCast DSP プラグイン等が使う独自プロトコル。HTTP/1.0 の変形。

**クライアント送信:**
```
<パスワード文字列>\r\n        ← HTTPメソッドの代わりにパスワードを送る
icy-name: チャンネル名\r\n
icy-genre: ジャンル\r\n
icy-url: http://...\r\n
icy-bitrate: 128\r\n
icy-pub: 1\r\n
content-type: audio/mpeg\r\n
\r\n
[ストリームデータ ...]
```

**サーバ応答:**
```
OK2\r\n
icy-caps:11\r\n
\r\n
```

**ソース: `servhs.cpp`**
```cpp
// ShoutCast broadcast
loginPassword.set(servMgr->password);   // pwd already checked
sock->writeLine("OK2");
sock->writeLine("icy-caps:11");
sock->writeLine("");
handshakeICY(Channel::SRC_SHOUTCAST, isHTTP);
```

### 2.2 Icecast (SOURCE) 接続

Icecast プロトコルを使うエンコーダが使用する。

**クライアント送信:**
```
SOURCE /<マウントポイント> ICE/1.0\r\n
Authorization: Basic <base64(user:password)>\r\n
content-type: audio/mpeg\r\n
ice-name: チャンネル名\r\n
ice-genre: ジャンル\r\n
ice-url: http://...\r\n
ice-bitrate: 128\r\n
\r\n
[ストリームデータ ...]
```

**サーバ応答 (HTTP):**
```
HTTP/1.0 200 OK\r\n
\r\n
```

**サーバ応答 (非 HTTP):**
```
OK\r\n
```

**ソース: `servhs.cpp` - `handshakeSOURCE()`**
```cpp
void Servent::handshakeSOURCE(char *in, bool isHTTP)
{
    if (!isAllowed(ALLOW_BROADCAST))
        throw HTTPException(HTTP_SC_UNAVAILABLE, 503);
    // ...
    handshakeICY(Channel::SRC_ICECAST, isHTTP);
}
```

### 2.3 HTTP Push 接続

HTTP POST でストリームを送り込む方式。ローカルホストのみ許可。

**クライアント送信:**
```
POST /?name=チャンネル名&genre=ジャンル&desc=説明&url=http://...&comment=コメント HTTP/1.0\r\n
Content-Type: video/x-flv\r\n
Transfer-Encoding: chunked\r\n    ← チャンク転送の場合
\r\n
[ストリームデータ ...]
```

| クエリパラメータ | 内容 |
|:---|:---|
| `name` | チャンネル名 (必須) |
| `genre` | ジャンル |
| `desc` | 概要 |
| `url` | 関連 URL |
| `comment` | コメント |
| `type` | コンテンツタイプ (FLV, MP4 等) |
| `ipv` | IP バージョン (`6` で IPv6 配信) |

**ソース: `servhs.cpp` - `handshakeHTTPPush()`**
```cpp
void Servent::handshakeHTTPPush(const std::string& args)
{
    Query query(args);
    HTTP http(*sock);
    http.readHeaders();

    if (query.get("name").empty())
        throw HTTPException(HTTP_SC_BADREQUEST, 400);

    ChanInfo info = createChannelInfo(chanMgr->broadcastID, chanMgr->broadcastMsg, query,
                                      http.headers.get("Content-Type"));
    // ...
    c->startHTTPPush(sock, chunked);
}
```

### 2.4 RTMP 接続 (外部プロセス経由)

OBS Studio 等からの RTMP ストリームは、外部プロセス `rtmpserver` (別途用意) が受け取り、HTTP Push に変換してローカルの PeerCast ノードへ送り込む。

```
[OBS Studio]                [rtmpserver (外部)]       [PeerCastノード]
     |                             |                         |
     |--- RTMP publish ----------> |                         |
     |                             |--- POST /?name=... ---> |
     |                             |   (HTTP Push)           |
     |                             |<--- 200 OK ------------ |
     |                             |--- FLV stream --------> |
```

`RTMPServerMonitor` が外部プロセスの死活監視と自動再起動を担当する。

**ソース: `rtmpmonit.cpp` - `makeEndpointURL()`**
```cpp
return "http://localhost:" + std::to_string(servMgr->serverHost.port) + "/?" + query.str();
```

---

## 3. チャンネル ID の生成

ICY 接続時、チャンネル ID は `broadcastID`（配信者固有の ID）を元にチャンネル名・マウントポイント・ビットレートのエンコードで決定する（ハイジャック防止）。

```cpp
info.id = chanMgr->broadcastID;
info.id.encode(nullptr, info.name.cstr(), loginMount.cstr(), info.bitrate);
```

`randomizeBroadcastingChannelID` フラグが有効な場合はランダム ID を使用する。

HTTP Push の場合は `createChannelInfo()` 内で同様の処理が行われる。

---

## 4. チャンネルのストリームスレッド

チャンネルが作成されると、専用スレッドでストリームの受信ループが動作する。

```
Channel::startICY / startHTTPPush
    └── Channel::startStream()
            └── [新規スレッド] Channel::stream()
                    └── sourceData->stream(ch)          ← ICYSource / HTTPPushSource
                            └── ch->readStream(sock, source)
                                    └── ループ:
                                        source->readHeader()   ← codec ヘッダ解析
                                        source->readPacket()   ← パケット読み込み
                                        ch->newPacket(pack)    ← rawData に書き込み
```

### 4.1 readHeader / readPacket

各フォーマット (`MP3Stream`, `FLVStream`, `OGGStream`, `MKVStream` 等) が `ChannelStream` を継承し、フォーマット固有の処理を実装する。

**フォーマットと `ChannelStream` の対応:**

| コンテンツタイプ | ChannelStream 実装 |
|:---|:---|
| `T_MP3` | `MP3Stream` |
| `T_FLV` | `FLVStream` |
| `T_OGG`, `T_OGM` | `OGGStream` |
| `T_MKV`, `T_WEBM` | `MKVStream` |
| `T_MP4` | `MP4Stream` |
| `T_NSV` | `NSVStream` |
| `T_WMA`, `T_WMV` | `MMSStream` |
| その他 | `RawStream` |

### 4.2 パケットバッファへの書き込み

受信したパケットは `rawData` (循環バッファ) に書き込まれる。

```cpp
void Channel::newPacket(ChanPacket &pack)
{
    if (pack.type == ChanPacket::T_PCP)
        return;   // PCP パケットはバッファに入れない

    rawData.writePacket(pack, true);
}
```

`headPack` にはフォーマットヘッダーが保持され、新規リレー接続時に最初に送信される。

---

## 5. リレーへのパケット配信

`rawData` に書き込まれたパケットは、接続中のリレーノード・直接視聴者へ配信される。

```cpp
// channel.cpp
servMgr->broadcastPacket(pack, ch->info.id, ch->remoteID, GnuID(), Servent::T_RELAY);
```

`broadcastPacket` は `T_RELAY` 型の全 Servent に対してパケットを送信する。

---

## 6. Tracker / YP へのチャンネル情報通知

配信中のノードは自身がトラッカーとして機能し、YP および接続中のノードへチャンネル情報を定期的に送信する。

### 6.1 通知タイミング

| トリガー | 間隔 |
|:---|:---|
| 通常の定期更新 | 30秒 (`lastTrackerUpdate` から) |
| rawData 最初の書き込み後 | 120秒ごと (`readStream` ループ内) |
| 配信終了時 | 即時 (force=true) |

### 6.2 Tracker アップデート atom

**ソース: `channel.cpp` - `writeTrackerUpdateAtom()`**

```
[PCP: bcst]                         ← ブロードキャスト
  [grp]  = ROOT (0x01)             ← ROOT グループへ転送 (→ YP)
  [hops] = 0
  [ttl]  = 7
  [from] = <sessionID>
  [ver]  = PCP_CLIENT_VERSION
  [chan]                            ← チャンネル情報
    [id]   = <channelID>
    [bcid] = <broadcastID>
    [info] ...                     ← チャンネルメタデータ
    [trck] ...                     ← 楽曲情報
  [host]                           ← このノードの情報
    [id]   = <sessionID>
    [ip]   = <グローバルIP>
    [port] = 7144
    [numl] = <リスナー数>
    [numr] = <リレー数>
    [uptm] = <稼働秒数>
    [trkr] = 1                     ← Tracker フラグ
    [oldp] = <最古パケット位置>
    [newp] = <最新パケット位置>
```

`T_COUT` 接続 (YP への接続) を経由して YP のルートノードへ届く。

---

## 7. メタデータの更新と伝播

### 7.1 ICY メタデータ (MP3)

MP3 ストリーム中の ICY メタデータ (`StreamTitle`, `StreamUrl`) が更新されると `processMp3Metadata()` が呼ばれ、`updateInfo()` でチャンネル情報を更新する。

### 7.2 updateInfo による伝播

チャンネル情報が変更されたとき、配信中であれば `bcst` パケットでリレーへ伝播する。

**ソース: `channel.cpp` - `updateInfo()`**

```
[PCP: bcst]                         ← ブロードキャスト
  [grp]  = RELAYS (0x04)           ← RELAY グループへ転送
  [ttl]  = 7
  [from] = <sessionID>
  [cid]  = <channelID>
  [chan]
    [id]   = <channelID>
    [info] ...                     ← 更新されたチャンネルメタデータ
    [trck] ...                     ← 更新された楽曲情報
```

メタデータ変更の伝播は 30秒に 1 回に制限される (`lastMetaUpdate`)。

---

## 8. 配信終了処理

1. エンコーダが接続を切断する (または配信停止操作)
2. `readStream()` のループが `eof` または `StreamException` で終了
3. `peercastApp->channelStop(&info)` を呼び出す
4. `broadcastTrackerUpdate(GnuID(), true)` (force=true) でオフエア状態を YP へ通知
5. チャンネルソケットをクローズ
6. `stayConnected` が true の場合は再接続を待機するループへ戻る

---

## 9. 全体フローまとめ

```
[エンコーダ]              [PeerCastノード]              [YP]       [リレーノード]
     |                          |                          |               |
     |                          |  (管理画面で配信設定)    |               |
     |-- ICY/POST/SOURCE ----> |                          |               |
     |   (ストリーム開始)       |                          |               |
     |                          | createChannel()          |               |
     |                          | startICY/HTTPPush()      |               |
     |                          | [新スレッド起動]          |               |
     |                          |                          |               |
     |-- [ストリームデータ] --> |                          |               |
     |                          | readHeader()             |               |
     |                          | readPacket() ×繰り返し  |               |
     |                          | rawData.writePacket()    |               |
     |                          |                          |               |
     |                          |-- bcst(tracker upd) ---> |               |
     |                          |   (30秒ごと)             |               |
     |                          |                          |               |
     |                          |-- chan.pkt(head/data) ----------------> |
     |                          |   (broadcastPacket)      |               |
     |          ...             |          ...             |               |
     |                          |                          |               |
     |-- [接続切断] ---------- |                          |               |
     |                          | broadcastTrackerUpdate() |               |
     |                          |-- bcst(offair) --------> |               |
     |                          | チャンネル終了           |               |
```

---

## 10. アクセス制御

| チェック | 条件 |
|:---|:---|
| `ALLOW_BROADCAST` | 配信受付が有効でなければ 503 |
| ローカルホスト | HTTP Push (`POST /`) はローカルホストのみ許可 |
| パスワード認証 | ShoutCast/Icecast はパスワード一致を要求 (ローカルかつ空の場合は省略可) |

---

## 11. 参照ソースファイル (peercast-yt)

| ファイル | 内容 |
|:---|:---|
| `core/common/servhs.cpp` | ソース接続ハンドシェイク (`handshakeSOURCE`, `handshakeHTTPPush`, `handshakeICY`) |
| `core/common/channel.cpp` | チャンネル管理・ストリームループ・Tracker 更新・メタデータ伝播 |
| `core/common/httppush.cpp` | `HTTPPushSource::stream()` 実装 |
| `core/common/rtmpmonit.cpp` | RTMP サーバモニター・外部プロセス管理 |
| `core/common/pcp.cpp` | PCP atom ストリーム処理 |
| `core/common/servmgr.cpp` | `broadcastPacket()` 実装 |
| `core/common/mp3.cpp` | MP3 ストリーム解析 (`MP3Stream`) |
| `core/common/flv.cpp` | FLV ストリーム解析 (`FLVStream`) |
| `core/common/ogg.cpp` | OGG/OGM ストリーム解析 (`OGGStream`) |
| `core/common/mkv.cpp` | MKV/WebM ストリーム解析 (`MKVStream`) |
