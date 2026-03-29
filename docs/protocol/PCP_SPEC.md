# PeerCast Protocol (PCP) Specification v1.0

## 1. General Architecture
PCP is a binary protocol used for P2P streaming. It operates over TCP and uses a **Tag-Length-Value (TLV)** packet structure. The protocol is strictly **Little Endian**.

### 1.1 Byte Order
All multi-byte numbers (uint16, uint32) are encoded in **Little Endian**.
- Example: `0x1234` -> `[0x34, 0x12]`

### 1.2 Basic Packet Structure (Header)
Every atom consists of an 8-byte header followed by a variable-length payload.

| Offset | Type     | Name   | Description |
|:-------|:---------|:-------|:------------|
| 0      | [4]byte  | Tag    | ASCII identifier (e.g., "helo", "chan"). Case-sensitive. |
| 4      | uint32   | Length | **Dual-purpose field.** If bit 31 (`0x80000000`) is set, this is a **container atom**: lower 31 bits = number of child atoms, payload is zero data bytes. If bit 31 is clear, this is a **data atom**: value = byte length of the data payload. |
| 8      | []byte   | Payload| For data atoms: raw bytes. For container atoms: sequence of child atoms. |

---

## 2. Data Types
Definitions of data types used within the Payload.

| Type Name | Go Type | Size | Description |
|:---|:---|:---|:---|
| **BYTE** | `uint8` | 1 | Single byte integer. |
| **SHORT**| `uint16`| 2 | 2-byte integer (Little Endian). |
| **INT** | `uint32`| 4 | 4-byte integer (Little Endian). |
| **ID** | `[16]byte`| 16 | Unique Identifier (GUID/UUID). |
| **STR** | `string`| Var | Null-terminated string (`\0`). Copilot must handle stripping/adding `\0`. |
| **RAW** | `[]byte`| Var | Binary data stream. |
| **PKT** | `[]Packet`| Var | **Container Type**. The Length field has bit 31 set; the lower 31 bits are the number of child atoms. The payload is zero bytes (children follow immediately). |

---

## 3. Protocol Flow & Handshake

### 3.1 Connection Start
1. **Client** sends `pcp\n` atom (`0x70 0x63 0x70 0x0a`) as a data atom with Length=4 and a 4-byte INT payload containing the protocol version number.
2. **Server** reads and validates the `pcp\n` atom.
3. **Client** immediately sends a `helo` container atom.
4. **Server** replies with an `oleh` container atom.
5. After handshake, both sides exchange root-level atoms (e.g., `bcst`, `chan`, `host`).

---

## 4. Packet Dictionary & Hierarchy
PCP packets are categorized into **Containers** (holding sub-packets) and **Atoms** (holding data).

### 4.1 Root Level Commands
These atoms appear at the top level of the stream.

| Tag | Type | Payload Structure | Description |
|:---|:---|:---|:---|
| `helo` | **PKT** | See [4.2 HELO Structure] | Handshake: Node information exchange (outgoing). |
| `oleh` | **PKT** | Same as `helo` | Handshake reply (incoming). |
| `ok`   | INT  | Error code | Acknowledgment / positive reply. |
| `quit` | INT  | Error code | Disconnect notification. Code = `PCP_ERROR_QUIT`(1000) + reason. |
| `bcst` | **PKT** | See [4.4 BCST Structure] | Broadcast packet with routing header. |
| `push` | **PKT** | See [4.4 PUSH Structure] | Push relay request. |
| `chan` | **PKT** | See [4.3 CHAN Structure] | Channel data / metadata. |
| `host` | **PKT** | See [4.5 HOST Structure] | Host info for channel discovery. |
| `root` | **PKT** | See [4.6 ROOT Structure] | Root-server directives. |
| `get`  | **PKT** | `id`(ID) + `name`(STR) | Request a specific channel. |
| `mode` | **PKT** | `gn06`(ID4) | Protocol mode selection. |
| `atom` | **PKT** | Nested root atoms | Wrapper forwarding multiple root atoms. |
| `mesg` | STR  | UTF-8 text | Text message (broadcast). |
| `spkt` | - | (varies) | Stream packet (legacy). |

