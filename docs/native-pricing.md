# Pricing the native actions

Status: the schedule in section 8 is the shipped default, stated in requirements.md (D17). This
document is the argument for it, not a second statement of it. Written 2026-11-16 against the code
at v0.20.1. Every figure is marked as measured, quoted, or assumed.

Amounts are in base units. One unit is a millionth of the world's token, so on the USDT0 worlds one
unit is $0.000001.

## 1. Summary

A native's price is the operator's charge for the use of its own machine. It is derived from cost.

- Every call costs the operator the same base amount, whatever the native does. The base charge
  is 50 units.
- A native that uses more than a bare call adds that use at a published rate.
- No native is free. A free native lets one caller take the whole kernel at no cost.
- Four prices hold only with a size limit that does not exist today.

## 2. The problem

Natives are the kernel's system calls (D17). Their owner is `sys`, so the whole price goes to the
operator. They are `local`, so no other kernel sells them.

Before this schedule every native shipped at price 0, except `tinygo/compile` at 5 units. D17 itself notes that they
are "backed by scarce local resources unbounded at price 0".

Three properties make the choice matter.

- **The price is the only limit.** Juice has no quotas. A price bounds what a call can spend, and a
  zero-price call needs no funds (U18).
- **Native prices are the floor of the economy.** A composed action must cover the full price of
  each action it calls (D2). Every composite on a kernel is priced above the natives it uses.
- **Prices must be stable.** The price is part of the quote hash (P2). A program cannot read an
  action's price at run time, so a composer fixes native prices into its own quote. A change
  refuses pinned runs and breaks tightly priced composites.

## 3. Precedents

| Precedent | Rule it gives |
|---|---|
| Time-sharing accounting (Multics, IBM SMF) | A job pays a fixed charge plus processor, input/output and storage at published rates |
| Ethereum gas schedule | Every transaction pays a fixed intrinsic amount whatever it does. Each resource adds a fixed quantity. Quantities are set by measurement on a reference machine |
| Ethereum, 2016 | Operations priced below their cost were used to attack the network, and were repriced (EIP-150) |
| Serverless tariffs | Two parts: a charge per request, and a charge per unit of resource |
| Payment networks | A fixed charge per transaction beside the percentage, because processing cost does not depend on the amount |
| Ethereum `SSTORE`, Arweave | Permanent storage is paid once, at the cost of keeping it for a long time |
| Ethereum precompiles | Built-in functions sit at the same address and the same cost on every node |

Unix system calls are free, and Unix controls abuse with quotas. Juice has no quotas, so Unix is not
the precedent here. Ethereum is.

## 4. The standard

```
price = base charge
      + (quantity of each extra resource × its rate)
      + what the operator pays outside
      + margin, rounded up
```

1. **Base charge.** The cost of processing one call and keeping its records. It is the price of
   `sink`, the native that does nothing.
2. **Quantities.** What a native uses beyond a bare call: stored bytes, processor seconds, GPU
   seconds. They are properties of the code and are measured.
3. **Rates.** The price of one unit of each resource on a reference machine, taken from public
   price lists. They are the only part that differs between operators.
4. **Outside costs.** A model provider's bill, or a chain fee.
5. **Own costs at the average.** An operator serves many calls, so cheap and expensive ones even
   out. This holds only when the input is capped. Without a cap, the callers with the most
   expensive requests gain the most from a flat price.
6. **Sub-calls at full price.** A call an action makes is paid from that one call's budget. It
   cannot be averaged.

The natives fall into two groups. Kernel functions (`sink`, `time`, `random`, `resolve`, `message`,
`lookup`, `transfer`) should cost the same on every kernel, so that one program costs the same
wherever it runs. Resold services (`web`, `tinygo/compile`, `llm/*`) are the operator acting as an
ordinary seller, and each operator sets its own.

## 5. Measurements

All were taken on one machine: an Intel i9-12900K with an RTX 4090.

### One call

