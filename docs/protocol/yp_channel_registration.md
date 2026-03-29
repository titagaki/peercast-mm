# PeerCast チャンネル掲載（YP登録）プロトコル技術仕様書

## 1. 概要

PeerCastにおける「チャンネル掲載（YP登録）」とは、配信クライアント（Tracker）がRoot Server（YPサーバ）に対してPCPプロトコルのコントロール接続（COUT/CIN）を確立し、自身が配信中のチャンネル情報（メタデータとホスト情報）をブロードキャストパケットで送信するプロセスである。

HTTPベースのREST APIではなく、**TCPベースのPCPバイナリプロトコル**による持続的接続を通じてチャンネル情報が送受信される。

### 1.1 通信アーキテクチャ

```
配信クライアント (Tracker)          Root Server (YP)
        |                                |
        |--- TCP接続 ------------------->|
        |--- "pcp\n" + バージョン ------>|
        |--- helo (HELO) --------------->|
        |<-- oleh (OLEH) + root atoms ---|
        |<-- ok -----------------------  |
        |<-- root > upd (更新要求) ------|
        |--- bcst > chan + host -------->|  ← チャンネル情報送信
        |         ...                    |
        |--- bcst > chan + host -------->|  ← 定期更新 (≥30秒間隔)
        |         ...                    |
        |--- quit --------------------->|  ← 配信終了
```

---

## 2. 接続の確立

### 2.1 エンドポイント

| 項目 | 値 |
|:---|:---|
| プロトコル | TCP (PCPバイナリプロトコル) |
| デフォルトポート | `7144` (`DEFAULT_PORT`) |
| 接続先 | `servMgr->rootHost` に設定されたホスト名/IPアドレス |
| 接続種別 | COUT (Control-Out) |

> **注意**: HTTPの `GET /admin/track` のようなエンドポイントは使用されない。PeerCastのYP登録はHTTPではなく、PCPバイナリプロトコルのコントロール接続で行われる。

### 2.2 接続トリガー

配信クライアントは `ServMgr::idleProc`（IDLEスレッド）にて30秒間隔で `connectBroadcaster()` を呼び出す。このメソッドは:

1. `rootHost` が設定されていること
2. COUT型のServentが存在しないこと

を確認し、条件を満たす場合に `Servent::initOutgoing(T_COUT)` で新しいCOUTスレッドを起動する。

**ソース: `servmgr.cpp`**
```cpp
void ServMgr::connectBroadcaster()
{
    if (!rootHost.isEmpty())
    {
        if (!numUsed(Servent::T_COUT))
        {
            Servent *sv = allocServent();
            if (sv)
                sv->initOutgoing(Servent::T_COUT);
        }
    }
}
```

### 2.3 接続先の決定（COUT outgoingProc）

`Servent::outgoingProc` は以下の優先順で接続先を決定する:

1. **プッシュ接続**: `pushSock` が設定されている場合はそれを使用
2. **ヒットリスト内のトラッカー**: `ChanHitList` から既知のトラッカーノードを検索
3. **rootHost への直接接続**: 上記が見つからず、かつ前回のYP接続から `MIN_YP_RETRY` 秒以上経過している場合、`rootHost` を解決してフォールバック接続

```cpp
// rootHost への直接接続
bestHit.host.fromStrName(servMgr->rootHost.cstr(), DEFAULT_PORT);
bestHit.yp = true;
chanMgr->lastYPConnect = ctime;
```

---

## 3. PCPハンドシェイク

### 3.1 クライアント → サーバ: 初期バイト列

クライアントはTCP接続確立後、以下のバイト列を送信する:

```
[4バイト] PCP_CONNECT タグ: 0x70 0x63 0x70 0x0a ("pcp\n")
[4バイト] データ長: 0x04 0x00 0x00 0x00 (4, Little Endian)
[4バイト] バージョン番号: 0x01 0x00 0x00 0x00 (1, Little Endian)
```

> これはHTTPリクエスト行がそのまま `"pcp"` で始まるため、サーバ側の `handshakeHTTP` では `http.isRequest(PCX_PCP_CONNECT)` として識別される。

### 3.2 クライアント → サーバ: HELO パケット

