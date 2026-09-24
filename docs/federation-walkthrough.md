# Federation, walked through

A working log. Pedro and Claude go through federation one part at a time; each session's
conclusions are written here as they are reached. It is a record of the walkthrough, not a
specification: `requirements.md` governs, and where this document and the code disagree, the code
is what runs.

## The outline

1. What a kernel is on the network — its key, and the network it belongs to.
2. How two kernels talk — the transport and its channels.
3. How kernels find each other — seeds, announcing, pulling.
4. What they exchange when they meet — catalogues and evidence.
5. Naming something on another kernel — references, petnames, the local stand-in.
6. Agreeing the price before paying it — the terms hash and re-quoting.
7. Making the call — the signed request, and what makes a retry safe.
8. Deciding whether to serve it — the seller's own money, and the limit on strangers.
9. Paying for it — the per-call draw, and what counts as payment.
10. Proof afterwards, and what happens when things break.

Parts 5 to 10 are in the manual already, told from the buyer's and the operator's side, across
`calling/running.md`, `calling/finding.md`, `calling/records.md` and `operating/network-economy.md`.
Parts 1 to 4 are in no manual page: "gossip" appears nowhere in the manual, "libp2p" once, and the
network fingerprint twice in passing. Whether they belong in the manual at all is still open.

## 1. What a kernel is on the network

**One key, and nothing else.** A kernel makes one Ed25519 keypair at first boot and keeps it for
life. There is no second identity: the address peers dial is derived from that same public key, by
a deterministic one-to-one mapping (`fed.PeerIDFromKey`, `fed/transport.go:53`). So dialing a
kernel and verifying what it signed rest on the same secret, and a kernel that loses the key loses
both. Keys are written as 43 characters of base64url. Key rotation is not supported.

**One network, named by a fingerprint.** The network is 32 bytes: SHA-256 over the canonical JSON
of the world's four defining fields — `chain_id`, `name`, `rail`, `token` (`rail/world.go`,
`World.Network`). Everything else in a world file — the node's address, the seed list, the gas
policy — is local and does not enter it.

**The fingerprint is mixed into every signature.** A signed payload is not the bare content. It is

    "juice/v1/" + <network fingerprint> + "/" + <domain> + "\n" + canonical JSON of the content

(`kernel/federation.go:423`), where the domain names the kind of thing being signed — there are
eleven: `receipt`, `rating`, `manifest`, `evidence_receipt`, `fed_call`, `step_complete`,
`step_list`, `step_auth`, `reveal`, `capability`, `recovery` (`kernel/federation.go:357`).

Two consequences follow:

- Nothing signed on one network verifies on another. A receipt from a play kernel cannot be
  presented as a receipt for real money, even though the same code made both.
- Nothing signed for one purpose verifies as another. A signature that authorises completing a step
  is not a signature that authorises a payment reveal.

**The fingerprint also separates the meeting place.** Kernels announce themselves under
`juice/fed/discovery/1/<fingerprint>` (`kernel/federation.go:414`), so two worlds sharing a
bootstrap node still never meet: they are looking in different places for each other. This is why
changing any of the four defining fields creates a new network rather than altering one.

### The eleven purposes

Nine are signed by a kernel with its own key, one by a user's recovery key, one never leaves the
machine.

Records of what happened:

- `receipt` — the serving kernel's record of an executed call: what was called, hashes of the
  arguments and the reply, the outcome, the money. Verified offline by the buyer and kept.
- `rating` — a kernel's signature over a buyer's rating before it is passed on, so the rating
  carries a kernel's word without naming the rater (`kernel/federation.go:1771`).
- `evidence_receipt` — the stripped projection of a kernel's own receipt, published as evidence:
  no amounts, no identities, only the shape of the call and how it ended.

What is on offer:

- `manifest` — the description of one public action: name, schemas, price, artifact hash. What a
  stranger reads before buying.

Live requests between kernels:

- `fed_call` — a call request.
- `step_list` — a request for the parked work waiting for the asker.
- `step_complete` — the input that finishes one parked step.
- `step_auth` — a home kernel's attestation that one of its users is the principal a step requires.

Money:

- `reveal` — the buyer's secret, which decides the per-call draw and so the payment
  (`kernel/economy.go:405`).

Not on the wire:

- `capability` — `{"cap": <trace id>}`, signed and verified by the same kernel, so code running
  inside a call can act within it without holding a login (`kernel/capability.go:25`).
- `recovery` — the only one a kernel does not sign: the user's recovery key, derived from the seed
  phrase, signs a challenge the kernel issued (`kernel/auth.go:277`).

The separation is required because all eleven are Ed25519 signatures by one key over JSON objects,
and several share fields. `step_list` and `step_auth` both sign `counterparty`, `recipient`,
`timestamp` and `user_id`. Without the domain in the signed bytes, a signature asking "what work is
waiting for my user?" would also read as "I attest this user is the one that step requires". The
code says so where it declines to add a `scope` field, at `kernel/federation.go:2403`.

### The same eleven, as one purchase

Alice has an account on the kernel `acme`. Bob sells a translation action on the kernel `brick`,
priced at 0.002 USDT0. Both kernels are on `arbitrum-one`.

1. Before anything happens, `brick` signs a card for `bob/translate` — its name, what it takes,
   what it returns, the price — and hands that card to any kernel that asks. `acme` stores it when
   Alice searches. That signature is `manifest`. Without it, anyone could hand `acme` a card saying
   Bob sells translation at a price Bob never set.
2. Alice runs the action. `acme` sends `brick` a request: run `bob/translate` on arguments with
   this hash, for `acme`, under the id `7f3a`, at the price we saw. `acme` signs it: `fed_call`.
   That signature is how `brick` knows which kernel owes it money.
3. `brick` runs the work, returns the text, and signs a record of it: this call, this argument
   hash, this reply hash, success, charged 0.002. That is `receipt`. Alice's kernel keeps it. If
   `brick` later denies the call, or Alice disputes the charge, this is the evidence.
4. Now the money. 0.002 USDT0 costs more to send on a chain than it is worth, so the two settle by
   draw: `acme` committed to a secret in step 2, `brick` signed a number into the receipt, and the
   two together decide whether this call pays 1.00 USDT0 or nothing — here a 1-in-500 chance, so the
   payment is 0.002 on average. `acme` signs the secret and sends it: `reveal`. The signature stops
   anyone but Alice's kernel producing a losing secret on her behalf.
5. Alice rates the call good. `acme` signs `{rating, note, which receipt}` and gossips it: `rating`.
   Bob's future customers see that a kernel vouches for a real paid call rated good. Nobody learns
   it was Alice.
6. Both kernels also gossip a stripped record of the call itself — that a call to `bob/translate`
   happened and ended in success, with no amounts and no names. That is `evidence_receipt`.

Steps are the other shape of work. Say the translation parks and waits for a person to approve it.

7. `acme` asks `brick` what work is waiting for it: `step_list`.
8. `acme` sends the approval that finishes the parked step: `step_complete`.
9. The step says only Alice may finish it. `acme` signs an attestation naming her: `step_auth`.
   Without it `brick` would know only that some request came from `acme`, not from whom.

The last two are not federation at all:

