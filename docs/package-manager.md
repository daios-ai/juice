# Package manager

A **package** is a file at a URL describing an application: the actions it provides and the actions
it depends on. The package manager installs one on a kernel, satisfying its dependencies first, and
remembers what it did so upgrades and removals respect them. This document states the problem,
walks the pieces of juice an installation touches, simulates installing a range of applications,
and derives from that the smallest set of rules a package manager must implement. Nothing here is
implemented.

## 1. Problem

A package manager exists to obtain software together with what it depends on, and to keep the
whole working. In juice an action's implementation is other actions: a composite calls them by name,
so an application depends on the actions its code calls. Today nothing declares those dependencies,
nothing satisfies them, and nothing remembers them. An operator who wants an application built from
several APIs assembles it by hand — finds each document, obtains each credential, starts each local
program, registers each part in the right order — and later upgrades or removes it from memory.

The precedent is Debian's apt. A package declares what it depends on; installing walks the
dependencies, installs what is missing, reuses what is present, and records the graph; removing
refuses over a dependent and cleans up what was installed only as a dependency. Juice adds one
thing apt lacks, a dependency satisfied by calling another kernel per use rather than by
installing anything, and takes one thing away, version solving, since one name holds one action.
Distribution follows Go rather than apt: a package is a file at a URL, a dependency names a URL,
and there is no registry.

## 2. What an installation touches

1. **Action kinds.** `http` — a hand-written endpoint or one operation of an imported document;
   `wasm` — a compiled composite; `native` — the kernel's `sys/*`, on every kernel, never installed;
   `remote_proxy` — the kernel's cache of a remote action, never created by hand. A package can
   provide only HTTP leaves and WASM composites.
2. **Composition by name.** A composite calls `juice.call("owner/name", args)`; the reference is a
   string in its code. The subcall runs as the composite's owner, so a dependency must be callable
   by that owner: its own actions at any visibility, a local one, a public one, or a remote one by
   `owner@kernel/name`.
3. **Compilation is an action.** `sys/tinygo/compile` turns Go source into a WASM artifact on the
   kernel, for a price. A package can ship source and be compiled where it lands, as Go modules are.
4. **Naming.** `owner/name`, `/` allowed in names, `(owner, name)` unique, `name/index` the entry
   point of a group (D15). The owner is whoever installs, so a package cannot know its own
   references before installation.
5. **Visibility.** Created private and disabled; enable validates the contract; `local` reaches
   this kernel's users; `public` is served to peers. A price, schema, or source change deactivates
   the row and revokes delegated consents (D4).
6. **The importer.** `action import <name> <spec-url> [--auth]` installs one document as the
   application at `<name>`, one row per operation, reconciled in place on re-import; price and
   operation names come from the document (`x-juice-price`, `x-juice-name`), the base URL from its
   `servers` entry, else the host it was fetched from (D21).
7. **Credentials.** Owner-held schemes attach at import or update and never surface (G5).
   Delegated schemes take each user's own consent through `user connect` over a directory; such
   actions never federate (D10, P6).
8. **Federation.** A remote action is resolved on first use, cached as a local-visibility proxy,
   re-resolved when its contract changes, and fails naming the peer when unreachable. Reading
   `owner@kernel/name` dials the peer and reports price and reachability without buying (D13).
9. **Lifecycle verbs.** Enable, disable, delete, visibility, price, and auth, by id or over an
   `owner/path` subtree; delete is soft and history stays (D20).
10. **Egress.** Loopback allowed by default, private networks not (G9); the kernel does the fetching
    and the dispatching, so a local program must listen on the kernel's machine.
11. **Layout.** One root, kernels per world, logins `handle@kernel` selected or given with `--as`,
    `services/<name>/` for a local program, `bin/` for executables, shipped files written once and
    never overwritten (`ecosystem-standard.md`).
12. **Money.** Installing costs nothing but compilation. A composite pays what it calls from its own
    price, remote actions at their all-in price.

## 3. Simulation

Seven applications chosen to differ in every property that bears on installation.

| App | Code runs | Interface | Credential | Depends on |
|---|---|---|---|---|
| A weather | vendor's host | OpenAPI document | one owner key | nothing |
| B gmail | vendor's host | OpenAPI document | each user's own consent | nothing |
| C whisper | a program on this machine | its own document at loopback | none | nothing |
| D briefing | in the kernel (WASM) | Go source | none of its own | A, B, `sys/llm/chat` |
| E digest | in the kernel (WASM) | Go source | none | `bob@kernel/translate`, `sys/llm/chat` |
| F ping-hook | vendor's host, one endpoint | none | one owner key | nothing |
| G haiku | in the kernel (WASM) | Go source | none | `sys/llm/chat` |

Audience and price are not columns: the installer sets the audience and the document or package
sets the price, for every row alike.

- **A.** Fetch the package, import the document, ask for the key (the package says where to obtain
  it), enable, set local. One prompt.
- **B.** Import the document with its delegated scheme, enable, set local. No prompt; the output
  tells each user to run `user connect pedro/gmail`. It cannot be published, which the kernel
  already enforces.
