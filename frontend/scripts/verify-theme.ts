import indexHtml from "../index.html?raw";

function fail(msg: string): never {
  console.error(msg);
  process.exit(1);
}

const stored = new Map<string, string>();
let storageDenied = false;
const storage = {
  getItem: (k: string): string | null => {
    if (storageDenied) throw new Error("storage denied");
    return stored.has(k) ? stored.get(k)! : null;
  },
  setItem: (k: string, v: string) => {
    if (storageDenied) throw new Error("storage denied");
    stored.set(k, v);
  },
};
const media = {
  matches: false,
  media: "(prefers-color-scheme: dark)",
  addEventListener: () => {},
  removeEventListener: () => {},
};
const documentElement = { dataset: {} as Record<string, string> };
(globalThis as Record<string, unknown>).window = {
  localStorage: storage,
  matchMedia: (q: string) => (q.includes("dark") ? media : { matches: false }),
};
(globalThis as Record<string, unknown>).localStorage = storage;
(globalThis as Record<string, unknown>).document = { documentElement };

async function main(): Promise<void> {
  const { getTheme, resolveTheme, setTheme } = await import("../src/theme");

  const scriptMatch = indexHtml.match(/<script>\s*(\/\/ Set data-theme[\s\S]*?)<\/script>/);
  if (!scriptMatch) fail("could not extract the prepaint script from index.html");
  const prepaint = new Function("window", "document", "localStorage", scriptMatch![1]);

  const runPrepaint = (opts: { stored?: string; deny?: boolean; osDark: boolean }): string => {
    const ls = {
      getItem: () => {
        if (opts.deny) throw new Error("denied");
        return opts.stored ?? null;
      },
    };
    const win = { matchMedia: (q: string) => ({ matches: q.includes("dark") ? opts.osDark : false }) };
    const doc = { documentElement: { dataset: {} as Record<string, string> } };
    prepaint(win, doc, ls);
    return doc.documentElement.dataset.theme;
  };

  const cases: { name: string; stored?: string; deny?: boolean }[] = [
    { name: "default (no stored pref)" },
    { name: "explicit light", stored: "light" },
    { name: "explicit dark", stored: "dark" },
    { name: "invalid stored pref", stored: "neon" },
    { name: "storage denied", deny: true },
  ];
  for (const osDark of [false, true]) {
    for (const c of cases) {
      const got = runPrepaint({ stored: c.stored, deny: c.deny, osDark });
      stored.clear();
      storageDenied = !!c.deny;
      if (c.stored !== undefined && !c.deny) storage.setItem("car.theme", c.stored);
      media.matches = osDark;
      const runtime = resolveTheme(getTheme());
      if (got !== runtime) {
        fail(`osDark=${osDark} ${c.name}: prepaint=${got} runtime=${runtime} disagree`);
      }
      const want = ["light", "dark"].includes(c.stored ?? "") && !c.deny ? c.stored! : osDark ? "dark" : "light";
      if (got !== want) fail(`osDark=${osDark} ${c.name}: resolved ${got}, want ${want}`);
    }
  }
  console.log("prepaint/runtime parity OK — 5 cases x OS light/dark");

  stored.clear();
  storageDenied = false;
  if (getTheme() !== "system") fail("getTheme: empty storage must default to system");
  storage.setItem("car.theme", "neon");
  if (getTheme() !== "system") fail("getTheme: invalid stored value must normalize to system");
  storageDenied = true;
  if (getTheme() !== "system") fail("getTheme: denied storage must fall back to system");
  setTheme("light");
  if (documentElement.dataset.theme !== "light") fail("setTheme: denied storage must still apply theme");
  storageDenied = false;
  stored.clear();
  media.matches = true;
  if (resolveTheme("system") !== "dark") fail("resolveTheme(system) must follow OS dark");
  if (resolveTheme("light") !== "light") fail("resolveTheme: explicit light must ignore OS dark");
  setTheme("dark");
  if (stored.get("car.theme") !== "dark") fail("setTheme must persist the choice");
  if (documentElement.dataset.theme !== "dark") fail("setTheme must apply to <html data-theme>");

  console.log("theme runtime checks OK");
  console.log("verify-theme: all checks passed");
}

void main();
