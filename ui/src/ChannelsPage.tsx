import { useCallback, useEffect, useState } from "react";
import {
  bumpChannel,
  getChannels,
  stopChannel,
  type ChannelEntry,
} from "./api";

function formatUptime(seconds: number): string {
  if (!seconds) return "-";
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  if (h > 0) return `${h}h${m}m${s}s`;
  if (m > 0) return `${m}m${s}s`;
  return `${s}s`;
}

export function ChannelsPage() {
  const [entries, setEntries] = useState<ChannelEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setEntries(await getChannels());
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload();
    const timer = setInterval(() => void reload(), 5000);
    return () => clearInterval(timer);
  }, [reload]);

  const selected = entries.find((e) => e.channelId === selectedId) ?? null;

  const onStop = async (channelId: string) => {
    if (!confirm("Stop this channel?")) return;
    try {
      await stopChannel(channelId);
      await reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const onBump = async (channelId: string) => {
    try {
      await bumpChannel(channelId);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <section>
      <header className="page-header">
        <h2>Channels</h2>
        <button onClick={reload} disabled={loading}>
          {loading ? "Loading..." : "Reload"}
        </button>
      </header>

      {error && <div className="error">{error}</div>}

      <table className="data-table">
        <thead>
          <tr>
            <th>Name</th>
            <th>Status</th>
            <th>Type</th>
            <th>Bitrate</th>
            <th>Listeners</th>
            <th>Relays</th>
            <th>Uptime</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {entries.length === 0 && !loading && (
            <tr>
              <td colSpan={8} className="empty">
                No channels.
              </td>
            </tr>
          )}
          {entries.map((c) => (
            <tr
              key={c.channelId}
              onClick={() => setSelectedId(c.channelId)}
              className={c.channelId === selectedId ? "selected" : ""}
            >
              <td>{c.info.name || "(unnamed)"}</td>
              <td>
                {c.status.status}
                {c.status.isBroadcasting ? " / broadcasting" : ""}
              </td>
              <td>{c.info.contentType}</td>
              <td>{c.info.bitrate} kbps</td>
              <td>
                {c.status.localDirects} / {c.status.totalDirects}
              </td>
              <td>
                {c.status.localRelays} / {c.status.totalRelays}
              </td>
              <td>{formatUptime(c.status.uptime)}</td>
              <td onClick={(e) => e.stopPropagation()}>
                <button onClick={() => onBump(c.channelId)}>Bump</button>{" "}
                <button onClick={() => onStop(c.channelId)}>Stop</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {selected && (
        <div className="detail-panel">
          <h3>Detail</h3>
          <dl>
            <dt>Channel ID</dt>
            <dd className="mono">{selected.channelId}</dd>
            <dt>Source</dt>
            <dd className="mono">{selected.status.source}</dd>
            <dt>Genre</dt>
            <dd>{selected.info.genre || "-"}</dd>
            <dt>Description</dt>
            <dd>{selected.info.desc || "-"}</dd>
            <dt>Comment</dt>
            <dd>{selected.info.comment || "-"}</dd>
            <dt>URL</dt>
            <dd>{selected.info.url || "-"}</dd>
            <dt>Track</dt>
            <dd>
              {selected.track.creator || selected.track.title
                ? `${selected.track.creator} - ${selected.track.title}`
                : "-"}
            </dd>
            <dt>Flags</dt>
            <dd>
              {selected.status.isReceiving ? "receiving " : ""}
              {selected.status.isRelayFull ? "relay-full " : ""}
              {selected.status.isDirectFull ? "direct-full " : ""}
            </dd>
          </dl>
        </div>
      )}
    </section>
  );
}
