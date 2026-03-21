# eth-tui

Live terminal dashboard for monitoring Polymarket ETH/USD 15-minute markets alongside the Chainlink oracle price feed.

## What it shows

```
┌──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│  POLYMARKET ETH/USD ORACLE MONITOR                                                              23:07:34 UTC  [q] quit │
├──────────────────────────────────┬─────────────────────────────────────────────────────────────────────────────────┤
│  $2,148.79                       │  eth-updown-15m-1774134900                                                       │
│  ▲ +0.5012 (+0.02%)              │  Window  23:15 → 23:30 UTC                                                       │
│  Round  32074868                 │  ⏱  22m 25s remaining                                                            │
│  Updated 18s ago                 │  Open $2,148.79  Now $2,148.79  +0.00 (+0.00%) ━                                 │
│                                  │  Up: 0.545   Down: 0.455   Spread: 0.010                                         │
│  ▁▂▃▄▅▆▇█▇▆▅▄▃▂▁▂▃▄             │  Best Bid: 0.57   Best Ask: 0.58                                                 │
├──────────────────────────────────┼─────────────────────────────────────────────────────────────────────────────────┤
│  ORDER BOOK (UP)                 │  ORDER BOOK (DOWN)                                                               │
│  0.63  645.00  ██████░░░░  ASK   │  0.50   45.00  ██░░░░░░░░  ASK                                                  │
│  0.61  208.00  ████░░░░░░  ASK   │  0.47  283.48  ████████░░  ASK                                                  │
│  0.60  706.00  ██████████  ASK   │  0.45   10.00  █░░░░░░░░░  ASK                                                  │
│  0.58   46.31  █░░░░░░░░░  ASK   │  0.43    4.11  ░░░░░░░░░░  ASK                                                  │
│  ─── mid: 0.575  spread: 0.010 ──│  ─── mid: 0.425  spread: 0.010 ──                                               │
│  0.57    4.11  ░░░░░░░░░░  BID   │  0.42   46.31  █░░░░░░░░░  BID                                                  │
│  0.55   10.00  █░░░░░░░░░  BID   │  0.40  706.00  ██████████  BID                                                  │
│  0.53  283.48  ████████░░  BID   │  0.39  208.00  ████░░░░░░  BID                                                  │
├──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│  ●2148.79 ▲2149.10 ▼2148.26 ━2148.26 ▲2149.00                                                                      │
├──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│  ● Live  Oracle: Chainlink ETH/USD (Polygon)  CLOB: clob.polymarket.com  Updated: 23:07:34                          │
└──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

**Top-left panel** — Chainlink ETH/USD on-chain price (Polygon mainnet), round ID, seconds since last update, and a sparkline of recent price movement.

**Top-right panel** — Active market window, countdown to resolution, opening price vs current price (direction indicator), and current Up/Down odds with best bid/ask.

**Order book panels** — CLOB bids and asks for both the Up token (left) and Down token (right). Red = asks, green = bids. Bar width shows relative size.

**Price history bar** — Last 18 Chainlink price readings with direction arrows.

## Requirements

- Go 1.21+
- No external Go dependencies (stdlib only)
- Internet access (public Polygon RPC + Polymarket APIs, no API keys needed)
- Terminal width: 120 columns recommended

## Run

```bash
# Auto-detect the next active ETH 15m market
go run main.go

# Pin a specific market by slug
go run main.go -slug eth-updown-15m-1774134900

# Faster polling (every 2s — be mindful of rate limits)
go run main.go -slug eth-updown-15m-1774134900
```

Press `q` or `Ctrl+C` to exit.

## Build a binary

```bash
go build -o eth-tui .
./eth-tui
./eth-tui -slug eth-updown-15m-1774134900
```

## Market slug format

Polymarket's ETH 15m slugs follow the pattern:

```
eth-updown-15m-<unix_timestamp>
```

Where the timestamp is the **resolution time** (end of the 15m window), aligned to 15-minute boundaries (900-second multiples). To find upcoming slugs:

```bash
python3 -c "
import time
t = int(time.time())
next_slot = ((t // 900) + 1) * 900
for i in range(8):
    print(f'eth-updown-15m-{next_slot + i*900}')
"
```

Not all slots have markets created yet — Polymarket typically opens them a few windows ahead.

## Data sources

| Feed | Source | Auth |
|------|--------|------|
| ETH/USD price | Chainlink AggregatorV3 on Polygon (`0xF9680D99D6C9589e2a93a78A04A279e509205945`) via `1rpc.io/matic` | None |
| Market info / odds | Polymarket Gamma API (`gamma-api.polymarket.com`) | None |
| Order book | Polymarket CLOB API (`clob.polymarket.com`) | None |

## Companion tools

- `oracle_monitor.go` in the parent directory — headless CSV logger for the same feeds, useful for recording full market windows for analysis
- `oracle_data/<slug>.csv` — output from the logger, one row per poll, with Chainlink round IDs for cross-referencing

## Research notes

For thesis analysis, compare:
- `chainlink_updated_at` (the Unix timestamp of the on-chain Chainlink round) at `start_date` vs `end_date` — these are the exact values used for resolution
- `up_price` / `down_price` trajectory leading into resolution — look for sharp moves in the last 60–90 seconds
- Order book depth asymmetry (large size on one side) near expiry
