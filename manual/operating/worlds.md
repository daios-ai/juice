---
title: Configuring worlds
parent: Operating a kernel
nav_order: 2
---

# Configuring worlds

A world defines the network a kernel serves: the money it uses, how payments
settle, and where it first meets peers. Its JSON file lives in
`$JUICE_HOME/worlds/`, or `~/.juice/worlds/` with the default installation root.
The filename without `.json` is the world's name.

The shipped worlds are installed on the first `kernel serve` and are never
overwritten. You can edit their connection settings or add a file for a network
of your own. Restart the kernel to apply changes. An unknown field causes
startup to fail.

## Changing connection settings

On a chain world, `rpc` is the URL of the node the kernel uses to read payments
and submit transactions. Edit it to use another provider or your own node on
the same chain.

`seeds` is a list of federation addresses for kernels already serving the world.
Obtain an address from that kernel's `juice admin kernel show`, or from the
`fed_addrs` field of its public `GET /health` response. Choose an address
reachable from the joining machine and copy it in full, including its peer id:

```json
"seeds": ["/ip4/203.0.113.10/tcp/31313/p2p/12D3KooW…"]
```

The address above is illustrative. Replace it with the seed's actual address
and complete peer id. A seed must serve the same world. With an empty list,
the kernel does not discover new peers through the network, though it can
continue synchronizing with known counterparties.

Changing the node or seeds keeps the kernel on the same network. Seeds provide
meeting points; they do not restrict membership.

## Creating a world

For a separate network with no real money, create
`~/.juice/worlds/workshop.json` with these contents:

```json
{
  "rail": "manual",
  "decimals": 6,
  "symbol": "credits",
  "description": "workshop credits with no real monetary value",
  "seeds": []
}
```

`rail` selects how payments settle. With `manual`, operators record users'
deposits and kernels settle their obligations automatically through payment
reveals. These credits have no real monetary backing. `decimals` sets the
display precision: here, 1000000 base units appear as 1 credit. `symbol` labels
amounts, and `description` explains the world when a kernel is created.

Start the first kernel:

```
$ juice kernel serve workshop
```

Its home is `~/.juice/kernels/workshop/`. First boot asks for the kernel's own
nickname and superuser password, as in [Running a kernel](running-a-kernel.html).
If another kernel is already running on the machine, give this one distinct
HTTP and federation listening addresses.

To connect more kernels, add the first kernel's reachable federation address
to `seeds` and give the file to the other operators. Each saves it under the
same filename in their installation and runs `juice kernel serve workshop`.
Keep the display precision consistent so everyone reads amounts the same way.
Anyone with the world definition can join; a separate world has no membership
approval mechanism.

## Using a chain

For a world whose payments settle on a chain, start from a shipped `evm` world
file. Keeping its filename and defining fields keeps you on that network;
copying it under a new filename creates a separate network, even when it uses
the same chain and token.

In addition to the display fields and seeds, an `evm` world contains:

| Field | Meaning |
|---|---|
| `rail` | `evm`, for finalized chain payments |
| `chainId` | the chain's numeric id |
| `token` | the token's contract address |
| `decimals` | the token's decimal precision, checked against the chain |
| `rpc` | the chain node's URL |
| `finality` | the block status used for settlement: `latest`, `safe`, or `finalized`; omitted means `finalized` |
| `venue` | the Uniswap V3 `router` and `quoter` addresses, the `wrappedNative` currency the router unwraps into fuel, pool `feeTier`, and `router02` selector |
| `gas` | the fuel purchase settings described below |

The venue must belong to the chosen chain. Within `gas`, `min`, `max`, and
`feeBound` are strings containing whole numbers of wei; `slippageBps` is a
number in basis points, and `paymentGas` and `swapGas` are gas-unit estimates.
The [fuel settings reference](../reference/config.html#fuel-on-a-chain-network)
explains the thresholds. Using `latest` or `safe` accepts payments earlier than
`finalized`, with the corresponding risk of a chain reorganization.

On first boot the kernel verifies the chain, token, decimals, and venue before
serving. It records its own starting block for payment scanning; there is no
scan-start setting in the world file. Follow
[Setting up on a chain](running-a-kernel.html#setting-up-on-a-chain) to fund it.

## What fixes the network

The world's name, `rail`, `chainId`, and `token` together identify the network.
Kernels must agree on them to discover each other and verify each other's
signed records. The kernel records that identity at creation and refuses to
start if the world later describes a different network.

To change those defining fields, create another world and a new kernel. Editing
`rpc`, `seeds`, or fuel settings does not change network identity. A kernel's
nickname is separate too: `acme` serving `play` still keeps its state in
`kernels/play/`.