| Quantity | Value | How it was obtained |
|---|---|---|
| Records kept per call | 4.7 KB | Measured: 199 calls in a kernel in use |
| Records kept per minimal call | 3.0 KB | Measured: 16,000 calls to `sink` |
| Processor time per call | 7 ms | Measured |
| Time inside the native itself | 5 ms for `resolve` | Measured |
| Calls per second, one kernel | about 150 | Measured, at 1 to 64 clients |
| Processor cores used at full load | 1.15 of 24 | Measured |

The records are the process, the trace, the transaction, the receipt and their indexes. G3 forbids
deleting them, so they are stored for the life of the kernel. Arguments and replies are stored in
the transaction, so every byte of a reply is also stored for good.

The kernel processes one call at a time. More clients wait longer; they do not raise the rate. A
free native runs at the same rate as a priced one.

Throughput first fell as records accumulated, from 160 to about 105 calls per second at 18,000
records. Two settlement queries read every past call. With indexes for them the rate holds at about
150.

### Models

| Quantity | Value | How it was obtained |
|---|---|---|
| `gemma4:26b`, reading input | 8,600 tokens per second | Measured |
| `gemma4:26b`, writing output | 150 tokens per second | Measured |
| GPU power under load | about 220 W | Measured |
| `nomic-embed-text` | 29 calls per second at 400 tokens each | Measured |
| Stored size of one embedding | 10 KB | Measured |

### Compilation

One small program compiled in 4 seconds of processor time and produced a 175 KB module, which is
233 KB once encoded in the reply. This was a single compile with a warm cache.

## 6. Rates

| Resource | Rate | Basis |
|---|---|---|
| Storage | 8 units per KB | Quoted: AWS block storage at $0.08 per GB per month. Assumed: paid for 100 months |
| Processor | 12 units per second | Quoted: an AWS `c7i.large`, two cores, at $0.0892 per hour |
| GPU | 220 units per second | Quoted: an AWS `g6.xlarge`, one GPU, at $0.8048 per hour |

The rates are AWS's on-demand prices in US East, AWS being the reference most operators know.

The storage rate decides the base charge, and the 100 months is a choice. Other disks give other
rates: Hetzner volumes about 5 units per KB, AWS object storage 2.3.

The GPU rate is the rental price. It includes the hardware and it is a public figure. Electricity
alone is about 27 units per second, assuming 320 W for the whole machine at $0.30 per kWh. The GPU
in a `g6.xlarge` is slower than the RTX 4090 the seconds were measured on, and AWS rents no RTX
4090, so the rate is applied to the measured seconds as they are.

## 7. The base charge

| Part | Units |
|---|---|
| Storage of 3.0 to 4.7 KB | 24 to 38 |
| 7 ms of processor | under 0.1 |
| Base charge, with margin, rounded | 50 |

Storage is almost all of the cost. Processor time is negligible.

The base charge also rations the kernel. A kernel has about 13 million calls a day to give, and one
caller in a loop can take all of them. At 50 units a call, doing so costs about $27 an hour. At
zero it costs nothing.

## 8. The schedule

| Native | Units | Dollars | Derivation |
|---|---|---|---|
| `sink`, `time`, `random` | 50 | $0.00005 | Base charge |
| `resolve` | 50 | $0.00005 | Base charge. A round trip to another kernel adds almost nothing |
| `message` | 110 | $0.00011 | Base + the `sink` task it pays for (50) + the stored task |
| `lookup` | 250 | $0.00025 | Base + about 20 KB of stored results + one query embedding |
| `transfer` | 50 | $0.00005 | Base charge. A chain payment to another kernel is the operator's fuel (D23) |
| `web` | 600 | $0.0006 | Base + a stored page of up to 64 KB |
| `tinygo/compile` | 2,500 | $0.0025 | Base + 233 KB stored + 4 s of processor |
| `llm/embed` on `nomic-embed-text` | 150 | $0.00015 | Base + the stored 10 KB vector + GPU time |
| `llm/chat`, `llm/json`, `llm/decide` on `gemma4:26b` | 1,800 | $0.0018 | Base + stored messages + 7 s of GPU |

