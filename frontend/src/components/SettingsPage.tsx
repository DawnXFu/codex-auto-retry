import { useState, type ReactNode } from "react";
import type { ManagementSnapshot } from "../types";
import { useI18n } from "../i18n";
import type { Lang } from "../strings";

// Presentational for now — T5 wires writes. Values reflect the snapshot.
// The language preference is the exception: it is frontend-side (car.lang in
// localStorage) and switches live, so it is fully wired here.
export default function SettingsPage(props: { snapshot: ManagementSnapshot | null }) {
  const snap = props.snapshot;
  const { t, lang, setLang } = useI18n();
  const [note, setNote] = useState<string | null>(null);

  const pending = () => {
    setNote(t("settings_pending_note"));
    window.setTimeout(() => setNote(null), 3000);
  };

  return (
    <section className="page">
      <h1>{t("settings_title")}</h1>
      <div className="card fields">
        <Field name={t("settings_auto_resume")} desc={t("settings_auto_resume_desc")}>
          <Toggle checked={snap ? !snap.paused : true} />
        </Field>
        <Field name={t("settings_shared")} desc={t("settings_shared_desc", { port: snap?.shared_app_server_port ?? 49621 })}>
          <Toggle checked={snap?.shared_app_server_enabled ?? false} />
        </Field>
        <Field name={t("settings_notify")} desc={t("settings_notify_desc")}>
          <Toggle checked={snap?.show_notifications ?? true} />
        </Field>
        <Field name={t("settings_boot")} desc={t("settings_boot_desc")}>
          <Toggle checked={snap?.startup_approved === "approved"} />
        </Field>
        <Field name={t("settings_lang")} desc={t("settings_lang_desc")}>
          <select value={lang} onChange={(e) => setLang(e.target.value as Lang)}>
            <option value="zh">{t("settings_lang_zh")}</option>
            <option value="en">{t("settings_lang_en")}</option>
          </select>
        </Field>
        <Field name={t("settings_tz")} desc={t("settings_tz_desc")}>
          <select defaultValue="sys">
            <option value="sys">{t("settings_tz_system")}</option>
            <option value="utc">UTC</option>
          </select>
        </Field>
      </div>

      <h2>{t("settings_retry_policy")}</h2>
      <div className="card fields">
        <Field name={t("settings_prompt")}>
          <input type="text" style={{ width: 110 }} defaultValue={snap?.retry_prompt ?? t("settings_prompt_default")} />
        </Field>
        <Field name={t("settings_rec_max")}>
          <input type="number" defaultValue={snap?.max_recovery_attempts ?? 15} />
        </Field>
        <Field name={t("settings_no_max")}>
          <input type="number" defaultValue={snap?.max_consecutive_retries ?? 5} />
        </Field>
        <Field name={t("settings_auth_max")}>
          <input type="number" defaultValue={snap?.auth_max_attempts ?? 3} />
        </Field>
        <Field name={t("settings_delay")}>
          <select defaultValue={snap?.delay_strategy ?? "linear"}>
            <option value="exponential">{t("strategy_exponential")}</option>
            <option value="fixed">{t("strategy_fixed")}</option>
            <option value="linear">{t("strategy_linear")}</option>
          </select>
        </Field>
        <Field name={t("settings_init_delay")}>
          <input type="number" defaultValue={snap?.initial_delay_seconds ?? 30} />
        </Field>
        <Field name={t("settings_max_delay")}>
          <input type="number" defaultValue={snap?.max_delay_seconds ?? 900} />
        </Field>
        <Field name={t("settings_delay_step")}>
          <input type="number" defaultValue={snap?.delay_increment_seconds ?? 60} />
        </Field>
        <Field name={t("settings_mem_limit")}>
          <input type="number" defaultValue={snap?.memory_limit_mb ?? 1024} />
        </Field>
      </div>

      <div style={{ marginTop: 12 }}>
        <button className="btn primary" onClick={pending}>
          {t("settings_save")}
        </button>{" "}
        <button className="btn ghost" onClick={pending}>
          {t("settings_reset")}
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
