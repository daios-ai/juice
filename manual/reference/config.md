---
title: Configuration
parent: Reference
nav_order: 4
---

# Configuration

Each kernel reads `config.json` from `$JUICE_HOME/kernels/<world>/`. First boot
creates this file; later starts read it without rewriting it. Apply a
configuration change by editing the file and restarting the kernel.

Known settings take their documented defaults when absent. An unrecognized key
causes startup to fail, helping catch misspellings that would otherwise appear
to configure a value. The file contains the credential-encryption key and is
stored with mode 0600.

Every setting is described in `~/.juice/schemas/config.schema.json`, which the
kernel rewrites each time it starts; world files and language-model endpoint
files have `world.schema.json` and `llm-endpoint.schema.json` beside it. A file
that names its schema in its first line is completed and checked by your editor
as you type:

```json
{
  "$schema": "../../schemas/config.schema.json",
```

Files Juice writes already carry that line. To add it to an older
`config.json`, use the line above; in a world or endpoint file, use
`"../schemas/world.schema.json"` or `"../schemas/llm-endpoint.schema.json"`.
The schema helps you write the file; the kernel still checks it when it
starts.

## Identity and network

| Key | Default | |
|---|---|---|
| `kernel_handle` | none | this kernel's own name: the kernel part of every address on it, `alice@acme`, and the nickname it reports on the network. First boot asks for it, since every kernel on a network shares the world's name |
| `listen_addr` | `:4040` | where this kernel answers clients, as `host:port`. Omit the host to answer on every interface; use port `0` to let the system choose one |
| `fed_listen_addrs` | port 31313 | where this kernel answers peers. Empty binds the standard port on both transports; set it to give this kernel its own addresses |
| `metrics_listen_addr` | empty | where this kernel serves `/metrics` for Prometheus, as `host:port`. Empty serves no metrics. Anyone who can reach the address can read them, so bind it to loopback or a monitoring network |
| `max_inbound_peers` | `64` | how many connections started by other kernels this kernel accepts at once; zero or a negative value uses the default |
| `relay_slots` | `128` | how many kernels behind routers this kernel carries traffic for at once; zero or a negative value uses the default |

The network itself is not configured here. It is the world named on
`juice kernel serve`, whose file in `~/.juice/worlds/` carries the money, the
node a chain world is reached through (`rpc`), and the `seeds` where peers are
met. The `play`, `polygon` and `arbitrum-one` files Juice comes with include
project seeds. For `arbitrum-sepolia`, add the address of a kernel already
serving that world. The network a kernel was created on is recorded in its database, and
a boot offering it another is refused.
See [Configuring worlds](../operating/worlds.html) for the file's contents and
how to configure a network of your own.

With no `fed_listen_addrs`, the kernel binds port 31313, the standard Juice
federation port, over both TCP and QUIC. If another program already holds it,
the kernel refuses to start and names this key, rather than starting on a port
nobody can predict. The check uses an ordinary socket, because libp2p opens its
own with `SO_REUSEPORT`: its bind would succeed against a held port and the two
kernels would share it. To run a second kernel on one machine, give this one
its own addresses, for example `["/ip4/0.0.0.0/tcp/31314", "/ip4/0.0.0.0/udp/31314/quic-v1"]`.

Each kernel configuration key is also an option of `juice kernel serve`, written as the
key with underscores replaced by dashes, and a nested key as a path:
`--listen-addr :4141`, `--fee-bps 500`, `--native.llm.chat ollama/gemma`.
An option given on the command line applies to that run only and is not written
to the file. On a first boot, where there is no file yet, what you pass is
written as the new kernel's configuration. The one key with no option is
`credentials_key`, because anyone with an account on the machine can read
another process's command line.

## Money

The fee rates below use basis points, or hundredths of a percent. The three
monetary settings—`lottery`, `lottery_max`, and `credit_limit`—use integer base
units.

| Key | Default | |
|---|---|---|
| `fee_bps` | `2000` | local execution fee on each provider's margin, in hundredths of a percent: 20% |
| `remote_bps` | `500` | export fee added when serving another kernel: 5% |
| `import_bps` | `500` | fee retained when a local user calls another kernel: 5% |
| `lottery` | `1000000` | the face value this kernel's buyers stake per cross-kernel call. `0` pays every debt exactly |
| `lottery_max` | `5000000` | the largest face value accepted from somebody else's buyer |
| `credit_limit` | `50000000` | the ceiling on work delivered to other kernels and not yet paid for, across all peers together |

