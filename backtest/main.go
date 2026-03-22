// backtest: tests whether pre-interval Chainlink momentum predicts Polymarket outcomes.
//
// How Polymarket up/down markets work:
//   slug "eth-updown-5m-{T}" → measures: will ETH be higher at T+5min vs T?
//   Chainlink price at T = "opening price", at T+5min = "closing price"
//   UP wins if close > open, DOWN wins if close < open
//
// This backtester computes signals using Chainlink prices BEFORE T and checks
// whether they predict the UP/DOWN outcome.
//
// Signals tested:
//   momentum   — majority of tick directions in last N rounds before T
//   trend      — overall price direction over last N rounds (trend-following)
//   contrarian — opposite of trend (mean-reversion bet)
//
// Usage:
//   go run main.go
//   go run main.go -n 100
//   go run main.go -config ETH/5m -n 50 -v

package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Config ────────────────────────────────────────────────────────────────────

const momentumRounds = 20 // chainlink rounds to examine before slot time

var rpcs = []string{
	"https://polygon-mainnet.g.alchemy.com/v2/GQbpR-HuBjaHo6PmNeDX_0Q6v_JzeeL-",
	"https://polygon-mainnet.g.alchemy.com/v2/5hhz-YQlBNS5P3a0yQP1d1NUYIH8Y60Y",
	"https://polygon-mainnet.g.alchemy.com/v2/ekhAY1tkpnoZoXunaPchIvQLNO_VK8Ez",
	"https://polygon-mainnet.g.alchemy.com/v2/hRaqPBE0W6yReQz2srxeEVyp4pWBZGno",
	"https://polygon-mainnet.g.alchemy.com/v2/Ehs0Eqavi_d94auCYWrZJfgY0OoyEu2Y",
}

type Config struct {
	Name          string
	SlugPrefix    string
	IntervalLabel string
	IntervalSecs  int64
	OracleAddr    string
}

var allConfigs = []Config{
	{"ETH/5m",  "eth-updown", "5m",  300,  "0xF9680D99D6C9589e2a93a78A04A279e509205945"},
	{"ETH/15m", "eth-updown", "15m", 900,  "0xF9680D99D6C9589e2a93a78A04A279e509205945"},
	{"BTC/5m",  "btc-updown", "5m",  300,  "0xc907E116054Ad103354f2D350FD2514433D57F6F"},
	{"BTC/15m", "btc-updown", "15m", 900,  "0xc907E116054Ad103354f2D350FD2514433D57F6F"},
	{"SOL/5m",  "sol-updown", "5m",  300,  "0x10C8264C0935b3B9870013e057f330Ff3e9C56dC"},
	{"SOL/15m", "sol-updown", "15m", 900,  "0x10C8264C0935b3B9870013e057f330Ff3e9C56dC"},
	{"XRP/5m",  "xrp-updown", "5m",  300,  "0x785ba89291f676b5386652eB12b30cF361020694"},
	{"XRP/15m", "xrp-updown", "15m", 900,  "0x785ba89291f676b5386652eB12b30cF361020694"},
}

// ── RPC helpers ───────────────────────────────────────────────────────────────

var (
	rpcMu  sync.Mutex
	rpcPos int
)