10. While `bob/translate` runs inside `brick`, the code needs to call `sys/llm/chat` and charge it
    to the same call. `brick` hands the running code a token — the call's trace id plus a signature
    over it — instead of a login. That is `capability`, and it dies with the call.
11. Alice forgets her password. Her kernel gives her a random challenge; she signs it with the key
    derived from her twelve words; the kernel checks that signature against what it stored when she
    enrolled. That is `recovery`, the only one a kernel does not sign itself.

And the fingerprint from part 1 is what stops step 3 being reusable: the same code runs `play` and
`arbitrum-one`, so without the network inside the signed bytes, a receipt for 0.002 in play money
would read as a receipt for 0.002 USDT0.

### Not endpoints: purposes and channels are different things

The eleven are purposes written inside signatures. They are not addresses and nothing listens on
them. What a kernel listens on is one port — 31313 by default, over TCP and QUIC — carrying one
libp2p connection per peer, and inside that connection there are five channels
(`fed/transport.go:485`):

| Channel | What goes through it |
|---|---|
| `/juice/fed/call/1` | one call, and the receipt or signed refusal that answers it |
| `/juice/fed/step/1` | the parked work waiting for the asker, and the input that finishes one |
| `/juice/fed/resolve/1` | one action's card, or one handle resolved to a principal |
| `/juice/fed/gossip/1` | a kernel's own catalogue, plus one page of evidence |
| `/juice/fed/settle/1` | one draw's secret |

Purposes map onto channels many-to-one. `fed_call` travels on the call channel and `receipt` comes
back on it; `step_list`, `step_complete` and `step_auth` all travel on the step channel, the third
carried inside the second; `manifest` is served on both resolve and gossip; `rating` and
`evidence_receipt` ride gossip. `capability` and `recovery` never leave the machine — one is handed
to code running inside a call, the other arrives on the ordinary client port (4040 by default).

So the port answers the question "where do I dial this kernel", the channel answers "what kind of
exchange is this", and the purpose answers "what does this signature authorise" — and only the last
is inside the signed bytes.

### Why the two are easy to confuse, and why they are separate

The names invite the confusion. `fed_call` is a purpose, `/juice/fed/call/1` is a channel; three
step purposes share one step channel; the purpose `reveal` travels on the channel named `settle`.
There are eleven purposes and five channels, and two of the purposes use no channel at all.

They cannot be the same thing for two reasons.

A channel is a fact about a conversation that is happening now. It exists while the connection is
open and disappears with it. Nothing about the channel survives into the artifact, so a receipt
read from a database a year later, by a program that was never party to any connection, has no
channel to consult — only the bytes that were signed.

A channel is also not covered by the signature. Whoever opens a stream chooses which one to open,
so a peer can send whatever it likes down whichever channel it likes. If the purpose were the
channel, the only thing distinguishing two signed objects would be a choice the sender makes at
send time and nobody can check afterwards. Since `step_auth` rides inside a `step_complete` on one
channel, the attestation and the completion would be indistinguishable by construction.

The purpose is inside the signed bytes because the channel is not.

### Which purpose travels where

Nearly one-to-one, with one exception and two that never travel.

| Purpose | Channel | Direction |
|---|---|---|
| `fed_call` | call | buyer → seller |
| `receipt` | call | seller → buyer, answering the same request |
| `step_list` | step | asker → holder of the parked work |
| `step_complete` | step | asker → holder |
| `step_auth` | step | carried inside a completion, signed by the user's home kernel |
| `manifest` | resolve **and** gossip | seller → anyone asking |
| `rating` | gossip | the rater's kernel → anyone pulling |
| `evidence_receipt` | gossip | either kernel that was party to the call → anyone pulling |
| `reveal` | settle | buyer → seller |
| `capability` | none | kernel → code running inside one of its own calls |
| `recovery` | none | user → their own kernel, over the client port |

`manifest` is the exception: the same signed card is served one at a time when a kernel resolves a
reference (`kernel/federation.go:2300`), and in bulk as part of a kernel's own catalogue during a
gossip pull (`kernel/federation.go:1607`). One artifact, two ways of asking for it.

The call channel is the one carrying two purposes in opposite directions: the request goes out
signed `fed_call`, and the receipt comes back signed `receipt`. A channel with one kind of traffic still carries more than one kind of signed object.

### What a message actually looks like

A real one, signed with the kernel's own code (deterministic keys, so it reproduces). Alice's
kernel `acme` calls `bob/translate` on `brick`, on `arbitrum-one`.

There are three layers.

**The frame.** Four bytes of length, big-endian, then that many bytes of JSON. Nothing else — no
headers, no method, no path. The channel was already chosen when the stream was opened, and the
connection is already authenticated and encrypted by libp2p, so the frame carries no identity of
its own.

**The message**, 533 bytes here:

```json
{
  "action": "0e0b5d6a-2f1c-4a77-9b1e-6b0f2c9a4d31",
  "args": { "text": "buenos dias", "to": "en" },
  "commitment": "c0ffee11…c0ffee88",
  "counterparty": "O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik",
  "expected_contract_hash": "9f2c1d55…5e6f",
  "idempotency_key": "7f3a1c2e-55aa-4f3b-9c21-0d8e4b6a7c19",
  "lottery": 1000000,
  "signature": "gAT2pLq-X41suoOTx3dbhophtXwfy0HrjHHJ9GwzQK6xrpxv_jcWlebqZbsLa7Rso6a8GbL_mxi02QZphNY_Bg",
  "timestamp": "2026-09-18T14:02:11Z"
}
```

`action` is the action's id on the seller's kernel. `counterparty` is the buyer kernel's public
key. `expected_contract_hash` is the terms the buyer saw. `idempotency_key` is this call's name,
reused on every retry. `commitment` and `lottery` are the draw's terms. `args` are the exact bytes
to run on.

**The signed bytes**, which are not the message:

```
juice/v1/04a8e2ce745261cc9cc92214d5ba3ce4b327c8949eb57c7e25b44695608dfef0/fed_call
{"action":"0e0b5d6a-…","args_hash":"a94a5b4b…57c1","commitment":"c0ffee11…","counterparty":"O2onvM62…","expected_contract_hash":"9f2c1d55…","idempotency_key":"7f3a1c2e-…","lottery":1000000,"recipient":"TLWr9q15-…","timestamp":"2026-09-18T14:02:11Z"}
```

Three differences from the message, each deliberate:

- The network fingerprint and the purpose are the first line. They are signed but never
  transmitted: the receiver knows its own network and which channel it is serving.
- `args_hash` replaces `args`. The arguments travel whole and are hashed into the signature, so a
  large payload is not signed twice and the receipt can name the same hash.
- `recipient` — the seller's own public key — is in the signed bytes and absent from the wire. The
  buyer signs whom it dialed; the receiver verifies with its own key. A captured request therefore
  cannot be replayed to a third kernel, which would otherwise see a valid signature from `acme`
  over a call it never asked for (`fed/fed.go:84`).

The JSON that is signed is canonical (RFC 8785): keys sorted, no insignificant whitespace, one
fixed encoding per value. Both sides must produce the same bytes from the same object or every
signature fails, so neither side may pretty-print, reorder, or add a field.

## 2. How two kernels talk

