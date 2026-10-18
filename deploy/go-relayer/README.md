# go-withdraw-relayer under systemd

One process per chain, as the template unit `go-withdraw-relayer@<name>`.

```
/app/go-relayer/
├── bin/go-withdraw-relayer               go build -trimpath -o bin/go-withdraw-relayer .
├── mainnet/
│   ├── relayer.config.json               chmod 644 (not group/other writable)
│   ├── .env                              chmod 600, owner gorelayer
│   └── data/                             SQLite + .lock + WAL (owner gorelayer, 700)
└── bsc/ ...
```

```bash
sudo useradd --system --home /app/go-relayer --shell /usr/sbin/nologin gorelayer
sudo install -d -o gorelayer -g gorelayer -m 700 /app/go-relayer/mainnet/data
sudo chown gorelayer:gorelayer /app/go-relayer/mainnet/.env
sudo chmod 600 /app/go-relayer/mainnet/.env
sudo chmod 644 /app/go-relayer/mainnet/relayer.config.json
sudo cp go-withdraw-relayer@.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now go-withdraw-relayer@mainnet
journalctl -u go-withdraw-relayer@mainnet -f
```

In each `.env` set an absolute `SQLITE_PATH` inside `data/`
(`SQLITE_PATH=/app/go-relayer/mainnet/data/relayer-oz.db`) and a distinct
`APP_PORT` per chain. The unit can write only to `data/`.

The log must show `OpenZeppelin Relayer verified ... accepting withdrawals`.
`TimeoutStopSec=60` covers the graceful shutdown (HTTP 15 s + workers 30 s).
