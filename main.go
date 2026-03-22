// eth-tui: Live terminal dashboard for Polymarket crypto up/down markets
//
// Assets: ETH, BTC, SOL, XRP  ×  5m and 15m windows
// Keys: [tab]/[n] next  [p] prev  [1-8] direct select  [q] quit
//
// Usage:
//   go run main.go

package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ── Market Configs ────────────────────────────────────────────────────────────

type AssetConfig struct {
	Name          string
	SlugPrefix    string
	IntervalSecs  int64
	IntervalLabel string
	OracleAddr    string
	// SignalMode is derived from backtest results:
	//   "trend"   — momentum continuation (ETH/5m, XRP/15m)
	//   "contra"  — mean-reversion / fade the trend (ETH/15m, BTC/15m, SOL/5m, SOL/15m, BTC/5m)
	//   "upbias"  — XRP/5m has a persistent ~57% UP win rate regardless of signal
	SignalMode    string
	// BacktestEdge is the backtested accuracy of the best signal (vs 50% random)
	BacktestEdge  float64
}

// Polygon Chainlink oracle addresses (verified).
// SignalMode and BacktestEdge derived from 100-market backtest (2026-03).
var allConfigs = []AssetConfig{
	{Name: "ETH/USD", SlugPrefix: "eth-updown", IntervalSecs: 300,  IntervalLabel: "5m",  OracleAddr: "0xF9680D99D6C9589e2a93a78A04A279e509205945", SignalMode: "trend",  BacktestEdge: 0.55},
	{Name: "ETH/USD", SlugPrefix: "eth-updown", IntervalSecs: 900,  IntervalLabel: "15m", OracleAddr: "0xF9680D99D6C9589e2a93a78A04A279e509205945", SignalMode: "contra", BacktestEdge: 0.57},
	{Name: "BTC/USD", SlugPrefix: "btc-updown", IntervalSecs: 300,  IntervalLabel: "5m",  OracleAddr: "0xc907E116054Ad103354f2D350FD2514433D57F6F", SignalMode: "contra", BacktestEdge: 0.55},
	{Name: "BTC/USD", SlugPrefix: "btc-updown", IntervalSecs: 900,  IntervalLabel: "15m", OracleAddr: "0xc907E116054Ad103354f2D350FD2514433D57F6F", SignalMode: "contra", BacktestEdge: 0.54},
	{Name: "SOL/USD", SlugPrefix: "sol-updown", IntervalSecs: 300,  IntervalLabel: "5m",  OracleAddr: "0x10C8264C0935b3B9870013e057f330Ff3e9C56dC", SignalMode: "contra", BacktestEdge: 0.55},
	{Name: "SOL/USD", SlugPrefix: "sol-updown", IntervalSecs: 900,  IntervalLabel: "15m", OracleAddr: "0x10C8264C0935b3B9870013e057f330Ff3e9C56dC", SignalMode: "contra", BacktestEdge: 0.62},
	{Name: "XRP/USD", SlugPrefix: "xrp-updown", IntervalSecs: 300,  IntervalLabel: "5m",  OracleAddr: "0x785ba89291f676b5386652eB12b30cF361020694", SignalMode: "upbias", BacktestEdge: 0.57},
	{Name: "XRP/USD", SlugPrefix: "xrp-updown", IntervalSecs: 900,  IntervalLabel: "15m", OracleAddr: "0x785ba89291f676b5386652eB12b30cF361020694", SignalMode: "trend",  BacktestEdge: 0.54},
}

// Multiple RPC endpoints — tried in order, rotated on failure
var polygonRPCs = []string{
	"https://polygon-mainnet.g.alchemy.com/v2/GQbpR-HuBjaHo6PmNeDX_0Q6v_JzeeL-",
	"https://polygon-mainnet.g.alchemy.com/v2/5hhz-YQlBNS5P3a0yQP1d1NUYIH8Y60Y",
	"https://polygon-mainnet.g.alchemy.com/v2/ekhAY1tkpnoZoXunaPchIvQLNO_VK8Ez",
	"https://polygon-mainnet.g.alchemy.com/v2/hRaqPBE0W6yReQz2srxeEVyp4pWBZGno",
	"https://polygon-mainnet.g.alchemy.com/v2/Ehs0Eqavi_d94auCYWrZJfgY0OoyEu2Y",
	"https://rpc.ankr.com/polygon",
	"https://polygon.llamarpc.com",
}

// ── Constants ────────────────────────────────────────────────────────────────

const (
	latestRoundDataSel = "0xfeaf968c"
	gammaBase          = "https://gamma-api.polymarket.com"
	clobBase           = "https://clob.polymarket.com"
	termWidth          = 120
	orderBookRows      = 4
	priceHistoryLen    = 18
	// Alternate screen buffer — prevents the frame from scrolling the terminal
	altScreenOn  = "\033[?1049h\033[H"
	altScreenOff = "\033[?1049l"
	hideCursor   = "\033[?25l"
	showCursor   = "\033[?25h"
)

// ── ANSI helpers ─────────────────────────────────────────────────────────────

const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	white  = "\033[37m"
)

func color(s, c string) string { return c + s + reset }

func pad(s string, n int) string {
	visible := stripANSI(s)
	diff := n - len([]rune(visible))
	if diff <= 0 {
		return s
	}
	return s + strings.Repeat(" ", diff)
}

