import { useState } from "react";
import type { ManagementSnapshot } from "../types";
import { useI18n } from "../i18n";

export default function AboutPage(props: { snapshot: ManagementSnapshot | null }) {
  const snap = props.snapshot;
  const { t } = useI18n();
  const [copied, setCopied] = useState(false);

  const copyDiag = async () => {
    try {
      await navigator.clipboard.writeText(JSON.stringify(snap ?? {}, null, 2));
      setCopied(true);
      window.setTimeout(() => setCopied(false), 2000);
    } catch {
      // clipboard unavailable (non-secure context) — ignore
    }
  };

  return (
    <section className="page">
      <h1>{t("about_title")}</h1>
      <div className="card">
        <div className="kv" style={{ padding: "8px 14px" }}>
          <span className="k">{t("about_version")}</span>
          <span>{snap ? `v${snap.version}` : t("about_unknown")}</span>
        </div>
        <div className="kv" style={{ padding: "8px 14px" }}>
          <span className="k">{t("about_daemon_pid")}</span>
          <span>
            {snap ? `— · ${snap.memory_usage_mb} MB / ${snap.memory_limit_mb} MB` : t("about_unknown")}
          </span>
        </div>
        <div className="kv" style={{ padding: "8px 14px" }}>
          <span className="k">{t("about_data_dir")}</span>
          <span>{t("about_unknown")}</span>
        </div>
        <div className="kv" style={{ padding: "8px 14px" }}>
          <span className="k">{t("about_log")}</span>
          <span>daemon.log</span>
        </div>
      </div>
      <div style={{ marginTop: 12 }}>
        <button className="btn ghost">{t("about_open_log")}</button>{" "}
        <button className="btn ghost" onClick={() => void copyDiag()}>
          {copied ? t("about_copied") : t("about_copy_diag")}
        </button>
      </div>
    </section>
  );
}
