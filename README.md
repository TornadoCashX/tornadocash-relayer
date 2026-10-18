# Go withdraw relayer (OpenZeppelin Relayer backend)

A Tornado-compatible regular withdraw API. This service validates and preflights
withdrawals; **OpenZeppelin Relayer (OZ) holds the signing key and does all
transaction sending**: sender address, nonce, signing, broadcast, fee bumps,
RPC failover and confirmation tracking. No private key is configured here.

Public API:

- `POST /relay`, `POST /v1/tornadoWithdraw`
- `GET /v1/jobs/:id`
- `GET /status`, `GET /v1/status`

Operational endpoints (require `Authorization: Bearer <HEALTH_TOKEN>`):
`GET /health`, `GET /ready`, `GET /health/oz` (OZ relayer address, balance,
pending count, nonce, paused/disabled, whether new withdrawals are admitted),
`GET /health/jobs`, `GET /metrics`.

## Responsibilities

| Go service | OpenZeppelin Relayer |
|---|---|
| request parsing (Pool / Proxy `methodId`), `_relayer == REWARD_ACCOUNT`, `_refund == 0` | signing key / signer, sender address |
| idempotency (request hash) and pending-nullifier lock in SQLite | nonce management |
| preflight on one `HTTP_RPC_URL` endpoint: pool identity, `isKnownRoot`, `isSpent`, recipient EOA, `estimateGas`, final `eth_call` from the OZ address | broadcast, fee bump / resubmit, NOOP on expiry |
| service fee + gas fee check (Oracle prices for ERC20) | RPC failover (`custom_rpc_urls`) |
| polling OZ, storing the OZ tx id and every hash OZ reports | confirmations (network config) |
| final verification on chain: receipt + calldata + `isSpent` | `gas_price_cap`, `whitelist_receivers`, `min_balance` |

## Flow

1. `POST` validates the request, then atomically stores the job, its request
   hash and the nullifier lock (`QUEUED`). Duplicates return the existing job id.
2. A worker runs the preflight (`PREPARING`). Failures are classified retryable
   or permanent and release the nullifier. Market conditions (gas price above
   `maxGasPriceGwei`, proof fee below the current gas cost, oracle price
   missing) are first retried: the job returns to `QUEUED` and is preflighted
   again every 30 s until 3 min after admission, keeping the nullifier locked;
   only then it fails as retryable. Each retry re-checks root and `isSpent`. A job left in `QUEUED` or
   `PREPARING` by a shutdown or crash is preflighted again after restart
   (nothing has been sent to OZ before `SUBMITTING`).
3. The job is persisted as `SUBMITTING`, then `POST /api/v1/relayers/{id}/transactions`
   is sent with `to`, `data`, `value=0`, `gas_limit`, `max_fee_per_gas`,
   `max_priority_fee_per_gas` and `valid_until`. The OZ transaction id is stored
   immediately (`SUBMITTED`).
   - 4xx (except 408/429), including 404 for an unknown relayer id: OZ created
     nothing; the job fails as retryable.
   - timeout / 5xx: outcome unknown. The reconciler lists OZ transactions and
     matches `to + data` created after submission (calldata contains the unique
     nullifier). A negative list result is not proof that nothing was created,
     so the job and nullifier remain quarantined for manual review (listed in
     `/health/jobs` `unknownSubmits` and `relayer_jobs_unknown_submit`; see
     "Unknown submit outcome" below). `to + data`
     are rebuilt from the request stored at admission, not re-checked against
     the current `REWARD_ACCOUNT` or pool list, so a configuration change
     cannot release a transaction OZ may still hold.
4. Every `POLL_SECONDS` the reconciler reads `GET .../transactions/{tx_id}` for
   every active job and stores the current `hash`. OZ replaces the hash on every
   fee bump (no webhook is sent for that), so all hashes are kept in `TxHashes`;
   `/v1/jobs/:id` shows the latest.
