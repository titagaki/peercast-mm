# JSON-RPC API 仕様

peercast-mm が提供する JSON-RPC 2.0 API の実装仕様。

実装: `internal/jsonrpc/server.go`

---

## エンドポイント

```
POST /api/1
```

ポート 7144 で受け付ける。PCP・HTTP 視聴リクエストと同居している。

### リクエスト形式

JSON-RPC 2.0 仕様に準拠する。パラメータは **位置指定配列** (`"params": [...]`) のみ対応。

```json
{
  "jsonrpc": "2.0",
  "method": "<メソッド名>",
  "params": [...],
  "id": 1
}
```

### レスポンス形式

成功時:
```json
{ "jsonrpc": "2.0", "id": 1, "result": <値> }
```

エラー時:
```json
{ "jsonrpc": "2.0", "id": 1, "error": { "code": <コード>, "message": "<説明>" } }
```

### エラーコード

| コード | 意味 |
|---|---|
| `-32700` | JSON パースエラー |
| `-32601` | メソッドが存在しない |
| `-32602` | パラメータが不正 |
| `-32603` | 内部エラー（チャンネル未発見等） |

---

## 実装メソッド一覧

| メソッド | パラメータ | 返却値 |
|---|---|---|
| `getVersionInfo` | なし | `{ agentName }` |
| `getSettings` | なし | `{ serverPort, rtmpPort }` |
| `getChannels` | なし | チャンネルオブジェクトの配列 |
| `getChannelInfo` | `[channelId]` | `{ info, track, yellowPages }` |
| `getChannelStatus` | `[channelId]` | status オブジェクト |
| `setChannelInfo` | `[channelId, info, track]` | `null` |
| `stopChannel` | `[channelId]` | `null` |
| `bumpChannel` | `[channelId]` | `null` |
| `getChannelConnections` | `[channelId]` | 接続情報の配列 |
| `stopChannelConnection` | `[channelId, connectionId]` | `boolean` |
| `getYellowPages` | なし | YP オブジェクトの配列 |
| `getChannelRelayTree` | `[channelId]` | ノードオブジェクトの配列 |

`channelId` は 32 文字の hex 文字列（大文字・小文字どちらも可）。

---

## 各メソッドの仕様

### `getVersionInfo`

**パラメータ:** なし

**返却値:**
```json
{ "agentName": "peercast-mm/0.0.1" }
```

---

### `getSettings`

**パラメータ:** なし

**返却値:**
```json
{ "serverPort": 7144, "rtmpPort": 1935 }
```

config.toml の `peercast_port` / `rtmp_port` の値を返す。

---

### `getChannels`

**パラメータ:** なし

**返却値:** チャンネルオブジェクトの配列。peercast-mm は常にチャンネルが 1 本なので要素数は 1。

```json
[
  {
    "channelId": "0123456789abcdef0123456789abcdef",
    "status": {
      "status": "Receiving",
      "source": "rtmp://127.0.0.1:1935/live",
      "totalDirects": 1,
      "totalRelays": 2,
      "isBroadcasting": true,
      "isRelayFull": false,
      "isDirectFull": false,
      "isReceiving": true
    },
    "info": {
      "name": "チャンネル名",
      "url": "https://example.com",
      "desc": "説明",
      "comment": "",
      "genre": "ジャンル",
      "type": "FLV",
      "bitrate": 500
    },
    "track": {
      "title": "",
      "creator": "",
      "url": "",
      "album": ""
    },
    "yellowPages": [
      { "yellowPageId": 0, "name": "0yp" }
    ]
  }
]
```

---

### `getChannelInfo`

**パラメータ:** `[channelId: string]`

**返却値:**
```json
{
  "info": {
    "name": "チャンネル名",
    "url": "https://example.com",
    "desc": "説明",
    "comment": "",
    "genre": "ジャンル",
    "type": "FLV",
    "bitrate": 500
  },
  "track": {
    "title": "",
    "creator": "",
    "url": "",
    "album": ""
  },
  "yellowPages": [
    { "yellowPageId": 0, "name": "0yp" }
  ]
}
```

`yellowPages` は config.toml の `[[yp]]` エントリに対応する。各エントリに 0 始まりの `yellowPageId` を付与する。

---

### `getChannelStatus`

**パラメータ:** `[channelId: string]`

**返却値:**
```json
{
  "status": "Receiving",
  "source": "rtmp://127.0.0.1:1935/live",
  "totalDirects": 1,
  "totalRelays": 2,
  "isBroadcasting": true,
  "isRelayFull": false,
  "isDirectFull": false,
  "isReceiving": true
}
```

| フィールド | 説明 |
|---|---|
| `status` | 常に `"Receiving"`（固定値） |
| `source` | `rtmp://127.0.0.1:<rtmpPort>/live`（固定値） |
| `totalDirects` | HTTP 直接視聴接続数（`Channel.NumListeners()`） |
| `totalRelays` | PCP リレー接続数（`Channel.NumRelays()`） |
| `isBroadcasting` | 常に `true`（固定値） |
| `isRelayFull` | 常に `false`（制限なし） |
| `isDirectFull` | 常に `false`（制限なし） |
| `isReceiving` | RTMP からデータを受信済みなら `true`（`ContentBuffer.HasData()`） |

