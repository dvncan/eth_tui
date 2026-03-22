# Polymarket Signal Backtest — March 2026

## What We Built

A backtester that tests whether pre-interval Chainlink price signals can predict Polymarket up/down market outcomes across ETH, BTC, SOL, and XRP.

---

## How Polymarket Up/Down Markets Work

Each market slug has the format `{asset}-updown-{interval}-{timestamp}`.

Example: `eth-updown-15m-1774137000`

- The **timestamp** (`T`) is the start of the measurement window
- The market asks: *"Will the price be higher at T + interval than at T?"*
- **UP wins** if Chainlink price at `T + interval` > price at `T`
- **DOWN wins** otherwise
- Markets are created ~24 hours in advance and trading closes at `T + interval`

---

## Signals Tested

Three signals were computed from Chainlink price history in the **N rounds before T**:

### 1. Momentum
Counts how many of the last 20 Chainlink rounds were up-ticks vs down-ticks.
- Majority up → predict UP
- Majority down → predict DOWN

### 2. Trend
Compares the last price to the first price in the 20-round window.
- Last > first → predict UP (trend continuation)
- Last < first → predict DOWN

### 3. Contrarian
The opposite of Trend — bets on **reversal** rather than continuation.

---

## How to Run the Backtest

```bash
cd eth-tui/backtest

# Default: 100 markets per config, all 8 configs
go run main.go

# Test fewer markets
go run main.go -n 50

# Test a single config
go run main.go -config ETH/15m -n 100 -v

# Verbose (shows per-market progress)
go run main.go -v
```

Results are printed to stdout and saved to `backtest_results.csv`.

---

## March 2026 Results (100 markets per config)

| Config   |  N  | UP Bias | Momentum | Trend | Contrarian |
|----------|-----|---------|----------|-------|------------|
| ETH/5m   | 100 |  52.0%  |  46.0%   | **55.0%** | 45.0%  |
| ETH/15m  | 100 |  51.0%  |  43.0%   | 43.0% | **57.0%**  |
| BTC/5m   | 100 |  51.0%  |  55.0%   | 51.0% | **55.0%**  |
| BTC/15m  | 100 |  48.0%  |  54.0%   | 46.0% | **54.0%**  |
| SOL/5m   | 100 |  48.0%  |  51.0%   | 45.0% | **55.0%**  |
| SOL/15m  | 100 |  53.0%  |  50.0%   | 38.0% | **62.0%**  |
| XRP/5m   | 100 | **57.0%** | 51.0%  | 51.0% | 49.0%      |
| XRP/15m  | 100 |  47.0%  |  46.0%   | **54.0%** | 46.0%  |

Baseline: 50% = pure random. Need >53% to cover Polymarket fees.

---

## Key Findings

### 1. Momentum is anti-predictive for 15m markets
The current (pre-backtest) TUI used momentum as its primary signal. For ETH/15m it scored **43%** — worse than always guessing randomly. The signal was literally betting backwards.

### 2. Mean reversion dominates 15m markets
When price has been trending up in the 20 rounds before a 15m market opens, it is **more likely to go DOWN** during the market window. This held across ETH, BTC, and SOL.

Best contrarian edges:
- **SOL/15m: 62%** — strongest signal in the dataset
- **ETH/15m: 57%**
- **BTC/15m: 54%**
- **SOL/5m: 55%**

### 3. XRP/5m has a persistent UP bias
57% of XRP 5m markets resolved UP regardless of any signal. "Always bet UP" outperforms any signal for this config.

### 4. 5m markets are noisier
The 5m window is too short for clear mean reversion. ETH/5m showed a mild trend signal (55%) but most 5m configs were near random.

---

## Algorithm Changes Applied to TUI

Based on the backtest, each config was assigned a `SignalMode`:

| Config   | Mode    | Edge  | Rationale |
|----------|---------|-------|-----------|
| ETH/5m   | trend   | 55%   | Follow pre-interval direction |
| ETH/15m  | contra  | 57%   | Fade the trend |
| BTC/5m   | contra  | 55%   | Fade the trend |
| BTC/15m  | contra  | 54%   | Fade the trend |
| SOL/5m   | contra  | 55%   | Fade the trend |
| SOL/15m  | contra  | 62%   | Strong mean reversion |
| XRP/5m   | upbias  | 57%   | Persistent UP bias, no signal needed |
| XRP/15m  | trend   | 54%   | Follow pre-interval direction |

In `contra` mode the momentum component of the composite score is **negated** — so a bullish pre-interval becomes a DOWN signal. In `upbias` mode a constant bullish offset is added.

---

## Limitations

- **100 markets per config** is a small sample. Results could shift with more data. Run with `-n 500` to validate.
- **No out-of-sample test** — the same period was used to discover and validate. Should re-run after a week of live trading to confirm edge persists.
- **Oracle edge and book skew signals were not independently backtested** — only the momentum/trend components were validated. These still contribute 75% of the composite score based on theory, not data.
- **Market regime matters** — the March 2026 backtest period had specific volatility. A period of strong trending (e.g. a bull run) might favor trend signals over contrarian ones.
- **Fees not modeled** — Polymarket charges ~2% on winning trades. The true break-even accuracy is ~52%, not 50%.

---

## How to Re-run and Update Signal Modes

1. Run the backtest: `go run backtest/main.go -n 200`
2. Check the Contrarian vs Trend column for each config
3. If a config flips (e.g. ETH/5m contrarian starts beating trend), update `SignalMode` in `main.go`:

```go
var allConfigs = []AssetConfig{
    {Name: "ETH/USD", ..., SignalMode: "contra", BacktestEdge: 0.58},
    ...
}
```

4. Rebuild: `go build .`
5. Update this doc with the new results and date.

---

## Data Sources

- **Market outcomes**: Polymarket Gamma API — `https://gamma-api.polymarket.com/events?slug={slug}`
- **Price history**: Chainlink on-chain oracles on Polygon (via Alchemy RPC)
  - ETH/USD: `0xF9680D99D6C9589e2a93a78A04A279e509205945`
  - BTC/USD: `0xc907E116054Ad103354f2D350FD2514433D57F6F`
  - SOL/USD: `0x10C8264C0935b3B9870013e057f330Ff3e9C56dC`
  - XRP/USD: `0x785ba89291f676b5386652eB12b30cF361020694`
