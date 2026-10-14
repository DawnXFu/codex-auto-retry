import { S } from "../strings";

export type Page = "dashboard" | "settings" | "about";

const ITEMS: { page: Page; icon: string; label: string }[] = [
  { page: "dashboard", icon: "📊", label: S.nav.dashboard },
  { page: "settings", icon: "⚙️", label: S.nav.settings },
  { page: "about", icon: "ℹ️", label: S.nav.about },
];

export default function Sidebar(props: {
  page: Page;
  onNav: (p: Page) => void;
  running: boolean;
  version?: string;
}) {
  return (
    <aside className="sidebar">
      <div className="brand">
        <span className="dot" style={props.running ? undefined : { background: "var(--red)" }} />
        {S.appName}
      </div>
      {ITEMS.map((it) => (
        <div
          key={it.page}
          className={`nav-item${props.page === it.page ? " active" : ""}`}
          onClick={() => props.onNav(it.page)}
        >
          {it.icon} <span>{it.label}</span>
        </div>
      ))}
      <div className="sidebar-foot">
        {props.running ? S.sidebarFoot.running : S.sidebarFoot.stopped}
        <br />
        {props.version ? `v${props.version}` : ""}
      </div>
    </aside>
  );
}
