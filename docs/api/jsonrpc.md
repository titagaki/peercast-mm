# JSON-RPC API 仕様

peercast-mi が提供する JSON-RPC 2.0 API の実装仕様。

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
| `issueStreamKey` | `[accountName, streamKey]` | `null` |
| `revokeStreamKey` | `[accountName]` | `null` |
| `broadcastChannel` | `[{ sourceUri, info, track }]` | `{ channelId }` |
| `relayChannel` | `[{ upstreamAddr, channelId }]` | `{ channelId }` |
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

### `issueStreamKey`

**パラメータ:** `[accountName: string, streamKey: string]`

アカウントに対してストリームキーを登録する。pcgw-0yp がキーを生成して呼び出す想定。
同じ `accountName` で再呼び出しすると旧キーが無効化され新キーに差し替えられる（再発行）。
登録内容はキャッシュファイル (`stream_keys.json`) に永続化される。

**返却値:** `null`

**エラー条件:**
- `accountName` または `streamKey` が空 → `-32602`
- キャッシュファイルへの書き込み失敗 → `-32603`

---

### `revokeStreamKey`

**パラメータ:** `[accountName: string]`

アカウントのストリームキーを無効化する。そのキーで放送中のチャンネルは停止しない。

**返却値:** `null`

**エラー条件:**
- `accountName` が未登録 → `-32603`

---

### `broadcastChannel`

**パラメータ:** `[{ sourceUri, info, track }]`

指定したストリームキーでチャンネルを開始する。

```json
[{
  "sourceUri": "rtmp://127.0.0.1:1935/live/sk_a1b2c3d4e5f6...",
  "info": {
    "name":    "チャンネル名",
    "genre":   "ジャンル",
    "url":     "https://example.com",
    "desc":    "説明",
    "comment": "",
    "bitrate": 3000
  },
  "track": {
    "title":   "",
    "creator": "",
    "album":   "",
    "url":     ""
  }
}]
```

| フィールド | 説明 |
|---|---|
| `sourceUri` | RTMP ソース URI。`/live/<streamKey>` の形式でストリームキーを含める |
| `info.name` | チャンネル名 (**必須**、空文字列不可) |
| `info.bitrate` | ビットレート (kbps)。`0` でもよい (RTMP の onMetaData で上書きされる) |

**返却値:**
```json
{ "channelId": "0123456789abcdef0123456789abcdef" }
```

ChannelID は入力パラメータから決定論的に生成される。同じ `sourceUri` と `info` で再呼び出しすると同じ `channelId` が返る。

**エラー条件:**
- `info.name` が空 → `-32602`
- `sourceUri` に `/live/<streamKey>` 形式がない → `-32602`
- ストリームキーが未登録 (`issueStreamKey` で登録していない) → `-32602`
- そのストリームキーで既に放送中 (`stopChannel` してから再呼び出しする) → `-32602`

---

### `relayChannel`

**パラメータ:** `[{ upstreamAddr, channelId }]`

指定した上流 PeerCast ノードからストリームを受け取るリレーチャンネルを開始する。

```json
[{
  "upstreamAddr": "192.168.1.10:7144",
  "channelId":    "0123456789abcdef0123456789abcdef"
}]
```

| フィールド | 説明 |
|---|---|
| `upstreamAddr` | 上流ノードの `host:port`。省略した場合は config の先頭 YP アドレスを使用 |
| `channelId` | リレーするチャンネルの ID (32 文字 hex) |

**返却値:**
```json
{ "channelId": "0123456789abcdef0123456789abcdef" }
```

接続は非同期で確立される。返却直後はまだ上流に接続していない場合がある。`getChannelStatus` の `isReceiving` が `true` になれば受信開始。

接続失敗・切断時は指数バックオフ (5 秒〜120 秒) で自動再接続する。

**エラー条件:**
- `channelId` が 32 文字 hex でない → `-32602`
- 同じ `channelId` のチャンネルが既に存在する → `-32602`
- `upstreamAddr` が空かつ config に YP が設定されていない → `-32602`

---

### `getVersionInfo`

**パラメータ:** なし

