import type { ReactNode } from "react";
import { useI18n } from "../i18n";
import type { StringKey } from "../strings";
import type { ManagementSnapshot } from "../types";
import { BrandMark, IconAbout, IconDashboard, IconSettings } from "../icons";
import DaemonVitals from "./DaemonVitals";

export type Page = "dashboard" | "settings" | "about";

const ITEMS: { page: Page; icon: ReactNode; key: StringKey }[] = [
  { page: "dashboard", icon: <IconDashboard size={15} />, key: "nav_dashboard" },
  { page: "settings", icon: <IconSettings size={15} />, key: "nav_settings" },
  { page: "about", icon: <IconAbout size={15} />, key: "nav_about" },
];

export default function Sidebar(props: {
  page: Page;
  onNav: (p: Page) => void;
  snapshot: ManagementSnapshot | null;
}) {
  const { t } = useI18n();
  return (
    <aside className="sidebar">
      <div className="brand">
        <BrandMark size={16} />
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
        <DaemonVitals snapshot={props.snapshot} />
        <div className="sidebar-ver">
          {props.snapshot?.version ? `v${props.snapshot.version}` : ""}
        </div>
      </div>
    </aside>
  );
}
