// Inline SVG icon set — stroke style, 16×16 grid, replaces the emoji glyphs.
// `size` scales the rendered box; stroke stays proportional (non-scaling look
// is intentional so icons keep hairline weight at every size).
import type { CSSProperties } from "react";

function S(props: { size?: number; children: React.ReactNode; fill?: boolean; style?: CSSProperties }) {
  const s = props.size ?? 16;
  return (
    <svg
      width={s}
      height={s}
      viewBox="0 0 16 16"
      fill={props.fill ? "currentColor" : "none"}
      stroke={props.fill ? "none" : "currentColor"}
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      style={props.style}
    >
      {props.children}
    </svg>
  );
}

export const IconDashboard = (p: { size?: number }) => (
  <S {...p}>
    <rect x="2" y="2" width="5" height="5" rx="1.2" />
    <rect x="9" y="2" width="5" height="5" rx="1.2" />
    <rect x="2" y="9" width="5" height="5" rx="1.2" />
    <rect x="9" y="9" width="5" height="5" rx="1.2" />
  </S>
);

export const IconSettings = (p: { size?: number }) => (
  <S {...p}>
    <circle cx="8" cy="8" r="2.1" />
    <path d="M8 1.8v1.7M8 12.5v1.7M1.8 8h1.7M12.5 8h1.7M3.6 3.6l1.2 1.2M11.2 11.2l1.2 1.2M12.4 3.6l-1.2 1.2M4.8 11.2l-1.2 1.2" />
  </S>
);

export const IconAbout = (p: { size?: number }) => (
  <S {...p}>
    <circle cx="8" cy="8" r="6.2" />
    <path d="M8 7.4v3.4" />
    <circle cx="8" cy="5.1" r="0.4" fill="currentColor" stroke="none" />
  </S>
);

// ---- queue state glyphs (rendered inside the tinted .qicon tile) ----

export const IconPending = (p: { size?: number }) => (
  <S {...p}>
    <circle cx="8" cy="8" r="5.8" />
    <path d="M8 4.8V8l2.2 1.6" />
  </S>
);

export const IconRunning = (p: { size?: number }) => (
  <S {...p}>
    <path d="M13.4 8a5.4 5.4 0 1 1-1.6-3.9" />
    <path d="M13.4 1.9v2.7h-2.7" />
  </S>
);

export const IconQuotaWait = (p: { size?: number }) => (
  <S {...p}>
    <path d="M4.4 2.2h7.2M4.4 13.8h7.2" />
    <path d="M5.4 2.2c0 2.6 1.4 3.4 2.6 5.8-1.2 2.4-2.6 3.2-2.6 5.8M10.6 2.2c0 2.6-1.4 3.4-2.6 5.8 1.2 2.4 2.6 3.2 2.6 5.8" />
  </S>
);

export const IconAttention = (p: { size?: number }) => (
  <S {...p}>
    <path d="M8 2.4 14.4 13H1.6Z" />
    <path d="M8 6.4v3" />
    <circle cx="8" cy="11.3" r="0.4" fill="currentColor" stroke="none" />
  </S>
);

export const IconStopped = (p: { size?: number }) => (
  <S {...p}>
    <circle cx="8" cy="8" r="5.8" />
    <path d="M5.6 8h4.8" />
  </S>
);

// ---- chrome ----

export const IconRefresh = (p: { size?: number }) => (
  <S {...p}>
    <path d="M13.6 8a5.6 5.6 0 1 1-1.7-4" />
    <path d="M13.6 1.7V4.4h-2.7" />
  </S>
);

export const IconMinimize = () => (
  <S size={12}>
    <path d="M3 8h10" />
  </S>
);

export const IconMaximize = () => (
  <S size={12}>
    <rect x="3.4" y="3.4" width="9.2" height="9.2" rx="1.4" />
  </S>
);

export const IconRestore = () => (
  <S size={12}>
    <rect x="2.6" y="5.4" width="7.6" height="7.6" rx="1.2" />
    <path d="M5.6 5.4V4.2c0-1 .7-1.6 1.6-1.6h5c1 0 1.6.6 1.6 1.6v5c0 1-.6 1.6-1.6 1.6h-1.2" />
  </S>
);

export const IconClose = () => (
  <S size={12}>
    <path d="M4 4l8 8M12 4l-8 8" />
  </S>
);

/** App brand mark — same glyph as the exe icon (dark tile + green retry arc). */
export const BrandMark = (p: { size?: number }) => {
  const s = p.size ?? 16;
  return (
    <svg width={s} height={s} viewBox="0 0 16 16" aria-hidden="true">
      <rect width="16" height="16" rx="3.6" fill="#14161b" />
      <path
        d="M10.9 5.3A4.2 4.2 0 1 0 12.2 8"
        fill="none"
        stroke="#16a34a"
        strokeWidth="2.1"
        strokeLinecap="round"
      />
      <path d="M12.55 3.4l.85 2.35-2.4.5z" fill="#16a34a" />
    </svg>
  );
};
