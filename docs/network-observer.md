# The network observer

A plan for building Juice's network crawler, in its own repository. The crawler is the second half
of rollout monitoring: each kernel an operator runs exports its own `/metrics` (Geth's pattern); one
observer, run by whoever wants the network view, crawls the network and exports what it saw
(Bitnodes, ethernodes, Nebula). Prometheus stores the history for both.

## What it measures

The five things every network crawler measures, and nothing else:

| Metric | Meaning |
|---|---|
| `juice_observer_peer_reachable{kernel}` | 1 if the observer reached that kernel on its last complete scan, else 0 |
| `juice_observer_peer_info{kernel,version,path}` | always 1; `version` is the build the kernel runs, `path` is `direct`, `relayed` or `unreachable` |
| `juice_observer_peer_rtt_seconds{kernel}` | one libp2p ping round trip; absent when the ping failed |
| `juice_observer_last_complete_scan_timestamp_seconds` | when the whole roster was last scanned successfully |

`kernel` is the kernel's public key — its public identity. Population, reachable share, version
distribution, relay dependence and churn are PromQL over these series; the exporter computes none of
them. It publishes no users, actions, transactions, amounts or counterparties, and it does not claim
to see traffic between other kernels: it cannot.

## How it works

The observer is a client of an ordinary Juice kernel the operator runs — the observer kernel — and
of nothing else. It drives the `juice` command line, which handles login, token rotation and key
pinning, exactly as `netsim/` does. It never links the kernel.

One pass, every `--interval` (default 5 minutes):

1. Read the whole roster, one page at a time, until a page comes back short:
   `juice --as sys@<kernel> admin peer list --all --json --limit 200 --offset N`.
   Each row carries `public_key`, `petname`, `nickname`, `has_account`, `actions`, `suspended_at`,
   `last_seen`, `last_contact_failed_at`. Only `public_key` is used.
2. For every key: `juice --as sys@<kernel> admin peer inspect <key> --json`. The reply's
   `reachability` object carries `path` (`direct` | `relayed` | `unreachable`), `rtt_millis` (a
   libp2p ping round trip; a peer that did not answer it is `unreachable`), `version` (`juice-kernel/<version>`, empty when
   the peer says none) and `error`. The kernel probes the peer when asked and writes nothing.
3. Build the new set of per-kernel series from those replies. If every command succeeded, replace
   the published set with it and set the scan timestamp to now. If any failed, publish nothing new:
   the old set and the old timestamp stay, so staleness is `time() − timestamp` and a dashboard can
   never show a dead exporter's last values as current.

Serve the set on `--listen` (default `127.0.0.1:9101`) at `/metrics` in Prometheus text format,
through the standard `prometheus/client_golang` registry. A kernel absent from the roster on a
complete pass has no series; Prometheus marks it stale by itself.

## Flags and environment

`--login sys@<kernel>` (passed to `juice` as `--as`), `--interval 5m`, `--listen 127.0.0.1:9101`,
`--juice <path>` (default `juice` on `PATH`; honour `JUICE` as the flow suite does). The observer
kernel's `sys` login is made once with `juice auth login sys@<kernel>`; the observer reads no
credential file and holds no token itself.

## What the kernel provides

Both hooks exist on every kernel; the observer adds nothing to it.

- `GET /v1/admin/peers?all=1&limit=&offset=` — `juice admin peer list`. Default page 50, ceiling 200.
- `GET /v1/admin/peers/{key}/inspect` — `juice admin peer inspect`. Bounded to eight seconds per
  peer, so an offline peer costs eight seconds, not the client timeout.

Both are superuser routes on the kernel's client API; see `API.md`.

## Acceptance

- Two loopback kernels, observer pointed at one: one `juice_observer_peer_reachable{kernel=…}` per
  peer, each with `rtt_seconds > 0` and `info{version=…,path="direct"}`.
- Stop a peer: its `reachable` goes to 0 and `path` to `unreachable` on the next pass; the scan
  timestamp advances.
- Stop the observer kernel: the published set is unchanged and the timestamp stops advancing.
- Roster of more than 200 kernels (fixture): every kernel appears; a pass whose second page fails
  publishes nothing.
- No series carries a label other than `kernel`, `version`, `path`.