**The address is the key.** A kernel's public key *is* its address. The transport turns the key
into a libp2p peer id by a fixed mapping (`fed/transport.go:53`) and asks the network where that
peer is. No URLs, no DNS names, no certificates: there is nothing to configure and nothing to
impersonate, because a peer that cannot sign as that key cannot complete the connection.

**What is bound.** By default the standard port 31313, on TCP and on QUIC, claimed with an ordinary
socket first so a second kernel on the same host is refused rather than silently sharing it
(`fed/transport.go:210`). An operator who sets `fed_listen_addrs` gets exactly those addresses. In
loopback mode, which is how the test suites run a whole network on one host, ports are
OS-assigned.

**Getting through a home router.** Three mechanisms, in order of preference: a direct dial when the
peer is publicly reachable; hole punching, where both sides dial out at once through their routers;
and a relay, where a third kernel forwards the bytes. Every kernel runs the relay service, so a
publicly reachable `juice kernel serve` becomes a meeting point for NAT-bound peers with no
separate seed program (`fed/transport.go:258`). Port forwarding is never required.

**One connection, five channels.** A stream is opened per exchange, on one of the five protocol
ids from part 1, and the connection is reused. Each stream has a 60-second deadline and an 8 MiB
frame cap (`fed/transport.go:437`).

**What the transport knows.** It moves opaque JSON between keys and nothing more. It does not read
receipts, check money, or decide trust; the kernel does all of that and never imports libp2p. The
transport's one piece of judgement is reporting whether a request provably never left the host — a
dial that failed before any byte was written — because that alone lets the kernel fail a call fast
instead of holding it open for retry (`fed/fed.go:21`). A write that succeeded and a read that then
failed are not that case: the peer may have the request, so the call stays pending.

**What it tells the kernel about the sender.** Just the peer's public key, recovered from the
connection (`fed/transport.go:479`). Ed25519 peer ids carry the key inside them, so this needs no
lookup and cannot be forged: it is the key the connection's encryption was negotiated with. Every
further question — is this peer suspended, does it owe us, may it call this action — is the
kernel's, answered from the key.

### QUIC, and why both

QUIC is a transport protocol built on UDP, standardised in 2021 and best known as what HTTP/3 runs
on. It occupies the same place TCP does — an ordered, reliable, encrypted byte stream — but is
implemented in the program rather than the operating system's kernel, which is why it could change
things TCP cannot.

Three differences matter here.

Encryption is not optional and not separate. TCP opens a connection and then TLS negotiates
encryption on top, two handshakes one after the other. QUIC does both at once, so a connection is
usable after one round trip instead of two or three. A kernel that meets a stranger, asks one
question and closes spends most of its time in handshakes, so this is the common case rather than
an optimisation for bulk transfer.

Loss of one packet does not stall everything else. A juice connection carries five channels; with
TCP they share one ordered byte stream, so a lost packet belonging to a gossip page also holds up
a call and a settlement reveal until it is retransmitted. QUIC's streams are independent: only the
exchange that lost a packet waits.

The connection survives a change of address. It is identified by a number inside the packets, not
by the four-tuple of addresses and ports, so a laptop moving from ethernet to wifi keeps its
connections instead of rebuilding them.

Both are bound because neither is sufficient. Some corporate and campus networks block or throttle
UDP outright, which takes QUIC away entirely; TCP is the fallback that always works. In the other
direction, hole punching through home routers is more reliable over UDP. A kernel offers both and
each peer uses whichever succeeds. The port number is the same, 31313, but a TCP port and a UDP
port of the same number are unrelated things, which is why the addresses list both
`/ip4/…/tcp/31313` and `/ip4/…/udp/31313/quic-v1`.

### Is federation encrypted?

Yes, always, and mutually authenticated at the same time. libp2p has no unencrypted mode and the
code enables none: over TCP the handshake is Noise, and over QUIC it is TLS 1.3, which QUIC
includes by construction. Both give forward secrecy, so recording the traffic and stealing a
kernel's key later does not decrypt what was already sent.

Authentication comes free with the addressing, because the address is the key. The dialer asks for
one specific peer id, which *is* the peer's public key, and the handshake only completes if the
other end proves it holds the matching private key. There is no certificate authority in the
picture and nothing to trust: a man in the middle cannot present someone else's key without that
key's secret half.

The connection keeps the conversation private and proves who is at the other end during it.
Signatures serve a different requirement: an artifact that verifies on its own, read later out of a
database by someone who was never on the connection.

The client edge — `juice` talking to
its own kernel on port 4040 — is plain HTTP by default. That is a local socket on the operator's
own machine, and a kernel exposed beyond that machine is meant to sit behind an ordinary TLS front
end like any other web service (`ecosystem-standard.md`). Kernel-to-kernel traffic on 31313 is
never plain.

## 3. How kernels find each other

There is no registry and no membership list. A kernel builds its own picture of the network by
repeating one pass, at startup and then every five minutes by default.

**The candidate set.** Three sources, merged into one list of public keys:

- the peers it already knows, from its own database — anyone it has traded with or met before;
- the seeds its world file names;
- whoever is currently advertising in this network's place on the DHT.

**The advertisement.** A kernel announces itself under `juice/fed/discovery/1/<fingerprint>` and
then asks who else is announced there. Both calls are time-boxed to ten seconds, and both are
skipped when the world names no seed, so a slow or unreachable DHT cannot stop the kernel syncing
with peers it already knows.

**The pull.** For each candidate key the kernel opens the gossip channel and asks for everything
after the cursor it last recorded for that peer. It then checks three things before believing any
of it: that the responder's claimed key equals the key that was dialed, that the reply parses, and
that every signed item inside verifies. A pull that fails any check counts as failed and writes
nothing.

**What is recorded.** From a good pull: the peer's row (key, nickname, blockchain address and the proof
of it), its catalogue entries, and one page of evidence. The cursor advances only after the page
has been committed, so an interrupted pull is re-fetched rather than skipped.

**What a failure means.** A dial that never connected is recorded as the peer being unreachable. A
stream that broke in the middle records nothing, because it proves nothing about the peer. Rows for
peers not seen for `peer_retention_days` (90 by default) are purged.

**What discovery does not do.** Being listed in the DHT grants nothing: the key is a candidate to
pull from, and until a first-party pull verifies, the kernel has learned only that some peer
claimed to serve this network. No peer is ever taken on another peer's word — a kernel learns
about a stranger only from that stranger, over a connection authenticated as them.

## 4. Gossip, and what it costs

### The picture

B keeps two things: a card index of what it sells, and a logbook of what it has done, in time
order. A visits every five minutes, shows the bookmark it left last time, and B hands back the
whole card index plus the next hundred pages after that bookmark, plus a new bookmark.

    kernel A (puller)                              kernel B (server)
         |                                                |
         |  open stream /juice/fed/gossip/1               |
         |----------------------------------------------->|
         |  { "cursor": "2026-09-18T11:04:02Z | r-8831" }  |
         |----------------------------------------------->|
         |                                                | read own identity
         |                                                | sign every public manifest (<=100)
         |                                                | select evidence after cursor (<=100)
         |  { public_key, handle, network, blockchain_address,   |
         |    action_manifests: [ ...10 cards... ],        |
         |    evidence:         [ ...100 items... ],       |
         |    next_cursor: "...| r-8931" }                 |
         |<-----------------------------------------------|
         |                                                |
         | check: responder key == key dialed             |
         | check: every signature verifies                |
         | write: peer row, catalogue, evidence           |
         | write: next_cursor, stored against B           |
         v

