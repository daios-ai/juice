# Proposal: aliases

Status: proposal. Nothing here is built or in `requirements.md`.

An **alias** is a second name for an existing action. The first use is the language-model
defaults: an operator points `sys/llm/chat` at whichever chat action they choose — `acme/gpt/chat`,
a local Ollama action, another kernel's model — and every script and agent that calls
`sys/llm/chat` reaches it. No provider code enters the kernel.

## Objects and names

An action is an object with an id. It holds everything about itself: owner, contract, price,
visibility, stats, evidence. A name is an entry in one list, `(owner, name) → action_id`, and
`(owner, name)` is unique across the whole list, so two names can never clash.

Every action has exactly one **canonical** entry. Any further entry pointing at it is an **alias**.
The entry is marked; the action stores no name of its own.

## Why the distinction

If every name were equal, an action with two names would sit under two paths, and everything the
kernel derives from a path would have two answers: which application it belongs to, which consents
cover it, which subtree commands reach it. This is the multiple-inheritance problem, and it has no
principled resolution.

So an action has one parent. Its canonical name alone confers properties:

- the name other kernels see and every transaction records (P6, D4);
- membership in an imported application (D21);
- coverage by a path-based consent selector (D10);
- being reached by a subtree command such as `action disable acme/gpt` (D20).

An alias confers nothing. It is a way to reach the action, and no more.

## Rules

1. **Resolution.** The resolver maps any name, canonical or alias, to the action id. Everything
   after it — price, quote hash, access check, execution, receipt, rating — sees only the action.
   An alias is not a kind of action: `Call()` never sees one.
2. **Aliases point at ids, not names.** So an alias never points at another alias: no chains, no
   cycles. Renaming the target's owner does not break it.
3. **Access is the action's.** Calling through an alias is decided by the action's own visibility.
   A name confers no access and no control: an alias in my namespace to your action does not let me
   change your action.
4. **Who creates names.** Only the owner of a namespace creates entries in it. The operator may
   create `sys/llm/chat`; `bob` may create `bob/chat` but not `sys/anything`.
5. **Search.** Search lists every name as its own result. Two names for one action are two results
   with the same id, quote hash, and evidence.
6. **Delete.** Deleting an alias removes that name only. Deleting by the canonical name retires the
   action (U9); its aliases then reach a retired, uncallable action.
7. **Remote actions.** A remote action is represented locally by its proxy, which is an ordinary
   action with an id. An alias points at it like at any other action. If retention purges the proxy
   (D16), the alias no longer resolves, exactly as when any target is deleted.
8. **Binding.** A reference is resolved once, where it is bound. A step keeps the action it was
   created for (D6); a pinned run keeps the quote it saw (P2). Repointing an alias affects new calls
   only, and a pinned run through a repointed alias is refused as changed terms.

## Consequences for the language-model natives

`sys/llm/chat` and `sys/llm/embed` read and write no kernel state, so they are not system calls
(D17). They become aliases owned by `sys`. The Ollama adapter leaves the kernel and ships as an
ordinary action the default installation points them at.

`sys/llm/decide` stays a native: it reads the kernel's canonical contracts for the caller.

## Open questions

- **Commands.** How an alias is created and repointed with the existing action verbs.
- **Federation.** Whether peers see aliases in their search. Peers key their discovery cache by
  `(kernel, action_id)` (D16), so two names for one action collapse there unless the key includes
  the name.
- **Model use inside the kernel.** `sys/llm/decide`, `sys/llm/json`, and lookup's indexing call a
  model themselves. Who pays for those calls, and where indexing may send private descriptions,
  is undecided.
- **Call depth.** The kernel has no call-depth limit, and a zero-price cycle between actions never
  runs out of budget. Aliases cannot form a cycle among themselves (rule 2), but a target can call
  back through an alias. This exists without aliases and needs its own guard.
