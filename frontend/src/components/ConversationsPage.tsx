import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { ApiError, dataSource, type Connection, type DataSource } from "../api";
import { useI18n } from "../i18n";
import type { StringKey } from "../strings";
import { relAgo, dayLabel } from "../time";
import { IconBack, IconChevronDown, IconRefresh } from "../icons";
import type {
  ConversationItem,
  ConversationItemsPage,
  ConversationListFilter,
  ConversationListPage,
  ConversationSummary,
} from "../types";

// Conversations page (spec #19 milestone 2): read-only, chat-first view over
// persisted rollout history. Identity is always the opaque
// (root_id, thread_id) pair; cursors are replayed verbatim and never
// interpreted. Content renders as plain text — JSX escaping covers HTML
// safety and no remote attachment is ever fetched.
//
// C-06 deep-link seam: pass `openThread` to jump straight into a detail view
// (bump `nonce` to re-open the same conversation). App.tsx holds the state;
// a queue-row handler only needs to set it and switch the page:
//   setConvoTarget({ rootId, threadId, title, nonce: Date.now() });
//   setPage("conversations");
export interface ConversationDeepLink {
  rootId: string;
  threadId: string;
  title?: string;
  /** change to force re-opening the same conversation */
  nonce?: number;
}

const FILTERS: ConversationListFilter[] = ["ordinary", "subagent", "archived", "all"];

/** Scroll anchoring needs layout timing on the client; on the server render
 * there is no layout, so fall back to useEffect to keep SSR output warning-free. */
const useIsoLayoutEffect = typeof window === "undefined" ? useEffect : useLayoutEffect;

const FILTER_KEYS: Record<ConversationListFilter, StringKey> = {
  ordinary: "conv_filter_ordinary",
  subagent: "conv_filter_subagent",
  archived: "conv_filter_archived",
  all: "conv_filter_all",
};

/** Detail forward-poll cadence — deliberately slower than the snapshot poll. */
const CONV_POLL_MS = 8000;
/** List auto-refresh cadence: the "recent" list re-fetches only its first
 * page while the list view is mounted — cheap enough to leave running. */
export const CONV_LIST_POLL_MS = 30_000;
const BOTTOM_PX = 48;

type DetailStatus = "loading" | "ready" | "not_found" | "error";

type View =
  | { name: "list" }
  | { name: "detail"; rootId: string; threadId: string; title?: string; seq: number };

function isChanged(e: unknown): boolean {
  return e instanceof ApiError && (e.status === 409 || e.reason === "conversation_changed");
}

function isNotFound(e: unknown): boolean {
  return e instanceof ApiError && (e.status === 404 || e.reason === "conversation_not_found");
}

