# PeerCastStation との互換性ノート

PeerCastStation のソースコードと比較して peercast-mi が合わせている動作・差異の記録。実装の根拠 (rationale) として残す。

対応ファイル: `internal/relay/client.go`, `internal/channel/content.go`, `internal/channel/channel.go`, `internal/servent/pcp.go`, `internal/servent/http.go`

## リレークライアント (RelayClient)

### 再接続ロジック

#### バックオフの削除

PeerCastStation は再接続時に delay=0 で即座にリトライし、接続可能なホストがなくなったら停止する (NoHost)。peercast-mi は当初 5s→120s の指数バックオフで待機していたが、PeerCastStation に合わせて即時再接続 + ホスト枯渇で停止に変更済み。

- 参照: `SourceStreamBase.StartConnection` → `OnConnectionStopped` → `args.Delay` (PCP では常に 0)
- 参照: `PCPSourceStream.SelectSourceHost` → null なら `DoStopStream(NoHost)`

#### OffAir / ConnectionError の再接続判断

PeerCastStation では tracker 以外のノードから OffAir や ConnectionError を受けた場合、そのノードを ignore して別ノードに再接続する。tracker から受けた場合のみ停止。peercast-mi は当初 OffAir を受けると一律停止していたため、中間リレーノードが落ちただけでリレーチェーン全体が切れていた。PeerCastStation に合わせて修正済み。

- 参照: `PCPSourceStream.OnConnectionStopped` (ConnectionError/OffAir で `connection.SourceUri != this.SourceUri` なら再接続)

#### `x-peercast-pos` でコンテンツ位置を送信

PeerCastStation は再接続時に `Channel.ContentPosition` (最新パケット末尾のバイト位置) を送り、上流が途中からデータを送れるようにしている。peercast-mi は当初常に 0 を送信していたため、再接続のたびに先頭から受け直していた。`ContentPosition()` メソッドを追加し、handshake で送信するよう変更済み。

- 参照: `PCPSourceStream.ProcessRelayRequest` → `x-peercast-pos:{Channel.ContentPosition}`

### コンテンツバッファリング

#### 再接続時のバッファクリア

PeerCastStation は `AddSourceStream()` で `contentHeader = null` + `contents.Clear()` してからストリームを受信し直す。さらに `streamIndex` をインクリメントして旧ストリームのデータを `ContentCollection.Add()` 内で自動排除する。peercast-mi は当初リレークライアント再接続時にリングバッファをクリアしないため、ヘッダ変更後にストリーム位置が巻き戻ると古いデータと新しいデータが混在する可能性があった。`SetHeader` でリングバッファ (`count`, `headerPos`) をリセットするよう変更済み。新しいヘッダ受信時に旧ストリームのデータパケットが自動的に破棄される。

- 参照: `Channel.AddSourceStream` → `contentHeader = null; contents.Clear(); streamIndex++`
- 参照: `ContentCollection.Add()` → `content.Stream < item.Stream` なら旧ストリームとして除去

### 出力ストリームへのコンテンツ配信

#### ソース切断時の出力ストリーム通知

PeerCastStation は `RemoveSourceStream` → 全 sink に `OnStopped` 送信 → sink リストクリアという明示的な通知を行う。peercast-mi は当初リレークライアント再接続中もチャンネル・出力ストリームはそのまま残り、データが来なくなると stall timeout で自然に閉じていた。`Client.Run()` 終了時 (全ホスト枯渇・tracker OffAir) に `defer ch.CloseAll()` で全出力ストリームを即座に閉じるよう変更済み。再接続ループ中 (ホスト切り替え) では呼ばないため、素早い再接続時に視聴を継続できる利点は維持。

- 参照: `Channel.RemoveSourceStream` → `sinks` に `OnStopped` → `sinks` クリア

### その他

#### `Stop()` によるブロッキング I/O の中断

PeerCastStation は `CancellationToken` で handshake 中の読み書きを中断できる。peercast-mi の `Stop()` は当初 `stopCh` を閉じるだけで、`net.Dial` や `pcp.ReadAtom` のブロッキング I/O を直接中断するメカニズムがなかった。`context.Context` を導入し、`DialContext` で接続中のキャンセルに対応。接続確立後は context キャンセル時に `conn.Close()` してブロッキング読み取りを即座に中断するよう変更済み。

## PCP 出力ストリーム (PCPOutputStream)

### ハンドシェイク

#### ハンドシェイク後に PCP_OK を送信

PeerCastStation は helo/oleh 交換後、リレー受け入れ時に `PCP_OK (1)` を送信する。oleh 送信後に `pcp.NewIntAtom(pcp.PCPOK, 1)` を送信するよう変更済み (リレー受け入れ時のみ)。

- 参照: `PCPOutputStream.cs` DoHandshake → `stream.WriteAsync(new Atom(Atom.PCP_OK, (int)1))`

#### ハンドシェイクタイムアウト

PeerCastStation はハンドシェイク全体に 18 秒のタイムアウトを設けている。`handshake` の冒頭で `conn.SetDeadline(time.Now().Add(18*time.Second))` を設定し、return 時に解除するよう変更済み。

