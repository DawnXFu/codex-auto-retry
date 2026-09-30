# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

## Before exploring, read these

- **`CONTEXT.md`** at the repo root: the project's domain glossary (this repo uses `CONTEXT.md` as the glossary filename, maintained by the `domain-modeling` skill — treat it as the equivalent of `GLOSSARY.md` in the standard layout).
- **`docs/adr/`**: read ADRs that touch the area you're about to work in.

If any of these files don't exist, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The `domain-modeling` skill (reached via `grill-with-docs` and `improve-codebase-architecture`) creates them lazily when terms or decisions actually get resolved.

## File structure

Single-context repo:

```
/
├── CONTEXT.md                        ← glossary
├── docs/adr/
│   ├── 0001-fork-strategy.md
│   └── 0002-quota-recovery-semantics.md
└── scripts/source/                   ← Go watchdog code
```

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in `CONTEXT.md`. Don't drift to synonyms the glossary explicitly avoids (e.g. say **Quota Window**, not "rate limit"; **Binding Window**, not "current limit").

If the concept you need isn't in the glossary yet, that's a signal: either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `domain-modeling`).

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0002 (quota recovery semantics), but worth reopening because…_
