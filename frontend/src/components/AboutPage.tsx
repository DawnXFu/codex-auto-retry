import { useState } from "react";
import type { ManagementSnapshot } from "../types";
import { S } from "../strings";

export default function AboutPage(props: { snapshot: ManagementSnapshot | null }) {
  const snap = props.snapshot;
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
      <h1>{S.about.title}</h1>
      <div className="card">
        <div className="kv" style={{ padding: "8px 14px" }}>
          <span className="k">{S.about.version}</span>
          <span>{snap ? `v${snap.version}` : S.about.unknown}</span>
        </div>
        <div className="kv" style={{ padding: "8px 14px" }}>
          <span className="k">{S.about.daemonPid}</span>
          <span>
            {snap ? `— · ${snap.memory_usage_mb} MB / ${snap.memory_limit_mb} MB` : S.about.unknown}
          </span>
        </div>
        <div className="kv" style={{ padding: "8px 14px" }}>
          <span className="k">{S.about.dataDir}</span>
          <span>{S.about.unknown}</span>
        </div>
        <div className="kv" style={{ padding: "8px 14px" }}>
          <span className="k">{S.about.log}</span>
          <span>daemon.log</span>
        </div>
      </div>
      <div style={{ marginTop: 12 }}>
        <button className="btn ghost">{S.about.openLog}</button>{" "}
        <button className="btn ghost" onClick={() => void copyDiag()}>
          {copied ? S.about.copied : S.about.copyDiag}
        </button>
      </div>
    </section>
  );
}
