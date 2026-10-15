import { useEffect, useState } from "react";

// UI-local theme (frontend-side like car.lang — never reaches the daemon).
// "system" resolves via prefers-color-scheme and re-resolves live while the
// OS theme changes. Applied as <html data-theme>, which index.css keys on.
export type Theme = "light" | "dark" | "system";
export type ResolvedTheme = "light" | "dark";

const STORAGE_KEY = "car.theme";

const media =
  typeof window !== "undefined" && window.matchMedia
    ? window.matchMedia("(prefers-color-scheme: dark)")
    : null;

export function getTheme(): Theme {
  try {
    const v = window.localStorage.getItem(STORAGE_KEY);
    if (v === "light" || v === "dark" || v === "system") return v;
  } catch {
    // storage unavailable
  }
  return "system";
}

export function resolveTheme(t: Theme): ResolvedTheme {
  if (t === "system") return media?.matches ? "dark" : "light";
  return t;
}

export function applyTheme(t: Theme) {
  document.documentElement.dataset.theme = resolveTheme(t);
}

export function setTheme(t: Theme) {
  try {
    window.localStorage.setItem(STORAGE_KEY, t);
  } catch {
    // in-memory still applies
  }
  applyTheme(t);
}

/** Reactive wrapper for the settings select — reflects the stored choice and
 * keeps the resolved attribute synced to OS flips while "system" is set. */
export function useTheme(): { theme: Theme; setTheme: (t: Theme) => void } {
  const [theme, setThemeState] = useState<Theme>(getTheme);

  useEffect(() => {
    applyTheme(theme);
    if (theme !== "system" || !media) return;
    const onChange = () => applyTheme("system");
    media.addEventListener("change", onChange);
    return () => media.removeEventListener("change", onChange);
  }, [theme]);

  const set = (t: Theme) => {
    setTheme(t);
    setThemeState(t);
  };
  return { theme, setTheme: set };
}
