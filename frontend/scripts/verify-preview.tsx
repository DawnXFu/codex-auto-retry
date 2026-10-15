// Verification: SSR-render Dashboard with the preview fixture and assert every
// retry state tag renders, in both languages, plus zh/en dict key parity.
// Run via:
//   npx vite build --ssr scripts/verify-preview.tsx --outDir dist-verify
//   node dist-verify/verify-preview.js
import { renderToString } from "react-dom/server";
import { createElement } from "react";
import Dashboard from "../src/components/Dashboard";
import SettingsPage from "../src/components/SettingsPage";
import AboutPage from "../src/components/AboutPage";
import Sidebar from "../src/components/Sidebar";
import { LangProvider } from "../src/i18n";
import { buildPreviewSnapshot } from "../src/fixture";
import { interpolate, strings, type Lang } from "../src/strings";

function fail(msg: string): never {
  console.error(msg);
  process.exit(1);
}

// dict parity: zh and en must cover identical key sets
const zhKeys = Object.keys(strings.zh).sort();
const enKeys = Object.keys(strings.en).sort();
if (zhKeys.join() !== enKeys.join()) {
  const onlyZh = zhKeys.filter((k) => !enKeys.includes(k));
  const onlyEn = enKeys.filter((k) => !zhKeys.includes(k));
  fail(`dict key parity failed — zh-only: ${onlyZh.join(",")} en-only: ${onlyEn.join(",")}`);
}
console.log(`dict parity OK — ${zhKeys.length} keys per language`);
if (zhKeys.concat(enKeys).some((k) => k.startsWith("quota_relimit"))) {
  fail("quota_relimit keys present in dicts");
}

const snap = buildPreviewSnapshot();
const now = new Date();

const states = new Set(snap.retries.map((r) => r.state));
const required = ["pending", "starting", "running", "waiting_for_reset", "reset_due", "needs_attention", "stopped"];
const missing = required.filter((s) => !states.has(s as never));
if (missing.length) fail("fixture missing states: " + missing.join(", "));

const quotaLeft = snap.quota
  ? Math.max(0, Math.min(100, Math.round(100 - snap.quota.used_percent)))
  : 0;

for (const lang of ["zh", "en"] as Lang[]) {
  const dict = strings[lang];
  const dash = renderToString(
    createElement(
      LangProvider,
      { initial: lang },
      createElement(Dashboard, { snapshot: snap, now, onRefresh: () => {} }),
    ),
  );
  const missingTags = required.filter((s) => !dash.includes(dict[`queue_tag_${s}` as keyof typeof dict]));
  if (missingTags.length) fail(`${lang}: dashboard did not render tags for: ${missingTags.join(", ")}`);
  for (const needle of [
    "queue-item",
    "win-bar",
    dict.stats_quota_wait,
    interpolate(dict.quota_left, { pct: quotaLeft }),
    interpolate(dict.quota_waiting, { n: snap.quota?.waiting ?? 0 }),
  ]) {
    if (!dash.includes(needle)) fail(`${lang}: dashboard output missing: ${needle}`);
  }
  if (/relimit/i.test(dash)) fail(`${lang}: dashboard renders a relimit budget`);

  for (const snapshot of [snap, null] as const) {
    const sidebar = renderToString(
      createElement(
        LangProvider,
        { initial: lang },
        createElement(Sidebar, { page: "dashboard", onNav: () => {}, snapshot }),
      ),
    );
    const needles = [
      "vitals",
      "live-dot",
      "vitals-stats",
      dict.daemon_title,
      dict.daemon_bg_note,
      `aria-label="${dict.daemon_restart}"`,
      dict.nav_dashboard,
    ];
    if (snapshot) {
      needles.push(interpolate(dict.daemon_memory, { mb: snapshot.memory_usage_mb.toFixed(1) }));
    } else {
      needles.push("disabled");
    }
    for (const needle of needles) {
      if (!sidebar.includes(needle))
        fail(`${lang}: sidebar(snapshot=${snapshot ? "preview" : "null"}) output missing: ${needle}`);
    }
  }

  const settings = renderToString(
    createElement(
      LangProvider,
      { initial: lang },
      createElement(SettingsPage, { snapshot: snap, theme: "system", setTheme: () => {} }),
    ),
  );
  for (const needle of [
    "field",
    "switch",
    dict.settings_save,
    'value="system"',
    'value="light"',
    'value="dark"',
    dict.theme_system,
    dict.theme_light,
    dict.theme_dark,
  ]) {
    if (!settings.includes(needle)) fail(`${lang}: settings output missing: ${needle}`);
  }
  const about = renderToString(
    createElement(LangProvider, { initial: lang }, createElement(AboutPage, { snapshot: snap })),
  );
  for (const needle of [
    "v" + snap.version,
    String(snap.daemon_pid),
    `${snap.memory_limit_mb} MB`,
    snap.desktop_transport,
    dict.daemon_auto_start_on,
  ]) {
    if (!about.includes(needle)) fail(`${lang}: about output missing: ${needle}`);
  }
}

// The prototype's bug class: after switching to en, no zh string may leak —
// assert rendered output contains no CJK outside allowed exceptions:
// fixture task titles (data, not UI copy) and the native language-name option.
const enDash = renderToString(
  createElement(
    LangProvider,
    { initial: "en" },
    createElement(Dashboard, { snapshot: snap, now, onRefresh: () => {} }),
  ),
);
const enSettings = renderToString(
  createElement(
    LangProvider,
    { initial: "en" },
    createElement(SettingsPage, { snapshot: snap, theme: "system", setTheme: () => {} }),
  ),
);
const enSidebar = renderToString(
  createElement(
    LangProvider,
    { initial: "en" },
    createElement(Sidebar, { page: "dashboard", onNav: () => {}, snapshot: snap }),
  ),
);
for (const [name, html] of [["dashboard", enDash], ["settings", enSettings], ["sidebar", enSidebar]] as const) {
  const scrubbed = html
    .replace(/title="[^"]*"/g, "") // fixture titles are zh data, not UI copy
    .replace(/value="[^"]*"/g, "") // retry_prompt comes from daemon config (data)
    .replace(/<span class="t">[^<]*<\/span>/g, "")
    .replaceAll(`>${strings.zh.settings_lang_zh}<`, "><") // language name stays native
    .replaceAll(`>${strings.zh.settings_lang}<`, "><"); // label is bilingual by design
  if (/\p{Script=Han}/u.test(scrubbed)) {
    const m = scrubbed.match(/.{0,60}\p{Script=Han}.{0,60}/u);
    fail(`en ${name} leaks CJK in UI chrome near: ${m?.[0]}`);
  }
}
console.log("en render OK — no CJK leaks outside fixture data / native lang names");

console.log("preview render OK — states:", [...states].sort().join(", "));