`Servent::writeHeloAtom` で構築される。

```
helo (PKT, 3〜6 children)
├── agnt (STR): User-Agent文字列 (例: "PeerCast/0.1218 (YT50)")
├── ver  (INT): プロトコルバージョン (例: 1218)
├── sid  (ID):  セッションID (16バイト, ランダム生成)
├── port (SHORT): リスニングポート ※FW_OFF時のみ送信
├── ping (SHORT): ファイアウォールチェック用ポート ※FW_UNKNOWN時のみ送信
└── bcid (ID):  ブロードキャストID ※YP接続(isTrusted)かつ配信中のみ送信
```

**送信条件の詳細:**

| フィールド | 送信条件 |
|:---|:---|
| `port` | `ServMgr::getFirewall() != FW_ON` (デフォルトフラグ設定時) |
| `ping` | `ServMgr::getFirewall() == FW_UNKNOWN` |
| `bcid` | `isTrusted && chanMgr->isBroadcasting()` |

### 3.3 サーバ → クライアント: OLEH パケット

Root Server（YP側）の `handshakeIncomingPCP` で構築される。

```
oleh (PKT, 5 children)
├── agnt (STR): サーバのUser-Agent文字列
├── sid  (ID):  サーバのセッションID
├── ver  (INT): サーバのプロトコルバージョン
├── rip  (INT/RAW[16]): クライアントのグローバルIP (IPv4: 4バイト, IPv6: 16バイト)
└── port (SHORT): クライアントの確認済みポート (ファイアウォール越しなら0)
```

### 3.4 サーバ: Root Atoms 送信

ハンドシェイク完了後、サーバが `isRoot == true` の場合、`writeRootAtoms` が呼ばれ、Root Server固有の追加情報が送信される:

```
root (PKT, 5〜6 children)
├── uint (INT): ホスト更新間隔 (秒) ※デフォルト120秒
├── url  (STR): ダウンロードURL接尾辞 ("download.php")
├── chkv (INT): 最小要求クライアントバージョン (1218)
├── next (INT): 次のRootパケットまでの秒数 (= hostUpdateInterval)
├── asci (STR): Root メッセージ
└── upd  (PKT, 0 children): 更新要求 ※getUpdate=true時のみ
```

### 3.5 サーバ: 接続受理

ハンドシェイク後、以下のバリデーションが行われる:

1. **バージョンチェック**: `version < PCP_CLIENT_MINVERSION(1200)` → `PCP_ERROR_BADAGENT` で切断
2. **セッションID未設定**: → `PCP_ERROR_NOTIDENTIFIED` で切断
3. **ループバック検知**: クライアントのセッションIDが自分自身と同じ → 例外で切断
4. **接続数制限**: `controlInFull()` → `PCP_ERROR_UNAVAILABLE`
5. **重複接続**: 同一セッションIDのCIN/COUTが既存 → `PCP_ERROR_ALREADYCONNECTED`
6. **非配信状態(非Root)**: `!isRoot && !isBroadcasting()` → `PCP_ERROR_OFFAIR`

すべてのバリデーションを通過すると:

```
ok (INT): 0
root (PKT, 1 child)
└── upd (PKT, 0 children)  ← 更新要求：クライアントにチャンネル情報の送信を促す
```

---

## 4. チャンネル情報の送信（Tracker Update）

### 4.1 トリガー

チャンネル情報のブロードキャストは以下のタイミングで発生する:

| タイミング | 発火条件 | force |
|:---|:---|:---|
| Root Server からの `root > upd` 受信 | `PCPStream::readRootAtoms` が `PCP_ROOT_UPDATE` を検出 | `true` |
| 定期更新 (readStream ループ内) | 前回の更新から120秒以上経過 | `false` |
| チャンネル情報変更時 | `Channel::updateInfo` でメタデータが変化 | `false` |
| IP アドレス変更検出時 | `ServMgr::checkForceIP` が変更を検出 | `true` |
| 配信終了時 | `Channel::readStream` の finally 相当部分 | `true` |

### 4.2 レート制限

`Channel::broadcastTrackerUpdate` は以下のレート制限を持つ:

```cpp
if (((ctime - lastTrackerUpdate) > 30) || (force))
```

