# Worlds

A **world** is one network: the money it settles in, the servers to meet it through, and nothing
else. It is a JSON file. This document says what that means, why the fingerprint exists, and what
an operator can do with a file of their own.

## The file

Every world file lives in `$JUICE_HOME/worlds/`. The worlds this build ships — `play`,
`arbitrum-sepolia`, `arbitrum-one` — are written there the first time `kernel serve` runs and
never overwritten, so an operator's edit outlives an upgrade and a network juice does not ship is
a file they add. The kernel reads worlds from that directory alone; the copies inside the binary
exist only to seed it.

```json
{
  "rail": "evm",
  "chainId": 421614,
  "rpc": "https://sepolia-rollup.arbitrum.io/rpc",
  "token": "0x8e87deee3BF1eFE27E8e96ABf205BEDF802ed568",
  "decimals": 6, "symbol": "USDT",
  "description": "fake USDT on the Arbitrum Sepolia test chain",
  "seeds": [], "finality": "latest",
  "venue": { … }, "gas": { … }
}
```

The filename is the world's name: the file carries none of its own, so the two cannot disagree.
`rail` names the adaptor that witnesses this network's money — `manual`, where a payment is final
when the operator records it or a buyer reveals it, or `evm`, where only a finalized chain payment
is. An `evm` world names its chain, its token, its decimals and the node it is reached through; a
`manual` world names none of them. Any other value is refused by name, as is any key this build
does not know.

`seeds` is the one place a meeting point is named. No kernel setting overrides it: a kernel that
needs another edits its world file, which is also how members of a private network introduce each
other.

## The fingerprint

Kernels are servers run by strangers, exchanging signed messages that move money. Each message must
be tied to one money agreement, so that a receipt made under one cannot be replayed or mistaken
under another. The plain way is to include the agreement in everything signed; the fingerprint is
that content in short form — `SHA-256(JCS({chain_id, name, rail, token}))`, recomputed by each
kernel from its own file, never stored in it.

It rides inside every signature prefix, names the discovery rendezvous, and gates gossip ingress.
Two kernels therefore verify each other exactly when they agree on those four facts, and worlds
that differ in any of them cannot meet, cannot pay each other, and cannot read each other's
catalogs. A name alone would not do: two people can pick one label for different money, and a
label can stay put while the token behind it is edited.

The adaptor is in the fingerprint because two worlds settling by different rules are different
money even where everything else matches. Endpoint, seeds, gas policy, symbol, decimals and
description are not: two operators of one network run their own nodes and their own meeting points
and must still verify each other's signatures. The three shipped fingerprints are pinned in
`rail/world_test.go`; if one moves, every kernel on that network stops verifying the others and
every receipt already stored reports invalid, so a change there is a deliberate protocol break.

What the fingerprint leaves out is worth knowing. Decimals are outside it, and on a chain the rail
checks them against the token before serving; on a manual world nothing does, so two kernels could
render the same base-unit amount differently. Settlement is unaffected — amounts cross the wire in
base units — but a price read by a person could mislead. Seeds are outside it too: whoever hands
you a world file chooses where your kernel first looks for the network. That is a question about
where the file came from, and no fingerprint can answer it.

## One kernel, one world

`juice kernel serve <world>` serves `$JUICE_HOME/kernels/<world>/`. One installation runs one
kernel per world, so the world is the whole of what `serve` is told, and the operator holds one
name rather than two. The kernel's own nickname — what it calls itself on the network, since every
kernel on one shares the world's name — is asked at first boot and kept in its `config.json`.

The database records the fingerprint of the network the kernel was created on, once the rail has
verified it. Every later boot compares the world it was given against that record and refuses a
different one, naming both, before the rail is opened: a kernel offered another network dials
nothing. Nothing maps a fingerprint back to a name, because the name is the file that produced it.

## A network of one's own

Write a file, hand it to whoever should join, and those kernels form an economy: their signatures
verify only among themselves, they discover only each other, and they pay only in the money the
file names. An unguessable name makes the network unfindable by anyone without the file — the file
is then the key to it. It is not, however, a network with a membership rule: anyone holding the
file is a member, and shutting someone out afterwards means suspending their kernel on each of
yours. An admission rule every member enforces would be a new feature, and it belongs to
federation, not to the rail: who may join is not a question about money.
