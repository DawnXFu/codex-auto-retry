import { useEffect, useState } from "react";
import { BrandMark, IconClose, IconMaximize, IconMinimize, IconRestore } from "../icons";
import { useI18n } from "../i18n";

declare global {
  interface Window {
    /** Wails v2 JS runtime — injected beside window.go; absent in browsers. */
    runtime?: {
      WindowMinimise?: () => void;
      WindowToggleMaximise?: () => void;
      WindowIsMaximised?: () => Promise<boolean>;
      Quit?: () => void;
    };
  }
}

// Frameless titlebar (ADR-0004 desktop client). The drag region is the whole
// bar; buttons are no-drag so clicks never start a move. In a plain browser
// (dev / ?preview=1) the runtime is absent and the window buttons hide.
export default function Titlebar() {
  const rt = window.runtime;
  const { t } = useI18n();
  const [maxed, setMaxed] = useState(false);

  useEffect(() => {
    if (!rt?.WindowIsMaximised) return;
    const check = () => void rt.WindowIsMaximised!().then(setMaxed).catch(() => {});
    check();
    window.addEventListener("resize", check);
    return () => window.removeEventListener("resize", check);
  }, [rt]);

  const toggleMax = () => {
    rt?.WindowToggleMaximise?.();
    // State lags the click; re-query after the frame settles.
    window.setTimeout(() => void rt?.WindowIsMaximised?.().then(setMaxed).catch(() => {}), 60);
  };

  return (
    <header className="titlebar" onDoubleClick={toggleMax}>
      <div className="tb-brand">
        <BrandMark size={15} />
        <span>{t("app_title")}</span>
      </div>
      <div className="tb-spacer" />
      {rt && (
        <div className="tb-btns" onDoubleClick={(e) => e.stopPropagation()}>
          <button
            className="tb-btn"
            onClick={() => rt.WindowMinimise?.()}
            title={t("window_minimize")}
            aria-label={t("window_minimize")}
          >
            <IconMinimize />
          </button>
          <button
            className="tb-btn"
            onClick={toggleMax}
            title={maxed ? t("window_restore") : t("window_maximize")}
            aria-label={maxed ? t("window_restore") : t("window_maximize")}
          >
            {maxed ? <IconRestore /> : <IconMaximize />}
          </button>
          <button
            className="tb-btn close"
            onClick={() => rt.Quit?.()}
            title={t("window_close")}
            aria-label={t("window_close")}
          >
            <IconClose />
          </button>
        </div>
      )}
    </header>
  );
}