5. Final state:
   - OZ `confirmed`, `to`/`data` equal the withdraw, receipt status 1 on
     `HTTP_RPC_URL` and `isSpent == true` -> `CONFIRMED`. Until the local RPC
     sees it, the job stays `MINED`.
   - OZ `failed` / `canceled` / `expired`, a `confirmed` NOOP (OZ replaced an
     expired transaction with a self-transfer), or the OZ record was pruned
     (OZ deletes terminal transactions after a few hours): every stored hash is
     checked on chain; a finalized successful withdrawal wins even when an
     older hash is still reported pending. Because OZ no longer vouches for
     the transaction here, a receipt only counts once its block is at or below
     the `finalized` block and its block hash matches the canonical header at
     that height. All recorded hashes are checked for such a success; absent
     one, any pending attempt, unverified receipt or lookup error keeps the job
     and nullifier waiting (it cannot fall through to a failure/release).
     Otherwise:
     - `isSpent == true` at the `finalized` block -> permanent failure,
       nullifier released (a spend seen only at `latest` keeps the job waiting).
       Recorded hashes are checked first, so a recorded hash that succeeded is
       `CONFIRMED`. Known limitation: if the spend came from an OZ replacement
       hash Go never recorded, the job shows "already spent" although this
       withdrawal succeeded (status only; funds are not affected);
     - a finalized revert of this withdrawal -> retryable failure, released;
     - the final OZ record has no hash and Go never saw one (OZ refused before
       signing, e.g. insufficient balance) -> retryable failure, released;
     - the final OZ record's `from` has a nonce at the `finalized` block past
       the record's `nonce` -> retryable failure, released. The original, every
       fee bump and any NOOP share that nonce, so a consumed nonce with an
       unspent nullifier proves the withdrawal can never execute;
     - OZ pruned the record (404), no recorded hash is mined or pending, the
       nullifier is unspent, and more than `OZ_TX_VALID_MINUTES` + 15 min have
       passed since submission -> retryable failure "abandoned", released.
       Nothing is resubmitted automatically. Abandoning cannot recall a
       transaction OZ already signed and broadcast: `valid_until` only stops OZ
       from sending it again, the chain itself has no expiry, so a copy still
       held by some node could in theory mine later (rare: OZ normally consumes
       the nonce with a NOOP). Then the withdrawal has in fact succeeded and only
       the local status says failed. If the user resubmits, preflight sees
       `isSpent` and refuses without sending; only if the old copy mines between
       that preflight and the new transaction does the new one revert, costing
       the relayer that gas. Funds are never at risk. A hash still pending on
       chain is never abandoned;
     - anything else (nonce not yet finalized, or a pruned record still inside
       that window) keeps the nullifier and is retried next round.
   - OZ unreachable: OZ is the source of truth, so the job keeps waiting and
     the nullifier stays locked; polling resumes by OZ transaction id (or the
     `to + data` list match for an unknown submit) once OZ answers again. When
     OZ's status endpoint fails, or 3 transaction polls in a row fail, the rest
     of that reconcile round skips OZ instead of waiting out one timeout per
     job. A single job whose OZ record keeps failing only affects itself, and
     each round starts at a rotating position so no job is always first.

The OZ transaction id is the anchor of every job: it never changes while OZ
bumps fees and replaces hashes. Set `TRANSACTION_EXPIRATION_HOURS=24` in the
OZ environment (default 4): long enough to outlive `OZ_TX_VALID_MINUTES` and
normal Go restarts, short because OZ records contain the withdraw calldata
(recipient). A record deleted during a longer Go outage falls back to stored
hashes + `isSpent` (and then cannot use the nonce proof above).

Public status mapping: `SUBMITTING`/`SUBMITTED` are shown as `SENT` with
`internalStatus`; `QUEUED`, `PREPARING`, `MINED`, `CONFIRMED`, `FAILED` unchanged.

## Run

```bash
cp .env.example .env && chmod 600 .env
cp relayer.config.example.json relayer.config.json
go run . --config ./relayer.config.json --env ./.env
# Built binary:
# ./go-withdraw-relayer --config /absolute/path/relayer.config.json --env /absolute/path/.env
```

Both flags are required. The service does not auto-discover `.env`, read
`RELAYER_CONFIG`, or fall back to a config in the working directory. Existing
process environment values take precedence over values in the selected env file.
On POSIX systems the env file must be a regular owner-only file (`chmod 600`),
and the config must be regular and not writable by group or others (`chmod 644`
or stricter). For production under systemd see `deploy/go-relayer/` (one
unit instance per chain, absolute `SQLITE_PATH`).

