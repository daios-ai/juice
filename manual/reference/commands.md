---
title: Commands
parent: Reference
nav_order: 1
---

# Commands

Commands generally follow `juice [admin] <noun> <verb>`. The exception is
`run`, which executes an action directly. This reference summarizes the
commands and common options; the linked chapters provide worked examples.

The `admin` prefix requires superuser authority. Its noun distinguishes user
accounts from peers, so a target is interpreted in the intended namespace.
The superuser can also use ordinary commands with wider access to records and
can disable any action.

## Global flags

| Flag | Effect |
|---|---|
| `--as USER@KERNEL` | act as this login for one command; `JUICE_AS` does the same |
| `--server URL` | send this one command to this address; carries no login |
| `--json` | print the server's reply as sent, instead of human-readable text |
| `--quiet` | print ids only, one per line |
| `--verbose` | show the underlying cause of an error |

## Running

| Command | |
|---|---|
| `juice run ACTION [JSON]` | run an action ([Running an action](../calling/running.html)) |

Use `--quote-hash H` to bind the request to previously inspected terms.
The JSON input can be supplied inline or read from a file with `@file.json`.

## kernel

| Command | |
|---|---|
| `juice kernel serve NAME` | start the kernel in `$JUICE_HOME/kernels/NAME/` ([Running a kernel](../operating/running-a-kernel.html)) |
| `juice kernel add URL [NAME]` | register a kernel this client can reach ([Identity](../calling/identity.html#registering-and-trusting-a-kernel)) |
| `juice kernel list` | the kernels known, marking the one in use |
| `juice kernel health [NAME]` | check a kernel is up, and which kernel it is |
| `juice kernel forget NAME` | drop the record and its logins' credentials |

For `kernel serve`, `--addr` selects the HTTP listening address used by clients.

## auth

| Command | |
|---|---|
| `juice auth login USER@KERNEL` | authenticate and act as that login |
| `juice auth use USER@KERNEL` | switch to a login already held |
| `juice auth list` | the logins held, marking the one in use |
| `juice auth logout [USER@KERNEL]` | end a session |
| `juice auth recover USER@KERNEL` | reset a password using the recovery phrase |

Authentication commands that accept `--password` or `--phrase` can receive
those values without an interactive prompt.

## user

| Command | |
|---|---|
| `juice user create USER@KERNEL` | create an account; prints the recovery phrase once |
| `juice user me` | handle, balance, locked funds, connections |
| `juice user update` | `--description`, `--password` |
| `juice user connect SELECTOR` | consent for an action to use your upstream account ([Consent](../calling/consent-and-steps.html)) |
| `juice user disconnect [SELECTOR]` | revoke it; `--account KEY` removes the whole upstream account |
| `juice user transfer RECIPIENT AMOUNT` | send money to another user of this kernel ([Funds](../money/funds.html)) |
| `juice user ledger` | deposits, withdrawals, transfers, delivered value |
| `juice user address [ADDRESS]` | register the address you pay from and are paid at ([Deposits and withdrawals](../money/deposits-and-withdrawals.html)) |
| `juice user deposit` | where to send money, and whether you are registered ([Deposits and withdrawals](../money/deposits-and-withdrawals.html)) |
| `juice user withdraw AMOUNT` | take money out ([Taking money out](../money/deposits-and-withdrawals.html#taking-money-out)) |
| `juice user withdrawals` | withdrawals made, and where each stands |

For retryable requests, give `transfer` an `--external-key` and `withdraw` an
`--id`, then reuse that key with the same terms. Both accept `--yes` for
non-interactive confirmation.

## action

| Command | |
|---|---|
| `juice action create NAME` | create an action, inactive and private ([Publishing](../providing/publishing.html)) |
| `juice action show ACTION` | full detail, including `quote_hash` |
| `juice action update ACTION\|PATH` | change price, schemas, source, visibility, credentials |
| `juice action enable ACTION\|PATH` | make callable |
| `juice action disable ACTION\|PATH` | make uncallable, reversibly |
| `juice action delete ACTION\|PATH` | retire; history survives |
| `juice action list` | active actions within your access; `--all` includes inactive actions in your scope |
| `juice action import NAME [SPEC_URL]` | install an OpenAPI document ([Wrapping a web API](../providing/web-apis.html)) |
| `juice action stats ACTION` | uses, successes, failures, latency, rating |
| `juice action ratings ACTION` | the public ratings |

`create` and `update` take `--kind`, `--source`, `--artifact`, `--method`,
`--param`, `--description`, `--price`, `--input-schema`, `--output-schema`,
`--auth`; `update` also takes `--visibility`.

## tx

| Command | |
|---|---|
| `juice tx list` | transactions you are party to |
| `juice tx show ID` | one transaction in full ([Records](../calling/records.html)) |
| `juice tx rate ID 0\|1` | rate a call you paid for, once; `--note` |
| `juice tx verify ID` | verify stored receipt evidence without contacting its issuer |

## process

| Command | |
|---|---|
| `juice process list` | your processes and what they hold |
| `juice process show ID` | one process |
| `juice process end ID` | close it, cancelling waiting steps and returning their money |

## step

| Command | |
|---|---|
| `juice step create ACTION` | set work aside ([Steps and processes](../providing/steps.html)) |
| `juice step list` | steps you may complete or own; `--status`, `--process`, `--peer` |
| `juice step show ID` | one step, with `allowed_input` |
| `juice step complete ID [JSON]` | supply what is missing and run it; `--peer` |

`step create` requires `--trace` and `--required-caller`, and takes
`--partial-args`.

## admin

| Command | |
|---|---|
| `juice admin user list` | local user accounts on this kernel |
| `juice admin user show USER` | one account |
| `juice admin user suspend USER` | suspend, reversibly ([Operator duties](../operating/duties.html)) |
| `juice admin user unsuspend USER` | restore |
| `juice admin user rename USER NEW_NAME` | the only way a handle changes |
| `juice admin user deposit USER [AMOUNT]` | credit against a payment received; `--ref` names it ([Crediting accounts](../operating/duties.html#crediting-accounts)) |
| `juice admin peer list` | known kernels, traded and discovered |
| `juice admin peer show PEER` | one peer's account here |
| `juice admin peer suspend PEER` | refuse its requests, reversibly |
| `juice admin peer unsuspend PEER` | restore |
| `juice admin peer rename PEER NEW_NAME` | bind a petname |
| `juice admin peer inspect PEER` | identity, catalogue, evidence, reachability |
| `juice admin kernel show` | identity, money position, rates, credit ([Operator duties](../operating/duties.html#the-one-view-to-read-first)) |
| `juice admin kernel deposits` | unattributed payments, and work delivered unpaid |

Peer targets use a public key or petname. User targets use a handle or account
ID. Keeping these namespaces separate avoids ambiguity when a handle and a
petname happen to be the same.
