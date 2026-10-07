---
title: Running a kernel
parent: Operating a kernel
nav_order: 1
---

# Running a kernel

Running a kernel gives you responsibility for its accounts, persistent records,
and external payments. This chapter covers creation, funding, maintenance, and
network participation. Routine supervision is covered in
[Operator duties](duties.html).

## Starting one

```console
$ juice kernel serve play --listen-addr :4040
```

The argument names the world to serve. A world is a network with a single
currency, described by a file in `~/.juice/worlds/` that gives the money it uses
and the servers to meet it through. The files for the worlds Juice comes with
(`play`, `polygon`, `arbitrum-one`, `arbitrum-sepolia`) are written there the first
time you serve, to read and to edit, and a world of your own is a file you add.
One installation runs one kernel per world, in `~/.juice/kernels/<world>/`. See
[Configuring worlds](worlds.html) to edit a world or create one of your own.

The `--listen-addr` option sets the HTTP listening address. On first start, the
command asks you to confirm creation and to name the kernel on the network:

```
There is no kernel on play here. No kernels here yet.
play is no real money: you credit accounts yourself and keep the records.
Create a kernel on play? [y/N] y

What will this kernel call itself on the network? Other operators see this name.
Name: acme
Superuser password:
Confirm password:
sys recovery phrase (write this down; it is shown only once and cannot be recovered):
  depart motion moon climb useless hole learn usage delay fish brand window
Press Enter once you have written it down:
Superuser "sys" created.
INF action.registered_native name=lookup
INF action.native_enabled name=lookup
…
INF client.self_registered kernel=acme outcome=added
INF server.ready handle=acme network=play addr=[::]:4040 public_key=L3ciw7zj…
```

The name is the kernel's nickname, which every operator on the network sees;
the world's name is shared by all of them, which is why the kernel needs one of
its own. The ready line names the kernel, network, HTTP address, and public key.
Federation listening addresses are available through `juice admin kernel show`
and `GET /health`.

