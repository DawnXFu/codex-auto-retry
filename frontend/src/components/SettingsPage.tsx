import { useEffect, useState, type ReactNode } from "react";
import type { ManagementSnapshot } from "../types";
import { saveSettings, setPaused, setSharedAppServer } from "../api";
import { useI18n } from "../i18n";
import type { Lang } from "../strings";

// Settings draft: the retry-policy block the daemon's save-settings mode
// persists. Toggles with their own bound setters (pause, shared backend)
// write immediately; everything else lands via the Save button. The language
// preference is frontend-side (car.lang in localStorage) and switches live.
// Startup approval has no write path — shown as a read-only toggle.
interface Draft {
  retry_prompt: string;
  max_recovery_attempts: number;
  max_consecutive_retries: number;
  auth_max_attempts: number;
  delay_strategy: string;
  initial_delay_seconds: number;
  max_delay_seconds: number;
  delay_increment_seconds: number;
  memory_limit_mb: number;
  show_notifications: boolean;
}

function draftFrom(snap: ManagementSnapshot): Draft {
  return {
    retry_prompt: snap.retry_prompt,
    max_recovery_attempts: snap.max_recovery_attempts,
    max_consecutive_retries: snap.max_consecutive_retries,
    auth_max_attempts: snap.auth_max_attempts,
    delay_strategy: snap.delay_strategy,
    initial_delay_seconds: snap.initial_delay_seconds,
    max_delay_seconds: snap.max_delay_seconds,
    delay_increment_seconds: snap.delay_increment_seconds,
    memory_limit_mb: snap.memory_limit_mb,
    show_notifications: snap.show_notifications,
  };
}