- **C.** Refuse unless the login's kernel is on this machine. Download the artifact for this
  platform, verify its hash, place it under `services/whisper/`, start it under the system's
  service manager on the recorded port, wait for its health path, then import the document from
  loopback, enable, set local.
- **D.** Walk the dependencies: A and B are packages, installed first if absent, reused if present;
  `sys/llm/chat` is a reference that must exist and be active here. Substitute the three bindings
  into the source, compile on the kernel, create, enable, set local. B's consent is per user, so the
  briefing works for a user only after that user connects gmail; the install output says so.
- **E.** `bob@kernel/translate` is resolved by reading it, which dials the peer and reports its
  all-in price and reachability; nothing is installed for it. Bind, compile, create, enable, set
  local.
- **F.** The package author writes a one-operation OpenAPI document; it is then A. No third way of
  providing an action is needed.
- **G.** Bind `sys/llm/chat`, compile, create, enable, set local. An unconfigured model is reported
  by the kernel at the first call; configuring natives is the operator's, not the package manager's.

## 4. Rules

1. **Two ways to provide an action:** import a document, or compile source with bindings. Only two
   kinds of action can be installed (§2.1), and each of these is the existing path for one of them;
   a bare endpoint (F) is a one-operation document, so a third path would duplicate the first for one
   special case.
2. **One way to declare a dependency:** a symbolic name with a default provider, either a package
   URL or a reference. The name is symbolic because the code cannot know the installer's handle
   (§2.4). Every reference — a sibling, a native, a remote action — is checked the same way, that it
   resolves and is callable by the installer, because that is the one condition the kernel will
   apply at call time (§2.2), and checking it at install turns a runtime failure into an install
   failure. A binding is to a package's root, and the code names the member it calls beneath it,
   as a program names a function inside a library it links; a dependency never names a member. A
   default remote reference carries the kernel's public key, never a petname, since a petname is
   local and never transmitted (D15); the output renders the local petname where one is bound.
   What install verifies about a remote dependency is its reachability, contract, visibility, and
   price at that moment; what happens to it later is governed by federation's own rules (D13, P7),
   and the package manager makes no further promise. `--bind` overrides a default, as a Debian
   virtual package is satisfied by the provider the operator chooses; without it D could not use a
   calendar the operator already has. The walk keeps a visited set and refuses a cycle, naming the
   chain.
3. **Two kinds of credential:** an owner-held one is asked once at install, from the package's
   statement of where it is obtained; a per-user one is never asked, and the output names the
   connect command. These are the two kinds the kernel has (§2.7): the first is sealed on the
   application by the import path that exists, the second belongs to each user and can only be
   given by that user, so the installer has nothing to ask. Where a key is obtained is in the
   package because looking it up is the friction the problem names (B needs nothing, A one prompt).
4. **One form of local program:** an artifact per platform with its SHA-256, an argument vector,
   a port, and a health path; placed under `services/<name>/`, supervised by the system, allowed
   only when the kernel is on this machine. The hash because a URL's content can change and a hash
   cannot; the argument vector because a shell line can carry a second command; the location and
   the supervisor because the ecosystem standard already assigns them (§2.11) and Debian packages
   ship services as systemd units for the same reason, that supervising a process is the system's
   job; the same-machine rule because the kernel dispatches to loopback on its own host (§2.10). The
   port chosen at first install is fixed for the life of the installation, since the imported rows
   carry it in their base URL. The kernel runs none of this: a kernel serves strangers and holds
   money, and a package that could make it execute code would turn every package URL into a
   code-execution path, which is why apt runs post-install scripts on the host and not in a daemon.
5. **Audience:** local on install, the install command being the act of making the application
   available to this kernel's users (U7), stated in its output. A kernel exists to serve its users
   and the operator installs for them, as apt installs for every user of the machine; the kernel's
   own standard library ships local for the same reason (D17); and the private default that guards
   a hand-written draft from accidental calls (U6) has no object for a finished interface installed
   deliberately. A package may mark an action private, a helper, because a composite's raw operations
   need not be sold separately (U20). Public remains the owner's separate command because exporting
   runs strangers' calls on the operator's money under the credit limit and spends the owner's
   upstream quota on outside demand, the consent U7 keeps apart.
6. **Price:** the document's for imports, the package's for composites. The importer already reads
   the document's price and a package-level one would be a second source of truth for one fact; a
   composite has no document, so its price is stated where its source is.
7. **The record is a graph per login,** because a dependency is a reference on one kernel under
   one owner, and the same package installed under two logins is two installations. It is written
   before each step because a failure between starting a program and recording it leaves a process
   the tool cannot explain; with progress recorded first, rerunning the command is the recovery,
   which is what dpkg's status file exists for. It keeps the SHA-256 of the fetched package file and
   of every source it named, as `go.sum` does, so an upgrade compares content rather than a version
   string, and a URL whose content changed under an unchanged version is seen. Remove refuses over a dependent because deleting an
   action a composite calls breaks the composite at its next call; it offers to drop what was
   installed only as a dependency and keeps what the operator installed themselves, which is apt's
   distinction between automatically and manually installed packages. Upgrade reconciles in place
   because the importer preserves ids and history on re-import and an in-place artifact update
   keeps the row; it re-checks dependents because a contract change deactivates the row (§2.5) and
   a composite bound to it must be seen to still resolve.
