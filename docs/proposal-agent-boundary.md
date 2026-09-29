# Proposal: the agent boundary

Status: proposal. Nothing here is built or in `requirements.md`.

## Context

Juice is a market of priced actions, and its intended buyers are agents: software that discovers,
selects and invokes actions (§2, U46). Agents such as Claude Code, Codex and OpenCode act on Juice
through the same CLI and HTTP API as people.

**The problem.** An agent reads content it cannot trust: an action's result, a message, a task
addressed to it. Any of these can instruct it to do something its owner never intended, and no
known technique reliably prevents an agent from being deceived. Meanwhile some operations are
damaging: they move money out, change where it goes, or alter an account. Safety therefore cannot
rest on the agent behaving well. It must rest on what the agent is able to do.

**The goal.** A person can let an agent act on Juice freely, knowing in advance the worst that can
happen.

**Desiderata.**

1. *Freedom inside.* Within its bound the agent needs no permission per call.
2. *An agent can design and create actions.* It is a provider as well as a buyer.
3. *An agent can transfer money*, for example to subcontract work.
4. *A bound known in advance*, set by the person: the money in the account and the upstream
   access delegated to it. The agent can conduct its own business to earn money.
5. *Harm stays within that bound.* Nothing a deceived agent does reaches other money, other
   control, or upstream access it was not given. Reputation is excepted: an agent rates what it
   paid for (U14).
6. *Enforced by the kernel*, so the bound holds for any harness and direct HTTP alike.
7. *The person stays in control*: they fund the agent, take money out, and log it out.
8. *No new mechanism where an existing one serves* (§12 rule 5).

## The claim

**An agent is an account without its password.** It may spend what the account holds, and publish
actions that do. It may not change who controls the account or where its money leaves the kernel.

## Why the account

Every authority check in the kernel is keyed on the caller's account and on nothing finer: money
moves only from the caller's own balance (U4, G1), access is `CanCall(C, action)` (D5), an upstream
credential applies only when its grantor pays (U27), a task completes only for its required caller
(D6), a record is read only by its parties (D11), and `admin` is refused to any caller but `sys`.
Whatever an agent does, it reaches only what its account holds. The precedent is the service
account; the monetary equivalent is a prepaid card.

An agent therefore has its own account, funded by transfer. It never shares a person's account:
that would give it the person's consents and the tasks addressed to the person (U22).

## Two boundaries

The account bounds what an agent does inside Juice, and the kernel enforces it: what the client
checks, a confirmation or `--yes`, an agent with a shell skips. What the agent reaches on the
machine is the operating system's to bound: a session is a file, and a program reads every file of
the user it runs as. The person chooses:

| The agent runs as | It can use | Cost |
|---|---|---|
| the person | every login stored under that user | stronger logins and a kernel's files are kept under another user |
| a user of its own | its own login | its own copy of the project and of the harness's setup |

## Three levels

| Level | Authority | Acts on | Gate |
|---|---|---|---|
| Admin | the `sys` account | other accounts, peers, the kernel, crediting deposits | `IsSuperuser` |
| User | any account | that account's own objects | caller is the owner or a party |
| Action | any account, or code acting for an action's owner | what the action's contract says, at its price | `Call()` (D2) |

`kernel …` and `auth …` come before any level: they manage the client and logins.

Every CLI command is a verb. Actions are not commands; they are named by address and reached
through two verbs: `run` starts a call, `task complete` resumes a parked one (D6).

## Criteria

Applied in order, to decide where an operation belongs:

1. It touches state the caller does not own: admin.
2. It judges or audits a call: a user verb, never an action (G8).
3. Code running inside an action must be able to do it: an action, since such code has only
   `juice.call` and the task functions (D7).
4. Otherwise: a user verb.

By criterion 3, paying (`transfer`), addressing work (`message`), searching (`lookup`), and the
clock, randomness and web fetch are actions. Reading one's balance or records, withdrawing,
registering an address, publishing and rating are user verbs. The action space needs no additions:
no story is unserved, a read made through `Call()` would write to the record it reads (D11), and a
second path to an existing route is one path too many (§12 rule 6).

## Spending and redirection

| Kind | Examples | What it reaches |
|---|---|---|
| Spending | `run`, `transfer`, `withdraw` to the registered address, a published action that pays | the account's money |
| Redirection | registering another blockchain address, changing the password | the person's control of the account, and the money they take out of it |

**Spending** is bounded by the account: over its lifetime, what the person puts in plus what the
agent earns.

**Publishing is spending.** A composed action pays transfer value from its owner's balance (U49),
which is how an agent subcontracts. An agent deceived into publishing an attacker's code has left a
standing instruction to spend, and it can spend money that arrives later. It still reaches only the
agent's account, and the action is listed, recorded in every transaction it makes, and disabled by
the person.

**Redirection** is not bounded by the account, and the attack is practical: the registration
message binds the kernel, the account id and the address (`kernel/rail.go`), so an attacker who
learns the id signs it with their own key and hands the agent the address and the signature. Every
later withdrawal, the person's own included, then pays the attacker.

## The rule

**An operation that redirects requires the password.** The operator creates the account; the
person logs in before the agent starts; the agent uses the saved session and cannot supply the
password.

| Operation | Password | Reason |
|---|---|---|
| Register or replace the blockchain address | yes | redirects every future withdrawal |
| Change the password | yes, already (D4) | locks the owner out |
| Withdraw | no | pays only the registered address (U51) |
| Run, transfer, complete a task | no | bounded by the account |
| Create, change, enable or delete an action | no | spending; an agent must be able to publish |
| Rate a call | no | payer only, once, and each rating costs a paid call (U14) |
| Connect or disconnect consent | no | within the credential's scopes, whatever the action |

Precedent: `sudo`, and a bank's stronger check to add a payee than to pay one. The rule adds no
kind of session, and it protects a person whose session token is stolen.

**What the person does.** The exposure is the account's balance, so they keep the allowance small
and withdraw what the agent earns. Withdrawal is safe because the address is protected.

## What the boundary does not cover

What the agent sends in arguments, or through `web`, leaves with it. The boundary limits what a
deceived agent can do, not what it can say.

## Decisions

1. **`user create` is the operator's.** A new account receives every `local` action and the free
   natives (D5, D17), and a stranger trades without one (U29). Revises U1.
2. **`user transfer` is removed.** `transfer` is an action, reached by `run`.

## Open decisions

1. **The agent skill.** Its setup has the agent create the account and hold the password.
2. **A leaked session.** `auth logout` ends only a session whose token the person holds; the
   operator's suspension ends any (U37).