export default function SettingsPage(props: {
  snapshot: ManagementSnapshot | null;
  onRefresh?: () => void;
}) {
  const snap = props.snapshot;
  const { t, tr, lang, setLang } = useI18n();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [dirty, setDirty] = useState(false);
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);

  // Keep the form synced to the daemon until the user edits it; a dirty form
  // is never clobbered by the 3s snapshot poll.
  useEffect(() => {
    if (snap && !dirty) setDraft(draftFrom(snap));
  }, [snap, dirty]);

  const patch = (p: Partial<Draft>) => {
    setDraft((cur) => (cur ? { ...cur, ...p } : cur));
    setDirty(true);
  };

  const flash = (msg: string, ms = 5000) => {
    setNote(msg);
    window.setTimeout(() => setNote(null), ms);
  };

  const errText = (reason: string, fallbackKey: "settings_err_failed") =>
    tr(`settings_err_${reason}`, t(fallbackKey, { error: reason }));

  const refreshSoon = () => {
    props.onRefresh?.();
    window.setTimeout(() => props.onRefresh?.(), 1500);
  };

  const toggleAutoResume = async (enabled: boolean) => {
    setBusy(true);
    const res = await setPaused(!enabled);
    setBusy(false);
    if (res.ok) {
      refreshSoon();
    } else {
      flash(res.reason === "unwired" ? t("action_unavailable") : t("action_failed", { error: res.reason }));
    }
  };

  const toggleShared = async (enabled: boolean) => {
    setBusy(true);
    const res = await setSharedAppServer(enabled);
    setBusy(false);
    if (res.ok) {
      refreshSoon();
    } else {
      flash(errText(res.reason, "settings_err_failed"), 8000);
    }
  };

  const save = async () => {
    if (!snap || !draft) return;
    setBusy(true);
    const res = await saveSettings({
      retry_prompt: draft.retry_prompt,
      max_consecutive_retries: draft.max_consecutive_retries,
      max_recovery_attempts: draft.max_recovery_attempts,
      auth_max_attempts: draft.auth_max_attempts,
      delay_strategy: draft.delay_strategy,
      initial_delay_seconds: draft.initial_delay_seconds,
      max_delay_seconds: draft.max_delay_seconds,
      delay_increment_seconds: draft.delay_increment_seconds,
      memory_limit_mb: draft.memory_limit_mb,
      show_notifications: draft.show_notifications,
      paused: snap.paused,
    });
    setBusy(false);
    if (res.ok) {
      setDirty(false);
      flash(t("settings_saved"), 3000);
      refreshSoon();
    } else {
      flash(errText(res.reason, "settings_err_failed"), 8000);
    }
  };

  const reset = () => {
    if (snap) setDraft(draftFrom(snap));
    setDirty(false);
  };

  const num = (v: string) => {
    const n = Number(v);
    return Number.isFinite(n) ? Math.trunc(n) : 0;
  };

  return (
    <section className="page">
      <h1>{t("settings_title")}</h1>
      <div className="card fields">
        <Field name={t("settings_auto_resume")} desc={t("settings_auto_resume_desc")}>
          <Toggle checked={snap ? !snap.paused : false} disabled={!snap || busy} onChange={(v) => void toggleAutoResume(v)} />
        </Field>
        <Field name={t("settings_shared")} desc={t("settings_shared_desc", { port: snap?.shared_app_server_port ?? 49621 })}>
          <Toggle
            checked={snap?.shared_app_server_requested ?? snap?.shared_app_server_enabled ?? false}
            disabled={!snap || busy}
            onChange={(v) => void toggleShared(v)}
          />
        </Field>
        <Field name={t("settings_notify")} desc={t("settings_notify_desc")}>
          <Toggle
            checked={draft?.show_notifications ?? true}
            disabled={!draft}
            onChange={(v) => patch({ show_notifications: v })}
          />
        </Field>
        <Field name={t("settings_boot")} desc={t("settings_boot_desc")}>
          <Toggle checked={snap?.startup_approved === "approved"} disabled />
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
          <input
            type="text"
            style={{ width: 110 }}
            value={draft?.retry_prompt ?? ""}
            disabled={!draft}
            onChange={(e) => patch({ retry_prompt: e.target.value })}
          />
        </Field>
        <Field name={t("settings_rec_max")}>
          <input
            type="number"
            value={draft?.max_recovery_attempts ?? 0}
            disabled={!draft}
            onChange={(e) => patch({ max_recovery_attempts: num(e.target.value) })}
          />
        </Field>
        <Field name={t("settings_no_max")}>
          <input
            type="number"
            value={draft?.max_consecutive_retries ?? 0}
            disabled={!draft}
            onChange={(e) => patch({ max_consecutive_retries: num(e.target.value) })}
          />
        </Field>
        <Field name={t("settings_auth_max")}>
          <input
            type="number"
            value={draft?.auth_max_attempts ?? 0}
            disabled={!draft}
            onChange={(e) => patch({ auth_max_attempts: num(e.target.value) })}
          />
        </Field>
        <Field name={t("settings_delay")}>
          <select
            value={draft?.delay_strategy ?? "linear"}
            disabled={!draft}
            onChange={(e) => patch({ delay_strategy: e.target.value })}
          >
            <option value="exponential">{t("strategy_exponential")}</option>
            <option value="fixed">{t("strategy_fixed")}</option>
            <option value="linear">{t("strategy_linear")}</option>
          </select>
        </Field>
        <Field name={t("settings_init_delay")}>
          <input
            type="number"
            value={draft?.initial_delay_seconds ?? 0}
            disabled={!draft}
            onChange={(e) => patch({ initial_delay_seconds: num(e.target.value) })}
          />
        </Field>
        <Field name={t("settings_max_delay")}>
          <input
            type="number"
            value={draft?.max_delay_seconds ?? 0}
            disabled={!draft}
            onChange={(e) => patch({ max_delay_seconds: num(e.target.value) })}
          />
        </Field>
        <Field name={t("settings_delay_step")}>
          <input
            type="number"
            value={draft?.delay_increment_seconds ?? 0}
            disabled={!draft}
            onChange={(e) => patch({ delay_increment_seconds: num(e.target.value) })}
          />
        </Field>
        <Field name={t("settings_mem_limit")}>
          <input
            type="number"
            value={draft?.memory_limit_mb ?? 0}
            disabled={!draft}
            onChange={(e) => patch({ memory_limit_mb: num(e.target.value) })}
          />
        </Field>
      </div>

      <div style={{ marginTop: 12 }}>
        <button className="btn primary" disabled={!draft || busy} onClick={() => void save()}>
          {busy ? t("settings_saving") : t("settings_save")}
        </button>{" "}
        <button className="btn ghost" disabled={!draft || busy} onClick={reset}>
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

// Minimal shadcn-style switch (controlled).
function Toggle(props: { checked: boolean; disabled?: boolean; onChange?: (v: boolean) => void }) {
  return (
    <label className="switch">
      <input
        type="checkbox"
        checked={props.checked}
        disabled={props.disabled}
        onChange={(e) => props.onChange?.(e.target.checked)}
      />
      <span className="slider" />
    </label>
  );
}