- **最小送信間隔**: 30秒
- `force == true` の場合は30秒制限を無視

### 4.3 BCSTパケット構造（writeTrackerUpdateAtom）

トラッカーアップデートは `PCP_BCST` パケットとして構築される:

```
bcst (PKT, 10 children)
├── grp  (BYTE): PCP_BCST_GROUP_ROOT (0x01)  ← Root宛
├── hops (BYTE): 0
├── ttl  (BYTE): 7
├── from (ID):   自ノードのセッションID
├── vers (INT):  PCP_CLIENT_VERSION (1218)
├── vrvp (INT):  PCP_CLIENT_VERSION_VP (27)
├── vexp (RAW[2]): バージョン拡張プレフィックス ("YT")
├── vexn (SHORT): バージョン拡張番号 (50)
├── chan (PKT, 4 children):  ← チャンネル情報
│   ├── id   (ID):   チャンネルID (16バイト)
│   ├── bcid (ID):   ブロードキャストID (16バイト)
│   ├── info (PKT):  チャンネルメタデータ ← §4.4参照
│   └── trck (PKT):  トラック情報 ← §4.5参照
└── host (PKT): ホスト情報 ← §4.6参照
```

**重要**: `grp` フィールドが `PCP_BCST_GROUP_ROOT (0x01)` に設定されている。これにより、Root Server（YP）に対して送信されるブロードキャストであることが明示される。

### 4.4 チャンネルメタデータ（info サブコンテナ）

`ChanInfo::writeInfoAtoms` で構築:

```
info (PKT, 7〜9 children)
├── name (STR): チャンネル名
├── bitr (INT): ビットレート (kbps)
├── gnre (STR): ジャンル ※タグは "gnre"（"genr"ではない）
├── url  (STR): 関連URL
├── desc (STR): 説明
├── cmnt (STR): コメント（「DJメッセージ」相当）
├── type (STR): コンテンツタイプ文字列 ("FLV", "MKV", "MP3" 等)
├── styp (STR): ストリームMIMEタイプ (任意、空でない場合のみ送信)
└── sext (STR): ストリーム拡張子 (任意、空でない場合のみ送信)
```

### 4.5 トラック情報（trck サブコンテナ）

`ChanInfo::writeTrackAtoms` で構築:

```
trck (PKT, 4 children)
├── titl (STR): 曲名/タイトル
├── crea (STR): アーティスト/作成者
├── url  (STR): 連絡先URL
└── albm (STR): アルバム名
```

### 4.6 ホスト情報（host サブコンテナ）

`ChanHit::writeAtoms` で構築。自ノードの配信状態を詳細に報告する:

```
host (PKT, 13〜19 children)
├── cid  (ID):     チャンネルID ※chanIDが設定されている場合のみ
├── id   (ID):     セッションID
├── ip   (INT/RAW[16]): グローバルIPアドレス (rhost[0])
├── port (SHORT):  グローバルポート (rhost[0])
├── ip   (INT/RAW[16]): ローカルIPアドレス (rhost[1])
├── port (SHORT):  ローカルポート (rhost[1])
├── numl (INT):    リスナー数
├── numr (INT):    リレー数
├── uptm (INT):    稼働時間 (秒)
├── ver  (INT):    プロトコルバージョン
├── vevp (INT):    VPバージョン
├── vexp (RAW[2]): 拡張バージョンプレフィックス ※versionExNumber != 0 の場合のみ
├── vexn (SHORT):  拡張バージョン番号 ※同上
├── flg1 (BYTE):   ステータスフラグ (§4.7参照)
├── oldp (INT):    最古ストリーム位置 (バイトオフセット)
├── newp (INT):    最新ストリーム位置 (バイトオフセット)
├── upip (INT/RAW[16]): 上流ホストIP ※uphost.ip が設定されている場合のみ
├── uppt (INT):    上流ホストポート ※同上
└── uphp (INT):    上流ホストまでのホップ数 ※同上
```

### 4.7 flg1 フラグの構成

トラッカーアップデートにおける `flg1` フラグは `ChanHit::initLocal` で初期化される:

