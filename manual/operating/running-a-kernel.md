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

```
$ juice kernel serve acme --addr :4040
```

The name `acme` selects the kernel's directory and supplies its initial nickname.
The `--addr` option sets its HTTP listening address. On first start, the command
asks you to confirm creation and select the network whose money the kernel will
use:

```
There is no kernel named acme. No kernels here yet.
Create acme as a new kernel? [y/N] y

Which money will acme use? This cannot be changed later.
  play  no real money: you credit accounts yourself and keep the records
  test  fake USDT on the Arbitrum Sepolia test chain
  real  USDT on Arbitrum One
Choice [play/test/real]: play
Superuser password:
Confirm password:
sys recovery phrase (write this down; it is shown only once and cannot be recovered):
  depart motion moon climb useless hole learn usage delay fish brand window
Press Enter once you have written it down:
Superuser "sys" created.
INF server.ready handle=acme network=play addr=[::]:4040 public_key=L3ciw7zj…
```

On `test` and `real`, first boot also reaches the chain and records the block it
starts watching for payments from. It asks nothing more: those networks name a
node to reach them through. See [Setting up on a chain](#setting-up-on-a-chain).
Declining creation or stopping before the network is chosen leaves no kernel
state on disk.

{: .warning }
> The network cannot be changed afterwards. A kernel serves the one it was created
> with for its whole life; using another means creating another kernel.

After verification, the selected network is recorded in the database. Later
starts use that record to ensure the kernel continues on the same network.
Operational settings such as listening addresses, fees, bootstrap peers, and
the chain endpoint can be changed in configuration.

{: .warning }
> Save the recovery phrase before pressing Enter. It is shown once, it is the only
> way to reset the `sys` password (`juice auth recover sys@acme`), and nobody can
> recover it for you.

With setup complete, later starts use the saved state and print a ready line
identifying the kernel, network, and public key.

## Setting up on a chain

A kernel on `test` or `real` needs access to the chain and funds for transaction
fees. It generates its own rail key during setup. The sequence is:

**1. Decide which node to use.** `test` and `real` name a public one, so there is
nothing to do here. The kernel reads payments and submits transactions through
it. To use a hosted node or your own instead, set `rail_rpc` in the kernel's
configuration; it overrides the network's and may be changed later.

The first boot must reach that node, and everything it checks there must answer:
the chain is the one named, the token at that address is the one named, and the
kernel can buy the gas that sends a payment. Creating a kernel fixes its network
for life and publishes the address people pay to, and it records the block from
which payments are watched for, which cannot be guessed afterwards. So a first
boot that cannot reach the chain, or finds any of that wrong, creates no kernel:
no superuser, nothing serving. What it had already written, including its chain
key, stays where it is, so fixing the cause and starting again continues from
there rather than beginning afresh.

Once the kernel exists, that is behind it. An unreachable node or a broken
venue only delays money: the kernel serves, and the payment commands wait and
say what is unready.

**2. Start the kernel.** First boot generates `rail.key` in the kernel's home.
This key controls its account on the chain.

{: .warning }
> `rail.key` controls the kernel's money on the chain. It is created once and
> never regenerated. Back it up with the rest of the home, and lose it and you
> lose what the kernel holds.

**3. Read the kernel's address.** After registering the kernel with your client
and logging in as `sys`, inspect its rail address:

```
$ juice admin kernel show
Handle:     bank
Network:    real
Paid at:    0xcaf2a882af8730c6ad92d76361b1952c71c0453f
Holdings:   0.00 USDT (gas 0.00) as of block 13
…
```

**4. Fund transaction fees.** Send ETH to that address, following the procedure
below.

## Funding the kernel

The kernel needs ETH to submit withdrawals and paying settlement tickets.
A new kernel has none, so fund its address before expecting outgoing payments
to complete. The address appears as `Paid at:` in `admin kernel show`.

The shipped Arbitrum One settings trigger a refill below 0.001 ETH and target
0.003 ETH; Arbitrum Sepolia uses 0.0002 and 0.0004 ETH respectively. These
thresholds describe the configured refill policy, rather than a guarantee of
how much a particular transaction will cost.

Once funded, the kernel can buy more ETH using its own USDT earnings. User
backing is excluded from that spending. A refill itself needs ETH, however,
so a kernel that falls below the cost of submitting one may need another
operator top-up. See
[How the kernel keeps itself in fuel](duties.html#how-the-kernel-keeps-itself-in-fuel).

{: .warning }
> Without sufficient ETH, outgoing payments cannot proceed. Deposits and calls
> can continue, but a blocked withdrawal or settlement payment requires the
> shortage to be resolved. Buying ETH also requires a transaction fee.

ETH sent to the rail address supplies the kernel's transaction fees and is not
credited to a user's balance. Users deposit USDT at the same address, so explain
the distinction when giving funding instructions; the deposit chapter covers it
in [Step 3](../money/deposits-and-withdrawals.html#step-3-send-the-usdt).

The kernel's own USDT ordinarily accumulates as fees in the `sys` account.
You may add to that balance through an ordinary deposit: register a sender
address for `sys` and send the token from it. These account funds are separate
from the ETH supplied for blockchain fees.

## Starting without a terminal

For unattended first boot, create the configuration with the network choice
in advance and provide the superuser password through
`JUICE_BOOTSTRAP_PASSWORD`:

```
$ mkdir -p ~/.juice/kernels/acme
$ echo '{"world":"play"}' > ~/.juice/kernels/acme/config.json
$ JUICE_BOOTSTRAP_PASSWORD=… juice kernel serve acme --addr :4040
```

If required configuration is missing and no terminal is available, startup
fails with a message identifying the missing setting. A network that names no
node of its own also requires `rail_rpc`.

## The kernel's home

Each kernel has a home directory containing the state needed to run it:

```
~/.juice/kernels/acme/
  juice.db        accounts, actions, ledger, and the signing key
  config.json     configuration, written once at first boot
  serve.lock      held by the running server
  cache/          regenerable; safe to delete
```

On a chain network, the directory also contains the rail key and payment
records. Keep these files together when moving or backing up the kernel: the
account ledger, signing identity, and external payments belong to the same
installation state.

To run another kernel under the same Juice installation, choose a different
name:

```
$ juice kernel serve beta --addr :4242
```

Choose a distinct HTTP listening address. Federation uses OS-assigned ports
by default; if you configure fixed `fed_listen_addrs`, avoid collisions there
as well. An exclusive lock permits only one server to use a kernel's home at
a time. Its database and configuration locations follow from the home rather
than separate `--db` or `--config` options.

## Stopping, backing up, upgrading

Stop the server with `SIGINT` or `SIGTERM`. It stops accepting requests, drains
current work, and exits. Juice has no separate stop command.

For a consistent backup, stop the server and copy the complete home directory.
Copying individual files while the server is running is not a supported backup
procedure.

To upgrade, stop the server, replace the executable, and start it again.
Startup applies forward database migrations. An older binary refuses a
database whose migration version it does not recognize.

Juice provides no kernel-removal command. Before archiving a home directory,
account for its balances and unsettled obligations, since the directory holds
the records and keys needed to resolve them.

## Joining the network

The default `bootstrap_peers` connect the kernel to the network, after which
it discovers peers and exchanges public catalogs. Eligible public actions are
then available to remote callers without another registration step.

Peers are identified by public key. A kernel behind a home router can be reached
directly, through hole punching, or through a relay, without configuring port
forwarding. Publicly reachable kernels also support routing and relay traffic;
public bootstrap nodes conventionally listen on port 31313.

Discovery is separated by network, so `play`, `test`, and `real` kernels find
peers in their own network. You can inspect the local transport addresses with:

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

Health checks require no login and report the server's identity as well as its
status. Clients use that identity to check they have reached the expected kernel.

## Logs

Logs are written to stderr and, if configured, a log file. Command response
data uses stdout. Transition records include the request, caller, process,
trace, action, and transaction identifiers needed to follow a call across
stages. Credentials and other secrets are excluded.