8. **The kernel changes nothing.** Import, create, update, enable, visibility, delete by id, and
   compile all exist, and every guarantee they carry — identity-preserving reconciliation, secrets
   sealed, history kept — is inherited rather than reimplemented. This is the conclusion the
   applications design reached for grouping and holds here for the same reason: the kernel
   resolves names and prices calls, and a package manager only arranges what the names resolve to.

Not handled, by decision: configuring natives (kernel configuration, G's case), containers (a
second artifact form, deferred until an application needs it), version solving (one name holds one
action, so at most one version is installed, as in apt, and a conflict is refused rather than
solved), publishing (rule 5), sandboxing a local program (the operator chose to run it), and a
catalogue of names (Go has none and needs none: a package is its URL; search is the kernel's
lookup, and a published application can carry its package URL in its index description).

## 5. The package file

```json
{
  "name": "weather",
  "version": "1.5.0",
  "description": "Forecasts with maps and news",
  "actions": {
    "api":   { "openapi": "https://api.example.com/openapi.json",
               "auth": { "scheme": "header", "config": { "name": "X-Api-Key" },
                         "secrets": { "value": { "obtain": "https://example.com/keys" } } } },
    "index": { "source": "index.go", "price": 20, "description": "Forecast for a city" }
  },
  "depends": {
    "world-map": { "package": "https://maps.example.org/package.json" },
    "news":      { "action": "news@k7Qm2vXz9pL4wRt8bN3cYf6hJ1sD0aGeUiOoPqWxZv5nMlB2rTk/weather-news" }
  },
  "service": null
}
```

`actions` provides the application's own rows under `<name>/`: an `openapi` entry imports a
document there, a `source` entry compiles Go into one composite. `depends` names what the sources
call, each with its default provider. `service`, for a program that must run here, is the form in
rule 4. Relative URLs resolve against the package's own.

**Binding.** Before a source is compiled, every occurrence of the token `${juice:symbol}` in it is
replaced by the root reference the symbol resolved to: `${juice:world-map}` by `pedro/world-map`,
`${juice:news}` by `news@<key>/weather-news`, `${juice:api}` by `pedro/weather/api`; the code
appends the member it calls, `${juice:world-map}/render`. The token is one that cannot occur as
valid Go by accident, so substitution touches nothing else. This is what a linker does with
symbols and what Go's `replace` does with module paths, and it is the only way a package can be
written without knowing who installs it, since the kernel refuses a reference with no owner. The
compiled artifact is therefore per installation, and the source, not the artifact, is what the
package pins.

## 6. Commands

The name of the executable is undecided; `jpm` stands for it below. It reads the same client
records and logins as the command line and adds no login flow of its own; `--as handle@kernel`
keeps its meaning everywhere.

- `jpm install <url> [--name <name>] [--bind sym=ref]... [--as login]` — fetch and verify the
  package; walk `depends` (a package already installed under this login is reused, one not
  installed is installed first, a reference is resolved and checked); start a local program; ask for
  missing owner-held secrets; import documents, compile sources with bindings, create; enable and
  set local by id; record each step before taking it.
- `jpm remove <name> [--as login] [--purge]` — refuse while a recorded dependent exists, naming it;
  delete the package's rows by id; offer to remove dependencies installed only for it; stop and
  remove its program when the last registration is gone, its state only with `--purge`.
- `jpm upgrade [<name>] [--as login]` — re-fetch the recorded package URL; if the version moved,
  reconcile imports in place, recompile and update composites in place, re-enable what was enabled,
  replace and restart a program, and re-check the dependents of anything whose contract changed.
- `jpm list [--as login]` — the graph: each package, its version, its actions, what it depends on,
  what depends on it.

An omitted login is the selected one in every command. The record lives under
`$JUICE_HOME/packages/<login>/` and holds, per package, the URL, the version, the hashes of the
package file and its sources, the bindings chosen, the row ids created, the program's port and
unit, and the edges in both directions.

## 7. Walkthrough

```
$ jpm install https://weather.example.org/package.json
weather 1.5.0 needs: world-map (package), news (news@k7Qm2v…/weather-news, petname daios)
  world-map   not installed → installing 1.0.1 ... 3 actions under pedro/world-map
  news        remote, resolved: 12 fUSDT per call, reachable
weather/api needs an API key (https://example.com/keys): ****
  api         imported, 4 actions
  index       compiled with bindings, price 20
Installed pedro/weather (local). Try: juice run pedro/weather '{"city":"Lisbon"}'

$ jpm remove world-map
weather depends on world-map. Remove weather too? [y/N] n
Nothing removed.
```

## 8. Open questions

1. Whether the package manager is a verb group of `juice` or a separate executable. The command
   line is a pure client of the server (U45) and a package manager runs programs on the host, which
   argues for a separate executable sharing the installation root; the cost is one more binary.
2. Whether a document hosted apart from its service, whose `servers` entry names another host,
   needs a base URL override on import. A package that serves its own document avoids it.