The cursor is a position in B's logbook, held by A:

    B's evidence, ordered by effective time
    +--------------------+------------------+---------------------+
    | r-0001 ... r-8831  | r-8832 ... r-8931| r-8932 ...          |
    +--------------------+------------------+---------------------+
      A already has        this pull          still to come
                         ^ cursor before    ^ cursor after

Across passes, one peer:

    pass 1   cursor ""         ->  catalogue + items 1-100      -> cursor at 100
    pass 2   cursor at 100     ->  catalogue + items 101-200    -> cursor at 200
    pass 3   cursor at 200     ->  catalogue + items 201-300    -> cursor at 300
    ...
    pass n   caught up         ->  catalogue + 0-3 new items    -> cursor at end

Two properties are visible in that column. The evidence column shrinks to nothing once A is caught
up, so it costs what B actually did. The catalogue column never shrinks: it is re-sent, re-read and
re-signed on every line, whether or not a card changed.

### The loop

Every five minutes by default, for each peer key it holds, a kernel does one exchange:

1. send that peer's stored cursor on the gossip channel;
2. receive one reply;
3. verify it — the responder's key is the key dialed, every signature checks;
4. commit what it contains;
5. store the reply's `next_cursor` as that peer's cursor.

The sender holds no state between pulls. All the state is the puller's one cursor per peer.

### The size of one reply

A reply has three parts (`kernel/federation.go:1579`):

| Part | Size | Sent when |
|---|---|---|
| identity: key, nickname, description, network, blockchain address and its proof | ~0.5 KB | every pull |
| catalogue: one signed manifest per public action, capped at 100 | ~1 KB each, so up to ~100 KB | every pull |
| evidence: items after the cursor, capped at 100 | ~0.4 KB each, so up to ~40 KB | every pull |

The catalogue is the fixed cost. Nothing in the request says what the puller already holds and
nothing in the reply says "unchanged", so a 100-action kernel sends its whole catalogue to every
peer on every pull, having re-read and re-signed all 100 manifests to do it
(`kernel/federation.go:2305`).

### The cost over time

One peer pulls 288 times a day. The catalogue alone is then ~29 MB per peer per day, nearly all of
it identical bytes, plus 28,800 signatures. A kernel with P peers pays that P times, and pays it
again in the other direction as it pulls from each of them. The work per interval is O(P × catalogue
size) with no term for how much changed.

### Whether the evidence backlog converges

Two rates decide it.

- Delivery: 100 items per pull, one pull per peer per pass — 20 a minute, 28,800 a day.
- Production: how many gossip-eligible calls the kernel executes.

If production stays below 28,800 a day, a peer catches up and stays caught up. Above it, the cursor
falls behind by the difference, every day, without bound. The evidence query has no floor
(`store/sqlite.go:3329`), so nothing trims the backlog: a peer starting fresh begins at the kernel's
first eligible receipt.

Two worked cases for an action with a million calls:

- A million receipts already stored: a new peer needs 10,000 pulls, about 35 days, and only if the
  kernel executes nothing further meanwhile.
- A million calls a month: 33,000 a day against 28,800 delivered. No peer ever catches up, and the
  gap grows for as long as the traffic lasts.

### What the volume does not affect

A manifest describes terms, not history: name, description, schemas, price, markup, kind, the
executable's hash. It carries no counts, so a million calls cost it the same bytes as ten, and its
signature moves only when its terms move, so a cached copy stays valid until the terms change.
Counts travel as per-call evidence instead, which exists for a different purpose: letting a third
party check a claim rather than trust a total.

### Each limit, and what it is now

| Limit | Where | What it does |
|---|---|---|
| catalogue re-sent unchanged | `GetGossip` | still open: every pull is answered with the page it asked for (S4) |
| catalogue capped at 100 actions | `catalogPage` | one page per pull by action id, marked into a scan generation and swept when the scan completes |
| one page of evidence per peer per pass | `gossipEvidencePageSize` | a peer is read until caught up or its page budget is spent, so a backlog drains |
| per-call evidence at high volume | design | still open: the served window bounds state, not the production rate (below) |

### 100,000 nodes, 10 actions each

Assumptions: 100,000 kernels, 10 public actions each, so 1,000,000 actions and about 1 GB of
catalogue network-wide at ~1 KB per manifest. Defaults: a pass every 300 s, at most 100 providers
enumerated per pass (`fed/transport.go:362`), a 30 s budget for the whole pass, Kademlia's k = 20.

**Who a node pulls from.** Not everyone. The pull set is its counterparties — peers it has actually
traded with, `HasAccount` in `PeerKeys` (`kernel/federation.go:1366`) — plus its seeds, plus up to
100 providers enumerated from the DHT this pass. A stranger discovered once is read once and is not
pulled again unless trade makes it a counterparty.

**Per-node traffic is small.** About 100 pulls per pass at ~11 KB each (identity, ten manifests, an
evidence page) is ~1.1 MB per 300 s, near 30 kbit/s. Serving is symmetric on average: 100,000 nodes
× 100 pulls ÷ 300 s = 33,000 pulls/s network-wide, which spread over 100,000 nodes is 0.33 inbound
pulls per node per second. Bandwidth is not what breaks.

**The rendezvous key is a hot spot.** Every kernel on the network advertises under one string,
`juice/fed/discovery/1/<fingerprint>`, whose hash is one point in the DHT keyspace. Kademlia stores
a provider record on the k ≈ 20 peers closest to that point, so those 20 nodes hold all 100,000
records and answer every lookup. Per pass the network writes 100,000 × 20 = 2,000,000 records and
issues 100,000 queries, so each of those 20 nodes takes ~333 record writes/s and ~333 queries/s,
serving 100 records out of a 100,000-entry set each time. Those nodes are chosen by key distance,
not by capacity or consent.

**The advertisement rate is higher than it needs to be.** `Advertise` is called every pass — 288
times a day per node — and routing discovery returns the TTL it granted, which the caller discards
(`cmd/juice/serve.go:441`). Provider records last hours, so most of those 2,000,000 writes per pass
re-state something already true.

**The sample is not uniform.** `FindPeers` with a limit of 100 returns what those ~20 holders
happen to serve first, not 100 random kernels out of 100,000. Nothing re-randomises it across
passes, so a node may meet a similar slice repeatedly while most of the network stays invisible to
it. Coverage, not bandwidth, is the scaling limit: at 100 new strangers per pass and perfect
sampling, seeing all 100,000 would take 1,000 passes, about 3.5 days — and the sampling is not
perfect.

**What a node ends up knowing.** Its own counterparties, kept fresh every pass, plus a slice of
strangers read once and ageing until the retention sweep drops them. Local search answers out of
that cache, so recall is bounded by the sample, not by the network.

**Order in which things break, and what would lift each.**

