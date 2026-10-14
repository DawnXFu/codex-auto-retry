import { useState } from "react";
import { useConnection, useNow, useSnapshot } from "./hooks";
import { S } from "./strings";
import Sidebar, { type Page } from "./components/Sidebar";
import Dashboard from "./components/Dashboard";
import SettingsPage from "./components/SettingsPage";
import AboutPage from "./components/AboutPage";

export default function App() {
  const conn = useConnection();
  const { snapshot, online, hasData, refresh } = useSnapshot(conn);
  const now = useNow(1000);
  const [page, setPage] = useState<Page>("dashboard");

  const offline = conn.mode !== "preview" && hasData && !online;
  const noConn = conn.mode === "none";

  return (
    <>
      <Sidebar page={page} onNav={setPage} running={snapshot?.running ?? false} version={snapshot?.version} />
      <main className="main">
        {(offline || noConn) && (
          <div className="banner">
            {noConn ? `${S.offline.connecting} — ${S.offline.hint}` : S.offline.banner}
          </div>
        )}
        {page === "dashboard" && <Dashboard snapshot={snapshot} now={now} onRefresh={refresh} />}
        {page === "settings" && <SettingsPage snapshot={snapshot} />}
        {page === "about" && <AboutPage snapshot={snapshot} />}
      </main>
    </>
  );
}