| ビット | マスク | フラグ名 | Tracker Update時の値 |
|:---|:---|:---|:---|
| 0 | `0x01` | `TRACKER` | **常に1** (トラッカーノードとして明示的に設定) |
| 1 | `0x02` | `RELAY` | リレースロットが空いていれば1 |
| 2 | `0x04` | `DIRECT` | ダイレクト接続が可能なら1 |
| 3 | `0x08` | `PUSH` | ファイアウォール越し（FW_OFF以外）なら1 |
| 4 | `0x10` | `RECV` | ストリーム受信中（`isPlaying()`）なら1 |
| 5 | `0x20` | `CIN` | CIN接続に空きがあれば1 |
| 6 | `0x40` | `PRIVATE` | 未設定 (initLocal では設定されない) |

**重要**: `hit.tracker = true` が `writeTrackerUpdateAtom` 内で明示的に設定される。これにより `flg1` のビット0が立ち、このノードがトラッカー（配信元）であることがサーバに伝わる。

---

## 5. サーバ（Root/YP）側の受信処理

### 5.1 パケットディスパッチ

Root Server は `PCPStream::readPacket` → `PCPStream::procAtom` でパケットを処理する。

BCSTパケットの処理フローは以下の通り:

```
procAtom()
  └→ readBroadcastAtoms()       # BCSTヘッダ (grp, from, ttl等) のパース
       ├→ readChanAtoms()        # chanアトムの処理
       │   ├→ readInfoAtoms()    # info子コンテナ → ChanInfo更新
       │   └→ readTrackAtoms()   # trck子コンテナ → TrackInfo更新
       └→ readHostAtoms()        # hostアトムの処理 → ChanHit追加/更新
```

### 5.2 チャンネル情報の保存（readChanAtoms）

`PCPStream::readChanAtoms` の処理:

1. `bcs.chanID` からチャンネルを検索 (`chanMgr->findChannelByID`, `findHitListByID`)
2. 各サブアトムをパース:
   - `PCP_CHAN_ID`: チャンネルIDの読み取り → チャンネル再検索
   - `PCP_CHAN_BCID`: ブロードキャストIDの読み取り
   - `PCP_CHAN_INFO`: `ChanInfo::readInfoAtoms` で情報読み取り
   - `PCP_CHAN_TRACK`: `ChanInfo::readTrackAtoms` でトラック情報読み取り
3. ヒットリストが存在しない場合、`chanMgr->addHitList(newInfo)` で新規作成
4. `chl->info.update(newInfo)` でチャンネル情報を更新

### 5.3 ChanInfo::update のバリデーション

`ChanInfo::update` は以下のバリデーションを行う:

1. **ID検証**: `info.id` が未設定なら更新拒否
2. **名前検証**: `info.name` が空なら更新拒否
3. **BCIDの整合性チェック**: 既存の `bcID` が設定されている場合、新しい情報の `bcID` と一致しなければ `"ChanInfo BC key not valid"` エラーで更新拒否

> **実装上の注意**: BCIDは初回設定以降、変更不可。これはチャンネルの「所有権」の簡易的な検証として機能する。別のクライアントが同じチャンネルIDで異なるBCIDを使ってチャンネル情報を上書きすることはできない。

### 5.4 ホスト情報の保存（readHostAtoms）

`PCPStream::readHostAtoms` の処理:

1. `ChanHit` を初期化し、各サブアトムから情報を読み取る
2. `hit.host = hit.rhost[0]` でプライマリアドレスを設定
3. `hit.chanID = chanID` でチャンネルIDを紐付け
4. `hit.numHops = bcs.numHops` でホップ数を設定
5. **条件分岐**:
   - `hit.recv == true` → `chanMgr->addHit(hit)` でヒット追加/更新
   - `hit.recv == false` → `chanMgr->delHit(hit)` でヒット削除

`ChanMgr::addHit` は:
1. `chanID` でヒットリストを検索
2. 見つからなければ `addHitList` で新規作成
3. `ChanHitList::addHit` で既存ヒットの更新 or 新規追加

### 5.5 ChanHitList::addHit の重複処理

ヒットの同一性は以下の基準で判定される:

