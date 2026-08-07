# Buying a remote action breaks it (2026-08-07, v0.12.11)

## TL;DR

`v0.12.11` priced the catalog, so browsing finally works. The step *after* browsing does not.
Resolving a remote action makes it **unfindable** and **unnameable**: the proxy is written into
neither search index, and it shadows the discovery row that used to be findable; its rendered
reference is empty because its owner is a handleless kernel account.

Purchase deletes discoverability. None of this is new in `v0.12.11` — and that is not a
defence, because `v0.12.11` is what made the catalog a place people shop.

## Why we didn't catch it

**Every federation flow in §15 stops at the moment money moves.** They are organised per
mechanism — settlement, rejection, relay carriage, suspension, offline parking — and each ends
on a charge, a refund, or a receipt. The closest one goes:

> …discovers the subject's action via its discovery cache (`sys/lookup`), resolves it directly
> by key, runs — its own Stats start at defaults and accumulate

It walks discover → resolve → run and stops one step short of the bug. Not one federation flow
does anything *after* a successful call: no second lookup, no re-invocation by name, no reading
back of the row that was just created. The suite tests the **transaction**, never the
**relationship** that outlives it.

Second cause: almost every federation assertion is economic or protocol-level (charge, premium,
refund, receipt hash, signature domain). The R8 rendering rule — outputs render a name a command
can consume, never a raw id — has flow coverage for transactions, steps and processes, but
**none for a proxy action**, which is the one row type whose owner cannot have a handle.

So the gap is structural, not an oversight in one test: flows are written per mechanism, and
mechanisms end at settlement. Users don't.

## What a user hits

Ana searches `compile tinygo`, sees `sys@esKD…/tinygo/compile — 7 credits`, and runs it. It
resolves, executes, she is charged. Then:

1. **She searches again. Nothing.** The action she just bought is gone from search — for her and
   for everyone on her kernel. Only the raw catalog listing reaches it.
2. **The listing shows `ref: ""`.** She cannot copy, click, or re-type what she bought. Only the
   raw action id works.
3. **Her operator raises `import_bps`.** Her card quoted 7; the cached proxy still charges the
   frozen import-time price. Catalog and till disagree.

## The three defects

| # | Defect | Verified | Introduced |
| - | ------ | -------- | ---------- |
| 1 | A resolved proxy is indexed into **neither** search leg, and shadows the discovery row | yes, live | resolve-on-use rewrite |
| 2 | A proxy's rendered `action` is `""`/`/name`, `owner_handle` null (R8 violation) | yes, live | accounts/kernels split |
| 3 | First-meaningful-use petname auto-bind never fires | reported, not reproduced here | unknown |

**Defect 1, evidence.** On the live kernel, the two proxies created by the current resolve path
(6 Aug) are absent from both `actions_fts` and `embed_vec`; all twenty-four July-era proxies
have both. The date split localises it to the resolve-on-use rewrite, not to `v0.12.11`. The
throwaway pair reproduced it cleanly because every proxy there was fresh; the live kernel masked
it because its catalog is dominated by correctly-indexed July rows. Fixing the path is not
enough — the existing unindexed rows need a one-off reindex.

**Defect 2, evidence.** The projection is `OwnerHandle + "/" + Name`; a kernel account holds no
handle by design. Live lookup output reads `/sys/tinygo/compile`, `/sys/make`, `/sys/message`.
The substitution should be petname, else raw key.

**Compounding.** All three land on the same row. A bought action is simultaneously unfindable,
unnameable, and — with the separately-recorded frozen-price defect — mispriced.

## The user stories we should have had

Written as §15 flow lines. Each one continues past the charge.

```text
— Federation: the buying loop —
a caller discovers a remote action via sys/lookup, resolves and runs it, then searches AGAIN:
  the action is still findable (now as the local proxy, shadowing its discovery row), its
  rendered ref is usable, and re-running it BY THAT REF succeeds without a second resolve
a resolved proxy renders a reference a command can consume: `action` is owner-qualified by the
  peer's petname (its raw key when unbound) and owner_handle is never null (R8)
first meaningful use binds a petname: after the first verified resolve the roster shows it, the
  bound name resolves, and the nickname form still never does
the operator raises import_bps: the catalog price and the price actually charged on the next
  call agree, with no manifest change and no re-pull
```

The first line alone would have caught defects 1 and 2 on the day they were introduced.

## Priority

1. **Defect 1** — silently destroys the catalog; needs a path fix plus a reindex of existing rows.
2. **Defect 2** — small substitution at the projection, but blocks the UI from rendering any
   proxy at all.
3. **Defect 3** — cosmetic until a user has to read a 43-char key.
4. Add the four flow lines above **before** the fixes, so they fail first.

Worth checking whether defect 1 and the frozen-price defect share a cause in
`importRemoteActionCore`: both look like work the older import path did and the resolve-on-use
path does not.