func stripANSI(s string) string {
	out := make([]byte, 0, len(s))
	inSeq := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\033' {
			inSeq = true
			continue
		}
		if inSeq {
			if s[i] == 'm' {
				inSeq = false
			}
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

// ── Per-config cache ─────────────────────────────────────────────────────────

type PricePoint struct {
	Price     float64
	RoundID   string
	UpdatedAt int64
	FetchedAt time.Time
}

type OrderLevel struct {
	Price float64
	Size  float64
}

type OrderBook struct {
	Bids      []OrderLevel
	Asks      []OrderLevel
	FetchedAt time.Time
}

type MarketInfo struct {
	Slug         string
	ConditionID  string
	TokenIDUp    string
	TokenIDDown  string
	StartDate    time.Time
	EndDate      time.Time
	UpPrice      float64
	DownPrice    float64
	BestBid      float64
	BestAsk      float64
	OpeningPrice float64
	FetchedAt    time.Time
}

// ConfigCache holds live data for one (asset, interval) combination.
type ConfigCache struct {
	mu           sync.RWMutex
	chainlink    *PricePoint
	priceHistory []PricePoint
	market       *MarketInfo
	bookUp       *OrderBook
	bookDown     *OrderBook
	lastError    string
}

func (c *ConfigCache) setError(e string) {
	c.mu.Lock()
	c.lastError = e
	c.mu.Unlock()
}

// ── Top-level state ───────────────────────────────────────────────────────────

type State struct {
	mu        sync.RWMutex
	activeIdx int
	caches    []*ConfigCache
}

func newState() *State {
	s := &State{caches: make([]*ConfigCache, len(allConfigs))}
	for i := range allConfigs {
		s.caches[i] = &ConfigCache{}
	}
	return s
}

func (s *State) active() (int, *ConfigCache) {
	s.mu.RLock()
	idx := s.activeIdx
	s.mu.RUnlock()
	return idx, s.caches[idx]
}

func (s *State) switchTo(idx int) {
	if idx < 0 {
		idx = len(allConfigs) - 1
	}
	if idx >= len(allConfigs) {
		idx = 0
	}
	s.mu.Lock()
	s.activeIdx = idx
	s.mu.Unlock()
}

// ── Prediction Tracker ────────────────────────────────────────────────────────

const (
	predBet  = 5.0                  // dollars staked per prediction
	predFile = "predictions.json"   // persisted to cwd
)

type Prediction struct {
	At         int64   `json:"at"`
	Config     string  `json:"cfg"`
	Slug       string  `json:"slug"`
	EndTs      int64   `json:"end_ts"`
	Signal     string  `json:"signal"`      // "BUY UP" | "BUY DOWN"
	EntryPrice float64 `json:"entry_price"` // market price at entry
	Resolved   bool    `json:"resolved"`
	Won        bool    `json:"won"`
	PnL        float64 `json:"pnl"`
}

type TrackerStats struct {
	Total, Pending, Wins, Losses int
	PnL                          float64
}

type PredictionTracker struct {
	mu    sync.Mutex
	items []*Prediction
	seen  map[string]bool
}

func newPredictionTracker() *PredictionTracker {
	t := &PredictionTracker{seen: map[string]bool{}}
	data, err := os.ReadFile(predFile)
	if err == nil {
		json.Unmarshal(data, &t.items)
		for _, p := range t.items {
			t.seen[p.Slug] = true
		}
	}
	return t
}

func (t *PredictionTracker) save() {
	data, _ := json.MarshalIndent(t.items, "", "  ")
	os.WriteFile(predFile, data, 0644)
}

// MaybeRecord records a prediction when the signal is non-neutral and not already seen.
func (t *PredictionTracker) MaybeRecord(cfgName string, mkt *MarketInfo, sig *Signals) {
	if sig == nil || mkt == nil {
		return
	}
	if sig.CompositeLabel != "BUY UP" && sig.CompositeLabel != "BUY DOWN" {
		return
	}
	if sig.CompositeConf == "LOW" {
		return
	}
	if time.Now().UTC().After(mkt.EndDate) {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.seen[mkt.Slug] {
		return
	}
	t.seen[mkt.Slug] = true

	entryPrice := mkt.UpPrice
	if sig.CompositeLabel == "BUY DOWN" {
		entryPrice = mkt.DownPrice
	}
	if entryPrice <= 0.01 {
		return
	}

	t.items = append(t.items, &Prediction{
		At:         time.Now().Unix(),
		Config:     cfgName,
		Slug:       mkt.Slug,
		EndTs:      mkt.EndDate.Unix(),
		Signal:     sig.CompositeLabel,
		EntryPrice: entryPrice,
	})
	t.save()
}

// resolveOne checks if a pending prediction has resolved and updates it.
func (t *PredictionTracker) resolveOne(client *http.Client, p *Prediction) {
	if p.Resolved || time.Now().UTC().Unix() < p.EndTs {
		return
	}
	resp, err := client.Get(gammaBase + "/events?slug=" + p.Slug)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var events []gammaEvent
	if err := json.Unmarshal(sanitizeJSON(raw), &events); err != nil || len(events) == 0 {
		return
	}
	if len(events[0].Markets) == 0 {
		return
	}
	var prices []string
	if err := json.Unmarshal([]byte(events[0].Markets[0].OutcomePricesRaw), &prices); err != nil || len(prices) < 2 {
		return
	}
	p0, _ := strconv.ParseFloat(strings.TrimSpace(prices[0]), 64)
	p1, _ := strconv.ParseFloat(strings.TrimSpace(prices[1]), 64)
	if p0 < 0.95 && p1 < 0.95 {
		return // not fully resolved yet
	}
	upWon := p0 > 0.95

	t.mu.Lock()
	defer t.mu.Unlock()
	p.Resolved = true
	p.Won = (p.Signal == "BUY UP" && upWon) || (p.Signal == "BUY DOWN" && !upWon)
	if p.Won {
		p.PnL = predBet * (1.0/p.EntryPrice - 1.0)
	} else {
		p.PnL = -predBet
	}
	t.save()
}

func (t *PredictionTracker) CheckResolutionsLoop(client *http.Client) {
	for {
		time.Sleep(30 * time.Second)
		t.mu.Lock()
		pending := make([]*Prediction, 0, len(t.items))
		for _, p := range t.items {
			if !p.Resolved {
				pending = append(pending, p)
			}
		}
		t.mu.Unlock()
		for _, p := range pending {
			t.resolveOne(client, p)
		}
	}
}

func (t *PredictionTracker) Stats() TrackerStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := TrackerStats{Total: len(t.items)}
	for _, p := range t.items {
		if !p.Resolved {
			s.Pending++
			continue
		}
		if p.Won {
			s.Wins++
		} else {
			s.Losses++
		}
		s.PnL += p.PnL
	}
	return s
}

func (t *PredictionTracker) Recent(n int) []*Prediction {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*Prediction, 0, n)
	for i := len(t.items) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, t.items[i])
	}
	return out
}

// ── Chainlink ────────────────────────────────────────────────────────────────

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

