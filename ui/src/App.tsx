import { useState } from "react";
import "./App.css";
import { ChannelsPage } from "./ChannelsPage";
import { StreamKeysPage } from "./StreamKeysPage";

type Tab = "channels" | "streamKeys";

export default function App() {
  const [tab, setTab] = useState<Tab>("channels");

  return (
    <div className="app">
      <nav className="app-nav">
        <h1>peercast-mi</h1>
        <button
          className={tab === "channels" ? "active" : ""}
          onClick={() => setTab("channels")}
        >
          Channels
        </button>
        <button
          className={tab === "streamKeys" ? "active" : ""}
          onClick={() => setTab("streamKeys")}
        >
          Stream Keys
        </button>
      </nav>
      <main className="app-main">
        {tab === "channels" ? <ChannelsPage /> : <StreamKeysPage />}
      </main>
    </div>
  );
}
