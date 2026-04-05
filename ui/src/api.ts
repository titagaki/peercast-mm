// Minimal JSON-RPC 2.0 client for peercast-mi.
//
// The backend listens on port 7144 bound to 127.0.0.1. During `vite dev` the
// UI itself is served from a different port (5173 by default), so requests go
// cross-origin; peercast-mi already sets Access-Control-Allow-Origin: *.

const ENDPOINT =
  (import.meta.env.VITE_PEERCAST_ENDPOINT as string | undefined) ??
  "http://127.0.0.1:7144/api/1";

export class RpcError extends Error {
  code: number;
  constructor(code: number, message: string) {
    super(message);
    this.code = code;
  }
}

let nextId = 1;

export async function rpc<T = unknown>(
  method: string,
  params: unknown[] = [],
): Promise<T> {
  const id = nextId++;
  const res = await fetch(ENDPOINT, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", method, params, id }),
  });
  if (!res.ok) {
    throw new Error(`HTTP ${res.status} ${res.statusText}`);
  }
  const body = await res.json();
  if (body.error) {
    throw new RpcError(body.error.code, body.error.message);
  }
  return body.result as T;
}

// ---------------------------------------------------------------------------
// Typed wrappers — one per method the UI uses.
// ---------------------------------------------------------------------------

export type StreamKeyEntry = {
  accountName: string;
  streamKey: string;
};

export type ChannelInfo = {
  name: string;
  url: string;
  genre: string;
  desc: string;
  comment: string;
  bitrate: number;
  contentType: string;
  mimeType: string;
};

export type TrackInfo = {
  title: string;
  genre: string;
  album: string;
  creator: string;
  url: string;
};

export type ChannelStatus = {
  status: string;
  source: string;
  uptime: number;
  localRelays: number;
  localDirects: number;
  totalRelays: number;
  totalDirects: number;
  isBroadcasting: boolean;
  isRelayFull: boolean;
  isDirectFull: boolean;
  isReceiving: boolean;
};

export type ChannelEntry = {
  channelId: string;
  status: ChannelStatus;
  info: ChannelInfo;
  track: TrackInfo;
};

export const listStreamKeys = () => rpc<StreamKeyEntry[]>("listStreamKeys");
export const issueStreamKey = (accountName: string, streamKey: string) =>
  rpc<null>("issueStreamKey", [accountName, streamKey]);
export const revokeStreamKey = (accountName: string) =>
  rpc<null>("revokeStreamKey", [accountName]);
export const getChannels = () => rpc<ChannelEntry[]>("getChannels");
export const stopChannel = (channelId: string) =>
  rpc<null>("stopChannel", [channelId]);
export const bumpChannel = (channelId: string) =>
  rpc<null>("bumpChannel", [channelId]);
