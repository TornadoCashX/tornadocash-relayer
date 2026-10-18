# OpenZeppelin Relayer for go-withdraw-relayer

OZ v1.8.0 + Redis via Docker Compose. OZ holds the signing key and does nonce,
signing, broadcast, fee bumps and confirmations; go-withdraw-relayer only
submits preflighted transactions and polls them.

```
deploy/oz/
├── docker-compose.yaml        OZ (127.0.0.1:8080, metrics :8081) + Redis (AOF, no RDB)
├── .env.example               -> .env (secrets, retention, storage)
└── config/
    ├── config.json            relayer "tornado-mainnet" + signer + policies
    ├── networks/ethereum.json mainnet + sepolia, your RPC URLs
    └── keys/                  tornado-signer.json (keystore v3, not committed)
```

## 1. Signer key

Create an encrypted keystore (v3) as `config/keys/tornado-signer.json`, e.g.

```bash
# with the OZ repo checkout
cargo run --example create_key -- --password '<PASS>' \
  --output-dir /path/to/deploy/oz/config/keys --filename tornado-signer.json
# or with Foundry (prompts for the password)
cast wallet new config/keys && mv config/keys/<uuid> config/keys/tornado-signer.json
```

Fund the address with native gas. go-withdraw-relayer reads it from OZ; nothing
is configured on the Go side.

## 2. `.env`

```bash
cp .env.example .env
openssl rand -hex 32        # API_KEY  (= OZ_API_KEY of go-withdraw-relayer)
openssl rand -hex 32        # WEBHOOK_SIGNING_KEY
openssl rand -base64 32     # STORAGE_ENCRYPTION_KEY
```

Set `KEYSTORE_PASSPHRASE` to the keystore password. Keep
`TRANSACTION_EXPIRATION_HOURS=24` (calldata contains recipients; a record
pruned during a longer Go outage is resolved by chain state) and
`RESET_STORAGE_ON_START=false`. Redis runs with AOF only (`--save ""`, no
`dump.rdb`); keep the Docker volume on an encrypted disk. Reset
storage (or lose the Redis volume) only after pausing the OZ relayer and seeing
`currentQueue: 0` in go-withdraw-relayer `/health/jobs` and OZ
`pending_transactions_count` 0; otherwise in-flight jobs lose their OZ record
and only resolve by chain state (at the latest `OZ_TX_VALID_MINUTES` + 15 min).

## 3. `config/networks/ethereum.json`

Replace the `REPLACE-WITH-...` URLs with your own RPCs (OZ fails over between
them). `required_confirmations` is the confirmation depth for `CONFIRMED`
(mainnet 3, same as `deploy/oz-native/`).

## 4. `config/config.json` (relayer policy)

The fields that Go verifies must match the file passed with `--config`; the
remaining fields are required operational policy:

| field | value |
|---|---|
| `id` | = `OZ_RELAYER_ID` (`tornado-mainnet`) |
| `network` | network name from `networks/*.json` (`mainnet`, `sepolia`) whose `chain_id` = `NET_ID` |
| `policies.gas_price_cap` | `maxGasPriceGwei` x 10^9 (1000 gwei -> `1000000000000`), must not be higher |
| `policies.whitelist_receivers` | exactly every pool of `NET_ID` (the file lists all mainnet pools) |
| `policies.eip1559_pricing` | `true` |
| `policies.gas_limit_estimation` | `false` (gas limit comes from Go) |
| `policies.min_balance` | `10000000000000000` (0.01 ETH); below it OZ disables the relayer and Go returns 503 |

For a testnet run, set `"id": "tornado-sepolia"`, `"network": "sepolia"`, the
Sepolia pools in `whitelist_receivers`, and `NET_ID=11155111` on the Go side.

`config.json` is loaded into Redis only on the first start. Later changes go
through the OZ API, e.g.

```bash
curl -X PATCH -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  http://127.0.0.1:8080/api/v1/relayers/tornado-mainnet \
  -d '{"policies":{"gas_price_cap":500000000000}}'
```

go-withdraw-relayer re-checks the relayer every 5 minutes.

## 5. Start and verify

```bash
docker compose up -d
H="Authorization: Bearer $API_KEY"
curl -s -H "$H" http://127.0.0.1:8080/api/v1/ready
curl -s -H "$H" http://127.0.0.1:8080/api/v1/relayers/tornado-mainnet          # address, policies
curl -s -H "$H" http://127.0.0.1:8080/api/v1/relayers/tornado-mainnet/status   # balance, nonce, paused
curl -s -H "$H" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' \
  http://127.0.0.1:8080/api/v1/relayers/tornado-mainnet/rpc                    # must be NET_ID
```

Then go-withdraw-relayer `.env`:

```
OZ_RELAYER_URL=http://127.0.0.1:8080
OZ_RELAYER_ID=tornado-mainnet
OZ_API_KEY=<API_KEY>
```

## Operations

- Back up the Redis volume (`redis_data`) and the keystore; Redis holds nonces
  and in-flight transactions.
- Never run two OZ instances on the same signer with different Redis instances.
- Pause new submissions: `PATCH .../relayers/tornado-mainnet {"paused": true}`
  (Go then answers 503 and keeps reconciling submitted jobs).
- Keep the API (8080) and metrics (8081) on loopback or a private network.