func fetchChainlink(client *http.Client, oracleAddr string) (*PricePoint, error) {
	body, _ := json.Marshal(rpcReq{
		JSONRPC: "2.0", Method: "eth_call",
		Params: []interface{}{
			map[string]string{"to": oracleAddr, "data": latestRoundDataSel},
			"latest",
		},
		ID: 1,
	})

	var lastErr error
	for _, rpc := range polygonRPCs {
		resp, err := client.Post(rpc, "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		raw = sanitizeJSON(raw)

		var r rpcResp
		if err := json.Unmarshal(raw, &r); err != nil {
			lastErr = fmt.Errorf("%s: %w", rpc, err)
			continue
		}
		if len(r.Error) > 0 && string(r.Error) != "null" {
			lastErr = fmt.Errorf("rpc %s: %s", rpc, r.Error)
			continue
		}
		data := strings.TrimPrefix(r.Result, "0x")
		if len(data) < 5*64 {
			lastErr = fmt.Errorf("short response from %s", rpc)
			continue
		}
		dec := func(s string) *big.Int {
			b, _ := hex.DecodeString(s)
			return new(big.Int).SetBytes(b)
		}
		roundID   := dec(data[0:64])
		answer    := dec(data[64:128])
		updatedAt := dec(data[192:256])

		if answer.Cmp(new(big.Int).Lsh(big.NewInt(1), 255)) >= 0 {
			answer.Sub(answer, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		price, _ := new(big.Float).Quo(
			new(big.Float).SetInt(answer),
			new(big.Float).SetInt(big.NewInt(1e8)),
		).Float64()

		return &PricePoint{
			Price:     price,
			RoundID:   roundID.String(),
			UpdatedAt: updatedAt.Int64(),
			FetchedAt: time.Now().UTC(),
		}, nil
	}
	return nil, lastErr
}

// ── Gamma / Market ───────────────────────────────────────────────────────────

type gammaMarket struct {
	ID               string      `json:"id"`
	ConditionID      string      `json:"conditionId"`
	ClobTokenIDsRaw  string      `json:"clobTokenIds"`
	OutcomePricesRaw string      `json:"outcomePrices"`
	BestBid          json.Number `json:"bestBid"`
	BestAsk          json.Number `json:"bestAsk"`
}
type gammaEvent struct {
	Slug      string        `json:"slug"`
	StartDate string        `json:"startDate"`
	EndDate   string        `json:"endDate"`
	Markets   []gammaMarket `json:"markets"`
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}
func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func sanitizeJSON(raw []byte) []byte {
	clean := make([]byte, 0, len(raw))
	for _, b := range raw {
		if b >= 0x20 || b == '\n' || b == '\r' || b == '\t' {
			clean = append(clean, b)
		}
	}
	return clean
}

func autoDetectSlug(client *http.Client, cfg AssetConfig) (string, error) {
	now := time.Now().UTC().Unix()
	currentSlot := (now / cfg.IntervalSecs) * cfg.IntervalSecs
	for i := int64(0); i < 8; i++ {
		slot := currentSlot + i*cfg.IntervalSecs
		slug := fmt.Sprintf("%s-%s-%d", cfg.SlugPrefix, cfg.IntervalLabel, slot)
		resp, err := client.Get(gammaBase + "/events?slug=" + slug)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var events []gammaEvent
		if err := json.Unmarshal(sanitizeJSON(raw), &events); err != nil || len(events) == 0 {
			continue
		}
		if len(events[0].Markets) == 0 {
			continue
		}
		endT := parseTime(events[0].EndDate)
		if time.Now().UTC().Before(endT.Add(30 * time.Second)) {
			return slug, nil
		}
	}
	return "", fmt.Errorf("no active %s %s market", cfg.Name, cfg.IntervalLabel)
}

func fetchMarket(client *http.Client, cfg AssetConfig) (*MarketInfo, error) {
	slug, err := autoDetectSlug(client, cfg)
	if err != nil {
		return nil, err
	}
	resp, err := client.Get(gammaBase + "/events?slug=" + slug)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var events []gammaEvent
	if err := json.Unmarshal(sanitizeJSON(raw), &events); err != nil {
		return nil, err
	}
	if len(events) == 0 || len(events[0].Markets) == 0 {
		return nil, fmt.Errorf("no event for slug %s", slug)
	}
	ev := &events[0]
	m := ev.Markets[0]

	var tokenIDs []string
	json.Unmarshal([]byte(m.ClobTokenIDsRaw), &tokenIDs)
	var prices []string
	json.Unmarshal([]byte(m.OutcomePricesRaw), &prices)

	upPrice, downPrice := 0.0, 0.0
	if len(prices) > 0 { upPrice = parseFloat(prices[0]) }
	if len(prices) > 1 { downPrice = parseFloat(prices[1]) }
	tokenDown := ""
	if len(tokenIDs) > 1 { tokenDown = tokenIDs[1] }

	return &MarketInfo{
		Slug:        ev.Slug,
		ConditionID: m.ConditionID,
		TokenIDUp:   tokenIDs[0],
		TokenIDDown: tokenDown,
		StartDate:   parseTime(ev.StartDate),
		EndDate:     parseTime(ev.EndDate),
		UpPrice:     upPrice,
		DownPrice:   downPrice,
		BestBid:     parseFloat(m.BestBid.String()),
		BestAsk:     parseFloat(m.BestAsk.String()),
		FetchedAt:   time.Now().UTC(),
	}, nil
}

// ── CLOB Order Book ──────────────────────────────────────────────────────────

type clobBook struct {
	Bids []struct {
		Price string `json:"price"`
		Size  string `json:"size"`
	} `json:"bids"`
	Asks []struct {
		Price string `json:"price"`
		Size  string `json:"size"`
	} `json:"asks"`
}

func fetchBook(client *http.Client, tokenID string) (*OrderBook, error) {
	resp, err := client.Get(clobBase + "/book?token_id=" + tokenID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var cb clobBook
	if err := json.Unmarshal(raw, &cb); err != nil {
		return nil, err
	}

	conv := func(entries []struct {
		Price string `json:"price"`
		Size  string `json:"size"`
	}) []OrderLevel {
		levels := make([]OrderLevel, 0, len(entries))
		for _, e := range entries {
			p := parseFloat(e.Price)
			s := parseFloat(e.Size)
			if p > 0 && s > 0 {
				levels = append(levels, OrderLevel{Price: p, Size: s})
			}
		}
		return levels
	}

	bids := conv(cb.Bids)
	asks := conv(cb.Asks)
	sort.Slice(bids, func(i, j int) bool { return bids[i].Price > bids[j].Price })
	sort.Slice(asks, func(i, j int) bool { return asks[i].Price < asks[j].Price })
	return &OrderBook{Bids: bids, Asks: asks, FetchedAt: time.Now().UTC()}, nil
}

// ── Polling goroutines ────────────────────────────────────────────────────────

// pollConfig polls oracle, market, and books for one config index.
// refreshCh: receives a signal to fetch immediately (e.g. on tab switch).
// activeTick: frequent tick used when this config is active.
// bgTick: slower tick used when this config is in the background.
func pollConfig(state *State, client *http.Client, cfgIdx int,
	refreshCh <-chan struct{}, activeTick, bgTick <-chan time.Time) {

	cfg   := allConfigs[cfgIdx]
	cache := state.caches[cfgIdx]

	doOracle := func() {
		p, err := fetchChainlink(client, cfg.OracleAddr)
		if err != nil {
			cache.setError("oracle: " + err.Error())
			return
		}
		cache.mu.Lock()
		cache.chainlink = p
		cache.priceHistory = append(cache.priceHistory, *p)
		if len(cache.priceHistory) > priceHistoryLen {
			cache.priceHistory = cache.priceHistory[len(cache.priceHistory)-priceHistoryLen:]
		}
		if cache.market != nil && cache.market.OpeningPrice == 0 {
			if time.Now().UTC().After(cache.market.StartDate) {
				cache.market.OpeningPrice = p.Price
			}
		}
		cache.lastError = ""
		cache.mu.Unlock()
	}

	doMarket := func() {
		m, err := fetchMarket(client, cfg)
		if err != nil {
			cache.setError("market: " + err.Error())
			return
		}
		cache.mu.Lock()
		if cache.market != nil && cache.market.Slug == m.Slug {
			m.OpeningPrice = cache.market.OpeningPrice
		}
		cache.market = m
		cache.mu.Unlock()
	}

	doBooks := func() {
		cache.mu.RLock()
		mkt := cache.market
		cache.mu.RUnlock()
		if mkt == nil || mkt.TokenIDUp == "" {
			return
		}
		if up, err := fetchBook(client, mkt.TokenIDUp); err == nil {
			cache.mu.Lock()
			cache.bookUp = up
			cache.mu.Unlock()
		}
		if mkt.TokenIDDown != "" {
			if down, err := fetchBook(client, mkt.TokenIDDown); err == nil {
				cache.mu.Lock()
				cache.bookDown = down
				cache.mu.Unlock()
			}
		}
	}

	doAll := func() {
		doOracle()
		doMarket()
		doBooks()
	}

	// Pre-fetch immediately at startup (all configs, not just active)
	go doAll()

	for {
		select {
		case <-refreshCh:
			// Tab was switched to this config — fetch everything immediately
			go doAll()

		case <-activeTick:
			activeIdx, _ := state.active()
			if activeIdx == cfgIdx {
				go doOracle()
				go doBooks()
			}

		case <-bgTick:
			activeIdx, _ := state.active()
			if activeIdx != cfgIdx {
				// Background refresh: keep data warm for instant switching
				go doAll()
			} else {
				// Active config: also refresh market on bg tick
				go doMarket()
			}
		}
	}
}

// ── Signals ──────────────────────────────────────────────────────────────────

type Signals struct {
	MomentumUpCount  int
	MomentumTotal    int
	MomentumAvgDelta float64
	MomentumLabel    string

	DeltaFromOpen   float64
	DeltaPct        float64
	VolPerInterval  float64
	IntervalsLeft   float64
	ZScore          float64
	FairValueUp     float64
	OracleEdge      float64
	OracleEdgeLabel string

	BookBidTotal  float64
	BookAskTotal  float64
	BookSkewRatio float64
	BookSkewLabel string

	CompositeScore float64
	CompositeLabel string
	CompositeConf  string
}

func normCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

func computeSignals(history []PricePoint, mkt *MarketInfo, bookUp *OrderBook, cfg AssetConfig) *Signals {
	sig := &Signals{}
	if len(history) < 2 || mkt == nil {
		sig.MomentumLabel = "INSUFFICIENT DATA"
		sig.OracleEdgeLabel = "INSUFFICIENT DATA"
		sig.BookSkewLabel = "INSUFFICIENT DATA"
		sig.CompositeLabel = "INSUFFICIENT DATA"
		return sig
	}

	changes := make([]float64, 0, len(history)-1)
	for i := 1; i < len(history); i++ {
		changes = append(changes, history[i].Price-history[i-1].Price)
	}
	upCount, sumChange := 0, 0.0
	for _, c := range changes {
		if c > 0.005 { upCount++ }
		sumChange += c
	}
	sig.MomentumUpCount = upCount
	sig.MomentumTotal = len(changes)
	sig.MomentumAvgDelta = sumChange / float64(len(changes))
	upRatio := float64(upCount) / float64(len(changes))
	switch {
	case upRatio >= 0.65: sig.MomentumLabel = "BULLISH"
	case upRatio <= 0.35: sig.MomentumLabel = "BEARISH"
	default:              sig.MomentumLabel = "NEUTRAL"
	}

	currentPrice := history[len(history)-1].Price
	sig.DeltaFromOpen = currentPrice - mkt.OpeningPrice
	if mkt.OpeningPrice > 0 {
		sig.DeltaPct = sig.DeltaFromOpen / mkt.OpeningPrice * 100
	}
	if len(changes) >= 3 {
		meanC := sumChange / float64(len(changes))
		variance := 0.0
		for _, c := range changes {
			d := c - meanC
			variance += d * d
		}
		sig.VolPerInterval = math.Sqrt(variance / float64(len(changes)))
	}
	remaining := mkt.EndDate.Sub(time.Now().UTC())
	if remaining < 0 { remaining = 0 }
	sig.IntervalsLeft = remaining.Seconds() / 5.0
	if sig.VolPerInterval > 0 && sig.IntervalsLeft > 0 {
		sig.ZScore = sig.DeltaFromOpen / (sig.VolPerInterval * math.Sqrt(sig.IntervalsLeft))
	} else if sig.DeltaFromOpen != 0 {
		if sig.DeltaFromOpen > 0 { sig.ZScore = 10 } else { sig.ZScore = -10 }
	}
	sig.FairValueUp = normCDF(sig.ZScore)
	sig.OracleEdge = sig.FairValueUp - mkt.UpPrice
	switch {
	case sig.OracleEdge > 0.05:  sig.OracleEdgeLabel = "BUY UP"
	case sig.OracleEdge < -0.05: sig.OracleEdgeLabel = "BUY DOWN"
	default:                     sig.OracleEdgeLabel = "FAIRLY PRICED"
	}

	if bookUp != nil {
		depth := 10
		for _, l := range bookUp.Bids[:min(depth, len(bookUp.Bids))] { sig.BookBidTotal += l.Size }
		for _, l := range bookUp.Asks[:min(depth, len(bookUp.Asks))] { sig.BookAskTotal += l.Size }
		if sig.BookAskTotal > 0 { sig.BookSkewRatio = sig.BookBidTotal / sig.BookAskTotal }
		switch {
		case sig.BookSkewRatio >= 1.5:  sig.BookSkewLabel = "BUY PRESSURE"
		case sig.BookSkewRatio <= 0.67: sig.BookSkewLabel = "SELL PRESSURE"
		default:                        sig.BookSkewLabel = "BALANCED"
		}
	} else {
		sig.BookSkewLabel = "NO DATA"
	}

	momentumScore := float64(upCount)/float64(len(changes))*2 - 1

	// Apply signal mode from backtest calibration
	switch cfg.SignalMode {
	case "contra":
		// Mean-reversion: fade the pre-interval trend (ETH/15m, BTC/15m, SOL/5m, SOL/15m, BTC/5m)
		momentumScore = -momentumScore
		sig.MomentumLabel = map[string]string{
			"BULLISH": "FADE (BEARISH)",
			"BEARISH": "FADE (BULLISH)",
			"NEUTRAL": "NEUTRAL",
		}[sig.MomentumLabel]
		if sig.MomentumLabel == "" {
			sig.MomentumLabel = "NEUTRAL"
		}
	case "upbias":
		// XRP/5m: persistent UP bias — add constant bullish boost
		momentumScore = momentumScore*0.3 + 0.4
	}

	edgeScore := math.Max(-1, math.Min(1, sig.OracleEdge*4))
	bookScore := 0.0
	if sig.BookSkewRatio > 0 {
		bookScore = math.Max(-1, math.Min(1, math.Log(sig.BookSkewRatio)))
	}
	sig.CompositeScore = momentumScore*0.25 + edgeScore*0.50 + bookScore*0.25

	absScore := math.Abs(sig.CompositeScore)
	switch {
	case absScore >= 0.5:  sig.CompositeConf = "HIGH"
	case absScore >= 0.25: sig.CompositeConf = "MEDIUM"
	default:               sig.CompositeConf = "LOW"
	}
	switch {
	case sig.CompositeScore >= 0.15:  sig.CompositeLabel = "BUY UP"
	case sig.CompositeScore <= -0.15: sig.CompositeLabel = "BUY DOWN"
	default:                          sig.CompositeLabel = "NEUTRAL"
	}
	return sig
}

// ── Render helpers ───────────────────────────────────────────────────────────

func max(a, b int) int { if a > b { return a }; return b }
func min(a, b int) int { if a < b { return a }; return b }

func bar(size, maxSize float64, width int) string {
	if maxSize <= 0 { return strings.Repeat("░", width) }
	filled := int(math.Round(float64(width) * size / maxSize))
	if filled > width { filled = width }
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func fmtPrice(p float64) string {
	s := fmt.Sprintf("%.2f", p)
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	neg := strings.HasPrefix(intPart, "-")
	if neg { intPart = intPart[1:] }
	var out []byte
	for i, c := range []byte(intPart) {
		if i > 0 && (len(intPart)-i)%3 == 0 { out = append(out, ',') }
		out = append(out, c)
	}
	result := "$" + string(out)
	if len(parts) > 1 { result += "." + parts[1] }
	if neg { result = "-" + result }
	return result
}

func fmtSize(s float64) string {
	if s >= 1000 { return fmt.Sprintf("%8.0f", s) }
	return fmt.Sprintf("%8.2f", s)
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 { return "EXPIRED" }
	return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func solidBar(score float64, width int) string {
	mid := width / 2
	filled := int(math.Round(math.Abs(score) * float64(mid)))
	if filled > mid { filled = mid }
	buf := []rune(strings.Repeat("░", width))
	if score >= 0 {
		for i := mid; i < mid+filled && i < width; i++ { buf[i] = '█' }
	} else {
		for i := mid - filled; i < mid && i >= 0; i++ { buf[i] = '█' }
	}
	buf[mid] = '┼'
	return string(buf)
}

func labelColor(label string) string {
	switch label {
	case "BULLISH", "BUY UP", "BUY PRESSURE": return color(label, bold+green)
	case "BEARISH", "BUY DOWN", "SELL PRESSURE": return color(label, bold+red)
	case "HIGH":   return color(label, bold+green)
	case "MEDIUM": return color(label, yellow)
	case "LOW":    return color(label, dim)
	default:       return color(label, dim)
	}
}

func renderSignals(sig *Signals, W int) []string {
	var lines []string
	lines = append(lines, pad(color("  ◆ SIGNALS", bold+cyan), W))

	momArrows := ""
	for i := 0; i < sig.MomentumTotal && i < 10; i++ {
		if i < sig.MomentumUpCount { momArrows += color("▲", green) } else { momArrows += color("▼", red) }
	}
	lines = append(lines, pad(fmt.Sprintf("  MOMENTUM   %s  %d/%d up  avg %+.3f$/5s  [%s]  %s",
		momArrows, sig.MomentumUpCount, sig.MomentumTotal, sig.MomentumAvgDelta,
		solidBar(float64(sig.MomentumUpCount)/float64(max(sig.MomentumTotal, 1))*2-1, 20),
		labelColor(sig.MomentumLabel)), W))

	deltaCol := green
	if sig.DeltaFromOpen < 0 { deltaCol = red }
	fvCol := green
	if sig.FairValueUp < 0.5 { fvCol = red }
	edgeCol := green
	if sig.OracleEdge < 0 { edgeCol = red }
	lines = append(lines, pad(fmt.Sprintf("  EDGE        Δ open %s  vol ±%.3f$/5s  Z=%+.2f  FV=%s  edge=%s  → %s",
		color(fmt.Sprintf("%+.2f (%+.2f%%)", sig.DeltaFromOpen, sig.DeltaPct), deltaCol),
		sig.VolPerInterval, sig.ZScore,
		color(fmt.Sprintf("%.3f", sig.FairValueUp), fvCol),
		color(fmt.Sprintf("%+.3f", sig.OracleEdge), edgeCol),
		labelColor(sig.OracleEdgeLabel)), W))

	skewCol := dim
	if sig.BookSkewLabel == "BUY PRESSURE" { skewCol = green } else if sig.BookSkewLabel == "SELL PRESSURE" { skewCol = red }
	lines = append(lines, pad(fmt.Sprintf("  BOOK SKEW   bid $%.0f / ask $%.0f = %.2fx  [%s]  %s",
		sig.BookBidTotal, sig.BookAskTotal, sig.BookSkewRatio,
		color(solidBar(math.Log(math.Max(sig.BookSkewRatio, 0.01)), 20), skewCol),
		labelColor(sig.BookSkewLabel)), W))

	compCol := dim
	switch sig.CompositeLabel {
	case "BUY UP":   compCol = bold + green
	case "BUY DOWN": compCol = bold + red
	}
	lines = append(lines, pad(fmt.Sprintf("  COMPOSITE   [%s]  score %+.3f  → %s  confidence: %s",
		color(solidBar(sig.CompositeScore, 30), compCol),
		sig.CompositeScore, color(sig.CompositeLabel, compCol),
		labelColor(sig.CompositeConf)), W))

	return lines
}

func renderPredictions(tracker *PredictionTracker, W int) []string {
	var lines []string
	lines = append(lines, pad(color("  ◆ PREDICTION TRACKER", bold+cyan), W))

	st := tracker.Stats()
	resolved := st.Wins + st.Losses
	winRate := 0.0
	if resolved > 0 {
		winRate = float64(st.Wins) / float64(resolved) * 100
	}
	pnlCol := green
	if st.PnL < 0 {
		pnlCol = red
	}
	lines = append(lines, pad(fmt.Sprintf("  Total: %d  Pending: %d  W/L: %d/%d  WinRate: %.1f%%  P&L: %s  (${%.0f}/trade)",
		st.Total, st.Pending, st.Wins, st.Losses, winRate,
		color(fmt.Sprintf("%+.2f", st.PnL), pnlCol), predBet), W))

	recent := tracker.Recent(5)
	if len(recent) == 0 {
		lines = append(lines, pad(color("  No predictions yet — signals generate entries automatically", dim), W))
	}
	for _, p := range recent {
		status := color("PENDING", yellow)
		pnlStr := ""
		if p.Resolved {
			if p.Won {
				status = color(" WIN  ", green)
				pnlStr = color(fmt.Sprintf(" %+.2f", p.PnL), green)
			} else {
				status = color(" LOSS ", red)
				pnlStr = color(fmt.Sprintf(" %+.2f", p.PnL), red)
			}
		}
		sigCol := green
		if p.Signal == "BUY DOWN" {
			sigCol = red
		}
		ts := time.Unix(p.At, 0).UTC().Format("15:04")
		lines = append(lines, pad(fmt.Sprintf("  %s  %-8s  %s  entry=%.3f  [%s]%s",
			ts, p.Config, color(p.Signal, sigCol), p.EntryPrice, status, pnlStr), W))
	}

	return lines
}

func renderOrderHalf(book *OrderBook, side string, width int) []string {
	const bw = 10
	lw := width - 1
	var lines []string
	lines = append(lines, pad(color(fmt.Sprintf("  ORDER BOOK (%s)", side), bold+cyan), lw))
	if book == nil {
		for i := 0; i < orderBookRows*2+3; i++ { lines = append(lines, strings.Repeat(" ", lw)) }
		return lines
	}
	displayed := append(book.Asks[:min(len(book.Asks), orderBookRows)], book.Bids[:min(len(book.Bids), orderBookRows)]...)
	maxSize := 0.0
	for _, l := range displayed { if l.Size > maxSize { maxSize = l.Size } }

	asks := book.Asks
	if len(asks) > orderBookRows { asks = asks[:orderBookRows] }
	for i := len(asks) - 1; i >= 0; i-- {
		l := asks[i]
		lines = append(lines, pad(fmt.Sprintf("  %s %s  %s %s",
			color(fmt.Sprintf("%.2f", l.Price), red), fmtSize(l.Size), color(bar(l.Size, maxSize, bw), red), "ASK"), lw))
	}
	spread := "  ─────────────────────────────────────"
	if len(book.Asks) > 0 && len(book.Bids) > 0 {
		sp := book.Asks[0].Price - book.Bids[0].Price
		mid := (book.Asks[0].Price + book.Bids[0].Price) / 2
		spread = fmt.Sprintf("  ─────── mid: %.3f  spread: %.3f ───────", mid, sp)
	}
	lines = append(lines, pad(color(spread, dim), lw))
	bids := book.Bids
	if len(bids) > orderBookRows { bids = bids[:orderBookRows] }
	for _, l := range bids {
		lines = append(lines, pad(fmt.Sprintf("  %s %s  %s %s",
			color(fmt.Sprintf("%.2f", l.Price), green), fmtSize(l.Size), color(bar(l.Size, maxSize, bw), green), "BID"), lw))
	}
	for len(lines) < orderBookRows*2+3 { lines = append(lines, strings.Repeat(" ", lw)) }
	return lines
}

// renderTabs returns two tab rows (4 tabs each) as separate strings.
func renderTabs(activeIdx, W int) (string, string) {
	makeRow := func(start, end int) string {
		var sb strings.Builder
		sb.WriteString(" ")
		for i := start; i < end && i < len(allConfigs); i++ {
			cfg := allConfigs[i]
			// Short label: "ETH/5m" style
			label := fmt.Sprintf("[%d] %s/%s", i+1, strings.Split(cfg.Name, "/")[0], cfg.IntervalLabel)
			if i == activeIdx {
				sb.WriteString(color(" "+label+" ", bold+cyan))
			} else {
				sb.WriteString(color(" "+label+" ", dim))
			}
			if i < end-1 && i < len(allConfigs)-1 {
				sb.WriteString(color("│", dim))
			}
		}
		vis := stripANSI(sb.String())
		sp := W - 2 - len([]rune(vis))
		if sp < 0 { sp = 0 }
		return sb.String() + strings.Repeat(" ", sp)
	}
	row1 := makeRow(0, 4)
	row2 := makeRow(4, 8)
	// append hint to row2
	hint := color("  tab/n=next  p=prev  q=quit", dim)
	vis2 := stripANSI(row2)
	sp := W - 2 - len([]rune(vis2)) - len([]rune(stripANSI(hint)))
	if sp < 0 { sp = 0 }
	row2 = strings.TrimRight(row2, " ") + strings.Repeat(" ", sp) + hint + strings.Repeat(" ", 0)
	// re-pad to W-2
	vis2 = stripANSI(row2)
	if extra := W - 2 - len([]rune(vis2)); extra > 0 {
		row2 += strings.Repeat(" ", extra)
	}
	return row1, row2
}

func renderFrame(state *State, tracker *PredictionTracker) string {
	idx, cache := state.active()
	cfg := allConfigs[idx]

	cache.mu.RLock()
	cl        := cache.chainlink
	history   := cache.priceHistory
	mkt       := cache.market
	bookUp    := cache.bookUp
	bookDown  := cache.bookDown
	lastErr   := cache.lastError
	cache.mu.RUnlock()

	W := termWidth
	half := (W - 1) / 2
	var b strings.Builder
	now := time.Now().UTC()

	write := func(line string) {
		vis := stripANSI(line)
		if len([]rune(vis)) > W { line = line[:W] }
		b.WriteString(line + "\n")
	}
	box := func(w int) string { return strings.Repeat("─", w) }

	// ── Title + tabs ─────────────────────────────────────────────────────────
	modeLabel := map[string]string{"trend": "TREND", "contra": "CONTRA", "upbias": "UPBIAS"}[cfg.SignalMode]
	title := color(fmt.Sprintf("  POLYMARKET %s %s  [%s mode  edge=%.0f%%]", cfg.Name, cfg.IntervalLabel, modeLabel, cfg.BacktestEdge*100), bold+cyan)
	ts := color(now.Format("15:04:05")+" UTC", dim)
	write("┌" + box(W-2) + "┐")
	write("│" + pad(pad(title, W-15)+ts, W-2) + "│")
	tab1, tab2 := renderTabs(idx, W)
	write("├" + box(W-2) + "┤")
	write("│" + tab1 + "│")
	write("│" + tab2 + "│")
	write("├" + box(half-1) + "┬" + box(W-half-2) + "┤")

	// ── Left: oracle  │  Right: market info ─────────────────────────────────
	var leftLines, rightLines []string

	if cl != nil {
		leftLines = append(leftLines, color(fmt.Sprintf("  %s", fmtPrice(cl.Price)), bold+yellow))
		if len(history) >= 2 {
			prev := history[len(history)-2].Price
			diff := cl.Price - prev
			pct := diff / prev * 100
			arrow, col := "▲", green
			if diff < 0 { arrow, col = "▼", red } else if diff == 0 { arrow, col = "━", dim }
			leftLines = append(leftLines, color(fmt.Sprintf("  %s %+.4f (%+.2f%%)", arrow, diff, pct), col))
		} else {
			leftLines = append(leftLines, "")
		}
		age := now.Unix() - cl.UpdatedAt
		leftLines = append(leftLines, color(fmt.Sprintf("  Round  %-10s", cl.RoundID[len(cl.RoundID)-8:]), dim))
		leftLines = append(leftLines, color(fmt.Sprintf("  Updated %ds ago", age), dim))
		leftLines = append(leftLines, color(fmt.Sprintf("  Oracle  %s...%s", cfg.OracleAddr[:6], cfg.OracleAddr[len(cfg.OracleAddr)-4:]), dim))
		leftLines = append(leftLines, "")
		if len(history) > 0 {
			sparks := "  "
			pmin, pmax := history[0].Price, history[0].Price
			for _, p := range history {
				if p.Price < pmin { pmin = p.Price }
				if p.Price > pmax { pmax = p.Price }
			}
			rng := pmax - pmin
			chars := []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
			for _, p := range history {
				idx2 := 0
				if rng > 0 { idx2 = int((p.Price-pmin)/rng*7) }
				sparks += chars[idx2]
			}
			leftLines = append(leftLines, color(sparks, cyan))
		}
	} else {
		leftLines = append(leftLines, color(fmt.Sprintf("  Fetching %s oracle...", cfg.Name), dim))
	}

	if mkt != nil {
		rightLines = append(rightLines, color("  "+mkt.Slug, bold))
		rightLines = append(rightLines, color(fmt.Sprintf("  Window  %s → %s", mkt.StartDate.Format("15:04"), mkt.EndDate.Format("15:04 UTC")), dim))
		remaining := mkt.EndDate.Sub(now)
		timeCol := green
		if remaining < 2*time.Minute { timeCol = red } else if remaining < 5*time.Minute { timeCol = yellow }
		rightLines = append(rightLines, color(fmt.Sprintf("  ⏱  %s remaining", fmtDuration(remaining)), timeCol))
		if mkt.OpeningPrice > 0 && cl != nil {
			delta := cl.Price - mkt.OpeningPrice
			pct := delta / mkt.OpeningPrice * 100
			dir, col := "UP ↑", green
			if delta < 0 { dir, col = "DOWN ↓", red }
			rightLines = append(rightLines, fmt.Sprintf("  Open %s  Now %s  %s",
				color(fmtPrice(mkt.OpeningPrice), dim), color(fmtPrice(cl.Price), bold),
				color(fmt.Sprintf("%+.2f (%+.2f%%) %s", delta, pct, dir), col)))
		} else {
			rightLines = append(rightLines, color("  Waiting for opening price...", dim))
		}
		rightLines = append(rightLines, "")
		upC, downC := white, white
		if mkt.UpPrice > mkt.DownPrice { upC = green } else { downC = green }
		rightLines = append(rightLines, fmt.Sprintf("  Up: %s   Down: %s   Spread: %.3f",
			color(fmt.Sprintf("%.3f", mkt.UpPrice), upC),
			color(fmt.Sprintf("%.3f", mkt.DownPrice), downC),
			mkt.BestAsk-mkt.BestBid))
		rightLines = append(rightLines, color(fmt.Sprintf("  Best Bid: %.2f   Best Ask: %.2f", mkt.BestBid, mkt.BestAsk), dim))
	} else {
		rightLines = append(rightLines, color(fmt.Sprintf("  Fetching %s %s market...", cfg.Name, cfg.IntervalLabel), dim))
	}

	for i := 0; i < 8; i++ {
		l, r := "", ""
		if i < len(leftLines)  { l = leftLines[i] }
		if i < len(rightLines) { r = rightLines[i] }
		lv, rv := stripANSI(l), stripANSI(r)
		lp := half - 2 - len([]rune(lv))
		rp := W - half - 2 - len([]rune(rv))
		if lp < 0 { lp = 0 }
		if rp < 0 { rp = 0 }
		write("│" + l + strings.Repeat(" ", lp) + " │ " + r + strings.Repeat(" ", rp) + "│")
	}

	// ── Order books ──────────────────────────────────────────────────────────
	write("├" + box(half-1) + "┼" + box(W-half-2) + "┤")
	upLines   := renderOrderHalf(bookUp,   "UP",   half-1)
	downLines := renderOrderHalf(bookDown, "DOWN", W-half-2)
	for i := 0; i < max(len(upLines), len(downLines)); i++ {
		l, r := "", ""
		if i < len(upLines)   { l = upLines[i] }
		if i < len(downLines) { r = downLines[i] }
		lv, rv := stripANSI(l), stripANSI(r)
		lp := half - 1 - len([]rune(lv))
		rp := W - half - 2 - len([]rune(rv))
		if lp < 0 { lp = 0 }
		if rp < 0 { rp = 0 }
		write("│" + l + strings.Repeat(" ", lp) + "│" + r + strings.Repeat(" ", rp) + "│")
	}

	// ── Signals ──────────────────────────────────────────────────────────────
	write("├" + box(W-2) + "┤")
	for _, sigLine := range renderSignals(computeSignals(history, mkt, bookUp, cfg), W-2) {
		vis := stripANSI(sigLine)
		sp := W - 2 - len([]rune(vis))
		if sp < 0 { sp = 0 }
		write("│" + sigLine + strings.Repeat(" ", sp) + "│")
	}

	// ── Price history ────────────────────────────────────────────────────────
	write("├" + box(W-2) + "┤")
	histLine := "  "
	if len(history) > 0 {
		prev := history[0].Price
		for i, p := range history {
			diff := p.Price - prev
			col, arrow := dim, "━"
			if diff > 0.01  { col, arrow = green, "▲" }
			if diff < -0.01 { col, arrow = red,   "▼" }
			if i == 0       { col, arrow = cyan,   "●" }
			histLine += color(fmt.Sprintf("%s%.2f ", arrow, p.Price), col)
			prev = p.Price
		}
	} else {
		histLine += color("Collecting price history...", dim)
	}
	hp := W - 2 - len([]rune(stripANSI(histLine)))
	if hp < 0 { hp = 0 }
	write("│" + histLine + strings.Repeat(" ", hp) + "│")

	// ── Predictions panel ────────────────────────────────────────────────────
	write("├" + box(W-2) + "┤")
	for _, predLine := range renderPredictions(tracker, W-2) {
		vis := stripANSI(predLine)
		sp := W - 2 - len([]rune(vis))
		if sp < 0 {
			sp = 0
		}
		write("│" + predLine + strings.Repeat(" ", sp) + "│")
	}

	// ── Status bar ───────────────────────────────────────────────────────────
	write("├" + box(W-2) + "┤")
	statusMsg := ""
	if lastErr != "" {
		statusMsg = "  " + color("⚠ "+lastErr, red)
	} else {
		statusMsg = "  " + color("● Live", green) +
			color(fmt.Sprintf("  Oracle: Chainlink %s (Polygon)  CLOB: clob.polymarket.com  %s", cfg.Name, now.Format("15:04:05")), dim)
	}
	sp2 := W - 2 - len([]rune(stripANSI(statusMsg)))
	if sp2 < 0 { sp2 = 0 }
	write("│" + statusMsg + strings.Repeat(" ", sp2) + "│")
	write("└" + box(W-2) + "┘")

	return b.String()
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	state   := newState()
	client  := &http.Client{Timeout: 10 * time.Second}
	tracker := newPredictionTracker()
	go tracker.CheckResolutionsLoop(client)

	// Enter alternate screen — keeps frame fixed, no scrolling
	fmt.Print(hideCursor + altScreenOn)

	cleanup := func() {
		fmt.Print(showCursor + altScreenOff)
	}

	// Per-config refresh channels — buffered so sends never block
	refreshChs := make([]chan struct{}, len(allConfigs))
	for i := range refreshChs {
		refreshChs[i] = make(chan struct{}, 1)
	}

	// Active config: oracle+books every 5s, market every 30s
	// Background configs: full refresh every 30s (keeps all data warm)
	activeTick := time.NewTicker(5 * time.Second)
	bgTick     := time.NewTicker(30 * time.Second)

	for i := range allConfigs {
		i := i
		go pollConfig(state, client, i, refreshChs[i], activeTick.C, bgTick.C)
		time.Sleep(100 * time.Millisecond) // light stagger to spread initial API calls
	}

	renderTick := time.NewTicker(500 * time.Millisecond)
	defer renderTick.Stop()

	osig := make(chan os.Signal, 1)
	signal.Notify(osig, syscall.SIGINT, syscall.SIGTERM)

	quit     := make(chan struct{})
	switchCh := make(chan int, 8)

	// Key reader
	go func() {
		buf := make([]byte, 1)
		for {
			n, _ := os.Stdin.Read(buf)
			if n == 0 { continue }
			ch := buf[0]
			switch {
			case ch == 'q' || ch == 'Q' || ch == 3:
				close(quit)
				return
			case ch == '\t' || ch == 'n' || ch == 'N':
				switchCh <- 1
			case ch == 'p' || ch == 'P':
				switchCh <- -1
			case ch >= '1' && ch <= '9':
				if idx := int(ch - '1'); idx < len(allConfigs) {
					switchCh <- 100 + idx
				}
			}
		}
	}()

	// Switch handler — also signals an immediate refresh on the new tab
	go func() {
		for v := range switchCh {
			state.mu.RLock()
			cur := state.activeIdx
			state.mu.RUnlock()
			var next int
			if v >= 100 {
				next = v - 100
			} else if v == 1 {
				next = (cur + 1) % len(allConfigs)
			} else {
				next = cur - 1
				if next < 0 { next = len(allConfigs) - 1 }
			}
			state.switchTo(next)
			// Non-blocking signal to refresh the new tab immediately
			select {
			case refreshChs[next] <- struct{}{}:
			default:
			}
		}
	}()

	for {
		select {
		case <-osig:
			cleanup()
			fmt.Println("Exiting.")
			os.Exit(0)
		case <-quit:
			cleanup()
			os.Exit(0)
		case <-renderTick.C:
			// Record signal prediction for active market (if actionable)
			activeIdx, activeCache := state.active()
			activeCache.mu.RLock()
			hist  := activeCache.priceHistory
			mkt   := activeCache.market
			bkUp  := activeCache.bookUp
			activeCache.mu.RUnlock()
			if mkt != nil {
				sig := computeSignals(hist, mkt, bkUp, allConfigs[activeIdx])
				tracker.MaybeRecord(allConfigs[activeIdx].Name, mkt, sig)
			}

			frame := renderFrame(state, tracker)
			fmt.Print("\033[H\033[2J\033[H")
			fmt.Print(frame)
		}
	}
}
