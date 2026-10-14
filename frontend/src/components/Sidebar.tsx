import { useI18n } from "../i18n";
import type { StringKey } from "../strings";

export type Page = "dashboard" | "settings" | "about";

const ITEMS: { page: Page; icon: string; key: StringKey }[] = [
  { page: "dashboard", icon: "📊", key: "nav_dashboard" },
  { page: "settings", icon: "⚙️", key: "nav_settings" },
  { page: "about", icon: "ℹ️", key: "nav_about" },
];

export default function Sidebar(props: {
  page: Page;
  onNav: (p: Page) => void;
  running: boolean;
  version?: string;
}) {
  const { t } = useI18n();
  return (
    <aside className="sidebar">
      <div className="brand">
        <span className="dot" style={props.running ? undefined : { background: "var(--red)" }} />
        {t("app_name")}
      </div>
      {ITEMS.map((it) => (
        <div
          key={it.page}
          className={`nav-item${props.page === it.page ? " active" : ""}`}
          onClick={() => props.onNav(it.page)}
        >
          {it.icon} <span>{t(it.key)}</span>
        </div>
      ))}
      <div className="sidebar-foot">
        {props.running ? t("sidebar_running") : t("sidebar_stopped")}
        <br />
        {props.version ? `v${props.version}` : ""}
      </div>
    </aside>
  );
}