| Breaks first | Why | Possible change |
|---|---|---|
| the 20 nodes holding the rendezvous key | one key for the whole network; every node advertises and queries it | shard the namespace (`…/<fingerprint>/<bucket>`) so providers spread over many keys |
| advertisement volume | re-advertised every pass, TTL discarded | honour the TTL routing discovery returns |
| coverage and freshness of the sample | 100 strangers per pass, not re-pulled, not randomised | remember and rotate a stranger set; re-pull on a slow cadence |
| catalogue re-send | only matters for a node with many counterparties | a validator the sender can compute without rebuilding the catalogue (S4) |

At 100,000 nodes the per-call and per-byte costs stay small. What does not survive is one
rendezvous key and a sample that never rotates.

## Open scaling issues

Raised while walking through parts 3 and 4, at a network of 100,000 kernels with 10 public actions
each. None is written; each changes the gossip payload, the discovery namespace, or both, so each
is a proposal.

**S1. One rendezvous key for the whole network.** Every kernel advertises under
`juice/fed/discovery/1/<fingerprint>`, one point in the DHT keyspace, so the k ≈ 20 peers nearest
that point hold every provider record and answer every lookup: ~333 record writes/s and ~333
queries/s each at 100,000 nodes. Those peers are chosen by key distance, not capacity or consent.
*Direction:* shard the namespace into buckets, `…/<fingerprint>/<bucket>`, and have each kernel
advertise in one and enumerate several.

**S2. Advertisement repeats regardless of its lifetime.** `Advertise` runs every pass, 288 times a
day per kernel. Routing discovery returns the TTL it granted and the caller discards it
(`cmd/juice/serve.go:441`). At 100,000 nodes that is ~2,000,000 provider-record writes per pass,
most of them restating a record that has not expired. *Direction:* honour the returned TTL and
re-advertise when it runs out.

**S3. Every known kernel is read in one order, and no peer starves.** A pass builds one set from
counterparties, the cached directory, the seeds and this pass's providers, and sorts it: an
unresolved obligation first, then longest unheard, then by key. A stranger met once is refreshed
like anyone else; the order is the same on every kernel, so a pass cut short resumes where it
stopped rather than re-reading a random few. What remains open is recall: local search still
answers from whatever the directory has turned up.

**S4. An unchanged catalogue is still re-sent.** Nothing in the request says what the puller
holds, so every pull is answered with the page it asked for. A manifest now changes only when its
terms do, so a validator is possible — but only one the sender can compute without rebuilding what
it is trying not to send. Hashing the manifests to answer "unchanged" signs the whole catalogue to
decide not to send it, which costs more than sending it; the version worth building caches each
manifest's signature on the action row and hashes the cached values. At this size neither is worth
the state, so it stays open until the bytes are measured to matter.

**S5. A backlog drains, but the production rate is still not bounded by anything.** A peer is now
read until it has nothing further to say or its page budget is spent, so a backlog converges
instead of trickling one page per interval; and the window served — the newest 200 trades per
subject action — is the window the receiver retains, so nothing is streamed that the consumer
evicts on arrival. That bounds state. It does not bound freshness: if a kernel produces eligible
evidence faster than a peer's service rate (budget ÷ interval), the queue still grows, and only
sampling or a coarser projection would close that.

**S6. The catalogue is scanned, not capped.** Manifests are served a page at a time in action-id
order after a cursor; the receiver upserts each page into a scan generation and, on the page that
ends the catalogue, removes what the scan never mentioned. A catalogue of any size converges after
⌈C/page⌉ pulls, and a page that happens to be empty erases nothing.

**S7. A pass that cannot finish says so.** The loop checks its deadline on every candidate and
reports how many it did not reach. A candidate never dialled is not counted as a failure and
nothing is recorded about its reachability.

### The two kinds of item

**A card** is an offer: what one action is, and what it costs. B signs one per public action
(`kernel/types.go:541`).

```json
{
  "action_id": "0e0b5d6a-2f1c-4a77-9b1e-6b0f2c9a4d31",
  "owner_id": "3c9a…", "owner_handle": "bob",
  "name": "translate",
  "description": "Translate short text between languages.",
  "input_schema":  { "type": "object", "properties": { "text": …, "to": … } },
  "output_schema": { "type": "object", "properties": { "text": … } },
  "price": 2000, "remote_bps": 500,
  "kind": "wasm", "artifact_hash": "sha256:1f0c…",
  "updated_at": "2026-09-12T08:31:00Z",
  "signature": "…"
}
```

A buyer reads a card for the terms. The schemas say what it takes and returns, `price` is the
all-in maximum, `remote_bps` the premium for buying it from abroad, and `artifact_hash` identifies
the exact code — the wasm artifact, or for an http action the endpoint it actually calls, so
repointing it changes the contract. The `action_id` is what a call names, and the same fields hash
to the terms a buyer pins. A card carries no history at all: history is the evidence stream, and a
card that changes only when its terms change is what lets a whole catalogue be cached by its
digest.

**An evidence item** is a claim about one past call: it happened, and it ended this way
(`kernel/types.go:664`).

```json
{
  "evidence_receipt": {
    "receipt_hash": "9c1f…",
    "subject_kernel_public_key": "ZmTu…",
    "subject_action_id": "0e0b5d6a-…",
    "counterparty_kernel_public_key": "O2on…",
    "status": "success",
    "started_at": "2026-09-18T11:02:44Z",
    "created_at": "2026-09-18T11:02:45Z",
    "signature": "…"
  },
  "rating": {
    "rating": 1, "note": "clean translation",
    "rated_receipt_hash": "9c1f…",
    "created_at": "2026-09-18T11:40:02Z",
    "signature": "…"
  }
}
```

What it deliberately does not carry: no transaction, trace or process id, no caller or payer, no
argument or reply hashes, no amounts, and on the rating no rater. A reader learns that this action
was called, whether it worked, roughly when, and — where a rating exists — how the buyer judged it.

`receipt_hash` is the hash of the full receipt the seller signed and the buyer holds. That link is
what makes the item checkable rather than merely asserted: a party to the call can produce the
receipt and show it hashes to this value, and the rating names the same hash, so a rating cannot
float free of a paid call. It is also why a seller's own account of itself can be doubted and
tested: its executions are one kernel's claims, and each buyer's account of the same trades either
confirms them or does not.

### How B gossips about C

B publishes evidence in two legs (P9):

- **(a) its own executions** — calls to B's own public actions. Subject is B, issuer is B. This is
  self-reported.
- **(b) its own purchases** — calls B made to C's action and settled against C's signed receipt.
  Subject is **C** and C's action; issuer is still B. Only admitted executions qualify: a rejection,
  a locally-manufactured settlement or a quarantined receipt is not evidence of anything C did.

So B does testify about C, but only about trades B was party to. What B learned from D about C is
never passed on (`learned evidence is never re-gossiped`, P9). Testimony travels one hop, from a
witness.

The two legs meet at the receipt hash:

    C's own item (leg a)                 B's item about C (leg b)
    issuer  = C                          issuer  = B
    subject = C / translate              subject = C / translate
    receipt_hash = 9c1f…                 remote_receipt_hash = 9c1f…
    counterparty = B                     rating (optional), signed by B
              \                                   /
               \_______ same call ________________/

A reader that holds both marks B's row corroborated: the hash matches a receipt C itself published,
and that receipt names B as the counterparty (`kernel/federation.go:1471`). A buyer's claim with no
such link is stored and displayed, never counted as trade-backed. A rating counts only through the
same link, so a rating cannot float free of a paid call.