A per-model native such as `llm/gemma/chat` carries the price of its model.

Two derivations need a note.

- **`message`** creates a `sink` task and pays its price from its own budget when the task is made
  (D6). Its price must therefore cover `sink` in full.
- **`llm/chat` on `gemma4:26b`** assumes a call of 3,000 tokens in and 1,000 out. That takes 7
  seconds of GPU, almost all of it writing the output, which is 1,540 units. The stored messages add
  about 130. Commercial hosts charge about 400 units for the same call on a comparable model,
  since they run many requests on one GPU at once.

### A hosted model

An operator may bind a hosted model instead. For Claude Sonnet 5.5 the provider charges, per token:
2 units for input, 2.5 for input written to its cache, 0.1 for input read from the cache, and 10
for output.

| Call | Tokens in / out | Cost in units |
|---|---|---|
| Small | 500 / 300 | 4,000 |
| Medium | 3,000 / 1,000 | 17,500 |
| Large | 20,000 / 4,096 | 91,000 |
| Largest the kernel accepts | 260,000 / 4,096 | 691,000 |

The kernel caps the reply at 4,096 tokens and does not cap the input. With input capped at 8,000
tokens the worst call costs 61,000 units, and a price of 25,000 to 30,000 would be reasonable. The
same medium call on the shipped local model costs about a tenth as much.

## 9. Prices that depend on a limit

A fixed price needs a bounded cost. Four natives have no bound today.

| Native | Assumed limit | Without it |
|---|---|---|
| `lookup` | Ten results | The number of results has no maximum |
| `web` | A page of 64 KB | A page may be 4 MiB, which costs about 33,000 units to store |
| `llm/*` on `gemma4:26b` | A reply of 1,000 tokens | The shipped entry sets no cap, so one call can hold the GPU indefinitely |
| `tinygo/compile` | A small program | A larger program stores more |

Adding a limit changes the native's contract. That is a separate decision.

## 10. Open questions

1. **The storage rate.** A disk the operator owns costs far less than a rented one. On that basis
   the base charge is 2 to 5 units.
2. **Free actions.** U18 lets a zero-price action run without funds, and such a call still costs
   the operator one base charge. For any action the operator earns only `fee_bps` of the margin,
   so at the shipped 20% an action covers its base charge only with a margin of 250 units or more.
   Pricing the natives does not change this.
3. **One GPU.** A kernel serves about 500 medium chat calls an hour on one GPU. The kernel itself
   handles 150 calls a second. The model is the bottleneck.

## 11. Not verified

- Typical model usage. The kernel records none. The "medium call" is an assumption, and the model
  prices move in proportion to it.
- The size of a lookup result. The 20 KB for ten results is an estimate.
- How much `llm/decide` adds by sending the candidates' schemas.
- The cost of a cold compile, or of a compile with the SDK prelude.
- Calls between kernels and composed calls. Throughput was measured with one user calling one
  native on one machine.
- The disk, processor and GPU prices come from price guides and one vendor page, not from live rate
  cards.
- How long the medium call takes on the GPU of a `g6.xlarge`.

## 12. Sources

- Anthropic pricing: https://platform.claude.com/docs/en/about-claude/pricing
- AWS EBS pricing: https://aws.amazon.com/ebs/pricing
- AWS S3 pricing, 2026: https://upstash.com/blog/aws-s3-pricing-explained-2026
- Hetzner volumes: https://docs.hetzner.com/cloud/volumes/overview
- Gemma 3 27B, hosted: https://openrouter.ai/google/gemma-3-27b-it
- AWS `c7i.large`: https://calculator.holori.com/aws/ec2/c7i.large/us-east-1
- AWS `g6.xlarge`: https://calculator.holori.com/aws/ec2/g6.xlarge
- OpenAI embeddings pricing, 2026:
  https://www.activepieces.com/blog/openai-embeddings-api-pricing-in-2026-dated-and-checked.md