OpenZeppelin Relayer must be running with Redis; `deploy/oz/` contains a ready
Docker Compose setup (see `deploy/oz/README.md`). Startup (and `/ready`) refuses withdrawals until
`GET /api/v1/relayers/{OZ_RELAYER_ID}` shows an EVM relayer that is not paused
or disabled, whose `eth_chainId` (through `/rpc`) equals `NET_ID`, whose
`whitelist_receivers` exactly matches the configured pools, whose
`eip1559_pricing` is true and `gas_limit_estimation` is false, and whose
`gas_price_cap` is set and not above `maxGasPriceGwei`. The same checks run
again every 5 minutes (OZ relayers can be changed at runtime); while they fail,
or while OZ reports the relayer paused / `system_disabled`, new withdrawals get
HTTP 503 and `/ready` fails. Jobs already submitted keep being reconciled.

Required OZ relayer policy:

- `gas_price_cap` must be positive and <= `maxGasPriceGwei` in wei (e.g. `1000` gwei -> `1000000000000`).
- `eip1559_pricing: true`, `gas_limit_estimation: false` (the gas limit comes
  from this service).
- `whitelist_receivers`: exactly the pool addresses of the chain.
- `min_balance`: OZ marks the relayer `system_disabled` below it; `/ready` then fails.
- Confirmation depth is the OZ network `required_confirmations` (shipped:
  mainnet 3, BSC 1). Releasing a nullifier never relies on it: the nonce proof,
  revert and `isSpent` checks read the `finalized` block. Every `HTTP_RPC_URL`
  endpoint must support the `finalized` block tag; without it such jobs keep
  waiting.

## Settings

- `NET_ID`, `HTTP_RPC_URL`, `REWARD_ACCOUNT`, `OZ_RELAYER_URL`, `OZ_RELAYER_ID`,
  `OZ_API_KEY` (>= 32 chars) and `HEALTH_TOKEN` (>= 32 chars) are required.
  `PRIVATE_KEY`, `HTTP_RPC_URLS` and `HTTP_RPC_URL_2` are rejected.
- `HTTP_RPC_URL`: read-only http(s) RPC (10 s request timeout), checked for
  chain id and EIP-1559 at startup. It never sends transactions. Either one URL
  or a JSON array of two distinct URLs, ideally different providers:
  `HTTP_RPC_URL=["https://rpc-a.example","https://rpc-b.example"]`. Set it in
  the `--env` file (read by this service itself; no shell quoting needed).
  Logs and `/metrics` name the endpoints `HTTP_RPC_URL[1]` / `[2]` (or
  `HTTP_RPC_URL` with one URL), never the URL. With two URLs both must serve
  `NET_ID` and support the `finalized` tag:
  - Round robin per operation: each preflight and each job reconciliation
    picks the next endpoint, and *all* reads of that operation use it. Safety
    decisions (receipt vs canonical header, pending vs finalized) never mix
    two nodes' views.
  - Failover: an endpoint is skipped for 30 s after a transport error, HTTP
    429/5xx, a request with no answer for 3 s or more (a hung node), a failed
    health probe (every `POLL_SECONDS`, 3 s per endpoint) or a head more than
    10 blocks behind the other endpoint; a successful probe restores it. With
    both down both are still tried. A preflight that failed for an RPC reason,
    or because the node did not know the withdraw root yet (it may lag), is
    rerun from scratch on the other endpoint; business outcomes (revert, spent
    nullifier, fee too low) are not retried elsewhere.
  - Startup needs one reachable endpoint; an endpoint on the wrong chain or
    without EIP-1559 stops startup. `/ready` fails only when no endpoint is
    healthy, or when any endpoint reports a wrong chain id.
- `ORACLE_RPC_URL`: Ethereum mainnet, needed for ERC20 pools / oracle tokens;
  on `NET_ID=1` it may be empty and `HTTP_RPC_URL` is reused.
- `SQLITE_PATH` (default `relayer-oz.db`): use a new file. Databases of the
  previous self-signing relayer (schema v1/v2) are refused; drain them with the
  old binary first.
- `OZ_TX_VALID_MINUTES` (default 10, 10-480; the range is this service's, OZ
  itself only requires a future time): OZ `valid_until`, set in `.env`
  of this service (restart to apply; `valid_until` is set on new submissions). Lower
  values release a stuck nullifier sooner (pruned records are abandoned after
  this + 15 min) but give OZ fewer fee bumps before the NOOP. The abandon
  window uses the current value for every job, so do not lower it while jobs
  are in flight (`/health/jobs` `currentQueue` 0 first): a job submitted with a
  longer validity could otherwise be abandoned while OZ may still send it.