- 参照: `PCPOutputStream.cs` `PCPHandshakeTimeout = 18000` → `handshakeCT.CancelAfter`

#### ping 時のサイトローカルアドレス判定

PeerCastStation は ping 成功してもリモートアドレスが `IsSiteLocal()` の場合は `remote_port = 0` (ポート未開放扱い) にする。`isSiteLocal` (10/8, 172.16/12, 192.168/16, 169.254/16) を追加し、ping 判定時にサイトローカルなら remotePort = 0 のままにするよう変更済み。

- 参照: `PCPOutputStream.cs` OnHandshakePCPHelo → `remoteEndPoint.Address.IsSiteLocal()` なら `remote_port = 0`

#### チャンネル Status チェック

PeerCastStation はリレーリクエスト時に `channel.Status != SourceStreamStatus.Receiving` ならば 404 を返す。`handlePCPRelay` でチャンネル存在確認後に `ch.HasData()` を判定し、データ未受信なら HTTP 404 を返すよう変更済み。peercast-mi には明示的な SourceStreamStatus がないため、バッファにデータがあるかで判定。

- 参照: `PCPOutputStream.cs` Invoke → `channel.Status != SourceStreamStatus.Receiving` → NotFound

### リレー枠管理

#### リレー満杯時に 503 + HOST リスト + QUIT を返す

PeerCastStation は `MakeRelayable()` で空きを作れない場合、HTTP 503 を返し、helo/oleh 交換後に代替ホストリストを送信して `QUIT + UNAVAILABLE` で切断する。admission 判定を handshake 前に行い、満杯時は HTTP 503 を返して helo/oleh 交換後に自ノード情報を HOST として送信し `QUIT + UNAVAILABLE` で切断するよう変更済み。BAN リストは未実装 (peercast-mi に BAN 機構がないため)。

- 参照: `PCPOutputStream.cs` DoHandshake → `isRelayFull` 時に `SelectSourceHosts` → `SendHost` → `HandshakeErrorException(UnavailableError)`

#### 劣勢リレー接続の強制切断 (MakeRelayable)

PeerCastStation はリレー枠が満杯でも、firewalled またはリレー能力がない下流ノードを切断して枠を空ける。`Channel.MakeRelayable(maxRelays)` を追加し、`canAdmitRelay` で呼び出し、firewalled (remotePort == 0) な PCP 出力ストリームを 1 つ退出させる。`PCPOutputStream` は `helo.Ping` と `helo.Port` の結果を `remotePort` に保持し、`IsFirewalled()` で判定。

- 参照: `Channel.cs` MakeRelayable → `IsFirewalled || (IsRelayFull && LocalRelays < 1)` な sink を `OnStopped(UnavailableError)` で切断

### データ送信

#### Overflow (送信遅延) 検出

PeerCastStation はキューの先頭と新メッセージのタイムスタンプ差が 5 秒を超えると Overflow として `QUIT + SKIP` を送信して切断する。`Content` に `Timestamp` フィールドを追加し、`sendDataPackets` でバッファ内最古のパケットの経過時間が 5 秒を超えた場合に `PCPErrorQuit + PCPErrorSkip` を送信して切断するよう変更済み。

- 参照: `PCPOutputStream.cs` Enqueue → `(msg.Timestamp-nxtMsg.Timestamp).TotalMilliseconds > 5000` → Overflow → `SendTimeoutError`

#### 大きいコンテンツパケットの分割送信

PeerCastStation は 15KB を超えるコンテンツパケットを 15KB 単位に分割し、2番目以降に `Fragment` フラグを付けて送信する。`sendDataPackets` で 15KB ずつ分割して送信するよう変更済み。2番目以降のチャンクには元の `ContFlags` に `Fragment` フラグ (0x01) を **OR** 合成して付与する (当初は上書きしていたため InterFrame/AudioFrame など元のフラグが失われていた。PCS の `content.ContFlag | Fragment` に合わせて修正済み)。

- 参照: `PCPOutputStream.cs` CreateContentBodyPacket → `MaxBodyLength = 15*1024` で分割、`content.ContFlag | (i==0 ? None : Fragment)`

#### Host atom flags1 の slot 状態反映

PeerCastStation は Host atom の flags1 を組み立てる際、`IsRelayFull` なら `Relay` bit を立てず、`IsDirectFull` なら `Direct` bit を立てない。peercast-mi は当初 Relay bit を常時立て、`HasGlobalIP` があれば Direct bit を無条件で立てていたため、枠が満杯でも下流が「接続可能」と誤認する可能性があった。`HostAtomParams` に `RelayFull` / `DirectFull` を追加し、`PCPOutputStream` と YP bcst の双方で `Channel.IsRelayFull(maxRelays)` / `IsDirectFull(maxListeners)` の結果を渡すよう変更済み。

- 参照: `PCPOutputStream.cs` CreateHostPacket → `IsRelayFull ? None : Relay`, `IsDirectFull ? None : Direct`

#### リレー満杯時の代替ノード送出