1. **IPアドレス+ポートの一致**: `rhost[0]` と `rhost[1]` の両方が一致する場合、既存ヒットを上書き更新
2. **セッションIDの一致**: IPが変わった場合でも、同じセッションIDを持つ古いヒットは削除され、新しいヒットが追加される

自身のセッションIDと同じヒットは追加されない (`servMgr->sessionID.isSame(h.sessionID)` チェック)。

---

## 6. 生存確認（Keep-alive）メカニズム

### 6.1 クライアント側（配信者）

チャンネル掲載の維持は以下の複合的なメカニズムで実現される:

#### 6.1.1 定期的なTracker Update送信

| 送信元 | 間隔 | 詳細 |
|:---|:---|:---|
| `Channel::readStream` | 120秒 | ストリーム読み取りループ内、`lastTrackerUpdate` に基づくチェック |
| `Channel::broadcastTrackerUpdate` | 最小30秒 | 内部レート制限。forceフラグで上書き可能 |

#### 6.1.2 Root Server からの更新要求

Root Server は CIN接続確立後、定期的に `root > upd` アトムを送信する:

- **初回**: ハンドシェイク完了直後（`processIncomingPCP` 内）
- **定期的**: `ServMgr::idleProc` が `hostUpdateInterval` 秒間隔で `broadcastRootSettings(true)` を呼び出す

クライアントは `root > upd` を受信すると `chanMgr->broadcastTrackerUpdate(remoteID, true)` を呼び出し、即座にチャンネル情報を再送する。

#### 6.1.3 nextRootPacket タイムアウト

クライアントは `root > next` アトムで受信した「次のRootパケット予定時刻」を監視する:

```cpp
if (sv->pcpStream->nextRootPacket)
    if (sys->getTime() > (sv->pcpStream->nextRootPacket + 30))
        error = PCP_ERROR_NOROOT;
```

Root Server からのパケットが予定時刻+30秒以内に届かなければ、`PCP_ERROR_NOROOT` で切断→再接続を試みる。

### 6.2 サーバ側（Root/YP）

#### 6.2.1 Dead Hit 削除

`ChanMgr::clearDeadHits`（`idleProc` から500ms間隔で呼び出し）は、一定時間更新のないヒットを削除する:

- **ヒットタイムアウト**: 180秒（`clearDeadHits` のハードコード定数 `interval`）
- `hit.time`（最終更新時刻）と現在時刻の差が180秒を超えたヒットは削除
- ヒットが0件になり、かつ自分がそのチャンネルの配信者でなく、チャンネルオブジェクトも存在しない場合、ヒットリスト自体が削除される

#### 6.2.2 Root Settings の定期ブロードキャスト

```cpp
// ServMgr::idleProc
if (servMgr->isRoot)
{
    if ((ctime - lastRootBroadcast) > chanMgr->hostUpdateInterval)
    {
        servMgr->broadcastRootSettings(true);
        lastRootBroadcast = ctime;
    }
}
```

`broadcastRootSettings` は接続中のすべてのCINサーバントに対して以下のBCSTパケットを送信する:

```
bcst (PKT, 9 children)
├── grp  (BYTE): PCP_BCST_GROUP_TRACKERS (0x02)
├── hops (BYTE): 0
├── ttl  (BYTE): 7
├── from (ID):   サーバのセッションID
├── vers (INT):  PCP_CLIENT_VERSION
├── vrvp (INT):  PCP_CLIENT_VERSION_VP
├── vexp (RAW[2]): バージョン拡張プレフィックス
├── vexn (SHORT): バージョン拡張番号
└── root (PKT):  ← writeRootAtoms() の内容（§3.4と同一構造）
```

---

## 7. チャンネル掲載の終了

### 7.1 正常終了

配信停止時、クライアントは以下を送信する:

1. **最終 Tracker Update**: `broadcastTrackerUpdate(GnuID(), true)` で最終状態を送信（`recv = false` = `isPlaying()` が false）
2. **QUIT パケット**: `quit (INT): PCP_ERROR_QUIT + PCP_ERROR_OFFAIR (1008)`

`recv == false` のホスト情報を受信したRoot Server は `chanMgr->delHit(hit)` でヒットを削除する。

### 7.2 異常切断