---

### `setChannelInfo`

**パラメータ:** `[channelId: string, info: object, track: object]`

`info` / `track` の構造は `getChannelInfo` の返却値と同じ。

`info.bitrate` が `0` の場合は現在値を維持する（`type` フィールドは書き込み不可）。

**返却値:** `null`

---

### `stopChannel`

**パラメータ:** `[channelId: string]`

全出力接続を即時切断する（`Channel.CloseAll()`）。RTMP ソース接続は切断しない。

**返却値:** `null`

---

### `bumpChannel`

**パラメータ:** `[channelId: string]`

YP への bcst を即時送信する（`YPClient.Bump()`）。YP 未設定の場合は no-op。

**返却値:** `null`

---

### `getChannelConnections`

**パラメータ:** `[channelId: string]`

**返却値:** 接続情報オブジェクトの配列。先頭要素が常にソース接続（`connectionId: -1`）。

```json
[
  {
    "connectionId": -1,
    "type": "source",
    "status": "Receiving",
    "sendRate": 0,
    "recvRate": 0,
    "protocolName": "RTMP",
    "remoteEndPoint": "127.0.0.1:1935"
  },
  {
    "connectionId": 3,
    "type": "relay",
    "status": "Connected",
    "sendRate": 65000,
    "recvRate": 0,
    "protocolName": "PCP",
    "remoteEndPoint": "203.0.113.5:7144"
  },
  {
    "connectionId": 5,
    "type": "direct",
    "status": "Connected",
    "sendRate": 65000,
    "recvRate": 0,
    "protocolName": "HTTP",
    "remoteEndPoint": "198.51.100.9:54321"
  }
]
```

| フィールド | 説明 |
|---|---|
| `connectionId` | 接続 ID。ソースは常に `-1`、出力接続は `Listener` が採番した正の整数 |
| `type` | `"source"` / `"relay"` / `"direct"` |
| `status` | ソースは `"Receiving"`、出力接続は `"Connected"`（固定値） |
| `sendRate` | bytes/sec（`OutputStream.SendRate()`）。ソースは常に `0` |
| `recvRate` | bytes/sec。現実装では常に `0` |
| `protocolName` | ソースは `"RTMP"`、PCP リレーは `"PCP"`、HTTP 直接は `"HTTP"` |
| `remoteEndPoint` | `"IP:port"` 文字列 |

---

### `stopChannelConnection`

**パラメータ:** `[channelId: string, connectionId: int]`

**返却値:** `boolean`（`true` = 切断成功、`false` = 対象なし または対象外）

**制約:** `type: "relay"`（PCPOutputStream）のみ切断可。`type: "direct"` および `type: "source"` は対象外で `false` を返す。

---

### `getYellowPages`

**パラメータ:** なし

**返却値:**
```json
[
  {
    "yellowPageId": 0,
    "name": "0yp",
    "uri": "pcp://yayaue.me/",
    "announceUri": "pcp://yayaue.me/",
    "channelCount": 1
  }
]
```

| フィールド | 説明 |
|---|---|
| `yellowPageId` | config.toml の `[[yp]]` エントリの 0 始まりインデックス |
| `name` | `[[yp]].name` |
| `uri` / `announceUri` | `[[yp]].addr`（`pcp://` スキームがなければ自動付与） |
| `channelCount` | YPClient が起動している場合は `1`、未設定の場合は `0` |

---

### `getChannelRelayTree`

**パラメータ:** `[channelId: string]`

自ノード（ブロードキャストノード）のみのツリーを返す。YP からのノード情報収集は未実装のため `children` は常に空配列。

**返却値:**
```json
[
  {
    "sessionId": "aabbccdd...",
    "address": "",
    "port": 7144,
    "isFirewalled": false,
    "localRelays": 2,
    "localDirects": 1,
    "isTracker": true,
    "isRelayFull": false,
    "isDirectFull": false,
    "isReceiving": true,
    "isControlFull": false,
    "version": 1218,
    "versionString": "peercast-mm/0.0.1",
    "children": []
  }
]
```

`address` は空文字列（グローバル IP の取得は YPClient の `oleh.rip` 経由のみであり、API サーバーからは参照不可）。

---

## 実装上の制約・注意事項

- peercast-mm は常に 1 チャンネルのみ管理する。`getChannels` は常に 1 要素の配列を返す。
- `channelId` の照合は大文字・小文字を区別しない（`strings.EqualFold`）。
- `getChannelStatus.status` は現在 `"Receiving"` 固定。RTMP 未接続時も同じ値を返す（`isReceiving: false` で区別すること）。
- `getChannelConnections` の `recvRate` は常に `0`（RTMP 受信レートの計測は未実装）。
- `getChannelRelayTree` の `address` は空文字列（グローバル IP 未取得）。