The two views are never added together (`kernel/federation.go:1487`). C's self-reported executions
stay one row; each buyer's observed calls stay their own row. A reader compares them rather than
summing them, so a seller inflating its own numbers is contradicted by the absence of buyers saying
the same thing.

**What this defeats.** A seller cannot manufacture reputation alone: its own evidence is
self-issued, and a reader sees how much of it any buyer confirms, and where the two contradict. A third party
cannot smear or boost a kernel it never traded with, because there is no path for a claim from a
non-witness. An issuer that tells two stories about one trade has that trade dropped entirely
(`kernel/federation.go:1407`).

**What it does not defeat.** A seller can run buyer kernels of its own, call itself, and have those
keys corroborate. The link requires a real paid call, so the cost is the operator fee and the rail
cost of settling, not zero, but it is not prohibitive either. What a reader can see is the shape:
corroboration arriving from a small set of keys that trade with nobody else. Nothing in the kernel
weighs that today — it is left to whoever reads the evidence.

### Which key verifies what

B's item about C is signed by B, and B's key is what verifies it.

**Authenticity of the item** is checked with the *issuer's* key — the kernel that gossiped it
(`kernel/federation.go:2024`, verifying against `issuerKey`). B's item about C is signed by B and
verifies with B's key. C's key verifies nothing in it, because C signed nothing in it.

**Whether C really did the thing** is not established by that signature at all. B's item carries
`remote_receipt_hash`, and a hash is not a signature: on its own it is a number B asserts. It gains
force only when C's own item, signed by C, names the same hash and reports the same call
(`kernel/federation.go:1471`).

One signature check says who wrote the item. The match against C's own item says C agrees.

The reason B cannot simply forward C's signed receipt is the privacy projection: the receipt carries
amounts, the caller's identity and the argument and reply hashes, none of which may travel (P9,
U39). B holds that receipt privately and can verify it with C's key — that is the buyer's own proof
for settlement and disputes — but what it publishes is the hash.

**S8. Two kernels contradicting each other is counted as a contradiction.** B's row about a call
and C's own row about it are matched on `remote_receipt_hash` and on C's receipt naming B as the
counterparty; the link now also requires the two statuses to agree. A pair that links but disagrees
is two signed statements that cannot both be true: it counts on the row's `contradictions` and
never as corroboration, on the pair rather than on either kernel, since the evidence does not say
which one lied. Equivocation — one issuer telling two stories about one trade — is separate and
voids that whole trade.

### What A can ask for, and what A does with it

**A cannot ask about a particular action or a particular kernel.** The request has one field, the
cursor (`kernel/types.go:678`). B decides the rest: its whole catalogue, and the next hundred
evidence items in effective-time order. There is no filter, no subject selector, no "tell me about
C".

**B chooses what to serve, within rules A can check.** A verifies that the responder's key is the
key it dialed, that every signature verifies against the issuer, and that a rating hashes to the
receipt it claims. What A cannot check is omission: B may simply not mention a call, and nothing in
the protocol reveals the gap. The cursor is B's own ordering, so a page is what B says comes next.

**A stores three things**, all in its own database:

- `kernels` — one row per kernel it has met: key, nickname, blockchain address and proof, last seen.
- `discovery_docs`, with a full-text index — the catalogue cards, which is what local search reads.
- `evidence` — one row per bundle, keyed by issuer, subject action and remote receipt hash.

**A derives, on demand, not on ingest.** `SubjectEvidence` (`kernel/federation.go:1455`) reads the
rows about one subject kernel and produces one row per issuer per action, holding uses, successes,
failures, average latency, trade-backed rating count and mean, unverified rating count, and — for a
buyer's row — how many of its interactions are corroborated by the subject's own evidence. The
self-reported row and each buyer's row are kept apart and never summed. `admin peer inspect` is
where an operator reads them.

So A does not compute a reputation score. It keeps the claims, marks which ones link up, and shows
them separately, leaving the judgement to whoever is reading.

### What a user on A actually sees about `bob@B/translate`

Traced through the read paths:

- **Search** (`sys/lookup`) returns the reference, the description, both schemas, the all-in price,
  a match score, and the same record the action's own read carries. Self-reported usage fields are
  still absent, and a test asserts it: what travels is evidence with its provenance, not totals.
- **`action show bob@B/translate`** cold-resolves the card from B, stores a proxy row, and shows
  the contract and the record: A's own calls through the proxy, B's own report about the action,
  and every other kernel's account of trading with B over it.
- **`action ratings bob@B/translate`** returns A's own users' ratings and the trade-backed ratings
  buyers gave on their own kernels, each marked `local` or `peer` — a rating paid for abroad is
  part of the provider's track record here too.
- **`admin peer inspect <B's key>`** is the same derivation over a whole kernel rather than one
  action, which is what makes the operator's view and the buyer's view impossible to disagree.

The join is `(subject kernel, subject action)`: B's key and the action's id on B for a proxy, this
kernel and the row's id for its own. One function computes it, so a local action and a remote one
are read the same way.

### Against the vision

The white paper's frame is that a wish is granted by composing strangers' services, that the money
is bounded in advance, and that conduct is disciplined afterwards rather than licensed beforehand.
The kernel enforces the money half: a price is a subtree bound, a signature binds a proof to one
call, and settlement is one atomic operation. It cannot check that a seller's code does what its
card advertises. Execution deviation — the action that takes the money and returns something worse,
or something else — is left to the market signal.

That signal is what parts 3 and 4 collect: the seller's own execution record, every buyer's record
of trading with it, ratings tied to paid calls, and the corroboration link that makes a fabricated
rating cost a real settlement.

S9 was therefore not a display defect. The only mechanism the design has against execution
deviation was gathered, verified, indexed by subject action, and then shown to nobody but the
operator, while the person deciding whether to call saw price and schemas — the two things that
bound the money and the shape of the reply, neither of which says anything about conduct. That is
closed: the record travels with the action, on a read and on a search hit alike. Ranking still uses
relevance alone, and deliberately: a rank computed from conduct is the opaque score the design
excludes, so the conduct is shown instead of scored.

**S9. The evidence reaches the buyer.** Reading an action — local or remote, by reference or by id,
and on every search hit — carries the record this kernel holds about it, keyed by the subject the
action names: this kernel and the row's id for its own, the provider and the remote id for a proxy.
It is kept in three parts that are never added together: what this kernel itself saw, what the
provider says about itself, and what every other kernel says about trading with it, each with the
share the provider's own record confirms. Ratings given on other kernels are admitted through the
same link. No score: the numbers are shown with their provenance and the reader decides.

## 5. Naming something on another kernel

**Three ways to write a name** (`kernel/call.go:259`):

- `bob/translate` — an action on this kernel.
- `bob@brick/translate` — `bob`'s action on the kernel A calls `brick`. The part after `@` is
  either a petname A chose for that kernel or the peer's 43-character key itself.
- a bare action id, which is an object rather than a path and is never extended.

A reference with no action name means the group's index: `bob@brick` resolves `bob/index` on
brick, the same convention a web server applies to a directory.

