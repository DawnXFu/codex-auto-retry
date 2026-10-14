import { useMemo, useState } from "react";
import { resolveConnection } from "./api";
import { useNow, useSnapshot } from "./hooks";
import { useI18n } from "./i18n";
import Sidebar, { type Page } from "./components/Sidebar";
import Dashboard from "./components/Dashboard";
import SettingsPage from "./components/SettingsPage";
import AboutPage from "./components/AboutPage";

export default function App() {
  const conn = useMemo(() => resolveConnection(), []);
  const { snapshot, online, hasData, refresh } = useSnapshot(conn);
  const now = useNow(1000);
  const [page, setPage] = useState<Page>("dashboard");
  const { t } = useI18n();

  const offline = conn.mode !== "preview" && hasData && !online;
  const noConn = conn.mode === "none";

  return (
    <>
      <Sidebar page={page} onNav={setPage} running={snapshot?.running ?? false} version={snapshot?.version} />
      <main className="main">
        {(offline || noConn) && (
          <div className="banner">
            {noConn ? `${t("offline_connecting")} — ${t("offline_hint")}` : t("offline_banner")}
          </div>
        )}
        {page === "dashboard" && <Dashboard snapshot={snapshot} now={now} onRefresh={refresh} />}
        {page === "settings" && <SettingsPage snapshot={snapshot} />}
        {page === "about" && <AboutPage snapshot={snapshot} />}
      </main>
    </>
  );
}
