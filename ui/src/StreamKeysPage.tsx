import { useCallback, useEffect, useState } from "react";
import {
  issueStreamKey,
  listStreamKeys,
  revokeStreamKey,
  type StreamKeyEntry,
} from "./api";

// Generates a reasonably unique stream key. Purely client-side — the server
// trusts whatever string the caller passes to issueStreamKey.
function generateStreamKey(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join(
    "",
  );
  return `sk_${hex}`;
}

export function StreamKeysPage() {
  const [entries, setEntries] = useState<StreamKeyEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [newAccount, setNewAccount] = useState("");
  const [newKey, setNewKey] = useState(generateStreamKey());

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setEntries(await listStreamKeys());
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  const onIssue = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newAccount.trim()) return;
    try {
      await issueStreamKey(newAccount.trim(), newKey);
      setNewAccount("");
      setNewKey(generateStreamKey());
      await reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const onRevoke = async (accountName: string) => {
    if (!confirm(`Revoke stream key for "${accountName}"?`)) return;
    try {
      await revokeStreamKey(accountName);
      await reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <section>
      <header className="page-header">
        <h2>Stream Keys</h2>
        <button onClick={reload} disabled={loading}>
          {loading ? "Loading..." : "Reload"}
        </button>
      </header>

      {error && <div className="error">{error}</div>}

      <form className="issue-form" onSubmit={onIssue}>
        <input
          type="text"
          placeholder="account name"
          value={newAccount}
          onChange={(e) => setNewAccount(e.target.value)}
          required
        />
        <input
          type="text"
          value={newKey}
          onChange={(e) => setNewKey(e.target.value)}
          required
        />
        <button
          type="button"
          onClick={() => setNewKey(generateStreamKey())}
          title="Regenerate"
        >
          ↻
        </button>
        <button type="submit">Issue</button>
      </form>

      <table className="data-table">
        <thead>
          <tr>
            <th>Account</th>
            <th>Stream Key</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {entries.length === 0 && !loading && (
            <tr>
              <td colSpan={3} className="empty">
                No stream keys issued.
              </td>
            </tr>
          )}
          {entries.map((e) => (
            <tr key={e.accountName}>
              <td>{e.accountName}</td>
              <td className="mono">{e.streamKey}</td>
              <td>
                <button onClick={() => onRevoke(e.accountName)}>Revoke</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