PeerCastStation は relay full で接続を拒否する際、自ノードの Host atom だけでなく `SelectSourceHosts()` で選んだ最大 8 個の代替リレー候補を HOST atom として送信してから `QUIT+Unavailable` を送る。下流はこれを見て別ノードに接続し直せる。peercast-mi は当初自ノードの Host atom 1 つしか送出していなかったため、クライアントが再接続先を推測できなかった。`Channel` に bcst 経由で観測した Host atom を蓄積する `knownHosts` キャッシュ (最大 32 件、session ID でデデュープ) と `SelectSourceHosts(max)` メソッドを追加。`PCPOutputStream.forwardBcst` で受信した bcst の中に Host atom があれば `AddKnownHost` に登録。`sendRelayDenied` では自ノード Host → 代替ホスト最大 8 件 → QUIT の順で送出するよう変更済み。

- 参照: `PCPOutputStream.cs` DoHandshake → `isRelayFull` 時に `SelectSourceHosts(8)` を全 SendHost

#### ChannelInfo/Track の bcst 送信 (配信チャンネルのリレー)

PeerCastStation は `IsBroadcasting` かつヘッダー送信済みの場合、ChannelInfo/Track 変更時に下流に `PCP_BCST` で wrapped した `PCP_CHAN` を送信する。`wrapBcstIfBroadcasting` を追加し、`sendInfoUpdate`/`sendTrackUpdate` で `IsBroadcasting` 時に chan atom を bcst でラップして送信するよう変更済み。

- 参照: `PCPOutputStream.cs` SendRelayBody → `ChannelInfo`/`ChannelTrack` 時に `BcstChannelInfo()`

### シャットダウン

#### シャットダウン時に上流ノード情報を返す

PeerCastStation はシャットダウン等で下流ノードを切断する際、自分が接続していた上流ノードの情報を HOST として返す。これにより下流は直接上流に接続し直せる。`Channel` に `upstreamSessionID/upstreamIP/upstreamPort` を追加し、リレークライアントが handshake 成功時に oleh から session ID と remote endpoint を記録する。`sendUpstreamHostAndQuit` で上流情報を HOST atom として送信してから QUIT する。

- 参照: `PCPOutputStream.cs` BeforeQuitAsync → `StopReason.UserShutdown` 時に上流の `RemoteEndPoint`/`RemoteSessionID` を HOST として送信

## HTTP 出力ストリーム (HTTPOutputStream)

### ヘッダー変更時の挙動

PeerCastStation は HTTP ストリームでヘッダーが変更されると新しいヘッダーを送信してストリームを継続する。`HTTPOutputStream.run` で `headerCh` 受信時に新ヘッダーを書き込み、`sent` と `waitingForKeyframe` をリセットしてそのまま配信継続するよう変更済み。データパケット送信の直前にも非ブロッキングで `headerCh` を確認し、`SetHeader`+`Write` の競合で新ボディが旧ヘッダーのまま送出される事を防ぐ。

- 参照: `HTTPOutputStream.cs` StreamHandler → `ContentHeader` 時に `WriteAsync(packet.Content.Data)` で新ヘッダーを送信して継続

### Content の Timestamp ベース順序保証

PeerCastStation は HTTP ストリームで `content.Timestamp > sent.body.Timestamp` で順序を保証し、古いコンテンツの再送を防ぐ。`ContentBuffer.PacketsAfter(ref Content)` を追加し、`(Timestamp, Pos)` の辞書順で厳密に新しいパケットのみを返すよう変更。HTTPOutputStream は `pos` ではなく直前に送った `Content` を保持して次回の基準に使う。ヘッダー変更でバッファの位置空間が巻き戻っても、古いパケットを取りこぼさずかつ二重送信もしない。

- 参照: `HTTPOutputStream.cs` StreamHandler → `c.Timestamp > sent.Value.body.Timestamp || (同一 Timestamp && Position > sent.body.Position)`

## YP クライアント (YPClient)

対応ファイル: `internal/yp/client.go`

### ルートからの即時更新要求の処理

PeerCastStation は YP (root) から `PCP_ROOT` atom に `update` フィールドが付いて届いた場合、即座に bcst を再送してチャンネル情報を更新する。peercast-mi は当初 handshake 内で `sendImmediately` フラグを算出しつつも `_ = sendImmediately` で破棄しており、さらに handshake 完了後は YP からの atom を一切読まないため、セッション中に root が update を要求しても無視していた。handshake 完了後に YP からの atom を読むリーダー goroutine を追加し、`PCP_ROOT` で immediate フラグを検出したら bcst を即時送信、`PCP_QUIT` を受けたら接続を切断するよう変更済み。

- 参照: `PCPYellowPageClient.cs` OnPCPRoot → `RootPacket.Update != null` で即時 announce

### YP bcst Host atom の slot 状態反映

上記「Host atom flags1 の slot 状態反映」と同様に、YP への bcst に含める Host atom にも `RelayFull` / `DirectFull` を反映するため、`yp.New` に `maxRelays` / `maxListeners` 引数を追加し、`buildBcst` で `ch.IsRelayFull(c.maxRelays)` / `ch.IsDirectFull(c.maxListeners)` を `HostAtomParams` に渡すよう変更済み。
