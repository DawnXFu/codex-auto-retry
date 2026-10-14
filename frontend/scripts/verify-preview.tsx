// Verification: SSR-render Dashboard with the preview fixture and assert every
// retry state tag renders. Run via:
//   npx vite build --ssr scripts/verify-preview.tsx --outDir dist-verify
//   node dist-verify/verify-preview.js
import { renderToString } from "react-dom/server";
import { createElement } from "react";
import Dashboard from "../src/components/Dashboard";
import SettingsPage from "../src/components/SettingsPage";
import AboutPage from "../src/components/AboutPage";
import { buildPreviewSnapshot } from "../src/fixture";
import { S } from "../src/strings";

const snap = buildPreviewSnapshot();
const now = new Date();

const states = new Set(snap.retries.map((r) => r.state));
const required = ["pending", "starting", "running", "waiting_for_reset", "reset_due", "needs_attention", "stopped"];
const missing = required.filter((s) => !states.has(s as never));
if (missing.length) {
  console.error("fixture missing states:", missing.join(", "));
  process.exit(1);
}

const dash = renderToString(createElement(Dashboard, { snapshot: snap, now, onRefresh: () => {} }));
const missingTags = required.filter((s) => !dash.includes(S.queue.tags[s as keyof typeof S.queue.tags]));
if (missingTags.length) {
  console.error("dashboard did not render tags for:", missingTags.join(", "));
  process.exit(1);
}
for (const needle of ["queue-item", "win-bar", "live-dot", "pulse-meta", S.stats.quotaWait]) {
  if (!dash.includes(needle)) {
    console.error("dashboard output missing:", needle);
    process.exit(1);
  }
}

const settings = renderToString(createElement(SettingsPage, { snapshot: snap }));
const about = renderToString(createElement(AboutPage, { snapshot: snap }));
for (const needle of ["field", "switch", S.settings.save]) {
  if (!settings.includes(needle)) {
    console.error("settings output missing:", needle);
    process.exit(1);
  }
}
if (!about.includes("v" + snap.version)) {
  console.error("about missing version");
  process.exit(1);
}

console.log("preview render OK — states:", [...states].sort().join(", "));