---

### 4.2 HELO / OLEH Structure (Handshake)
**Parent Tag:** `helo` or `oleh`
**Description:** Defines the node capabilities and session info.

| Sub-Tag | Type | Go Field Name | Description |
|:---|:---|:---|:---|
| `agnt` | STR   | `Agent`       | User Agent string. |
| `ostp` | INT   | `OsType`      | OS Type. Known values: `lnux`, `w32`, `osx`, `wamp`, `zaur`. |
| `sid`  | ID    | `SessionID`   | 16-byte random session ID per client. Wire: `{0x73,0x69,0x64,0x00}`. |
| `port` | SHORT | `ListenPort`  | The port number this node is listening on. |
| `ping` | SHORT | `PingPort`    | Request the remote to ping this port (firewall check). |
| `rip`  | INT or RAW[16] | `RemoteIP` | The sender's global IP as observed by the peer. 4 bytes = IPv4, 16 bytes = IPv6. |
| `ver`  | INT   | `Version`     | Client protocol version (e.g., 1218). |
| `bcid` | ID    | `BroadcastID` | ID of the channel being broadcast. |
| `dis`  | INT   | `Disable`     | If 1, the connection is rejected. |

> **Note:** Tags shorter than 4 bytes are padded with **null bytes** (`0x00`), not spaces.
> The `ID4` class initializes all 4 bytes to `0`; only the actual characters are written.
> Example: `"sid"` is stored as `{0x73, 0x69, 0x64, 0x00}` on the wire.

> **Note on `pong`:** `PCP_HELO_PONG = "pong"` is defined in the source as a HELO sub-atom (the response to a firewall ping test), **not** as a standalone root-level command.

---

### 4.3 CHAN Structure (Channel Data)
**Parent Tag:** `chan`
**Description:** Top-level channel container. Contains the channel ID and nested sub-containers.

#### 4.3.1 Direct sub-atoms of `chan`

| Sub-Tag | Type | Description |
|:---|:---|:---|
| `id`   | ID | Unique 16-byte Channel ID. |
| `bcid` | ID | Broadcast ID. |

#### 4.3.2 `info` sub-container (channel metadata)
**Parent Tag:** `info` (child of `chan`)

| Sub-Tag | Type | Description |
|:---|:---|:---|
| `name` | STR | Channel name. |
| `url`  | STR | Related website URL. |
| `desc` | STR | Channel description. |
| `cmnt` | STR | Comment. |
| `gnre` | STR | Content genre. (**`gnre`**, not `genr`.) |
| `type` | STR | Content type string (e.g., `"MP3"`, `"FLV"`, `"MKV"`). |
| `styp` | STR | Stream MIME type (e.g., `"video/x-flv"`). |
| `sext` | STR | Stream file extension (e.g., `".flv"`). |
| `bitr` | INT | Bitrate in kbps. |

#### 4.3.3 `trck` sub-container (track info)
**Parent Tag:** `trck` (child of `chan`)

| Sub-Tag | Type | Description |
|:---|:---|:---|
| `titl` | STR | Track title. |
| `crea` | STR | Track creator / artist. |
| `url`  | STR | Track URL / contact. |
| `albm` | STR | Album name. |

#### 4.3.4 `pkt` sub-container (stream packet)
**Parent Tag:** `pkt` (child of `chan`)

| Sub-Tag | Type | Description |
|:---|:---|:---|
| `type` | ID4  | Packet type: `head` (header) or `data` (media data). |
| `pos`  | INT  | Stream byte position / sequence number. |
| `cont` | BYTE | Continuation flag (non-zero if packet continues previous). |
| `data` | RAW  | Binary media data payload. |

