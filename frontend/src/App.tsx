import { useState } from "react";
import { useConnection, useNow, useSnapshot } from "./hooks";
import { useI18n } from "./i18n";
import Titlebar from "./components/Titlebar";
import Sidebar, { type Page } from "./components/Sidebar";
import Dashboard from "./components/Dashboard";
import SettingsPage from "./components/SettingsPage";
import AboutPage from "./components/AboutPage";
import ConversationsPage from "./components/ConversationsPage";
import { useTheme } from "./theme";

export default function App() {
  const conn = useConnection();
  const { snapshot, online, hasData, refresh } = useSnapshot(conn);
  const now = useNow(1000);
  const [page, setPage] = useState<Page>("dashboard");
  const { t } = useI18n();
  const { theme, setTheme } = useTheme();

  const offline = conn.mode !== "preview" && hasData && !online;
  const noConn = conn.mode === "none";

  return (
    <>
      <Titlebar />
      <div className="app-body">
        <Sidebar page={page} onNav={setPage} snapshot={snapshot} />
        <main className="main">
          {(offline || noConn) && (
            <div className="banner" style={{ marginBottom: 14 }}>
              {noConn ? `${t("offline_connecting")} — ${t("offline_hint")}` : t("offline_banner")}
            </div>
          )}
          {page === "dashboard" && <Dashboard snapshot={snapshot} now={now} onRefresh={refresh} />}
          {page === "conversations" && <ConversationsPage conn={conn} now={now} />}
          {page === "settings" && (
            <SettingsPage snapshot={snapshot} onRefresh={refresh} theme={theme} setTheme={setTheme} />
          )}
          {page === "about" && <AboutPage snapshot={snapshot} />}
        </main>
      </div>
    </>
  );
}
