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
import ConversationsPage from "../src/components/ConversationsPage";
import { LangProvider } from "../src/i18n";
import {
  buildPreviewSnapshot,
  buildPreviewAccountQuota,
  buildPreviewConversations,
  type PreviewQuotaVariant,
} from "../src/fixture";
import { dataSource } from "../src/api";
import { interpolate, strings, type Lang } from "../src/strings";
import type { ManagementSnapshot } from "../src/types";

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

// The conversations page seeds itself synchronously from the fixture in
// preview mode, so SSR renders real list/detail output without effects.
const previewConvs = buildPreviewConversations("ordinary");
if (previewConvs.items.length === 0) fail("preview fixture has no ordinary conversations");
const previewThread = previewConvs.items[0];
const archivedOnly = buildPreviewConversations("archived");
if (archivedOnly.items.some((c) => !c.archived)) {
  fail("preview fixture leaked non-archived items into the archived filter");
}
const subagentsOnly = buildPreviewConversations("subagent");
if (subagentsOnly.items.some((c) => c.kind !== "subagent" || c.archived)) {
  fail("preview fixture leaked non-subagent items into the subagent filter");
}

// The data source is the single preview/live/none seam: pages consume it
// instead of branching on conn.mode or importing the fixture directly. The
// fixture adapter also exposes the synchronous seeds SSR relies on. Fetch
// assertions ride .then() chains — the fixture resolves synchronously and
// the SSR target does not allow top-level await.
{
  const previewSource = dataSource({ mode: "preview" });
  if (previewSource.kind !== "fixture") {
    fail("preview connection did not select the fixture data source");
  }
  const seededList = previewSource.seedConversations?.("ordinary");
  if (!seededList || seededList.items.length === 0) {
    fail("fixture source did not offer a synchronous list seed");
  }
  const seededDetail = previewSource.seedConversationItems?.(
    previewThread.root_id,
    previewThread.thread_id,
    { direction: "backward" },
  );
  if (!seededDetail || seededDetail.items.length === 0) {
    fail("fixture source did not offer a synchronous detail seed");
  }
  void previewSource.fetchConversations("ordinary").then((page) => {
    if (page.items.length === 0) fail("fixture source returned no conversations");
  });
  void previewSource
    .fetchConversationItems(previewThread.root_id, previewThread.thread_id, {
      direction: "backward",
    })
    .then((page) => {
      if (page.items.length === 0) fail("fixture source returned no detail items");
    });
  void previewSource.fetchSnapshot().then((snap) => {
    if (snap.retries.length === 0) fail("fixture source returned no retries");
  });
  const dead = dataSource({ mode: "none" });
  if (dead.kind !== "none") fail("unconnected mode did not select the none source");
  if (dead.seedConversations !== undefined || dead.seedConversationItems !== undefined) {
    fail("unconnected source offered a synchronous seed");
  }
  void dead.fetchSnapshot().then(
    () => fail("unconnected source resolved a snapshot"),
    () => undefined,
  );
}