TCP接続が切断された場合:
- Root Server 側の `PCPStream::readPacket` が `StreamException` ("Send too slow" 等) またはEOFを検出
- そのノードのヒットはタイムアウト（180秒）後に自動削除される

### 7.3 サーバ起因の切断

Root Server が以下のエラーコードでQUITを送信する場合がある:

| コード | 定数 | 意味 |
|:---|:---|:---|
| 1002 | `QUIT + ALREADYCONNECTED` | 同一セッションIDの接続が既存 |
| 1003 | `QUIT + UNAVAILABLE` | CIN接続数が上限 |
| 1007 | `QUIT + BADAGENT` | クライアントバージョンが不十分 |
| 1008 | `QUIT + OFFAIR` | サーバが非配信状態（非Root時のみ） |
| 1009 | `QUIT + SHUTDOWN` | サーバがシャットダウン中 |

---

## 8. PeerCast特有のHTTPヘッダ

PCPプロトコル接続自体にはHTTPヘッダは使用されないが、リレー接続やストリーム配信のHTTP over PCP接続では以下のPeerCast固有ヘッダが使用される（参考情報）:

| ヘッダ | 定数名 | 用途 |
|:---|:---|:---|
| `x-peercast-pcp:` | `PCX_HS_PCP` | PCP対応フラグ（1=IPv4, 100=IPv6） |
| `x-peercast-pos:` | `PCX_HS_POS` | ストリーム位置 |
| `x-peercast-channelid:` | `PCX_HS_CHANNELID` | チャンネルID |
| `x-peercast-sessionid:` | `PCX_HS_SESSIONID` | セッションID |
| `x-peercast-os:` | `PCX_HS_OS` | OS種別 |
| `x-peercast-port:` | `PCX_HS_PORT` | リスニングポート |
| `x-peercast-remoteip:` | `PCX_HS_REMOTEIP` | リモートIPアドレス |
| `x-peercast-id:` | `PCX_HS_ID` | ノードID |
| `x-peercast-msg:` | `PCX_HS_MSG` | メッセージ |
| `x-peercast-pingme:` | `PCX_HS_PINGME` | Pingリクエスト |

> **注**: これらはYP登録プロトコルでは直接使用されない。YP登録はPCPバイナリプロトコルで完結する。

---

## 9. 主要データ構造

### 9.1 ChanInfo

チャンネルのメタデータを保持する構造体。

| フィールド | 型 | PCPタグ | 説明 |
|:---|:---|:---|:---|
| `name` | `String` | `name` | チャンネル名 |
| `id` | `GnuID` | `id` | チャンネルID (16バイト) |
| `bcID` | `GnuID` | `bcid` | ブロードキャストID（所有権検証キー） |
| `bitrate` | `int` | `bitr` | ビットレート (kbps) |
| `contentType` | `String` | `type` | コンテンツ種別 ("FLV", "MKV"等) |
| `MIMEType` | `String` | `styp` | MIME タイプ |
| `streamExt` | `String` | `sext` | ストリーム拡張子 |
| `genre` | `String` | `gnre` | ジャンル |
| `desc` | `String` | `desc` | 説明 |
| `url` | `String` | `url` | 関連URL |
| `comment` | `String` | `cmnt` | コメント/DJメッセージ |
| `track` | `TrackInfo` | - | トラック情報 (子構造体) |
| `createdTime` | `unsigned int` | - | 作成時刻 (ローカル管理用) |

### 9.2 TrackInfo

| フィールド | 型 | PCPタグ | 説明 |
|:---|:---|:---|:---|
| `title` | `String` | `titl` | 曲名 |
| `artist` | `String` | `crea` | アーティスト |
| `contact` | `String` | `url` | 連絡先URL |
| `album` | `String` | `albm` | アルバム名 |

### 9.3 ChanHit

リレーネットワーク上のノード情報を保持する構造体。