On a chain network, first boot also reaches the chain and records the block it
starts watching for payments from. It asks nothing more: the world names the node
to reach it through. See [Setting up on a chain](#setting-up-on-a-chain).
Declining creation, or stopping before those answers, leaves no kernel state on
disk.

{: .warning }
> The network cannot be changed afterwards. A kernel serves the one it was created
> with for its whole life; using another means creating another kernel.

After verification, the selected network is recorded in the database. Later
starts use that record to ensure the kernel continues on the same network.
Operational settings such as listening addresses and fees can be changed in
configuration; what belongs to the network — its chain endpoint and its meeting
points — is in the world file.

{: .warning }
> Save the recovery phrase before pressing Enter. It is shown once, it is the only
> way to reset the `sys` password (`juice auth recover sys@acme`), and nobody can
> recover it for you.

With setup complete, later starts use the saved state and print a ready line
identifying the kernel, network, and public key.

## Setting up on a chain

This section sets up a kernel on `polygon`, the standard world with real money.
The two Arbitrum worlds are set up the same way, with ETH in place of POL.

A kernel on `polygon` has one address on the Polygon blockchain. Two currencies
arrive at that address, and they serve different purposes.

**USDT0** backs the balances inside the kernel. Each deposit is credited to the
account that registered the sending address, including `sys` when the operator
is depositing. The `sys` account has administrative authority, receives kernel
fees, and holds the operator's own balance. There is no kernel USDT0 balance
outside these accounts. All balances must remain backed, including funds
reserved for work or payments; money held for an outgoing payment or awaiting
attribution is unavailable for the operator to spend.

**POL** pays Polygon's transaction fees, also called gas; the currency a chain
charges its fees in is its **fuel**. The kernel needs POL to send withdrawals and
settlement payments. Receiving a deposit costs the kernel nothing, since the
sender pays that fee. POL sent to the kernel credits no account.

When its POL runs low, the kernel buys more with available `sys` USDT0 on an
exchange named in the world file, its **venue**. This purchase is a **refill**.
Other accounts' balances cannot fund it. A refill is itself a transaction and
needs POL, so the operator supplies the first POL and replenishes it directly if
too little remains to make a purchase. The setup sequence is:

**1. Decide which node to use.** The kernel reads payments and submits
transactions through a Polygon node. The `polygon` world file names a public one,
so there is nothing to do here. To use a hosted node or your own instead, edit
`rpc` in `~/.juice/worlds/polygon.json`; the node may be changed at any time.

On first boot, the kernel checks that it can reach the node, that the chain,
token and decimals match the world file, and that the venue supports refills.
Creating a kernel fixes its world for life, publishes the address people pay to,
and records the block from which it watches for payments, which cannot be
guessed afterwards. A first boot that cannot reach the chain, or finds any of
these wrong, therefore creates no kernel: no superuser, nothing serving. What it
had already written, including its chain key, stays where it is, so fixing the
cause and starting again continues from there.

Once the kernel exists, an unreachable node or a broken venue only delays money:
the kernel serves, and the payment commands wait and say what is not ready.

**2. Start the kernel.**

```console
$ juice kernel serve polygon
```

First boot generates `rail.key` in the kernel's home. This key controls the
kernel's address on the chain.

{: .warning }
> `rail.key` controls the kernel's money on the chain. It is created once and
> never regenerated. Back it up with the rest of the home: lose it and you lose
> what the kernel holds.

**3. Read the kernel's address.** The serving machine's client already knows
the kernel. Log in as `sys` and inspect its blockchain address:

```console
$ juice admin kernel show
Handle:     bank
Network:    polygon
Paid at:    0xcaf2a882af8730c6ad92d76361b1952c71c0453f
Holdings:   0.00 USDT0 (gas 0.00) as of block 13
…
```

`Holdings` reports the kernel's USDT0 and, as `gas`, its POL. `juice user
deposit` reports the same `Paid at` address, together with the accepted USDT0
contract and the current account's registered sender address.

**4. Supply the first POL.** From your own wallet, send POL on Polygon to the
`Paid at:` address. With the settings in `polygon.json` the kernel refills below 5 POL and
buys up to 15 POL, so sending 15 POL lets the first payments go out without a
refill.

**5. Fund the operator's USDT0 balance.** Refills are paid from `sys`'s USDT0,
so `sys` needs some before fees have accumulated: enough to buy 15 POL at the
current price covers one refill. Remain logged in as `sys`, and register the
wallet address from which you will send USDT0:

```console
$ juice user blockchain-address <your-wallet-address>
$ juice user deposit
```

The first command asks for a signature proving control of your wallet, and for
the `sys` password. Follow the second command's instructions to send USDT0 from
that wallet to the kernel's address. Once Polygon reports the payment as final
and the kernel has seen it, it credits `sys`. The signing and deposit procedure
is covered step by step in [Deposits and withdrawals](../money/deposits-and-withdrawals.html#step-1-register-your-wallets-address).

**6. Verify funding.** Run `juice user me` to check the available `sys` balance,
and `juice admin kernel show` to inspect the kernel's final USDT0 and POL
holdings, its accounting checks, and any payment halt. The solvency difference
should be zero; custody is compared when the payment scan allows it, as
explained in [Operator duties](duties.html#the-one-view-to-read-first).

## Funding the kernel

Continued operation requires enough POL to send payments and, when a refill is
needed, enough available `sys` USDT0 to buy it. There is no fixed minimum `sys`
balance. A zero balance is valid, but cannot fund a refill. The USDT0 a refill
needs depends on the exchange's price and the allowed slippage, and its
transaction fee must fit the configured fee limit. Solvency concerns the backing
of all internal balances; it does not establish that the kernel has enough POL
to transact.

The `polygon` world refills below 5 POL and targets 15 POL. The Arbitrum worlds
use ETH: `arbitrum-one` refills below 0.001 ETH and targets 0.003 ETH, and
`arbitrum-sepolia` uses 0.0002 and 0.0004 ETH. These thresholds describe the
refill policy, not the cost of any one transaction.

Fees replenish `sys`, but whether they cover fuel depends on activity and costs.
The operator can add USDT0 through the deposit procedure above, or send POL
directly. Automatic refills also require a reachable node, a working venue, and
a purchase within the configured limits. See
[How the kernel keeps itself in fuel](duties.html#how-the-kernel-keeps-itself-in-fuel).

{: .warning }
> Without enough POL, outgoing payments cannot proceed. Deposits and calls
> continue, but a blocked withdrawal or settlement payment waits until the
> shortage is resolved. Buying POL also costs a transaction fee.

Ticket settlement has its own funding rules and imposes no minimum operator
balance. See [Who funds a ticket](network-economy.html#who-funds-a-ticket) for
the distinction between funding the payment and paying its blockchain fee.

## Starting without a terminal

For unattended first boot, name the kernel on the command line and provide the
superuser password through `JUICE_BOOTSTRAP_PASSWORD`:

```console
$ JUICE_BOOTSTRAP_PASSWORD=… juice kernel serve play --kernel-handle acme --listen-addr :4040
```

Naming settings is consent to create the kernel, so nothing is asked. Writing
them to `config.json` beforehand does the same, and the two can be mixed. On a
first boot the effective settings are written to that file as the new kernel's
configuration; on every later boot an option applies to that run alone.

The kernel prints the `sys` recovery phrase once on stderr. A program that
boots the kernel for someone generates the phrase itself and passes only its
public key, the same value `POST /v1/users` takes as `recovery_public_key`:

```console
$ JUICE_BOOTSTRAP_PASSWORD=… JUICE_BOOTSTRAP_RECOVERY_KEY=… juice kernel serve play --kernel-handle acme
```

The kernel then enrolls that key and prints no phrase.

If required configuration is missing and no terminal is available, startup
fails with a message identifying the missing setting. A missing or short
password, or a malformed recovery key, fails the same way, naming its
variable, before anything is written.

## The kernel's home

Each kernel has a home directory containing the state needed to run it:

```
~/.juice/kernels/play/
  juice.db        accounts, actions, ledger, and the signing key
  config.json     configuration, written once at first boot
  serve.lock      held by the running server
  cache/          regenerable; safe to delete
```

On a chain network, the directory also contains the rail key and payment
records. Keep these files together when moving or backing up the kernel: the
account ledger, signing identity, and external payments belong to the same
installation state.

To run another kernel under the same Juice installation, choose another world.
For example, after creating `workshop.json` as described in
[Configuring worlds](worlds.html#creating-a-world), start it with:

```console
$ juice kernel serve workshop --listen-addr :4242 --fed-listen-addrs /ip4/0.0.0.0/tcp/31314
```

Choose a distinct HTTP listening address, and give the second kernel its own
`fed_listen_addrs`: the first one holds the standard federation port, and the
second refuses to start rather than share it. An exclusive lock permits only one server to use a kernel's home at
a time. Its database and configuration locations follow from the home rather
than separate `--db` or `--config` options.

## Stopping, backing up, upgrading

Stop the server with `SIGINT` or `SIGTERM`. It stops accepting requests, drains
current work, and exits. Juice has no separate stop command.

For a consistent backup, stop the server and copy the complete home directory.
Copying individual files while the server is running is not a supported backup
procedure.

To upgrade, coordinate with the other operators on your network. Stop new
cross-kernel calls and let all existing calls settle on every kernel before
stopping the servers, replacing the executables, and restarting them together.
Startup refuses the migration while a call is still in doubt here, but cannot
check another kernel's records. Skipping this procedure can execute an old call
again when its buyer retries.

Startup applies forward database migrations. An older binary refuses a
database whose migration version it does not recognize.

Juice provides no kernel-removal command. Before archiving a home directory,
account for its balances and unsettled obligations, since the directory holds
the records and keys needed to resolve them.

## Joining the network

The world file's `seeds` are addresses of kernels to contact when joining the
network. Through them, kernels find peers and exchange public catalogues.
`play`, `polygon` and `arbitrum-one` include project seeds. On
`arbitrum-sepolia`, add the address of a kernel already serving that world.
To use a different seed on any world, edit `seeds` in its world file. Eligible
public actions then become available to remote callers without another
registration step.

Peers are identified by public key. A kernel behind a home router can be reached
directly, by hole punching through the router, or through another kernel
carrying its traffic as a relay, without configuring port forwarding. Publicly
reachable kernels also help route traffic;
every kernel listens on port 31313 unless its configuration says otherwise, so
a seed is dialable at a known address with nothing to configure.

Discovery is separated by network, so kernels find peers on their own network
and nowhere else. You can inspect the local transport addresses with:

```console
$ juice admin kernel show
…
Listen addresses:
  /ip4/127.0.0.1/tcp/31313/p2p/12D3KooWJHdK…
```

From another machine, request `GET /health` at the kernel's HTTP address. Its
`fed_addrs` field lists the federation addresses; no login is needed.

## Checking it is up

On the serving machine, `serve` has already registered `acme`; no `kernel add`
is needed.

```console
$ juice kernel health acme
ok  acme  network play  fdlMi64P…  v0.14.46
```

Health checks require no login and report the server's identity and version as
well as its status. Clients use that identity to check they have reached the expected kernel.

### Metrics

A kernel can export its own counters to Prometheus. Set `metrics_listen_addr`
in `config.json`, or pass `--metrics-listen-addr`:

```console
$ juice kernel serve play --metrics-listen-addr 127.0.0.1:9100
```

and point Prometheus at it:

```yaml
scrape_configs:
  - job_name: juice
    static_configs:
      - targets: ["127.0.0.1:9100"]
```

The page reports the build, the number of connected peers, calls by outcome
and how long they took, and requests to and from peers. It also reports retries,
calls still waiting for a peer's answer and the oldest of them, the oldest
payment a peer still owes, whether outgoing payments are halted, and whether
the books balance. It names no user, peer or action.
It needs no login, so anyone who can reach the address can read it: keep it on
loopback or a network only your monitoring reaches.

## Logs

Logs are written to stderr and, if configured, a log file. Command response
data uses stdout. Transition records include the request, caller, process,
trace, action, and transaction identifiers needed to follow a call across
stages. Credentials and other secrets are excluded.