**返却値:**
```json
{ "agentName": "peercast-mi/0.0.1" }
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

**返却値:** チャンネルオブジェクトの配列。`broadcastChannel` で開始中のチャンネルが全て含まれる。

```json
[
  {
    "channelId": "0123456789abcdef0123456789abcdef",
    "status": {
      "status": "Receiving",
      "source": "rtmp://127.0.0.1:1935/live/sk_a1b2c3...",
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
  "source": "rtmp://127.0.0.1:1935/live/sk_a1b2c3...",
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
| `status` | `"Receiving"` (データ受信中) または `"Idle"` (未受信)。`ContentBuffer.HasData()` に基づく |
| `source` | ブロードキャストチャンネル: `rtmp://127.0.0.1:<rtmpPort>/live/<streamKey>`。リレーチャンネル: 上流ノードの `host:port` |
| `totalDirects` | HTTP 直接視聴接続数（`Channel.NumListeners()`） |
| `totalRelays` | PCP リレー接続数（`Channel.NumRelays()`） |
| `isBroadcasting` | ブロードキャストチャンネル (RTMP ソース) なら `true`、リレーチャンネルなら `false` |
| `isRelayFull` | リレー接続が上限に達していれば `true`（`Channel.IsRelayFull()`）。上限未設定時は常に `false` |
| `isDirectFull` | 直接視聴接続が上限に達していれば `true`（`Channel.IsDirectFull()`）。上限未設定時は常に `false` |
| `isReceiving` | ストリームデータを受信済みなら `true`（`ContentBuffer.HasData()`） |

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
| `status` | ソースは `"Receiving"` または `"Idle"`（`HasData()` に基づく）、出力接続は `"Connected"`（固定値） |
| `sendRate` | bytes/sec（`OutputStream.SendRate()`）。ソースは常に `0` |
| `recvRate` | bytes/sec。現実装では常に `0` |
| `protocolName` | ブロードキャストチャンネルのソースは `"RTMP"`、リレーチャンネルのソースは `"PCP"`、下流 PCP リレーは `"PCP"`、HTTP 直接は `"HTTP"` |
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
    "announceUri": "pcp://yayaue.me/"
  }
]
```

| フィールド | 説明 |
|---|---|
| `yellowPageId` | config.toml の `[[yp]]` エントリの 0 始まりインデックス |
| `name` | `[[yp]].name` |
| `uri` / `announceUri` | `[[yp]].addr`（`pcp://` スキームがなければ自動付与） |

---

### `getChannelRelayTree`

**パラメータ:** `[channelId: string]`

自ノードのリレーツリーを返す。

- **ブロードキャストチャンネル:** 自ノードのみの単一要素配列。`isTracker: true`。`children` は常に空配列（YP 経由のノード情報収集は未実装）。
- **リレーチャンネル:** 上流ノードをルートとし、その `children` として自ノードを含む 2 段ツリーを返す。

**返却値（ブロードキャストチャンネルの場合）:**
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
    "versionString": "peercast-mi/0.0.1",
    "children": []
  }
]
```

**返却値（リレーチャンネルの場合）:**
```json
[
  {
    "sessionId": "",
    "address": "192.168.1.10",
    "port": 7144,
    "isTracker": false,
    "isReceiving": true,
    "children": [
      {
        "sessionId": "aabbccdd...",
        "address": "",
        "port": 7144,
        "isTracker": false,
        "localRelays": 1,
        "localDirects": 0,
        "isReceiving": true,
        "children": []
      }
    ]
  }
]
```

`address` は空文字列（グローバル IP の取得は YPClient の `oleh.rip` 経由のみであり、API サーバーからは参照不可）。上流ノードの `sessionId` は未取得のため空文字列。

---

## 実装上の制約・注意事項

- ストリームキーはキャッシュファイル (`stream_keys.json`) に永続化される。プロセス再起動後も有効。
- `revokeStreamKey` はキーを無効化するが、そのキーで放送中のチャンネルは停止しない。
- 同じストリームキーで放送中に再度 `broadcastChannel` を呼ぶとエラー。`stopChannel` してから再呼び出しする。
- `broadcastChannel` より先に RTMP push が来ても受け付ける（ストリームキーが発行済みであれば）。チャンネルが作成されるまでの RTMP データは静かにドロップされる。
- `channelId` の照合は大文字・小文字を区別しない。
- `getChannelStatus.status` は `"Receiving"` (データ受信中) または `"Idle"` (未受信)。
- `getChannelConnections` の `recvRate` は常に `0`（受信レートの計測は未実装）。
- `getChannelRelayTree` の `address` は空文字列（グローバル IP 未取得）。
- `relayChannel` は返却直後に上流への接続が完了していない場合がある。`getChannelStatus.isReceiving` で確認する。
- リレーチャンネルでも `bumpChannel` は機能する（YP への bcst が送信される）。
