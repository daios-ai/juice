---
title: Running a kernel
parent: Operating a kernel
nav_order: 1
---

# Running a kernel

## Starting one

```
$ juice kernel serve acme --addr :4040
```

`acme` names the kernel. It is the nickname the kernel reports on the network and
the name of the directory holding its state. `--addr` is where it answers HTTP
clients.

The first time, the command asks two things it cannot revise later, and creates
nothing until they are answered:

```
There is no kernel named acme. No kernels here yet.
Create acme as a new kernel? [y/N] y

Which money will acme use? This cannot be changed later.
  play  no real money: you credit accounts yourself and keep the records
  test  fake USDC on the Arbitrum Sepolia test chain
  real  USDC on Arbitrum One
Choice [play/test/real]: play
Superuser password:
Confirm password:
sys recovery phrase (write this down; it is shown only once and cannot be recovered):
  depart motion moon climb useless hole learn usage delay fish brand lab
Press Enter once you have written it down:
Superuser "sys" created.
INF server.ready handle=acme network=play addr=[::]:4040 public_key=L3ciw7zj…
```

On a network with a chain there is one further question, for the endpoint that
reaches it. See [Setting up on a chain](#setting-up-on-a-chain).

Declining, or stopping before the network is chosen, leaves nothing on disk.

{: .warning }
> The network cannot be changed afterwards. A kernel serves the one it was created
> with for its whole life; using another means creating another kernel.

It is recorded in the database once the kernel has verified it, and from then on
the database is what says which network this kernel serves. Everything else — the
address, the fees, the peers, the chain endpoint — is configuration you can
change.

{: .warning }
> Save the recovery phrase before pressing Enter. It is shown once, it is the only
> way to reset the `sys` password (`juice auth recover sys@acme`), and nobody can
> recover it for you.

Later boots ask nothing and print the ready line, which names the nickname, the
network and the public key that answered.

## Setting up on a chain

A kernel on `test` or `real` needs three things from you: a way to reach the
chain, its own key, and enough ETH to pay a transaction fee. Do them in this
order.

**1. Get an endpoint for the chain.** This is a URL the kernel uses to read the
chain and send payments. Hosted node providers give them out, free tiers
included, and you can run your own node instead. First boot asks for it, and it
is kept in `rail_rpc`, which you can change later.

**2. Boot the kernel.** It creates its own key for the chain, in its home, as
`rail.key`.

{: .warning }
> `rail.key` controls the kernel's money on the chain. It is created once and
> never regenerated. Back it up with the rest of the home, and lose it and you
> lose what the kernel holds.

**3. Read the kernel's address.** It exists only after that first boot, because
the key is made then.

```
$ juice admin kernel show
Handle:     bank
Network:    real
Paid at:    0xcaf2a882af8730c6ad92d76361b1952c71c0453f
Holdings:   0.00 USDC (gas 0.00) as of block 13
…
```

**4. Send it ETH**, as described next.

## Funding the kernel

The kernel pays a transaction fee, in ETH, on every payment it makes: a user's
withdrawal, and a payment owed to another kernel. It cannot make its first one
without some ETH of its own, and it starts with none.

Send ETH to the address `admin kernel show` prints as `Paid at:`, from any wallet.
On Arbitrum One the kernel treats 0.001 ETH as its floor and tops itself up to
0.003, so send a few thousandths — a few dollars' worth at ordinary prices. On
Arbitrum Sepolia the figures are ten times smaller and the ETH is free from a
faucet.

This is initial funding rather than a one-off. Once the kernel has ETH it keeps
itself supplied by selling a little of its own USDC for more, on the exchange the
network names, spending its earnings and never a user's balance. It can still fall
back to you: a kernel whose ETH drops below what that purchase itself costs can no
longer make it, and needs sending more. See
[How the kernel keeps itself in fuel](duties.html#how-the-kernel-keeps-itself-in-fuel).

{: .warning }
> Until it has that ETH the kernel can take deposits and run paid calls, but
> cannot pay anything out: withdrawals and payments to other kernels wait, and
> `admin kernel show` reports a halt. The kernel cannot fix this itself, because
> buying ETH is also a payment and needs ETH to send.

This ETH is the kernel's own and is credited to no account. It arrives at the same
address your users send their USDC to, which is why they must be told to send USDC
and not ETH:
[Step 3](../money/deposits-and-withdrawals.html#step-3-send-the-usdc) warns them.

The kernel needs no USDC of its own to operate: its own USDC accumulates as the
fees it earns, held in the `sys` account. If you want to put more of your own USDC
into `sys`, deposit it the way any account holder would, by registering an address
for `sys` and paying from it. That is an ordinary deposit and has nothing to do
with the ETH above.

## Starting without a terminal

Write the answers into the configuration first. That file is the consent a machine
with no terminal can give.

```
$ mkdir -p ~/.juice/kernels/acme
$ echo '{"world":"play"}' > ~/.juice/kernels/acme/config.json
$ JUICE_BOOTSTRAP_PASSWORD=… juice kernel serve acme --addr :4040
```

Without both of those, a kernel with no terminal refuses to start and names the
key and the file that would have answered.

## The kernel's home

Everything a kernel is lives in one directory:

```
~/.juice/kernels/acme/
  juice.db        accounts, actions, ledger, and the signing key
  config.json     configuration, written once at first boot
  serve.lock      held by the running server
  cache/          regenerable; safe to delete
```

On a network with a chain, the rail key and its records are here too.

The ledger, the key that signs its receipts, the key that settles them and the
network all three belong to are only meaningful together, which is why they live
in one directory. Back it up, move it and lock it as a unit.

A second kernel is a second name beside the first, not a second installation:

```
$ juice kernel serve beta --addr :4242
```

Each needs its own HTTP address and its own `fed_listen_addrs`. Nothing allocates
ports; a collision is a startup failure.

One server runs per kernel, enforced by the lock.

There is no `--db` and no `--config`. A kernel is its directory.

## Stopping, backing up, upgrading

`SIGINT` or `SIGTERM` stops accepting new requests, drains what is running, and
exits. There is no `juice stop`.

To back a kernel up, stop the server and copy the directory. Copying it while the
server runs is not a supported way to take a consistent snapshot.

To upgrade, stop the server, replace the binary, and start it again. Migrations run
forward at startup. A database recording a migration newer than the binary knows is
refused rather than opened.

Removing a kernel is not a command. Its directory holds a ledger, two keys, and
possibly obligations that have not settled, so archive or destroy it deliberately.

## Joining the network

Kernels find each other by public key over their own transport. Joining requires
nothing beyond starting with the default `bootstrap_peers`; serving requires
nothing beyond marking an action `public`.

A kernel behind a home NAT federates like any other. It advertises no address and
needs no port forwarding: it is found by key, and reached directly, by hole
punching, or through a relay.

A kernel on a public host is also the network's bootstrap node and relay. Those
bind port 31313 by convention.

Kernels on different networks never meet. `play`, `test` and `real` discover each
other in separate namespaces.

```
$ juice admin kernel show
…
Listen addresses:
  /ip4/127.0.0.1/tcp/31401/p2p/12D3KooWJHdK…
```

## Checking it is up

```
$ juice kernel health acme
ok  acme  network play  fdlMi64P…
```

Unauthenticated, and the thing a client should check before trusting a server.

## Logs

Logs go to stderr, and to a file if one is configured. Only an action's output goes
to stdout. Every kernel transition logs its start and end with the request, caller,
process, trace, action and transaction ids, so one call can be followed end to end.
Secrets are never logged.