---

### 4.4 BCST / PUSH Structure (Routing)

#### 4.4.1 `bcst` — Broadcast with routing header
**Description:** Wraps content atoms with routing/TTL metadata. After header atoms are parsed, remaining child atoms (e.g., `chan`, `host`) are processed recursively.

| Sub-Tag | Type | Description |
|:---|:---|:---|
| `ttl`  | BYTE | Time-to-live. Decremented at each hop; dropped when it reaches 0. |
| `hops` | BYTE | Hop count. Incremented at each relay. |
| `from` | ID   | Sender's session ID. Used for loop detection. |
| `dest` | ID   | Destination session ID. Empty/zero = broadcast to all matching nodes. |
| `grp`  | BYTE | Target group bitmask: `0x01`=root, `0x02`=trackers, `0x04`=relays, `0xFF`=all. |
| `cid`  | ID   | Target channel ID. |
| `vers` | INT  | Sender's protocol version. |
| `vrvp` | INT  | VP extension version. |
| `vexp` | RAW[2] | Extended version prefix (2 ASCII bytes). |
| `vexn` | SHORT | Extended version number. |

After the above header atoms, nested atoms such as `chan`, `host`, `push`, or `mesg` appear as children and are processed by the standard atom dispatcher.

#### 4.4.2 `push` — Push relay request
**Description:** Requests a node to initiate an outbound connection (for firewalled nodes).

| Sub-Tag | Type | Description |
|:---|:---|:---|
| `ip`   | INT or RAW[16] | Target host IP address. |
| `port` | SHORT | Target host port. |
| `cid`  | ID   | Channel ID the push is for. |

---

### 4.5 HOST Structure (Network Discovery)
**Parent Tag:** `host` (appears inside `bcst` or at root level)
**Description:** Describes a relay node's network address, capabilities, and status.

| Sub-Tag | Type | Description |
|:---|:---|:---|
| `id`   | ID            | Session ID (node identifier). |
| `ip`   | INT or RAW[16]| Host IP address. 4 bytes = IPv4, 16 bytes = IPv6. Multiple `ip`+`port` pairs may appear to advertise multiple addresses. |
| `port` | SHORT         | Port corresponding to the preceding `ip` atom. |
| `numl` | INT           | Number of connected listeners. (**`numl`**, not `num`.) |
| `numr` | INT           | Number of currently connected relay nodes. |
| `uptm` | INT           | Uptime in seconds. |
| `oldp` | INT           | Oldest available stream position (byte offset). |
| `newp` | INT           | Newest stream position (byte offset). |
| `cid`  | ID            | Channel ID this host is serving. |
| `flg1` | BYTE          | Status flags bitmask (see below). |
| `ver`  | INT           | Host protocol version. |
| `vevp` | INT           | VP extension version. |
| `vexp` | RAW[2]        | Extended version prefix. |
| `vexn` | SHORT         | Extended version number. |
| `trkr` | (INT)         | Non-zero if this node is a tracker. |
| `upip` | INT or RAW[16]| Upstream (parent) host IP. |
| `uppt` | INT           | Upstream host port. |
| `uphp` | INT           | Number of hops to upstream host. |

**`flg1` bit flags:**

| Bit | Mask   | Meaning |
|:----|:-------|:--------|
| 0   | `0x01` | `TRACKER` — this node is a tracker |
| 1   | `0x02` | `RELAY` — relay slots available |
| 2   | `0x04` | `DIRECT` — direct connections available |
| 3   | `0x08` | `PUSH` — node is firewalled (push required) |
| 4   | `0x10` | `RECV` — node is currently receiving the stream |
| 5   | `0x20` | `CIN` — node is a channel-in (source) servent |
| 6   | `0x40` | `PRIVATE` — private node |

---

### 4.6 ROOT Structure (Root Server Directives)
**Parent Tag:** `root` (only processed when node is **not** a root server)
**Description:** Directives sent by the root (tracker) server.