| フィールド | 型 | PCPタグ | 説明 |
|:---|:---|:---|:---|
| `host` | `Host` | - | プライマリアドレス (= rhost[0]) |
| `rhost[0]` | `Host` | `ip`+`port` (1組目) | グローバルアドレス |
| `rhost[1]` | `Host` | `ip`+`port` (2組目) | ローカルアドレス |
| `sessionID` | `GnuID` | `id` | ノードのセッションID |
| `chanID` | `GnuID` | `cid` | チャンネルID |
| `numListeners` | `unsigned int` | `numl` | リスナー数 |
| `numRelays` | `unsigned int` | `numr` | リレー数 |
| `upTime` | `unsigned int` | `uptm` | 稼働時間 (秒) |
| `version` | `unsigned int` | `ver` | プロトコルバージョン |
| `tracker` | `bool` | `flg1` bit0 | トラッカーフラグ |
| `recv` | `bool` | `flg1` bit4 | 受信中フラグ |
| `relay` | `bool` | `flg1` bit1 | リレー可能フラグ |
| `direct` | `bool` | `flg1` bit2 | ダイレクト接続可能フラグ |
| `firewalled` | `bool` | `flg1` bit3 | ファイアウォール越しフラグ |
| `cin` | `bool` | `flg1` bit5 | CIN接続可能フラグ |
| `time` | `unsigned int` | - | 最終更新時刻 (ローカル管理) |

### 9.4 ChanHitList

チャンネルごとのヒット（ノード）リストを管理するリンクリスト構造。

| フィールド | 型 | 説明 |
|:---|:---|:---|
| `info` | `ChanInfo` | このリストに対応するチャンネル情報 |
| `hit` | `shared_ptr<ChanHit>` | ヒットのリンクリスト先頭 |
| `lastHitTime` | `unsigned int` | 最終ヒット受信時刻 |
| `used` | `bool` | リスト使用中フラグ |

---

## 10. 実装上の注意事項

### 10.1 BCSTパケットのルーティング

- Tracker Update のBCSTは `grp = PCP_BCST_GROUP_ROOT (0x01)` で送信される
- Root Server 側で `PCP_BCST_GROUP_ROOT` を含むパケットを受信した場合、`broadcastPacketUp` と `broadcastPacket(T_COUT)` で上流・COUTへ転送される
- Root Server が `procAtom` で `PCP_ROOT` アトムを受信した場合、`isRoot == true` なら `"Unauthorized root message"` として例外をスローする（Root同士の循環防止）

### 10.2 チャンネル情報の更新通知の伝搬

チャンネルのメタデータ変更時（`Channel::updateInfo`）、配信者は:
1. **リレー向け**: `PCP_BCST_GROUP_RELAYS (0x04)` でチャンネル情報をブロードキャスト
2. **Root/YP向け**: `broadcastTrackerUpdate(GnuID())` で Tracker Update を送信

この2段階の通知により、リレーツリーとYPサーバの両方がメタデータの変更を受け取る。

### 10.3 IP アドレス更新

クライアントはOLEHの `rip` フィールドで自身のグローバルIPを学習し、以降のTracker Update に反映する。IPアドレスの変更が検出されると、即座にTracker Updateが送信される。

### 10.4 ファイアウォール越しノードの扱い

- ファイアウォール判定はHELOの `ping` ポートへの接続テストで行われる
- テスト失敗時（またはFW_ON判定時）、`rhost[0].port = 0` が設定される
- Root Server は `flg1` の `PUSH` ビットでファイアウォール状態を認識する

### 10.5 PCP_CONNECT 初期バイト列の解釈

TCP接続の最初のバイト列は以下の固定形式:

```
70 63 70 0a    # "pcp\n" (タグ)
04 00 00 00    # データ長 = 4
01 00 00 00    # バージョン = 1
```

合計12バイト。これは `PCPStream::readVersion` で読み取られ、バージョン値はログ出力のみに使用される（現時点ではバージョン値による動作分岐はない）。

### 10.6 文字列のNull終端

PCPプロトコルの文字列(`STR`型)はNull終端。`AtomStream::writeString` はNull文字を含む長さを書き込み、`readString` は読み取り後にNull文字を除去する。

### 10.7 BCST内のアトム順序

`readBroadcastAtoms` ではBCSTのヘッダアトム（`grp`, `ttl`, `hops`, `from` 等）を先に処理し、それ以外のアトム（`chan`, `host` 等）はコピー＆処理のパイプラインで逐次処理される。ヘッダアトムの出現順序に厳密な制約は無いが、ペイロードアトムより前に配置されることが期待される。