See [What an action costs](../money/funds.html#what-an-action-costs) for the
fees and ticket from the buyer's and provider's perspectives. [The network
economy](../operating/network-economy.html) covers the operator's settlement
and funding duties.

## Timing and retention

| Key | Default | |
|---|---|---|
| `remote_retry_interval_seconds` | `60` | how often parked cross-kernel calls are retried and sellers are told their draw outcomes |
| `discovery_interval_seconds` | `300` | how often peers are enumerated and catalogues exchanged |
| `peer_retention_days` | `90` | how long an idle peer's cached data is kept. Non-positive disables purging |

## Execution

| Key | Default | |
|---|---|---|
| `allow_local_sources` | `false` | permit private-network URLs as action sources. Loopback is always permitted |
| `http_callback_url` | derived | the address dispatched endpoints call back on. Loopback may be plain HTTP; a public address requires TLS |
| `credentials_key` | generated at first boot | the key sealing upstream credentials. A value that is not a 32-byte key refuses the boot |
| `native.<name>` | see below | per-action price and settings for the standard library |

The language models a kernel can use are described by files in `~/.juice/llm/`,
one per provider, which Juice writes on first use and never overwrites. Each
names how the provider is spoken to (`openai` or `anthropic`), its address,
whether it needs a key, and the models you use there under short names of
your own:

```json
{
  "protocol": "anthropic",
  "url": "https://api.anthropic.com/v1",
  "key_required": true,
  "models": {
    "opus": {"id": "claude-opus-5-5", "kind": "chat", "max_tokens": 4096}
  }
}
```

`native.llm` says which model `sys/llm/chat`, `sys/llm/json`, `sys/llm/decide`
and `sys/llm/embed` use, as `<file>/<model>` — by default `ollama/gemma` for the
first three and `ollama/nomic` for embeddings, a local Ollama — and holds each
provider's key and each model's price:

```json
"llm": {
  "chat": "anthropic/opus",
  "json": "anthropic/opus",
  "decide": "anthropic/opus",
  "embed": "ollama/nomic",
  "endpoints": {
    "anthropic": {"key": "sk-ant-…", "prices": {"opus": 20000}}
  }
}
```

A provider that needs a key is used only once its key is here, and then every
one of its models must have a price, 0 included, since each call costs you
money. The keys have no command-line flag. A provider that is down when the
kernel starts is noted in the log, and calls to it fail until it is back.
`native.lookup.default_limit` defaults to `10`. The compilation action defaults
to a price of `5` base units through `native.tinygo.price`; other built-in
actions default to zero.

### Fuel, on a chain network

The world file supplies the rail's fuel policy. These settings determine when
it buys fuel, how much it buys, and the limits on the purchase. Fuel is POL on
Polygon and ETH on Arbitrum. These are world settings, stored outside the
kernel's `config.json`.

| Key | `polygon` | `arbitrum-one` | `arbitrum-sepolia` | |
|---|---|---|---|---|
| `gas.min` | 5 POL | 0.001 ETH | 0.0002 ETH | buy more below this |
| `gas.max` | 15 POL | 0.003 ETH | 0.0004 ETH | buy up to this |
| `gas.feeBound` | 4 POL | 0.0003 ETH | 0.0001 ETH | most it will pay for one purchase |
| `gas.slippageBps` | 100 | 100 | 500 | tolerance above the quoted price |
| `venue` | Uniswap V3 | Uniswap V3 | Uniswap V3 | the exchange used to buy fuel; its contract addresses and fee tier are set in the world file |

See
[How the kernel keeps itself in fuel](../operating/duties.html#how-the-kernel-keeps-itself-in-fuel).

## Logging, sessions, scripts

| Key | |
|---|---|
| `log_level`, `log_file`, `log_format` | structured logging. A `log_file` that cannot be opened refuses the boot |
| `auth_issuer`, `auth_audience`, `token_ttl` | the issuer and audience written into session tokens, and how long an access token lasts |
| `script_timeout_ms`, `script_memory_bytes` | the time and memory one WebAssembly execution may use |

## Environment variables

The following environment variables provide bootstrap values, runtime
overrides, and client login selection. Other environment variables do not
configure Juice.

| Variable | |
|---|---|
| `JUICE_HOME` | the installation root. Default `~/.juice`, absolute |
| `JUICE_BOOTSTRAP_PASSWORD` | the `sys` password at first boot, for a machine with no terminal |
| `JUICE_SECRET_KEY` | the session-signing secret, runtime only; never written to disk |
| `JUICE_CREDENTIALS_KEY` | the credential-sealing key, runtime only |
| `JUICE_LOG_LEVEL` | log level |
| `JUICE_ALLOW_LOCAL_SOURCES` | as `allow_local_sources` |
| `JUICE_AS` | the login the command line acts as |

## Client files

The command-line client keeps kernel registrations and saved sessions under
`$JUICE_HOME/client/`:

```
client/config.json        the kernels known, and which login is selected
client/credentials/       one file per login, named handle@kernel, mode 0600
```

Each saved login has its own token file, with access serialized by a session
lock during refresh. Programs using the same saved login share that session;
separate logins allow an agent and a person to authenticate independently.
Credentials are sent only to the recorded address for their kernel.

The rest of the installation shares the same root. `bin/` holds the executables,
`worlds/` the network files, and `kernels/<world>/` each kernel. Agents, services
and the interface keep their own state under `agents/<name>/`, `services/<name>/`
and `ui/`, so removing one of them never touches a kernel or another program's
records.
