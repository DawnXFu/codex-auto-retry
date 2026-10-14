import { useState, type ReactNode } from "react";
import type { ManagementSnapshot } from "../types";
import { S } from "../strings";

// Presentational for now — T5 wires writes. Values reflect the snapshot.
export default function SettingsPage(props: { snapshot: ManagementSnapshot | null }) {
  const snap = props.snapshot;
  const [note, setNote] = useState<string | null>(null);

  const pending = () => {
    setNote(S.settings.pendingNote);
    window.setTimeout(() => setNote(null), 3000);
  };

  return (
    <section className="page">
      <h1>{S.settings.title}</h1>
      <div className="card fields">
        <Field name={S.settings.autoResume} desc={S.settings.autoResumeDesc}>
          <Toggle checked={snap ? !snap.paused : true} />
        </Field>
        <Field name={S.settings.shared} desc={S.settings.sharedDesc(snap?.shared_app_server_port ?? 49621)}>
          <Toggle checked={snap?.shared_app_server_enabled ?? false} />
        </Field>
        <Field name={S.settings.notify} desc={S.settings.notifyDesc}>
          <Toggle checked={snap?.show_notifications ?? true} />
        </Field>
        <Field name={S.settings.boot} desc={S.settings.bootDesc}>
          <Toggle checked={snap?.startup_approved === "approved"} />
        </Field>
        <Field name={S.settings.lang} desc={S.settings.langDesc}>
          <select defaultValue="zh">
            <option value="zh">简体中文</option>
            <option value="en">English</option>
          </select>
        </Field>
        <Field name={S.settings.tz} desc={S.settings.tzDesc}>
          <select defaultValue="sys">
            <option value="sys">{S.settings.tzSystem}</option>
            <option value="utc">UTC</option>
          </select>
        </Field>
      </div>

      <h2>{S.settings.retryPolicy}</h2>
      <div className="card fields">
        <Field name={S.settings.prompt}>
          <input type="text" style={{ width: 110 }} defaultValue={snap?.retry_prompt ?? "继续"} />
        </Field>
        <Field name={S.settings.recMax}>
          <input type="number" defaultValue={snap?.max_recovery_attempts ?? 15} />
        </Field>
        <Field name={S.settings.noMax}>
          <input type="number" defaultValue={snap?.max_consecutive_retries ?? 5} />
        </Field>
        <Field name={S.settings.authMax}>
          <input type="number" defaultValue={snap?.auth_max_attempts ?? 3} />
        </Field>
        <Field name={S.settings.delay}>
          <select defaultValue={snap?.delay_strategy ?? "linear"}>
            <option value="exponential">{S.settings.strategies.exponential}</option>
            <option value="fixed">{S.settings.strategies.fixed}</option>
            <option value="linear">{S.settings.strategies.linear}</option>
          </select>
        </Field>
        <Field name={S.settings.initDelay}>
          <input type="number" defaultValue={snap?.initial_delay_seconds ?? 30} />
        </Field>
        <Field name={S.settings.maxDelay}>
          <input type="number" defaultValue={snap?.max_delay_seconds ?? 900} />
        </Field>
        <Field name={S.settings.delayStep}>
          <input type="number" defaultValue={snap?.delay_increment_seconds ?? 60} />
        </Field>
        <Field name={S.settings.memLimit}>
          <input type="number" defaultValue={snap?.memory_limit_mb ?? 1024} />
        </Field>
      </div>

      <div style={{ marginTop: 12 }}>
        <button className="btn primary" onClick={pending}>
          {S.settings.save}
        </button>{" "}
        <button className="btn ghost" onClick={pending}>
          {S.settings.reset}
        </button>
      </div>
      {note && (
        <div className="banner" style={{ marginTop: 10 }}>
          {note}
        </div>
      )}
    </section>
  );
}

function Field(props: { name: string; desc?: string; children: ReactNode }) {
  return (
    <div className="field">
      <div>
        <div className="fname">{props.name}</div>
        {props.desc && <div className="fdesc">{props.desc}</div>}
      </div>
      {props.children}
    </div>
  );
}

// Minimal shadcn-style switch (uncontrolled — presentational until T5).
function Toggle(props: { checked: boolean }) {
  return (
    <label className="switch">
      <input type="checkbox" defaultChecked={props.checked} />
      <span className="slider" />
    </label>
  );
}
