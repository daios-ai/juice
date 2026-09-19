# How `sys/lookup` ranks actions

This describes exactly what happens when someone runs `sys/lookup` with a search
query, start to finish, in plain terms. It reflects the current code in
`kernel/kernel.go` (`Kernel.Lookup`).

> **Status note.** The step that used to reweight results by how often an action
> *succeeds when called* has been **temporarily removed and is under revision**
> (see the "Quality weighting (removed)" section below). Right now, results are
> ranked purely by how well the action's text matches the query.

## The inputs

- `query` — the search text (required).
- `limit` — how many results to return (optional; default 10, capped at 50).

Internally the search first gathers a wider pool than `limit` — ten times as many
candidates — so that the later filtering step doesn't leave you short. Call this
pool size the *oversample*.

## Step 1 — Two independent ways of matching, run in parallel

Each action carries a bundle of searchable text: its owner handle, its name, its
description, and the field names and field descriptions from its input and output
schemas. Lookup scores the query against every active action in two separate ways.

**A. Keyword match (the "lexical" leg).**
This is ordinary full-text search. It looks for the words in your query inside each
action's text and ranks actions by a standard relevance measure (BM25) that rewards
an action for containing your query words, especially rarer ones, and especially
when it contains several of them. The query is sanitised first: punctuation and
search-engine operators in your query are treated as plain words, not as commands,
so a query like `@minibox/sys/llm/chat` simply becomes the words
`minibox`, `sys`, `llm`, `chat`. The leg returns the best `oversample` actions,
best first.

**B. Meaning match (the "semantic" leg).**
This finds actions whose *description means something similar* to your query, even
if the exact words differ. It works only when a language model is configured to
turn text into a list of numbers (an "embedding") that captures meaning. If one is
configured, lookup turns your query into such a list and compares it to the stored
list for each action's description; closeness is measured by the angle between the
two lists (cosine similarity). The closest `oversample` actions are kept, best
first.

This leg is optional. If no embedding model is configured, or the query fails to
embed, lookup simply skips it and relies on the keyword leg alone — so search still
works on a machine with no language model. A stored list whose length doesn't match
the query's (for instance, after switching embedding models) is skipped rather than
compared, so a model change can never crash the search or produce a nonsense score.

## Step 2 — Combining the two lists (reciprocal-rank fusion)

Now there are up to two ranked lists: one by keyword, one by meaning. They can't be
added up directly because their raw scores are on different scales. Instead lookup
combines them by *position*, not by raw score — a method called reciprocal-rank
fusion:

- For each list an action appears in, it earns points equal to `1 / (60 + its
  position in that list)`, where position counts from 0 at the top.
- An action's points from both lists are added together.

The effect: being near the top of either list is worth a lot; being near the bottom
is worth a little; being near the top of *both* lists is best of all. The number 60
is a standard smoothing constant that keeps any single list from completely
dominating. Because every action that matched at all gets a small positive number of
points, an action that matched *something* always ranks above an action that matched
*nothing*.

## Step 3 — Quality weighting (REMOVED — under revision)

**This step is currently disabled.** It previously took each action's combined
points from Step 2 and *multiplied* them by a "quality" number between 0 and 1,
based on how reliably the action had succeeded when called:

- the number was `(1 + successes) / (2 + total calls)`;
- a never-called action sat at `1/2`;
- an action that always succeeded approached `1`;
- an action that always failed approached `0`.

It was removed because that multiplier is unbounded at the bottom: an action that
had failed many times could have its quality number driven so close to zero that it
was pushed *below* barely-relevant actions — so an exact, obviously-correct match
could end up buried far down the list purely because its past calls had failed. That
behaviour is being redesigned. When the replacement lands, it will be reintroduced
at this same point in the procedure.

**Until then:** an action's final score is simply its combined relevance points from
Step 2. Two actions with the same relevance rank the same regardless of their success
history. What is known about each hit travels with it instead: every result carries
the same evidence the action's own page shows — this kernel's own calls, the
provider's own report, and other kernels' accounts of trading with it — so a caller
weighs the record itself rather than a number somebody else computed from it.

## Step 4 — Sort, then keep only what the caller may actually use

Actions are sorted by final score, highest first. Then, going down that sorted list,
lookup keeps an action only if the caller is actually allowed to call it — it must be
active, and either public or owned by the caller, and its owner must not be
suspended. This check happens **before** cutting the list down to `limit`, so a run
of actions the caller can't use doesn't crowd out the ones they can.

Lookup stops once it has collected `limit` allowed actions.

## The output

For each returned action, lookup reports: its id, its reference as `owner/name`,
its description, its final score, and its input and output schemas — enough for a
caller (human or language model) to understand and invoke it.

## What lookup does *not* do

- It does **not** filter by rating, price, or success history. (The success-based
  reweighting described in Step 3 is currently removed; even when present it only
  reordered results, never excluded any.)
- It does **not** hide an action for being remote, offline, or unfunded — those are
  display hints elsewhere, not lookup inputs.
- The only thing that removes an action from results is the caller not being allowed
  to call it (Step 4), or it not matching the query text at all.