**Petnames are local.** `brick` is A's private label for a key. It is never signed, never
transmitted, and never learned from a peer, so a stranger cannot claim a name A already uses by
announcing it. A petname is bound when A itself resolves that kernel successfully; an inbound call
from an unknown peer creates the account with no petname at all.

**What happens on `bob@brick/translate`:**

    ref "bob@brick/translate"
      |
      | 1. resolve the kernel part: petname "brick" -> key ZmTu…, and the
      |    local account that stands for that peer (the "mount")
      v
    look for a cached stand-in under that account, named "bob/translate"
      |                                   |
      | found, active, priced             | missing, inactive, or price-less
      | -> use it, no round trip          v
      |                     2. open /juice/fed/resolve/1 to the peer
      |                        ask for "bob/translate"
      |                     3. peer returns one signed card
      |                     4. write the stand-in row:
      |                          owner  = the mount account
      |                          kind   = remote_proxy
      |                          name   = "bob/translate"
      |                          price  = card price + this kernel's import fee
      |                          hash   = the card's content hash
      |                          visibility: private -> local on success
      v
    a local action id A can call like any other

**The stand-in is the kernel's, not the operator's.** It is enabled and disabled by resolution, not
by hand: `action enable` and `action disable` are refused on a proxy. It re-resolves when it is
absent, inactive, or missing its frozen price; a contract-hash mismatch at call time returns
`ErrTermsChanged` and refreshes it; any non-funding signed rejection deactivates it, so a withdrawn
remote action drops out of A's world after one refusal. The durable lever an operator does have is
`admin peer suspend`.

**Cost.** One round trip the first time, none afterwards while the row stays valid. A group costs
the same as a single action, because `bob@brick` resolves to one index action rather than to a
listing.

## 6. Agreeing the price before paying it

**The quote hash.** Six fields of an action hash to one number: action id, effect, description,
input schema, output schema, price (`kernel/call.go:197`). The id used is the stable one — for a
stand-in, the seller's id rather than A's local row id — so a card seen in search and the stand-in
resolved from it hash identically (`kernel/call.go:211`). Every action read hands that number back
as `quote_hash`.

**Pinning is the caller's choice.** A run may carry the hash it was shown. An empty pin means
nothing was pinned, and the hash is not even computed in that case. With a pin, the check runs
before input validation and before any money is locked: if the current terms hash differently, the
call is refused with `ErrTermsChanged`, carrying the new hash and the new price
(`kernel/errors.go:179`). The price rides along because a client given only the hash could re-arm
and retry blindly, which would defeat the pin; a person re-consents to a number.