- Fee bumps are done by OZ, not by this service (OZ v1.8.0
  `should_resubmit`): a transaction is resent with higher fees when it has gone
  unmined for 3 min since its last send (default speed `fast`; the Go service
  sends explicit EIP-1559 fees, so no speed is set), doubling each attempt
  (3, 6, 12, 24 ... min). It is time based, not a block count (3 min is about
  15 mainnet blocks / 60 BSC blocks). Each bump raises both fees by at least
  10% or to the current market, capped by the relayer policy `gas_price_cap`.
  If the cap prevents a 10% bump, OZ skips that resubmission and keeps waiting
  (the transaction stays pending at its last price). After `valid_until` OZ
  stops bumping and replaces the nonce with a NOOP (the NOOP ignores the cap).
- The initial fees are computed by this service, not by OZ: tip = min(node
  `eth_maxPriorityFeePerGas`, median of the last 20 blocks' median tips),
  `maxFeePerGas` = min(2 x base fee + tip, min(`maxGasPriceGwei`, OZ
  `gas_price_cap`)). The same values are used for the fee-budget check and the
  final `eth_call`, so what is checked is what OZ sends.
- `NET_ID`: only Ethereum mainnet (1) and BSC (56) are deployed and tested.
  `NET_ID=10` (Optimism) is refused. Other OP-stack L2s (Base, etc.) are not
  refused, but the fee check does not include their L1 data fee, so the relayer
  may lose gas there; do not run on them without adding that fee.
- `POLL_SECONDS` (default 5), `JOB_TIMEOUT_SECONDS` (default 300, preflight +
  submit only).
- `MAX_ACTIVE_JOBS` (default 100), `MAX_PROTECTED_JOBS` (default 1000, held
  nullifiers), `MAX_PREFLIGHT_CONCURRENCY` (default 8), `IDEMPOTENCY_TTL_SECONDS`.
- The file passed with `--config` is required. Its `networks.netIdX` contains
  `version`, `serviceFeeBps`, `maxGasLimit`, `maxGasPriceGwei` and optional
  `oraclePriceTokens`; `instances.netIdX` is the sole pool allowlist and metadata
  source for that process.
- `APP_HOST` must be loopback; CORS, `CLIENT_IP_MODE`, `TRUSTED_PROXY_CIDRS`,
  `CLIENT_IP_HEADER`, `RELAY_RATE_*` are unchanged.

### Fees

- `fee > service fee` is required (otherwise permanent failure).
- `fee - service fee` must cover the expected cost
  `estimateGas x (base fee + tip) x (1 - GAS_FEE_TOLERANCE_PERCENT)` at the
  current block (tolerance 0-20%, default 5%, paid by the operator); otherwise
  the job fails as retryable.
- `gasLimit = estimateGas x (1 + GAS_LIMIT_BUFFER_PERCENT) <= maxGasLimit` is
  execution headroom only; unused gas is not paid, so it is not charged to the user.
- tip = min(`eth_maxPriorityFeePerGas`, median of the per-block median tips of the
  last 20 blocks from `eth_feeHistory`). This corrects nodes that keep suggesting
  an outdated floor (e.g. 1 gwei on BSC); without fee history the node value is used.
- The transaction is priced from the market, never lowered by the user budget:
  `max_fee_per_gas = min(2 x base fee + tip, maxGasPriceGwei, OZ gas_price_cap)`.
  A market price above that limit is refused (retryable).
- Price rises after submission, gas used above the estimate and OZ fee bumps are
  paid by the operator, never more than `gasLimit x OZ gas_price_cap` per transaction.

## Failure classes

`failedReason` ends with `retryable: the same request can be submitted again` or
`not retryable`. Retryable: RPC errors, fee too low for the current gas price,
gas price above `maxGasPriceGwei`, missing oracle price, OZ rejected or did not
complete the submit, transaction failed/expired without executing while the
nullifier is unspent, transaction reverted because its merkle root left the
root history. Permanent: nullifier already spent, contract recipient, fee below
service fee, gas limit above `maxGasLimit`, simulation revert, any other
on-chain revert. Reasons never contain raw RPC
errors or URLs.

A withdrawal that passed simulation but reverted on chain (e.g. the recipient
added an EIP-7702 delegation after preflight) fails permanently, and every new
request for that nullifier (any proof or recipient) is refused with HTTP 400,
with no expiry, so one note cannot make the relayer pay for reverted
transactions repeatedly. The note can still be withdrawn through another
relayer or directly. The record is kept in the SQLite table
`reverted_nullifiers` and is not removed by the idempotency cleanup.
Exception: if the proof's `_root` is no longer `isKnownRoot` when the revert is
resolved (it left the pool's root history while OZ queued or re-priced the
transaction), the job fails as retryable and the note is not refused; the user
needs a new proof with a current root. A root still known then was also known
when the transaction reverted, so it cannot have been the cause.

While the last health probe reached no `HTTP_RPC_URL` endpoint, withdraw
requests get HTTP 503 `rpc unavailable` (`Retry-After: 30`) instead of a job.

## Unknown submit outcome

A job whose OZ submit timed out or got 5xx, and whose transaction the
reconciler cannot find in OZ's list, stays `SUBMITTING` (public `SENT`) and
keeps its nullifier. After 2 min it is listed in `GET /health/jobs`
(`unknownSubmits`: id, submittedAt, oldest first, at most 100) and counted by
`relayer_jobs_unknown_submit` / `relayer_jobs_unknown_submit_oldest_age_seconds`.
To resolve one (`<pool>` and the calldata come from the job's stored request):

1. Search OZ (`GET /api/v1/relayers/{id}/transactions`) for a transaction with
   `to = <pool>` and that calldata. If found, stop the service, back up the
   database, and attach it; the reconciler then follows it normally:
   ```sql
   UPDATE jobs SET oz_tx_id='<oz tx id>'
   WHERE id='<job id>' AND status='SUBMITTING' AND oz_tx_id IS NULL;
   ```
2. Release only if OZ has no such transaction, `isSpent(nullifier)` is false,
   OZ `pending_transactions_count` is 0 and more than `OZ_TX_VALID_MINUTES`
   have passed since `submittedAt`. Stop the service, back up, then:
   ```sql
   BEGIN;
   UPDATE jobs SET status='FAILED', retryable=1,
     failed_reason='submission outcome unknown; released by operator; retryable: the same request can be submitted again',
     updated_at=CAST(strftime('%s','now') AS INTEGER)*1000
   WHERE id='<job id>' AND status='SUBMITTING' AND oz_tx_id IS NULL;
   DELETE FROM pending_nullifiers WHERE job_id='<job id>'
     AND (SELECT status FROM jobs WHERE id='<job id>')='FAILED';
   COMMIT;
   ```
   If a copy of the transaction still mines later, the withdrawal has
   succeeded; a resubmission is then refused by preflight (`isSpent`).

## Withdraw request

The request body is compatible with the official relayer:

```json
{
  "methodId": "0xb438689f",
  "proof": "0x...",
  "contract": "0xTornadoProxy",
  "args": [
    "0xInstanceAddress",
    "0xroot",
    "0xnullifierHash",
    "0xrecipient",
    "0xrelayer",
    "0xfee",
    "0xrefund"
  ]
}
```


For this seven-item Proxy-compatible shape, `args[0]` is `_tornado` and proof stays
in the top-level `proof` field. The API also accepts the complete eight ABI arguments
`[instance, proof, root, nullifier, recipient, relayer, fee, refund]`; if both proof
locations are supplied they must match. The server normalizes either representation to
the internal direct-pool form before validation, idempotency, nullifier locking or
persistence. `contract` is only the discarded outer Proxy target;
it is never submitted as `tx.to`. Proxy-compatible requests must set `methodId` to
`0xb438689f`. The existing six-item direct-pool
shape remains accepted and, when it supplies `methodId`, must use `0x21a0adb6`.

For complete ABI arrays, the Pool form reads `_relayer`, `_fee`, and `_refund`
from `args[4]`, `args[5]`, and `args[6]`; the Proxy form reads them from
`args[5]`, `args[6]`, and `args[7]`. For the split-proof compatibility forms,
the corresponding positions are Pool `args[3..5]` and Proxy `args[4..6]`.
After method-specific parsing, the relayer normalizes both forms to
`[root, nullifier, recipient, relayer, fee, refund]` and requires `_refund == 0`.
A non-zero refund is rejected with HTTP 400 before a job is created. ERC20
requests still validate the service fee, fresh Oracle price, native gas budget
and gas price cap.

The method-specific `_relayer` value must match `rewardAccount`. It does not have
to match the OpenZeppelin Relayer sender address. No recipient/sender
relationship rule is enforced.
The recipient must only be a non-zero EOA under the existing code check.

The effective instance address—`args[0]` for the Proxy-compatible shape or
`contract` for the direct-pool shape—must exist in `instances.netIdX` of the
explicitly selected relayer config. That file is the only pool trust source and
may be updated without rebuilding the binary. Treat it as security-sensitive:
restrict write access, review changes, and keep the matching OZ
`whitelist_receivers` synchronized.

## Relayer config

```json
{
  "networks": {
    "netId1": {
      "version": "4.1.6",
      "serviceFeeBps": 36,
      "maxGasLimit": 420000,
      "maxGasPriceGwei": "1000",
      "oraclePriceTokens": {
        "torn": {
          "tokenAddress": "0x77777FeDdddFfC19Ff86DB637967013e6C6A116C",
          "decimals": 18
        }
      }
    }
  },
  "instances": {
    "netId1": {
      "eth": {
        "instanceAddress": {
          "0.1": "0x...",
          "1": "0x..."
        },
        "symbol": "ETH",
        "decimals": 18
      },
      "dai": {
        "instanceAddress": {
          "100": "0x..."
        },
        "tokenAddress": "0x...",
        "oracleTokenAddress": "0x...",
        "symbol": "DAI",
        "decimals": 18
      }
    }
  }
}
```

The withdraw request sends the selected pool in `contract`. A transaction is
only handed to OpenZeppelin Relayer after all of these pass:

1. **Config allowlist (per chain, in memory).** `contract` must be one of the
   pools under `instances.netIdX` for the active `NET_ID` in the file selected
   by `--config`. Otherwise HTTP 400.
2. **Batched preflight on one `HTTP_RPC_URL` endpoint.** One JSON-RPC batch: `denomination()`
   (and `token()` for ERC20 pools) must match the config, `isKnownRoot(root)`,
   `isSpent(nullifierHash)` and recipient `eth_getCode` (recipient must be an
   EOA). Providers without batch support fall back to sequential calls.
3. **Fee budget, estimate and simulation.** `estimateGas` (with an `eth_call`
   revert diagnosis) and a final `eth_call` with the exact submitted gas/fee fields run with
   `from = OZ relayer address`, the configured pool as `to`, zero value and the
   server-built calldata. Any revert fails the job permanently before OZ sees it.

For every ERC20 in the active `instances.netIdX` and every explicit
`networks.netIdX.oraclePriceTokens` entry, the relayer periodically updates
`/status.ethPrices` through the configured OffchainOracle. A missing or stale
price only affects ERC20 pools of that currency (their withdrawals are retried,
then refused as retryable): it is dropped from `ethPrices` and listed in
`health.errorsLog` (`price oracle unavailable: <currency>`), but
`health.status` stays `"true"` so ETH pools keep receiving requests. `/ready`
still fails on it, for alerting. Chains without either type of price
token do not open the oracle RPC client or expose `ethPrices`.

## Notes

- SQLite (`WAL`, `synchronous=FULL`) is chain-bound and process-locked. Schema
  v3 stores jobs (request JSON, OZ tx id, latest hash, all hashes) and pending
  nullifier locks; a terminal status releases the lock in the same transaction.
- On-disk residue: a terminal status clears the stored request (proof,
  recipient). `secure_delete=ON` zeroes the cleared and deleted content, and
  every 100th cleanup round runs `wal_checkpoint(TRUNCATE)` so old WAL frames
  are removed too. The first start of this version on an existing database
  runs one `VACUUM` (marked in `store_meta`) to purge residue written by older
  binaries; it rewrites the whole file, so expect a short startup delay on a
  large database.
- Memory: at startup the process sets `RLIMIT_CORE=0` and, on Linux,
  `PR_SET_DUMPABLE=0` (no core file, no `/proc/<pid>/mem` or ptrace access by
  other processes of the same user). The systemd units add `LimitCORE=0` and
  `MemorySwapMax=0` (cgroup v2). Go's GC frees memory but does not zero it;
  these settings keep it from leaving RAM.
- The nullifier lock is held from admission until a definite outcome; an
  unknown OZ submit outcome keeps it until reconciliation.
- On SIGINT/SIGTERM the HTTP server stops, workers are cancelled and awaited
  for up to 30 seconds, then SQLite and RPC clients are closed. An in-flight OZ
  submit is not aborted; its outcome is reconciled on the next start.
- Back up SQLite with the relayer stopped (keep `-wal`/`-shm` files together).
- The OZ webhook is not used: polling is authoritative.
