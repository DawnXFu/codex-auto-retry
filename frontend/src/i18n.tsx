import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { interpolate, strings, type Lang, type Params, type StringKey } from "./strings";

// Tiny i18n: a React context carrying the active language, plus a `t` lookup
// with {name} interpolation and a `tr` lookup for runtime-composed keys
// (enum-driven class/action/stop_reason/tag labels) that falls back to the
// raw value so an unmapped enum never renders a key name.

export type TFunc = (key: StringKey, params?: Params) => string;
export type TRFunc = (key: string, fallback: string) => string;

const STORAGE_KEY = "car.lang";

interface I18nValue {
  lang: Lang;
  setLang: (l: Lang) => void;
  t: TFunc;
  tr: TRFunc;
}

const Ctx = createContext<I18nValue>({
  lang: "zh",
  setLang: () => {},
  t: (key) => strings.zh[key] ?? key,
  tr: (_key, fallback) => fallback,
});

function persistedLang(): Lang {
  try {
    const v = window.localStorage.getItem(STORAGE_KEY);
    if (v === "zh" || v === "en") return v;
  } catch {
    // SSR / storage unavailable — default zh
  }
  return "zh";
}

export function LangProvider(props: { children: ReactNode; initial?: Lang }) {
  const [lang, setLangState] = useState<Lang>(props.initial ?? persistedLang);

  const setLang = (l: Lang) => {
    setLangState(l);
    try {
      window.localStorage.setItem(STORAGE_KEY, l);
    } catch {
      // storage unavailable — in-memory switch still works
    }
  };

  useEffect(() => {
    try {
      document.documentElement.lang = lang === "zh" ? "zh-CN" : "en";
    } catch {
      // SSR — no document
    }
  }, [lang]);

  const value = useMemo<I18nValue>(() => {
    const dict = strings[lang];
    return {
      lang,
      setLang,
      t: (key, params) => interpolate(dict[key] ?? key, params),
      tr: (key, fallback) => dict[key as StringKey] ?? fallback,
    };
  }, [lang]);

  return <Ctx.Provider value={value}>{props.children}</Ctx.Provider>;
}

export function useI18n(): I18nValue {
  return useContext(Ctx);
}
