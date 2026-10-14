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
import { LangProvider } from "../src/i18n";
import { buildPreviewSnapshot } from "../src/fixture";
import { strings, type Lang } from "../src/strings";

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

const snap = buildPreviewSnapshot();
const now = new Date();

const states = new Set(snap.retries.map((r) => r.state));
const required = ["pending", "starting", "running", "waiting_for_reset", "reset_due", "needs_attention", "stopped"];
const missing = required.filter((s) => !states.has(s as never));
if (missing.length) fail("fixture missing states: " + missing.join(", "));

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
  for (const needle of ["queue-item", "win-bar", "live-dot", "pulse-meta", dict.stats_quota_wait]) {
    if (!dash.includes(needle)) fail(`${lang}: dashboard output missing: ${needle}`);
  }
  const settings = renderToString(
    createElement(LangProvider, { initial: lang }, createElement(SettingsPage, { snapshot: snap })),
  );
  for (const needle of ["field", "switch", dict.settings_save]) {
    if (!settings.includes(needle)) fail(`${lang}: settings output missing: ${needle}`);
  }
  const about = renderToString(
    createElement(LangProvider, { initial: lang }, createElement(AboutPage, { snapshot: snap })),
  );
  if (!about.includes("v" + snap.version)) fail(`${lang}: about missing version`);
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
  createElement(LangProvider, { initial: "en" }, createElement(SettingsPage, { snapshot: snap })),
);
for (const [name, html] of [["dashboard", enDash], ["settings", enSettings]] as const) {
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