// The fixture cycles account_quota variants on a wall-clock timer; pin the
// base render to the fresh variant so the assertions below stay
// deterministic. Its primary window is used_percent=100, so the legacy
// "0% left" needle still holds in account mode.
const baseSnap: ManagementSnapshot = { ...snap, account_quota: buildPreviewAccountQuota("fresh") };

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
      createElement(Dashboard, {
        snapshot: baseSnap,
        now,
        onRefresh: () => {},
        onOpenConversation: () => {},
      }),
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
    // account_quota mode (fresh fixture): both window labels, credits row,
    // exhausted tag, and the labeled source/freshness line.
    dict.quota_acct_win_5h,
    dict.quota_acct_win_week,
    dict.quota_acct_exhausted,
    dict.quota_acct_credits,
    interpolate(dict.quota_acct_credits_n, { n: 3 }),
    dict.quota_acct_src_account_read,
  ]) {
    if (!dash.includes(needle)) fail(`${lang}: dashboard output missing: ${needle}`);
  }
  if (/relimit/i.test(dash)) fail(`${lang}: dashboard renders a relimit budget`);

  // C-06 deep link: the backend stamps root_id on a retry whose rollout
  // resolves under a monitored root — exactly one fixture retry carries it
  // (thread ...0006). Its title renders as a button with the localized
  // open-conversation hint; the other titles stay plain spans.
  const tlinks = dash.match(/class="t tlink"/g) ?? [];
  if (tlinks.length !== 1) {
    fail(`${lang}: expected exactly one linked queue title, found ${tlinks.length}`);
  }
  const plainTitles = dash.match(/class="t"/g) ?? [];
  if (plainTitles.length !== snap.retries.length - 1) {
    fail(
      `${lang}: expected ${snap.retries.length - 1} plain queue titles, found ${plainTitles.length}`,
    );
  }
  if (!dash.includes(`title="${dict.queue_btn_open_conversation}"`)) {
    fail(`${lang}: linked queue title is missing its open-conversation hint`);
  }

  // Every account_quota state must render distinctly: stale source label,
  // rollout fallback + duration-unknown slot label + unavailable credits,
  // and the explicit unavailable state.
  const variantNeedles: Record<PreviewQuotaVariant, string[]> = {
    fresh: [], // covered by the base render above
    stale: [
      dict.quota_acct_src_account_read,
      dict.quota_acct_stale,
      interpolate(dict.quota_acct_credits_stale, { n: 1 }),
    ],
    rollout: [
      dict.quota_acct_src_rollout,
      dict.quota_acct_credits_unavailable,
      interpolate(dict.quota_acct_win_unknown, { slot: dict.quota_acct_slot_secondary }),
    ],
    unavailable: [
      dict.quota_acct_unavailable,
      dict.quota_acct_src_none,
      dict.quota_acct_credits_unavailable,
    ],
  };
  for (const variant of Object.keys(variantNeedles) as PreviewQuotaVariant[]) {
    if (!variantNeedles[variant].length) continue;
    const vSnap: ManagementSnapshot = { ...snap, account_quota: buildPreviewAccountQuota(variant) };
    const vDash = renderToString(
      createElement(
        LangProvider,
        { initial: lang },
        createElement(Dashboard, { snapshot: vSnap, now, onRefresh: () => {} }),
      ),
    );
    for (const needle of variantNeedles[variant]) {
      if (!vDash.includes(needle)) fail(`${lang}: dashboard(${variant}) output missing: ${needle}`);
    }
  }

  // Legacy fallback: no account_quota → the old single-window summary renders.
  const legacySnap: ManagementSnapshot = { ...snap };
  delete legacySnap.account_quota;
  const legacyDash = renderToString(
    createElement(
      LangProvider,
      { initial: lang },
      createElement(Dashboard, { snapshot: legacySnap, now, onRefresh: () => {} }),
    ),
  );
  for (const needle of [dict.quota_window, interpolate(dict.quota_left, { pct: quotaLeft })]) {
    if (!legacyDash.includes(needle)) fail(`${lang}: dashboard(legacy) output missing: ${needle}`);
  }
  // C-06: the link is driven by the retry's own root_id — strip it from
  // every retry and no title may render linked markup.
  const unlinkedSnap: ManagementSnapshot = {
    ...snap,
    retries: snap.retries.map((r) => ({ ...r, root_id: undefined })),
  };
  const unlinkedDash = renderToString(
    createElement(
      LangProvider,
      { initial: lang },
      createElement(Dashboard, {
        snapshot: unlinkedSnap,
        now,
        onRefresh: () => {},
        onOpenConversation: () => {},
      }),
    ),
  );
  if (/class="t tlink"/.test(unlinkedDash)) {
    fail(`${lang}: dashboard rendered a linked title without a root_id`);
  }

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

  // Conversations list: seeded from the preview fixture synchronously, so
  // SSR output carries the filter row and real rows.
  const convList = renderToString(
    createElement(
      LangProvider,
      { initial: lang },
      createElement(ConversationsPage, { conn: { mode: "preview" }, now }),
    ),
  );
  for (const needle of [
    dict.nav_conversations,
    dict.conv_filter_ordinary,
    dict.conv_filter_subagent,
    dict.conv_filter_archived,
    dict.conv_filter_all,
    "conv-item",
    "conv-list",
    dict.conv_last_active.split("{ago}")[0].trim(),
  ]) {
    if (!convList.includes(needle)) fail(`${lang}: conversations list output missing: ${needle}`);
  }
  if (!convList.includes(previewThread.title ?? "")) {
    fail(`${lang}: conversations list did not render fixture title`);
  }

  // Conversations detail: the deep-link prop seeds the newest persisted
  // page — messages, collapsed tool rows, and boundary markers render.
  const convDetail = renderToString(
    createElement(
      LangProvider,
      { initial: lang },
      createElement(ConversationsPage, {
        conn: { mode: "preview" },
        now,
        openThread: { rootId: previewThread.root_id, threadId: previewThread.thread_id },
      }),
    ),
  );
  for (const needle of [
    dict.conv_back,
    dict.conv_load_older,
    dict.conv_you,
    dict.conv_assistant,
    dict.conv_marker_compaction,
    dict.conv_tool_call,
    dict.conv_tool_result,
    "conv-msg",
    "conv-tool",
    "conv-boundary",
    "conv-unknown",
    "conv-omitted",
  ]) {
    if (!convDetail.includes(needle)) fail(`${lang}: conversations detail output missing: ${needle}`);
  }

  // Detail error path: an unknown identity renders the not-found state.
  const convMissing = renderToString(
    createElement(
      LangProvider,
      { initial: lang },
      createElement(ConversationsPage, {
        conn: { mode: "preview" },
        now,
        openThread: { rootId: "r0000000000000000", threadId: "019f9d5d-dead-7000-8000-000000000000" },
      }),
    ),
  );
  if (!convMissing.includes(dict.conv_not_found)) {
    fail(`${lang}: conversations detail did not render the not-found state`);
  }
}