| Sub-Tag | Type | Description |
|:---|:---|:---|
| `uint` | INT  | Update interval in seconds. |
| `chkv` | INT  | Minimum required client version. |
| `url`  | STR  | Suffix for the PeerCast download URL. |
| `upd`  | (PKT)| Trigger an immediate tracker update broadcast. |
| `next` | INT  | Seconds until the next expected root packet. |

---

## 5. Error Codes

Error codes are sent as the INT payload of `quit` atoms and as components of internal error values.

| Constant | Value | Meaning |
|:---|:---|:---|
| `PCP_ERROR_QUIT` | 1000 | Base for quit errors |
| `PCP_ERROR_BCST` | 2000 | Base for broadcast errors |
| `PCP_ERROR_READ` | 3000 | Base for read errors |
| `PCP_ERROR_WRITE` | 4000 | Base for write errors |
| `PCP_ERROR_GENERAL` | 5000 | General error base |
| + `PCP_ERROR_SKIP` | +1 | Packet skipped |
| + `PCP_ERROR_ALREADYCONNECTED` | +2 | Already connected |
| + `PCP_ERROR_UNAVAILABLE` | +3 | Unavailable |
| + `PCP_ERROR_LOOPBACK` | +4 | Loopback detected |
| + `PCP_ERROR_NOTIDENTIFIED` | +5 | Not identified |
| + `PCP_ERROR_BADRESPONSE` | +6 | Bad response |
| + `PCP_ERROR_BADAGENT` | +7 | Bad user agent |
| + `PCP_ERROR_OFFAIR` | +8 | Channel is off-air |
| + `PCP_ERROR_SHUTDOWN` | +9 | Server shutdown |
| + `PCP_ERROR_NOROOT` | +10 | No root available |
| + `PCP_ERROR_BANNED` | +11 | Client is banned |

---

## 6. Implementation Requirements for Copilot

1.  **Container vs Data Atom Detection**:
    - Read the 4-byte tag and 4-byte length field.
    - If `length & 0x80000000 != 0`: container atom. Number of children = `length & 0x7FFFFFFF`. Parse that many child atoms recursively.
    - If `length & 0x80000000 == 0`: data atom. Read exactly `length` bytes as payload.

2.  **Unknown Tags**:
    - The library MUST handle unknown tags gracefully by calling `skip(numChildren, dataLen)`.
    - For unknown container atoms, recursively skip all children. For unknown data atoms, skip `dataLen` bytes.
    - Do not error on unknown tags.

3.  **Strings**:
    - When writing `STR` type, append a null byte (`0x00`); include it in the length.
    - When reading `STR` type, read `dataLen` bytes, then strip the trailing null byte.

4.  **Tag Padding**:
    - Tags are fixed 4 bytes. Short tag names are **zero-padded** (null bytes, `0x00`), not space-padded.
    - `"id"` encodes as `{0x69, 0x64, 0x00, 0x00}`.
    - `"sid"` encodes as `{0x73, 0x69, 0x64, 0x00}`.

5.  **IP Address Fields**:
    - `ip`/`rip`/`upip` atoms may be 4 bytes (IPv4) or 16 bytes (IPv6). Check `dataLen` at read time.
    - IPv6 addresses are stored in **reversed byte order** in the atom (the source calls `std::reverse` before write and after read).

6.  **HOST IP/Port pairing**:
    - In a `host` atom, `ip` and `port` sub-atoms come in pairs. Each `port` atom closes the preceding `ip`. Up to 2 pairs may appear (index 0 and 1).

7.  **Go Struct Tags**:
    - Use struct tags to map PCP tags to fields.
    - Example:
      ```go
      type HeloPacket struct {
          Agent     string    `pcp:"agnt"`
          SessionID [16]byte  `pcp:"sid"`  // zero-padded, not space-padded
          Port      uint16    `pcp:"port"`
      }
      ```