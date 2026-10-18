# OpenZeppelin Relayer without Docker

Layout on the server (all paths absolute; OZ resolves relative paths from its
working directory):

```
/app/oz-relayer/
├── bin/openzeppelin-relayer          built binary
├── .env                              from .env.example (chmod 600)
├── logs/
└── config/
    ├── config.json                   relayers tornado-mainnet + tornado-bsc, one signer
    ├── networks/ethereum.json        mainnet (chain 1)
    ├── networks/bsc.json             binance-smart-chain (chain 56)
    └── keys/tornado-signer.json      keystore v3 (chmod 600)
```

## 1. Build and install

```bash
git clone --branch v1.8.0 https://github.com/OpenZeppelin/openzeppelin-relayer
cd openzeppelin-relayer && cargo build --release
sudo useradd --system --home /app/oz-relayer ozrelayer
sudo mkdir -p /app/oz-relayer/{bin,logs,config/networks,config/keys}
sudo cp target/release/openzeppelin-relayer /app/oz-relayer/bin/
# signer key
cargo run --example create_key -- --password '<PASS>' \
  --output-dir /tmp/oz-keys --filename tornado-signer.json
sudo mv /tmp/oz-keys/tornado-signer.json /app/oz-relayer/config/keys/
```

## 2. Redis

Install Redis 7 locally and add `redis.conf.snippet` to `redis.conf`
(loopback only, AOF on, RDB snapshots off). Remove any other `save` lines,
restart Redis, then delete the old `dump.rdb` in the Redis `dir`. Keep
`/var/lib/redis` on an encrypted disk: the AOF holds transaction calldata
(recipients) until `TRANSACTION_EXPIRATION_HOURS` (24) prunes it and the AOF
is rewritten. Keep Redis out of swap as well:
`sudo systemctl edit redis-server` and add `[Service]`, `LimitCORE=0`,
`MemorySwapMax=0`. `oz-relayer.service` depends on the
Debian/Ubuntu unit name `redis-server.service`; on RHEL-like systems change it
to `redis.service`.

## 3. Configuration

```bash
sudo cp .env.example /app/oz-relayer/.env                  # fill in secrets
sudo cp config/config.json /app/oz-relayer/config/
sudo cp config/networks/*.json /app/oz-relayer/config/networks/   # set RPC URLs
sudo chown -R ozrelayer:ozrelayer /app/oz-relayer
sudo chmod 600 /app/oz-relayer/.env /app/oz-relayer/config/keys/tornado-signer.json
```

One signer serves both chains (OZ keeps a separate nonce per chain). Every
relayer policy must match the go-withdraw-relayer instance that uses it, or Go
refuses withdrawals:

- `whitelist_receivers`: exactly all pools of that chain (mainnet 21, BSC 4).
- `gas_price_cap` <= `maxGasPriceGwei` x 10^9 of that chain (shipped equal:
  mainnet 1000 gwei, BSC 10 gwei).
- `eip1559_pricing: true`, `gas_limit_estimation: false`.
- `min_balance` is 0.01 ETH / 0.01 BNB (`10000000000000000`). The signer needs
  native balance above it on every chain, or OZ refuses to start.
- `required_confirmations`: mainnet 3, BSC 1 (`networks/*.json`).

`networks/` may only contain network files: every `*.json` there is loaded.
`config.json` is copied into Redis on the first start only; afterwards use the
OZ API, or restart once with `RESET_STORAGE_ON_START=true` when no transaction
is in flight: pause the OZ relayers first, then wait until every
go-withdraw-relayer `/health/jobs` shows `currentQueue: 0` and OZ
`pending_transactions_count` is 0. Losing Redis data has the same effect as a
reset. Otherwise in-flight jobs lose their OZ record and only resolve by chain
state (at the latest `OZ_TX_VALID_MINUTES` + 15 min later).

## 4. Run

```bash
sudo cp oz-relayer.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now oz-relayer
journalctl -u oz-relayer -f
```

Manual run for a first test: `cd /app/oz-relayer && ./bin/openzeppelin-relayer`
(reads `./.env`).

The log must show `Relayer initialization completed succeeded=2 failed=0`
and no `NOAUTH`. Verify:

```bash
H="Authorization: Bearer $API_KEY"
for id in tornado-mainnet tornado-bsc; do
  curl -s -H "$H" http://127.0.0.1:8089/api/v1/relayers/$id/status; echo   # paused/system_disabled false
  curl -s -H "$H" -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' \
    http://127.0.0.1:8089/api/v1/relayers/$id/rpc; echo                     # 0x1 / 0x38
done
```

Then run one go-withdraw-relayer per chain with explicit files (same
`OZ_RELAYER_URL` and `OZ_API_KEY`, different `SQLITE_PATH` and `APP_PORT`):

```bash
/app/go-relayer/bin/go-withdraw-relayer \
  --config /app/go-relayer/mainnet/relayer.config.json \
  --env /app/go-relayer/mainnet/.env
```

```
mainnet: NET_ID=1   OZ_RELAYER_ID=tornado-mainnet  OZ_RELAYER_URL=http://127.0.0.1:8089
BSC:     NET_ID=56  OZ_RELAYER_ID=tornado-bsc      OZ_RELAYER_URL=http://127.0.0.1:8089
```