type rpcReq struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}
type rpcResp struct {
	Result string          `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func sanitizeJSON(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	for _, b := range raw {
		if b >= 0x20 || b == '\n' || b == '\r' || b == '\t' {
			out = append(out, b)
		}
	}
	return out
}

func ethCall(client *http.Client, to, data string) (string, error) {
	body, _ := json.Marshal(rpcReq{
		JSONRPC: "2.0",
		Method:  "eth_call",
		Params:  []interface{}{map[string]string{"to": to, "data": data}, "latest"},
		ID:      1,
	})

	rpcMu.Lock()
	start := rpcPos
	rpcPos = (rpcPos + 1) % len(rpcs)
	rpcMu.Unlock()

	for i := 0; i < len(rpcs); i++ {
		rpc := rpcs[(start+i)%len(rpcs)]
		resp, err := client.Post(rpc, "application/json", bytes.NewReader(body))
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		raw = sanitizeJSON(raw)

		var r rpcResp
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		if len(r.Error) > 0 && string(r.Error) != "null" {
			continue
		}
		if r.Result == "" || r.Result == "0x" {
			continue
		}
		return r.Result, nil
	}
	return "", fmt.Errorf("all RPCs failed for %s", to)
}

// ── Chainlink ─────────────────────────────────────────────────────────────────

const (
	latestRdSel = "0xfeaf968c"
	getRdSel    = "0x9a6fc8f5"
)

type RoundInfo struct {
	RoundID   *big.Int
	Price     float64
	UpdatedAt int64
}

func decodeRound(result string) (*RoundInfo, error) {
	s := strings.TrimPrefix(result, "0x")
	if len(s) < 5*64 {
		return nil, fmt.Errorf("short response (%d)", len(s))
	}
	decBI := func(chunk string) *big.Int {
		b, _ := hex.DecodeString(chunk)
		return new(big.Int).SetBytes(b)
	}
	answer    := decBI(s[64:128])
	updatedAt := decBI(s[192:256])
	roundID   := decBI(s[0:64])

	if answer.Cmp(new(big.Int).Lsh(big.NewInt(1), 255)) >= 0 {
		answer.Sub(answer, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	price, _ := new(big.Float).Quo(
		new(big.Float).SetInt(answer),
		new(big.Float).SetFloat64(1e8),
	).Float64()

	return &RoundInfo{RoundID: roundID, Price: price, UpdatedAt: updatedAt.Int64()}, nil
}

// roundCache avoids redundant RPC calls for the same (addr, roundID).
var (
	roundCache   = map[string]*RoundInfo{}
	roundCacheMu sync.RWMutex
)

func getRoundByID(client *http.Client, addr string, roundID *big.Int) (*RoundInfo, error) {
	key := addr[:8] + roundID.String()
	roundCacheMu.RLock()
	if r, ok := roundCache[key]; ok {
		roundCacheMu.RUnlock()
		return r, nil
	}
	roundCacheMu.RUnlock()

	b := roundID.FillBytes(make([]byte, 32))
	result, err := ethCall(client, addr, getRdSel+hex.EncodeToString(b))
	if err != nil {
		return nil, err
	}
	r, err := decodeRound(result)
	if err != nil {
		return nil, err
	}
	r.RoundID = new(big.Int).Set(roundID)

	roundCacheMu.Lock()
	roundCache[key] = r
	roundCacheMu.Unlock()
	return r, nil
}

func latestRound(client *http.Client, addr string) (*RoundInfo, error) {
	result, err := ethCall(client, addr, latestRdSel)
	if err != nil {
		return nil, err
	}
	r, err := decodeRound(result)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// phaseComponents splits a Chainlink uint80 round ID into (phaseBase, aggrID).
// Upper 16 bits = phaseId, lower 64 bits = aggregatorRound.
func phaseComponents(roundID *big.Int) (phaseBase *big.Int, aggrID *big.Int) {
	shift := new(big.Int).Lsh(big.NewInt(1), 64)
	phase := new(big.Int).Div(roundID, shift)
	base  := new(big.Int).Mul(phase, shift)
	return base, new(big.Int).Sub(roundID, base)
}

// findRoundAtOrBefore returns the last round with updatedAt <= targetTs.
func findRoundAtOrBefore(client *http.Client, addr string, targetTs int64, latest *RoundInfo) (*RoundInfo, error) {
	if latest.UpdatedAt <= targetTs {
		return latest, nil
	}

	base, latestAggr := phaseComponents(latest.RoundID)
	lo := big.NewInt(1)
	hi := new(big.Int).Set(latestAggr)

	var best *RoundInfo
	for iter := 0; iter < 50 && lo.Cmp(hi) <= 0; iter++ {
		mid := new(big.Int).Rsh(new(big.Int).Add(lo, hi), 1)
		roundID := new(big.Int).Add(base, mid)
		r, err := getRoundByID(client, addr, roundID)
		if err != nil {
			hi.Sub(mid, big.NewInt(1))
			continue
		}
		if r.UpdatedAt <= targetTs {
			best = r
			lo.Add(mid, big.NewInt(1))
		} else {
			hi.Sub(mid, big.NewInt(1))
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no round found before ts %d (earliest round may be newer)", targetTs)
	}
	return best, nil
}

// pricesBefore returns prices for the n rounds ending at (and including) endRound,
// ordered oldest → newest.
func pricesBefore(client *http.Client, addr string, endRound *RoundInfo, n int) []float64 {
	base, endAggr := phaseComponents(endRound.RoundID)
	prices := make([]float64, 0, n+1)
	for i := n; i >= 0; i-- {
		aggrID := new(big.Int).Sub(endAggr, big.NewInt(int64(i)))
		if aggrID.Sign() <= 0 {
			continue
		}
		r, err := getRoundByID(client, addr, new(big.Int).Add(base, aggrID))
		if err != nil {
			continue
		}
		prices = append(prices, r.Price)
	}
	return prices
}

// ── Gamma API ─────────────────────────────────────────────────────────────────

const gammaBase = "https://gamma-api.polymarket.com"

type gammaEvent struct {
	Slug    string `json:"slug"`
	Markets []struct {
		OutcomePricesRaw string `json:"outcomePrices"`
	} `json:"markets"`
}

type ResolvedMkt struct {
	Slug     string
	SlotTs   int64 // timestamp embedded in slug = start of measurement window
	UpWon    bool  // true = UP token resolved to 1.0
}

// slotFromSlug extracts the unix timestamp from a slug like "eth-updown-5m-1742400000".
func slotFromSlug(slug string) (int64, error) {
	parts := strings.Split(slug, "-")
	if len(parts) < 1 {
		return 0, fmt.Errorf("can't parse slug %s", slug)
	}
	ts, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("non-numeric last part in %s: %w", slug, err)
	}
	return ts, nil
}

// fetchResolvedMarkets iterates slots backwards from now to collect resolved markets.
// It works around the Gamma API's broken slug_prefix filter.
func fetchResolvedMarkets(client *http.Client, cfg Config, limit int, verbose bool) ([]ResolvedMkt, error) {
	now := time.Now().UTC().Unix()
	// Start 3 slots back (give most recent markets time to resolve)
	slot := (now/cfg.IntervalSecs)*cfg.IntervalSecs - 3*cfg.IntervalSecs

	var out []ResolvedMkt
	misses := 0
	maxMisses := 20 // consecutive misses before giving up

	for len(out) < limit && misses < maxMisses {
		slug := fmt.Sprintf("%s-%s-%d", cfg.SlugPrefix, cfg.IntervalLabel, slot)
		url  := gammaBase + "/events?slug=" + slug

		resp, err := client.Get(url)
		if err != nil {
			misses++
			slot -= cfg.IntervalSecs
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var events []gammaEvent
		if err := json.Unmarshal(sanitizeJSON(raw), &events); err != nil || len(events) == 0 {
			misses++
			slot -= cfg.IntervalSecs
			continue
		}

		ev := events[0]
		if len(ev.Markets) == 0 {
			misses++
			slot -= cfg.IntervalSecs
			continue
		}

		var prices []string
		if err := json.Unmarshal([]byte(ev.Markets[0].OutcomePricesRaw), &prices); err != nil || len(prices) < 2 {
			misses++
			slot -= cfg.IntervalSecs
			continue
		}

		p0, _ := strconv.ParseFloat(strings.TrimSpace(prices[0]), 64)
		p1, _ := strconv.ParseFloat(strings.TrimSpace(prices[1]), 64)

		if p0 < 0.95 && p1 < 0.95 {
			// Not yet fully resolved
			if verbose {
				fmt.Printf("  [%s] %s not resolved (%.2f / %.2f)\n", cfg.Name, slug, p0, p1)
			}
			misses = 0
			slot -= cfg.IntervalSecs
			continue
		}

		misses = 0
		out = append(out, ResolvedMkt{
			Slug:   slug,
			SlotTs: slot,
			UpWon:  p0 > 0.95,
		})

		if verbose && len(out)%20 == 0 {
			fmt.Printf("  [%s] %d markets collected...\n", cfg.Name, len(out))
		}

		slot -= cfg.IntervalSecs
	}

	return out, nil
}

// ── Signals ───────────────────────────────────────────────────────────────────

// momentumSignal: majority of ticks going up → predict UP
func momentumSignal(prices []float64) bool {
	if len(prices) < 2 {
		return false
	}
	up := 0
	for i := 1; i < len(prices); i++ {
		if prices[i] > prices[i-1] {
			up++
		}
	}
	return up*2 > len(prices)-1
}

// trendSignal: last price > first price → predict UP (trend continuation)
func trendSignal(prices []float64) bool {
	if len(prices) < 2 {
		return false
	}
	return prices[len(prices)-1] > prices[0]
}

// momentumStrength: how strong is the momentum signal (0=balanced, 1=all one direction)
func momentumStrength(prices []float64) float64 {
	if len(prices) < 2 {
		return 0
	}
	up := 0
	total := len(prices) - 1
	for i := 1; i < len(prices); i++ {
		if prices[i] > prices[i-1] {
			up++
		}
	}
	return math.Abs(float64(up)/float64(total) - 0.5) * 2
}

// ── Backtest ──────────────────────────────────────────────────────────────────

type BacktestResult struct {
	Cfg          Config
	Total        int
	Skipped      int
	UpWonCount   int
	MomCorrect   int
	TrendCorrect int
	ContraCorrect int
}

func (r BacktestResult) pct(n int) float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(n) / float64(r.Total) * 100
}

func backtestConfig(client *http.Client, cfg Config, limit int, verbose bool) BacktestResult {
	res := BacktestResult{Cfg: cfg}

	fmt.Printf("[%s] Collecting %d resolved markets...\n", cfg.Name, limit)
	markets, err := fetchResolvedMarkets(client, cfg, limit, verbose)
	if err != nil {
		fmt.Printf("[%s] ERROR: %v\n", cfg.Name, err)
		return res
	}
	fmt.Printf("[%s] Found %d resolved markets\n", cfg.Name, len(markets))
	if len(markets) == 0 {
		return res
	}

	// Get current Chainlink round as anchor for binary search
	latest, err := latestRound(client, cfg.OracleAddr)
	if err != nil {
		fmt.Printf("[%s] ERROR getting latest round: %v\n", cfg.Name, err)
		return res
	}
	fmt.Printf("[%s] Chainlink anchor: round=%s  price=%.4f  ts=%s\n",
		cfg.Name,
		latest.RoundID.String(),
		latest.Price,
		time.Unix(latest.UpdatedAt, 0).UTC().Format("2006-01-02 15:04"),
	)

	for i, mkt := range markets {
		if i > 0 && i%25 == 0 {
			fmt.Printf("[%s] %d/%d  upBias=%.1f%%  mom=%.1f%%  trend=%.1f%%  contra=%.1f%%\n",
				cfg.Name, i, len(markets),
				res.pct(res.UpWonCount),
				res.pct(res.MomCorrect),
				res.pct(res.TrendCorrect),
				res.pct(res.ContraCorrect),
			)
		}

		// Find the Chainlink round at the slot start time (= measurement window open)
		slotRound, err := findRoundAtOrBefore(client, cfg.OracleAddr, mkt.SlotTs, latest)
		if err != nil {
			if verbose {
				fmt.Printf("  [%s] skip %s: %v\n", cfg.Name, mkt.Slug, err)
			}
			res.Skipped++
			continue
		}

		// Get the N rounds BEFORE the slot start (pre-interval price history)
		prices := pricesBefore(client, cfg.OracleAddr, slotRound, momentumRounds)
		if len(prices) < 5 {
			if verbose {
				fmt.Printf("  [%s] skip %s: only %d prices\n", cfg.Name, mkt.Slug, len(prices))
			}
			res.Skipped++
			continue
		}

		mom   := momentumSignal(prices)
		trend := trendSignal(prices)

		res.Total++
		if mkt.UpWon {
			res.UpWonCount++
		}
		if mom == mkt.UpWon {
			res.MomCorrect++
		}
		if trend == mkt.UpWon {
			res.TrendCorrect++
		}
		if !trend == mkt.UpWon {
			res.ContraCorrect++
		}
	}

	return res
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	limitFlag   := flag.Int("n", 100, "resolved markets per config")
	filterFlag  := flag.String("config", "", "only test this config e.g. ETH/5m")
	verboseFlag := flag.Bool("v", false, "verbose per-market output")
	flag.Parse()

	client := &http.Client{Timeout: 15 * time.Second}

	cfgs := allConfigs
	if *filterFlag != "" {
		cfgs = nil
		for _, c := range allConfigs {
			if strings.EqualFold(c.Name, *filterFlag) {
				cfgs = append(cfgs, c)
			}
		}
		if len(cfgs) == 0 {
			fmt.Fprintf(os.Stderr, "unknown config %q. valid: %v\n", *filterFlag,
				func() []string {
					names := make([]string, len(allConfigs))
					for i, c := range allConfigs {
						names[i] = c.Name
					}
					return names
				}())
			os.Exit(1)
		}
	}

	fmt.Printf("=== Polymarket Backtest (%d markets/config) ===\n\n", *limitFlag)

	results := make([]BacktestResult, 0, len(cfgs))
	for _, cfg := range cfgs {
		r := backtestConfig(client, cfg, *limitFlag, *verboseFlag)
		results = append(results, r)
		fmt.Printf("[%s] RESULT: n=%d skip=%d  upBias=%.1f%%  mom=%.1f%%  trend=%.1f%%  contra=%.1f%%\n\n",
			cfg.Name, r.Total, r.Skipped,
			r.pct(r.UpWonCount), r.pct(r.MomCorrect),
			r.pct(r.TrendCorrect), r.pct(r.ContraCorrect),
		)
	}

	// ── Summary ──────────────────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("┌──────────┬───────┬──────────┬──────────┬──────────┬────────────┐")
	fmt.Println("│  Config  │   N   │ UP Bias  │ Momentum │  Trend   │ Contrarian │")
	fmt.Println("├──────────┼───────┼──────────┼──────────┼──────────┼────────────┤")
	for _, r := range results {
		fmt.Printf("│ %-8s │ %5d │  %5.1f%%  │  %5.1f%%  │  %5.1f%%  │   %5.1f%%    │\n",
			r.Cfg.Name, r.Total,
			r.pct(r.UpWonCount),
			r.pct(r.MomCorrect),
			r.pct(r.TrendCorrect),
			r.pct(r.ContraCorrect),
		)
	}
	fmt.Println("└──────────┴───────┴──────────┴──────────┴──────────┴────────────┘")
	fmt.Println()
	fmt.Println("Baseline: 50.0% = random.  Need >53% to beat fees.")
	fmt.Println("UP Bias:  if consistently ≠50%, always-bet-one-side beats signals.")
	fmt.Println("Trend:    bet continuation.  Contrarian: bet reversal.")

	// ── CSV ──────────────────────────────────────────────────────────────────
	f, err := os.Create("backtest_results.csv")
	if err == nil {
		defer f.Close()
		fmt.Fprintln(f, "config,total,skipped,up_bias_pct,momentum_pct,trend_pct,contrarian_pct")
		for _, r := range results {
			fmt.Fprintf(f, "%s,%d,%d,%.2f,%.2f,%.2f,%.2f\n",
				r.Cfg.Name, r.Total, r.Skipped,
				r.pct(r.UpWonCount), r.pct(r.MomCorrect),
				r.pct(r.TrendCorrect), r.pct(r.ContraCorrect),
			)
		}
		fmt.Println("\nSaved to backtest_results.csv")
	}
}
