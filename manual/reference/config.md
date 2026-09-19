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

## Identity and network

| Key | Default | |
|---|---|---|
| `kernel_handle` | none | the nickname this kernel reports on the network. First boot asks for it, since every kernel on a network shares the world's name |
| `listen_addr` | `:4040` | where this kernel answers clients, as `host:port`. Omit the host to answer on every interface; use port `0` to let the system choose one |
| `fed_listen_addrs` | port 31313 | where this kernel answers peers. Empty binds the standard port on both transports; set it to give this kernel its own addresses |

The network itself is not configured here. It is the world named on
`juice kernel serve`, whose file in `~/.juice/worlds/` carries the money, the
node a chain world is reached through (`rpc`), and the `seeds` where peers are
met — the only place a meeting point is named. `arbitrum-one` names this
project's seed; `play` and `arbitrum-sepolia` name none, so write one in to
connect kernels on them. The network a kernel was created on is
recorded in its database, and a boot offering it another is refused.
See [Configuring worlds](../operating/worlds.html) for the file's contents and
how to configure a network of your own.

With no `fed_listen_addrs`, the kernel binds port 31313, the standard Juice
federation port, over both TCP and QUIC. If another program already holds it,
the kernel refuses to start and names this key, rather than starting on a port
nobody can predict. The check uses an ordinary socket, because libp2p opens its
own with `SO_REUSEPORT`: its bind would succeed against a held port and the two
kernels would share it. To run a second kernel on one machine, give this one
its own addresses, for example `["/ip4/0.0.0.0/tcp/31314", "/ip4/0.0.0.0/udp/31314/quic-v1"]`.

Every key on this page is also an option of `juice kernel serve`, written as the
key with underscores replaced by dashes, and a nested key as a path:
`--listen-addr :4141`, `--fee-bps 500`, `--native.llm.url http://localhost:11434`.
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
| `fee_bps` | `2000` | the kernel's fee on each provider's margin, in hundredths of a percent: 20% |
| `remote_bps` | `500` | markup added when serving another kernel: 5% |
| `import_bps` | `500` | fee retained when a local user calls another kernel: 5% |
| `lottery` | `1000000` | the face value this kernel's buyers stake per cross-kernel call. `0` pays every debt exactly |
| `lottery_max` | `5000000` | the largest face value accepted from somebody else's buyer |
| `credit_limit` | `50000000` | the ceiling on work delivered to other kernels and not yet paid for, across all peers together |

See [The network economy](../operating/network-economy.html).

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

The `native.llm` settings select the language-model endpoint and model names;
the default endpoint is Ollama at `http://localhost:11434`.
`native.lookup.default_limit` defaults to `10`. The compilation action defaults
to a price of `5` base units through `native.tinygo.price`; other built-in
actions default to zero.

### Fuel, on a chain network

The world file supplies the rail's fuel policy. These settings determine when
the rail buys ETH, the balance it targets, and the limits applied to that
purchase. They are not keys in the kernel's `config.json`.

| Key | Arbitrum One | Arbitrum Sepolia | |
|---|---|---|---|
| `gas.min` | 0.001 ETH | 0.0002 ETH | buy more below this |
| `gas.max` | 0.003 ETH | 0.0004 ETH | buy up to this |
| `gas.feeBound` | 0.0003 ETH | 0.0001 ETH | most it will pay for one purchase |
| `gas.slippageBps` | 100 | 500 | tolerance above the quoted price |
| `venue` | — | — | the exchange it buys at: a Uniswap V3 router, quoter, wrapped-ETH address and fee tier |

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

The rest of the installation's layout — where agents, services and interfaces keep
their state — is described in
[`ecosystem-standard.md`](https://github.com/daios-ai/juice/blob/master/ecosystem-standard.md).