**Across kernels the pin is mandatory.** A federated request carries
`expected_contract_hash` in its signed bytes (part 1's message). The seller compares it with its
own current terms. A mismatch is a signed refusal with `refresh_proxy` set, which is the one
condition that invalidates the stand-in and forces a fresh card. Terms that changed are therefore
discovered at the seller, on its own data, rather than trusted from the buyer's cache.

**What this protects.** The description and both schemas are inside the hash, not just the price, so
an action cannot be quietly repurposed while keeping its number. A seller may change terms at any
time; what it cannot do is have a caller pay under terms the caller never saw.

    buyer reads card ----> quote_hash h1
                              |
                      run with pin h1
                              |
              seller: hash current terms -> h2
                              |
                 h1 == h2 ? execute : refuse, return h2 and the new price

### Open question: should a local run check the price it was quoted?

A cross-kernel call always carries `expected_contract_hash`; a local run carries a pin only when
the caller supplies one, and `juice run` supplies one only with `--quote-hash`
(`cmd/juice/cmd.go:1661`).

The remote call sends the hash because the buyer is working from a cached copy of the seller's
terms, which can be out of date. The seller checks it against its own row. A local run reads the
kernel's own row, so there is no cached copy to check. What is left
unprotected in both cases is the same thing: the gap between the price a person read and the price
in force when they run. Without a pin, that gap is unchecked locally and remotely alike — the
remote hash catches a stale cache, not a fresh price change at the seller.

History: the pin was made mandatory on every root run in v0.12.22 (`e10267e`) and reverted in
v0.12.23 (`e1f6795`). The revert's grounds were that it was not asked for, that it broke every
client not sending one — four call sites in juice-ui and juice-services — and that it arrived under
invented vocabulary; 419 deletions against 156 insertions. Those grounds concerned how the change was made. Whether local runs should check the price was
not settled.

Options, undecided:

1. The CLI pins by default: `run` reads the action, takes its hash, sends it, with a flag to skip.
   No protocol change and no other client breaks. It pins what the CLI read a moment earlier rather
   than what the person read, so it is weaker than it looks.
2. The server requires a pin on every root run, as v0.12.22 did. Real consent at the funding
   boundary, and every client that does not send one breaks.
3. Leave it opt-in and say so in the manual, resting on the advertised price being an upper bound
   on the whole call tree.

Decided 2026-09-19: option 1, extended — `run` reads the action, shows the price, asks on a
terminal and proceeds off one, and sends that read's hash as the pin, local and remote alike. The
kernel is unchanged; the pin stays optional on the server. In the plan.

## 7. Making the call

### What the buyer sends

The message from part 1: the action's id on the seller's kernel, the arguments, the terms hash the
buyer is working from, the draw's commitment and face value, a timestamp, and a name for this call
— the idempotency key. The signature covers all of it plus the seller's own key.

### What the seller does, in order

Order matters, because each step decides what the buyer can be told afterwards
(`cmd/juice/fedservice.go:522`).

1. **Check the timestamp.** Too old or too far ahead and the call is refused outright.
2. **Check the signature**, using the seller's own key as the recipient. A request captured from
   another kernel's wire does not verify here.
3. **Find or create the buyer's account.** A stranger with a valid signature gets a zero-balance
   account. No petname is bound: being called is not an act of naming.
4. **Answer a name already settled from its receipt.** Looked up by (name of the call, buyer),
   before anything else is read, because that answer outlives everything else here.
5. **Look up the action by id.** Anything this kernel does not serve abroad — absent, inactive,
   private, an import — is refused with one signed answer that says nothing about which. A refusal
   is deterministic, so it writes nothing at all.
6. **Take the lock**, keyed by (name of the call, buyer): it exists only while work may be running.
7. **Run it**, and let the commit that writes the receipt delete the lock in the same transaction.

### What the name of the call buys

A record exists if and only if execution may have begun, so the second arrival of the same name
from the same buyer finds either the answer or the work:

    first arrival    -> nothing stored  -> take the lock -> execute -> receipt, lock gone -> reply
    retry, finished  -> receipt stored  -> return it, and the reply committed with it, unchanged
    retry, mid-flight-> lock held       -> 409, "duplicate in flight"
    refusal          -> nothing stored  -> the same signed refusal again

The receipt returned on a finished retry is the original one, not a fresh execution, and it stays
answerable for as long as the receipt exists — which is forever, since receipts are never deleted.
That is what lets the buyer retry freely after a lost reply: at worst it gets the same answer
twice, never two charges. The key is scoped to the buyer, so two kernels choosing the same string
never collide, and the receipt itself names both, so a receipt signed for one call can never
settle another.

### The buyer's side of a lost reply

The buyer cannot tell a lost reply from work that never happened, so it keeps the call open and
retries with the same name. Three outcomes end it:

- a receipt arrives, and the call settles against it;
- a signed refusal arrives, and the money is released.

Nothing else ends it. No timer closes it, and the owner cannot force the process shut while it is
waiting: the buyer cannot distinguish work that never ran from work whose answer was lost, so any
deadline would be wrong in one of the two histories, and closing the process would cancel a debt
the seller may already have earned.

The one case where a first attempt fails fast is a dial that never connected: the transport says so
explicitly, the request provably never left, and the buyer refunds immediately rather than waiting
(`fed/fed.go:21`). Any failure after bytes were written stays open, because the seller may have
them.

## 8. Deciding whether to serve it

Three left after this: paying, proof, and failure.

### The seller pays for the work up front

A foreign call runs on the seller's own money. The process that executes it is owned by the action's
owner, and the price is locked from that owner's balance before execution, exactly as if they had
called it themselves (`kernel/kernel.go:2406`). The buyer's kernel owes for it afterwards. So a
provider whose balance cannot cover its own action's price is told no, and the buyer abroad hears
only that the call was declined — the reason belongs to the provider and is logged where they can
read it (`call.provider_unfunded`).

### The limit is on the kernel, not the caller

One number per kernel: everything delivered to foreign buyers, less the cash actually received for
it. Admitting a call raises it by the most that call could owe, markup included, and the call is
admitted only if the result stays under `credit_limit` — default 50,000,000 base units, fifty
units of the world's money. When the call finishes, the figure is corrected down to what was really owed.

That the limit is per kernel rather than per buyer is what makes it Sybil-proof. A buyer who creates
a thousand identities on their own kernel still meets one number here, because the number counts
what this kernel has handed out, not who asked. A peer's account on this kernel holds no money at
all — it exists for identity, attribution and moderation, and its balance is zero on every path.

### Refusals are signed

A refusal for want of credit is a signed 402 carrying `ErrPeerUnfunded`, so the buyer can prove it
was turned away and release the money at once instead of holding it against an answer that is
never coming. The same applies
to a call whose draw exceeds the seller's own ceiling: the ticket's face value is checked against
`lottery_max` and refused in a signed reply, because no retry would change it.

### What the number does not include

Free calls add nothing to it. Neither does a refusal. Only delivered work counts, and only cash
reduces it — a promise to pay does not.

## 9. Paying for it

### Why not just pay

A cross-kernel call typically owes a fraction of a cent. Sending that on a chain costs more than the
debt. The alternatives are to run a tab, which needs bilateral credit and a way to enforce it, or to
pay an amount worth sending, sometimes. Juice does the second.

### The draw

Each obligation `D` settles on its own, under a ticket named by the call's own idempotency key
scoped to the counterparty. Both sides supply half the randomness:

- the buyer commits `SHA-256(secret)` in the call, before the work;
- the seller signs a nonce into the receipt, after it.

Then both compute the same function (`kernel/economy.go:101`):

    D >= L, or L = 0      -> pay D exactly
    otherwise             -> pay L if SHA-256(ticket_id ‖ secret ‖ nonce)[:8] mod L < D, else 0

With `L` = 1 unit and `D` = 0.002, that is one payment of 1.00 in every 500 calls. The expected
payment is `D`, so over many calls each side receives what it is owed, and nobody is paid for
carrying the variance. `remote_bps`, the serving markup, is what prices that variance.

Neither side can steer the outcome: the buyer fixed its half before seeing the nonce, the seller
chose its half without seeing the secret.

### Who holds what, and when

At dispatch the buyer stakes the whole face value `L` from the immediate caller's balance — not the
debt, the face value, because that is what a win costs. The face value is the buyer's choice, and a
seller refuses a ticket above its own `lottery_max`, so the two only trade where the buyer's ticket
is one the seller will take.

At settlement the stake is released and, if the draw pays, the payment is reserved from the same
caller in the same commit.

### The reveal

The buyer sends the secret on the settle channel, signed: immediately when it loses, and once the
payment is final when it wins. The seller recomputes the draw against the commitment it stored and
accepts a loss however it arrives. The trace remembers the secret and whether the seller has
acknowledged it, so an unacknowledged reveal is sent again.

A buyer that never reveals is bounded by the credit limit from part 8, like any other unpaid work.

### What counts as payment

On a chain world, a finalized transfer from the payer frozen when the call was admitted, for the
amount drawn, naming its transaction. One payment closes one obligation.

On a world with no chain, the signed reveal *is* the payment: there is nothing else to wait for, so
the same commit that accepts the reveal closes the obligation.

No operator keeps any part of a draw. `E[payment] = D`, and only cash reduces exposure.

## 10. Proof afterwards, and what breaks

### The receipt

The seller signs one record per executed call: who issued it, the call's ids, hashes of the
arguments and the reply, the outcome, the money split, and the nonce that was its half of the draw
(P5). The buyer stores it.

A buyer can check the whole thing without asking anyone, months later
(`requirements.md:159`): the signature against the seller's key; the stored JSON against its stored
hash; that the receipt names the action the stand-in was for; that the outcome matches what was
recorded; that `net = charge + premium`; that the premium follows the rate recorded at dispatch;
that `charge + premium` never exceeds the quote; that the import fee follows its recorded rate; that
the refund adds up; that the ticket paid what the stored secret, the receipt's nonce and the debt
decide; and that the argument and reply hashes match what was sent and received.

Nothing in that list requires the seller to be reachable, or honest, at the time of checking.

### The public record

Each side publishes a stripped version as evidence, as in part 4: the seller its own execution, the
buyer its purchase, both naming the same receipt hash and neither carrying amounts or identities.
That is the only part of a call that becomes public.

### When things break

**The peer cannot be reached.** A dial that never connected refunds immediately. Anything after
bytes were written stays open and is retried under the same name, since the seller may hold it.

**No receipt ever arrives.** The call stays parked, retried with backoff, for as long as it takes,
and the process cannot be closed over it. The money stays reserved and visible with its age. There
is no bound to configure, because there is no honest answer to give before the peer's.

**A signed refusal arrives.** The money is released at once. A 402 means the seller declined for
funding — its own or the buyer's credit — and the stand-in survives. Any other signed refusal
deactivates the stand-in, so a withdrawn action leaves the buyer's world after one attempt.

**The terms moved.** The seller answers with a mismatch refusal carrying `refresh_proxy`; the
stand-in is dropped, the next call re-resolves and the buyer re-quotes.

**The seller misbehaves.** There is no arbitration. The buyer keeps a signed receipt, publishes
evidence of what it experienced, can rate the call, and can suspend the peer, which freezes the
account in both directions. Everything else is the market's job, and the market now reads it: that
evidence is on the action the next buyer looks at.

**The reply does not fit the contract.** A signed success whose reply fails the seller's own
published output schema is quarantined rather than paid for: charge 0, and the receipt kept as
evidence of what was sent.

**The buyer never pays.** Bounded by the credit limit of part 8, and the peer can be suspended.

### What is not covered

Key rotation does not exist: a kernel that loses its key loses its identity and its reachability.
Two kernels disagreeing about the same call is detected and counted (S8), but nothing adjudicates
it: the reader sees a contradiction, not a verdict. A kernel's own record of a call it was not
party to does not exist, by design — there is nothing to appeal to beyond the two parties and what
each of them published.