// The prototype's bug class: after switching to en, no zh string may leak —
// assert rendered output contains no CJK outside allowed exceptions:
// fixture task titles (data, not UI copy) and the native language-name option.
const enDash = renderToString(
  createElement(
    LangProvider,
    { initial: "en" },
    createElement(Dashboard, {
      snapshot: baseSnap,
      now,
      onRefresh: () => {},
      onOpenConversation: () => {},
    }),
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
const enVariantDashes = (["stale", "rollout", "unavailable"] as PreviewQuotaVariant[]).map(
  (v) =>
    [
      `dashboard(${v})`,
      renderToString(
        createElement(
          LangProvider,
          { initial: "en" },
          createElement(Dashboard, {
            snapshot: { ...snap, account_quota: buildPreviewAccountQuota(v) },
            now,
            onRefresh: () => {},
          }),
        ),
      ),
    ] as const,
);
const enConvList = renderToString(
  createElement(
    LangProvider,
    { initial: "en" },
    createElement(ConversationsPage, { conn: { mode: "preview" }, now }),
  ),
);
const enConvDetail = renderToString(
  createElement(
    LangProvider,
    { initial: "en" },
    createElement(ConversationsPage, {
      conn: { mode: "preview" },
      now,
      openThread: { rootId: previewThread.root_id, threadId: previewThread.thread_id },
    }),
  ),
);
for (const [name, html] of [
  ["dashboard", enDash],
  ["settings", enSettings],
  ["sidebar", enSidebar],
  ["conversations", enConvList],
  ["conversations-detail", enConvDetail],
  ...enVariantDashes,
] as const) {
  const scrubbed = html
    .replace(/title="[^"]*"/g, "") // fixture titles are zh data, not UI copy
    .replace(/value="[^"]*"/g, "") // retry_prompt comes from daemon config (data)
    .replace(/<span class="t">[^<]*<\/span>/g, "")
    .replace(/<button[^>]*class="t tlink"[^>]*>[^<]*<\/button>/g, "") // linked titles are fixture data too
    .replaceAll(`>${strings.zh.settings_lang_zh}<`, "><") // language name stays native
    .replaceAll(`>${strings.zh.settings_lang}<`, "><"); // label is bilingual by design
  if (/\p{Script=Han}/u.test(scrubbed)) {
    const m = scrubbed.match(/.{0,60}\p{Script=Han}.{0,60}/u);
    fail(`en ${name} leaks CJK in UI chrome near: ${m?.[0]}`);
  }
}
console.log("en render OK — no CJK leaks outside fixture data / native lang names");

console.log("preview render OK — states:", [...states].sort().join(", "));