export default function ConversationsPage(props: {
  conn: Connection;
  now: Date;
  openThread?: ConversationDeepLink | null;
}) {
  const { conn, now } = props;
  const source = useMemo(() => dataSource(conn), [conn]);
  const { t } = useI18n();
  const navSeq = useRef(0);
  const [view, setView] = useState<View>(() => {
    const l = props.openThread;
    return l
      ? { name: "detail", rootId: l.rootId, threadId: l.threadId, title: l.title, seq: 0 }
      : { name: "list" };
  });

  const openDetail = useCallback((rootId: string, threadId: string, title?: string) => {
    navSeq.current += 1;
    setView({ name: "detail", rootId, threadId, title, seq: navSeq.current });
  }, []);

  // C-06 deep link: a new openThread prop switches the page into detail view.
  // When the page mounts with a link already set, the initial view above is
  // the detail — skip the first effect call so the detail isn't remounted
  // (and its items re-fetched) right after the seed.
  const link = props.openThread;
  const linkNonce = link?.nonce ?? 0;
  const skipFirstLink = useRef(!!props.openThread);
  useEffect(() => {
    if (!link) return;
    if (skipFirstLink.current) {
      skipFirstLink.current = false;
      return;
    }
    openDetail(link.rootId, link.threadId, link.title);
  }, [link?.rootId, link?.threadId, linkNonce, openDetail]);

  // ---- list state ----------------------------------------------------------
  const [filter, setFilter] = useState<ConversationListFilter>("ordinary");
  // Root scope: "" = all monitored roots; a root_id from the page's roots[].
  // Dir scope: "" = all working directories; a cwd value from the page's
  // dirs[]. Cursors bind to (filter, root, dir) server-side, so changing any
  // resets pagination — the list is always re-anchored at its first page.
  const [root, setRoot] = useState("");
  const [dir, setDir] = useState("");
  const [list, setList] = useState<ConversationListPage | null>(
    () => source.seedConversations?.("ordinary") ?? null,
  );
  const [listErr, setListErr] = useState<string | null>(() =>
    source.kind === "none" ? "no connection params" : null,
  );
  const [listLoading, setListLoading] = useState(() => source.kind === "live");
  const [loadingMore, setLoadingMore] = useState(false);

  const reseed = (f: ConversationListFilter, r: string, d: string) => {
    const seeded = source.seedConversations?.(f, r || undefined, d || undefined);
    if (seeded) {
      // Fixture resolves synchronously — swap without a loading flash.
      setList(seeded);
    } else {
      setListLoading(true);
    }
  };

  const selectFilter = (f: ConversationListFilter) => {
    setFilter(f);
    setListErr(null);
    reseed(f, root, dir);
  };

  const selectRoot = (r: string) => {
    setRoot(r);
    setListErr(null);
    reseed(filter, r, dir);
  };

  const selectDir = (d: string) => {
    setDir(d);
    setListErr(null);
    reseed(filter, root, d);
  };

  const onList = view.name === "list";
  useEffect(() => {
    if (!onList) return;
    let alive = true;
    source.fetchConversations(filter, { rootId: root || undefined, dir: dir || undefined })
      .then((page) => {
        if (!alive) return;
        setList(page);
        setListErr(null);
      })
      .catch((e) => {
        if (!alive) return;
        setListErr(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (alive) setListLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [source, filter, root, dir, onList]);

  // Low-rate auto-refresh of the first page while the list view is mounted.
  // The refresh replaces the list wholesale (pagination resets to head — this
  // is a "recent" view), keeps the existing items rendered while fetching,
  // and fails silently so a transient error never blanks a stale list. A
  // detail view polls its own tail_cursor instead, so this effect is gated
  // on onList; a hidden document skips the tick.
  const listPollInFlight = useRef(false);
  useEffect(() => {
    if (!onList) return;
    let alive = true;
    const tick = () => {
      if (listPollInFlight.current) return;
      if (typeof document !== "undefined" && document.hidden) return;
      listPollInFlight.current = true;
      source
        .fetchConversations(filter, { rootId: root || undefined, dir: dir || undefined })
        .then((page) => {
          if (!alive) return;
          setList(page);
          setListErr(null);
        })
        .catch(() => {
          /* silent: keep the stale list */
        })
        .finally(() => {
          listPollInFlight.current = false;
        });
    };
    const id = window.setInterval(tick, CONV_LIST_POLL_MS);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, [source, filter, root, dir, onList]);

  const loadMore = async () => {
    const cursor = list?.next_cursor;
    if (!cursor || loadingMore) return;
    setLoadingMore(true);
    try {
      // Cursors bind to the (filter, root, dir) scope that minted them —
      // replay under the current scope verbatim.
      const page = await source.fetchConversations(filter, {
        cursor,
        rootId: root || undefined,
        dir: dir || undefined,
      });
      setList((prev) => ({
        ...page,
        items: [...(prev?.items ?? []), ...page.items],
      }));
    } catch (e) {
      setListErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoadingMore(false);
    }
  };

  // Directory filter options come from the page's dirs[] — the whole
  // filtered scope, so the list never collapses to the selection. If the
  // current selection is absent (e.g. it matched nothing under a new
  // filter/root scope) keep it listed so the select can still show it.
  const dirOptions = useMemo(() => {
    const refs = list?.dirs ?? [];
    if (dir && !refs.some((d) => d.dir === dir)) {
      return [...refs, { dir, label: dirBaseName(dir) }];
    }
    return refs;
  }, [list?.dirs, dir]);

  if (view.name === "detail") {
    return (
      <ConversationDetail
        key={`${view.rootId}/${view.threadId}#${view.seq}`}
        source={source}
        now={now}
        rootId={view.rootId}
        threadId={view.threadId}
        title={view.title}
        onBack={() => setView({ name: "list" })}
      />
    );
  }

  return (
    <section className="page">
      <div className="card strip">
        <h1>{t("nav_conversations")}</h1>
        <div className="conv-filters">
          {FILTERS.map((f) => (
            <button
              key={f}
              className={`mini-btn${filter === f ? " sel" : ""}`}
              onClick={() => selectFilter(f)}
            >
              {t(FILTER_KEYS[f])}
            </button>
          ))}
        </div>
        <select
          className="conv-root-select"
          aria-label={t("conv_root_filter")}
          value={root}
          onChange={(e) => selectRoot(e.target.value)}
        >
          <option value="">{t("conv_root_all")}</option>
          {(list?.roots ?? []).map((r) => (
            <option key={r.root_id} value={r.root_id}>
              {r.label}
            </option>
          ))}
        </select>
        <select
          className="conv-dir-select"
          aria-label={t("conv_dir_filter")}
          value={dir}
          onChange={(e) => selectDir(e.target.value)}
        >
          <option value="">{t("conv_dir_all")}</option>
          {dirOptions.map((d) => (
            <option key={d.dir} value={d.dir}>
              {d.label}
            </option>
          ))}
        </select>
        <span className="spacer" />
        <button
          className="mini-btn"
          title={t("strip_refresh")}
          onClick={() => {
            setListLoading(true);
            source.fetchConversations(filter, { rootId: root || undefined, dir: dir || undefined })
              .then((page) => {
                setList(page);
                setListErr(null);
              })
              .catch((e) => setListErr(e instanceof Error ? e.message : String(e)))
              .finally(() => setListLoading(false));
          }}
        >
          <IconRefresh size={13} />
        </button>
      </div>

      {listErr && (
        <div className="banner">
          {t("conv_error")}{" "}
          <button className="mini-btn" onClick={() => selectFilter(filter)}>
            {t("conv_retry")}
          </button>
        </div>
      )}
      {list?.partial && <div className="banner">{t("conv_partial")}</div>}

      <div className="card conv-list">
        {!list && listLoading && <div className="queue-empty">{t("conv_loading")}</div>}
        {!list && !listLoading && <div className="queue-empty">{t("conv_empty")}</div>}
        {list && list.items.length === 0 && <div className="queue-empty">{t("conv_empty")}</div>}
        {list?.items.map((c) => (
          <ConversationRow
            key={`${c.root_id}/${c.thread_id}`}
            conv={c}
            now={now}
            onOpen={() => openDetail(c.root_id, c.thread_id, c.title)}
          />
        ))}
      </div>
      {list?.next_cursor && (
        <button className="btn ghost conv-more" onClick={() => void loadMore()} disabled={loadingMore}>
          {loadingMore ? t("conv_loading") : t("conv_load_more")}
        </button>
      )}
    </section>
  );
}

// Last path segment of a recorded working directory — splits on both
// separator styles since a cwd may come from another platform's paths.
function dirBaseName(p: string): string {
  const t = p.trim().replace(/[/\\]+$/, "");
  const i = Math.max(t.lastIndexOf("/"), t.lastIndexOf("\\"));
  return i >= 0 ? t.slice(i + 1) : t;
}

function ConversationRow(props: { conv: ConversationSummary; now: Date; onOpen: () => void }) {
  const { t } = useI18n();
  const c = props.conv;
  return (
    <div className="conv-item" onClick={props.onOpen} role="button" tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") props.onOpen();
      }}
    >
      <div className="qbody">
        <div className="qtitle">
          <span className="t">{c.title || t("conv_untitled")}</span>
          {c.kind === "subagent" && <span className="qtag blue">{t("conv_tag_subagent")}</span>}
          {c.archived && <span className="qtag amber">{t("conv_tag_archived")}</span>}
        </div>
        <div className="qmeta">
          {t("conv_last_active", { ago: relAgo(c.last_activity, props.now, t) })}
          {c.cwd && (
            <span className="conv-cwd" title={c.cwd}>
              {" · "}
              {dirBaseName(c.cwd)}
            </span>
          )}
        </div>
      </div>
    </div>
  );
}

// ---- detail view -----------------------------------------------------------
// Items are kept chronological. The first page is the newest persisted page
// (backward, no cursor); "load older" walks next_cursor backward and prepends
// with the scroll position preserved, and a low-rate forward poll anchored at
// tail_cursor appends newly persisted records. A 409 conversation_changed
// means the rollout was truncated/replaced/rotated — everything re-anchors at
// the latest page.

function ConversationDetail(props: {
  source: DataSource;
  now: Date;
  rootId: string;
  threadId: string;
  title?: string;
  onBack: () => void;
}) {
  const { source, rootId, threadId } = props;
  const { t } = useI18n();

  // The fixture source resolves synchronously so ?preview=1 and SSR
  // verification render real content with no loading flash; live starts
  // at "loading".
  const [seed] = useState(() => ({
    page:
      source.seedConversationItems?.(rootId, threadId, { direction: "backward" }) ?? null,
  }));
  const [items, setItems] = useState<ConversationItem[]>(seed.page?.items ?? []);
  const [olderCursor, setOlderCursor] = useState<string | undefined>(seed.page?.next_cursor);
  const [meta, setMeta] = useState<Pick<ConversationItemsPage, "state" | "archived" | "kind">>(() => ({
    state: seed.page?.state ?? "complete_persisted",
    archived: seed.page?.archived,
    kind: seed.page?.kind,
  }));
  const [status, setStatus] = useState<DetailStatus>(() => {
    if (source.kind === "fixture") return seed.page ? "ready" : "not_found";
    if (source.kind === "none") return "error";
    return "loading";
  });
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [pendingNew, setPendingNew] = useState(0);
  const [notice, setNotice] = useState<StringKey | null>(null);
  const [readErr, setReadErr] = useState(false);

  const rootRef = useRef<HTMLElement>(null);
  const scrollRef = useRef<HTMLElement | null>(null);
  const tailCursorRef = useRef<string | undefined>(seed.page?.tail_cursor);
  const atBottomRef = useRef(true);
  const stickBottom = useRef(source.kind === "fixture");
  // applied=true only once the prepended items actually committed, so a
  // concurrent forward-append can never consume the restore by accident.
  const prependRestore = useRef<{ height: number; top: number; applied: boolean } | null>(null);
  const polling = useRef(false);
  const aliveRef = useRef(true);
  useEffect(() => {
    aliveRef.current = true;
    scrollRef.current = rootRef.current?.closest(".main") ?? null;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const applyLatest = useCallback(
    (p: ConversationItemsPage) => {
      setItems(p.items);
      setOlderCursor(p.next_cursor);
      tailCursorRef.current = p.tail_cursor;
      setMeta({ state: p.state, archived: p.archived, kind: p.kind });
      setPendingNew(0);
      stickBottom.current = true;
    },
    [],
  );

  const reanchor = useCallback(async () => {
    const p = await source.fetchConversationItems(rootId, threadId, { direction: "backward" });
    if (aliveRef.current) applyLatest(p);
  }, [source, rootId, threadId, applyLatest]);

  // Initial load (and re-load if the connection rotates underneath us).
  useEffect(() => {
    let cancelled = false;
    source.fetchConversationItems(rootId, threadId, { direction: "backward" })
      .then((p) => {
        if (cancelled || !aliveRef.current) return;
        applyLatest(p);
        setStatus("ready");
        setReadErr(false);
      })
      .catch((e) => {
        if (cancelled || !aliveRef.current) return;
        setStatus(isNotFound(e) ? "not_found" : "error");
      });
    return () => {
      cancelled = true;
    };
  }, [source, rootId, threadId, applyLatest]);

  // Resolve the scroll container (.main) once mounted.
  useEffect(() => {
    const sc = scrollRef.current;
    if (!sc) return;
    const onScroll = () => {
      const at = sc.scrollHeight - sc.scrollTop - sc.clientHeight < BOTTOM_PX;
      atBottomRef.current = at;
      if (at) setPendingNew(0);
    };
    sc.addEventListener("scroll", onScroll, { passive: true });
    onScroll();
    return () => sc.removeEventListener("scroll", onScroll);
  }, [status]);

  // Stable scroll anchoring: after a prepended older page lands, restore the
  // previous viewport offset relative to the grown content; otherwise follow
  // the tail only when stickBottom was requested.
  useIsoLayoutEffect(() => {
    const sc = scrollRef.current;
    if (!sc) return;
    const restore = prependRestore.current;
    if (restore?.applied) {
      prependRestore.current = null;
      sc.scrollTop = restore.top + (sc.scrollHeight - restore.height);
    } else if (stickBottom.current) {
      stickBottom.current = false;
      sc.scrollTop = sc.scrollHeight;
    }
  }, [items]);

  const loadOlder = useCallback(async () => {
    const cursor = olderCursor;
    if (!cursor || loadingOlder) return;
    const sc = scrollRef.current;
    prependRestore.current = sc ? { height: sc.scrollHeight, top: sc.scrollTop, applied: false } : null;
    setLoadingOlder(true);
    try {
      const p = await source.fetchConversationItems(rootId, threadId, {
        direction: "backward",
        cursor,
      });
      if (!aliveRef.current) return;
      setItems((prev) => {
        const seen = new Set(prev.map((i) => i.id));
        const merged = [...p.items.filter((i) => !seen.has(i.id)), ...prev];
        if (prependRestore.current) prependRestore.current.applied = true;
        return merged;
      });
      setOlderCursor(p.next_cursor);
      setMeta((m) => ({ ...m, state: p.state }));
    } catch (e) {
      prependRestore.current = null;
      if (!aliveRef.current) return;
      if (isChanged(e)) {
        setNotice("conv_changed");
        await reanchor().catch(() => setReadErr(true));
      } else {
        setReadErr(true);
      }
    } finally {
      if (aliveRef.current) setLoadingOlder(false);
    }
  }, [source, rootId, threadId, olderCursor, loadingOlder, reanchor]);

  // Low-rate forward poll for appended records. Drains consecutive forward
  // pages (bounded) so bursts arrive in one tick; a 409 re-anchors.
  useEffect(() => {
    const tick = async () => {
      let cursor = tailCursorRef.current;
      if (!cursor || polling.current) return;
      polling.current = true;
      try {
        let appended = 0;
        for (let i = 0; i < 4 && cursor; i++) {
          const p = await source.fetchConversationItems(rootId, threadId, {
            direction: "forward",
            cursor,
          });
          if (!aliveRef.current) return;
          if (p.items.length) {
            appended += p.items.length;
            setItems((prev) => {
              const seen = new Set(prev.map((x) => x.id));
              return [...prev, ...p.items.filter((x) => !seen.has(x.id))];
            });
            setMeta((m) => ({ ...m, state: p.state }));
          }
          tailCursorRef.current = p.next_cursor ?? p.tail_cursor ?? cursor;
          cursor = p.next_cursor;
        }
        if (appended > 0) {
          if (atBottomRef.current) {
            stickBottom.current = true;
          } else {
            setPendingNew((n) => n + appended);
          }
        }
        setReadErr(false);
      } catch (e) {
        if (!aliveRef.current) return;
        if (isChanged(e)) {
          setNotice("conv_changed");
          await reanchor().catch(() => setReadErr(true));
        } else if (isNotFound(e)) {
          setStatus("not_found");
        } else {
          setReadErr(true);
        }
      } finally {
        polling.current = false;
      }
    };
    const id = window.setInterval(() => void tick(), CONV_POLL_MS);
    return () => window.clearInterval(id);
  }, [source, rootId, threadId, reanchor]);

  useEffect(() => {
    if (notice === null) return;
    const id = window.setTimeout(() => setNotice(null), 5000);
    return () => window.clearTimeout(id);
  }, [notice]);

  const jumpToLatest = () => {
    const sc = scrollRef.current;
    if (sc) sc.scrollTo({ top: sc.scrollHeight, behavior: "smooth" });
    setPendingNew(0);
  };

  return (
    <section className="page conv-detail" ref={rootRef}>
      <div className="card strip">
        <button className="mini-btn" onClick={props.onBack}>
          <IconBack size={13} />
          {t("conv_back")}
        </button>
        <h1 className="conv-title">
          <span className="t">{props.title || t("conv_untitled")}</span>
        </h1>
        {meta.kind === "subagent" && <span className="qtag blue">{t("conv_tag_subagent")}</span>}
        {meta.archived && <span className="qtag amber">{t("conv_tag_archived")}</span>}
      </div>

      {notice && <div className="banner">{t(notice)}</div>}
      {readErr && <div className="banner">{t("conv_read_error")}</div>}
      {meta.state === "partial" && <div className="banner">{t("conv_state_partial")}</div>}
      {meta.state === "unsupported" && <div className="banner">{t("conv_state_unsupported")}</div>}

      {status === "loading" && (
        <div className="card">
          <div className="queue-empty">{t("conv_loading")}</div>
        </div>
      )}
      {status === "not_found" && (
        <div className="card">
          <div className="queue-empty">{t("conv_not_found")}</div>
        </div>
      )}
      {status === "error" && (
        <div className="card">
          <div className="queue-empty">
            {t("conv_error")}{" "}
            <button
              className="mini-btn"
              onClick={() => {
                setStatus("loading");
                void reanchor()
                  .then(() => setStatus("ready"))
                  .catch((e) => setStatus(isNotFound(e) ? "not_found" : "error"));
              }}
            >
              {t("conv_retry")}
            </button>
          </div>
        </div>
      )}

      {status === "ready" && (
        <div className="card conv-thread">
          {olderCursor ? (
            <button className="conv-older" onClick={() => void loadOlder()} disabled={loadingOlder}>
              {loadingOlder ? t("conv_loading") : t("conv_load_older")}
            </button>
          ) : (
            <div className="conv-older-hint">{t("conv_history_start")}</div>
          )}
          {items.length === 0 && <div className="queue-empty">{t("conv_empty")}</div>}
          {items.map((item) => (
            <ConversationItemRow key={item.id} item={item} now={props.now} />
          ))}
          {pendingNew > 0 && (
            <button className="conv-new" onClick={jumpToLatest}>
              {t("conv_new_items", { n: pendingNew })}
            </button>
          )}
        </div>
      )}
    </section>
  );
}

function ConversationItemRow(props: { item: ConversationItem; now: Date }) {
  const { t, tr } = useI18n();
  const item = props.item;
  const time = item.timestamp ? dayLabel(item.timestamp, props.now, t) : "";

  switch (item.kind) {
    case "user_message":
    case "assistant_message": {
      const mine = item.kind === "user_message";
      return (
        <div className={`conv-msg ${mine ? "user" : "assistant"}`}>
          <div className="conv-bubble">
            <div className="conv-meta">
              {mine ? t("conv_you") : t("conv_assistant")}
              {time && ` · ${time}`}
            </div>
            <div className="conv-text">{item.text}</div>
            {item.omitted && <div className="conv-omitted">{t("conv_omitted")}</div>}
          </div>
        </div>
      );
    }
    case "tool_call":
    case "tool_result":
      return <ToolRow item={item} time={time} />;
    case "boundary": {
      const label = item.marker ? tr(`conv_marker_${item.marker}`, item.marker) : t("conv_marker_session");
      return (
        <div className="conv-boundary">
          <span className="conv-boundary-label">{label}</span>
          {item.text && <span className="conv-boundary-text">{item.text}</span>}
          {time && <span className="conv-boundary-time">{time}</span>}
        </div>
      );
    }
    default:
      return (
        <div className="conv-unknown">
          {t("conv_unknown_item")}
          {item.marker ? ` · ${item.marker}` : ""}
          {time ? ` · ${time}` : ""}
        </div>
      );
  }
}

function ToolRow(props: { item: ConversationItem; time: string }) {
  const { t } = useI18n();
  const { item, time } = props;
  const [open, setOpen] = useState(false);
  const label =
    item.kind === "tool_call"
      ? item.tool
        ? `${t("conv_tool_call")} · ${item.tool}`
        : t("conv_tool_call")
      : t("conv_tool_result");
  return (
    <div className={`conv-tool${open ? " open" : ""}`}>
      <button className="conv-tool-head" onClick={() => setOpen((o) => !o)}>
        <IconChevronDown size={12} />
        <span className="conv-tool-label">{label}</span>
        {item.omitted && <span className="conv-omitted">{t("conv_omitted")}</span>}
        {time && <span className="conv-tool-time">{time}</span>}
      </button>
      {open && <pre className="conv-tool-body">{item.text || "—"}</pre>}
    </div>
  );
}
