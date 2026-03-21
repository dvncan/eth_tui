// eth-tui: Live terminal dashboard for Polymarket crypto up/down markets
//
// Shows:
//   - Chainlink oracle price (on-chain, Polygon)
//   - Active market info + countdown to resolution
//   - CLOB order book for Up and Down tokens (side-by-side)
//   - Price history sparkline
//   - Trading signals
//
// Usage:
//   go run main.go
//   go run main.go -slug eth-updown-15m-1774131300   # pin a specific market
//   Keys: [tab]/[n] next market  [p] prev market  [1-8] select directly  [q] quit

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
}

var allConfigs = []AssetConfig{
	{
		Name:          "ETH/USD",
		SlugPrefix:    "eth-updown",
		IntervalSecs:  900,
		IntervalLabel: "15m",
		OracleAddr:    "0xF9680D99D6C9589e2a93a78A04A279e509205945",
	},
	{
		Name:          "ETH/USD",
		SlugPrefix:    "eth-updown",
		IntervalSecs:  3600,
		IntervalLabel: "1h",
		OracleAddr:    "0xF9680D99D6C9589e2a93a78A04A279e509205945",
	},
	{
		Name:          "ETH/USD",
		SlugPrefix:    "eth-updown",
		IntervalSecs:  14400,
		IntervalLabel: "4h",
		OracleAddr:    "0xF9680D99D6C9589e2a93a78A04A279e509205945",
	},
	{
		Name:          "BTC/USD",
		SlugPrefix:    "btc-updown",
		IntervalSecs:  900,
		IntervalLabel: "15m",
		OracleAddr:    "0xc907E116054Ad103354f2D350FD2514433D57F6F",
	},
	{
		Name:          "BTC/USD",
		SlugPrefix:    "btc-updown",
		IntervalSecs:  3600,
		IntervalLabel: "1h",
		OracleAddr:    "0xc907E116054Ad103354f2D350FD2514433D57F6F",
	},
	{
		Name:          "SOL/USD",
		SlugPrefix:    "sol-updown",
		IntervalSecs:  900,
		IntervalLabel: "15m",
		OracleAddr:    "0x10C8264C0935b3B9870013e057f330Ff3e9C56dC",
	},
	{
		Name:          "MATIC/USD",
		SlugPrefix:    "matic-updown",
		IntervalSecs:  900,
		IntervalLabel: "15m",
		OracleAddr:    "0xAB594600376Ec9fD91F8e885dADF0CE036862dE0",
	},
	{
		Name:          "XRP/USD",
		SlugPrefix:    "xrp-updown",
		IntervalSecs:  900,
		IntervalLabel: "15m",
		OracleAddr:    "0x785ba89291f676b5386652eB12b30cF361020694",
	},
}

// ── Constants ────────────────────────────────────────────────────────────────

const (
	polygonRPC         = "https://1rpc.io/matic"
	latestRoundDataSel = "0xfeaf968c"
	gammaBase          = "https://gamma-api.polymarket.com"
	clobBase           = "https://clob.polymarket.com"
	termWidth          = 120
	orderBookRows      = 7
	priceHistoryLen    = 18
)

// ── ANSI helpers ─────────────────────────────────────────────────────────────

const (
	reset      = "\033[0m"
	bold       = "\033[1m"
	dim        = "\033[2m"
	red        = "\033[31m"
	green      = "\033[32m"
	yellow     = "\033[33m"
	cyan       = "\033[36m"
	white      = "\033[37m"
	hideCursor = "\033[?25l"
	showCursor = "\033[?25h"
	clearScr   = "\033[2J\033[H"
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

func rpad(s string, n int) string {
	vis := stripANSI(s)
	diff := n - len([]rune(vis))
	if diff <= 0 {
		return s
	}
	return strings.Repeat(" ", diff) + s
}

// ── State ────────────────────────────────────────────────────────────────────

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

type State struct {
	mu           sync.RWMutex
	activeIdx    int
	chainlink    *PricePoint
	priceHistory []PricePoint
	market       *MarketInfo
	bookUp       *OrderBook
	bookDown     *OrderBook
	lastRender   time.Time
	errors       []string
}

func (s *State) addError(e string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, e)
	if len(s.errors) > 3 {
		s.errors = s.errors[len(s.errors)-3:]
	}
}

func (s *State) switchMarket(idx int) {
	if idx < 0 {
		idx = len(allConfigs) - 1
	}
	if idx >= len(allConfigs) {
		idx = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx == s.activeIdx {
		return
	}
	s.activeIdx = idx
	s.chainlink = nil
	s.priceHistory = nil
	s.market = nil
	s.bookUp = nil
	s.bookDown = nil
	s.errors = nil
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
	resp, err := client.Post(polygonRPC, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var r rpcResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if len(r.Error) > 0 && string(r.Error) != "null" {
		return nil, fmt.Errorf("rpc: %s", r.Error)
	}
	data := strings.TrimPrefix(r.Result, "0x")
	if len(data) < 5*64 {
		return nil, fmt.Errorf("short response")
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
	for i := int64(0); i < 6; i++ {
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
	return "", fmt.Errorf("no active %s %s market found", cfg.Name, cfg.IntervalLabel)
}

func fetchActiveMarket(client *http.Client, cfg AssetConfig, slug string) (*MarketInfo, error) {
	if slug == "" {
		var err error
		slug, err = autoDetectSlug(client, cfg)
		if err != nil {
			return nil, err
		}
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
	if len(prices) > 0 {
		upPrice = parseFloat(prices[0])
	}
	if len(prices) > 1 {
		downPrice = parseFloat(prices[1])
	}
	tokenDown := ""
	if len(tokenIDs) > 1 {
		tokenDown = tokenIDs[1]
	}

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

func normCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

func computeSignals(history []PricePoint, mkt *MarketInfo, bookUp *OrderBook) *Signals {
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
	upCount := 0
	sumChange := 0.0
	for _, c := range changes {
		if c > 0.005 {
			upCount++
		}
		sumChange += c
	}
	sig.MomentumUpCount = upCount
	sig.MomentumTotal = len(changes)
	sig.MomentumAvgDelta = sumChange / float64(len(changes))
	upRatio := float64(upCount) / float64(len(changes))
	switch {
	case upRatio >= 0.65:
		sig.MomentumLabel = "BULLISH"
	case upRatio <= 0.35:
		sig.MomentumLabel = "BEARISH"
	default:
		sig.MomentumLabel = "NEUTRAL"
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
	if remaining < 0 {
		remaining = 0
	}
	sig.IntervalsLeft = remaining.Seconds() / 5.0

	if sig.VolPerInterval > 0 && sig.IntervalsLeft > 0 {
		sig.ZScore = sig.DeltaFromOpen / (sig.VolPerInterval * math.Sqrt(sig.IntervalsLeft))
	} else if sig.DeltaFromOpen != 0 {
		if sig.DeltaFromOpen > 0 {
			sig.ZScore = 10
		} else {
			sig.ZScore = -10
		}
	}

	sig.FairValueUp = normCDF(sig.ZScore)
	sig.OracleEdge = sig.FairValueUp - mkt.UpPrice
	switch {
	case sig.OracleEdge > 0.05:
		sig.OracleEdgeLabel = "BUY UP"
	case sig.OracleEdge < -0.05:
		sig.OracleEdgeLabel = "BUY DOWN"
	default:
		sig.OracleEdgeLabel = "FAIRLY PRICED"
	}

	if bookUp != nil {
		depth := 10
		for _, l := range bookUp.Bids[:min(depth, len(bookUp.Bids))] {
			sig.BookBidTotal += l.Size
		}
		for _, l := range bookUp.Asks[:min(depth, len(bookUp.Asks))] {
			sig.BookAskTotal += l.Size
		}
		if sig.BookAskTotal > 0 {
			sig.BookSkewRatio = sig.BookBidTotal / sig.BookAskTotal
		}
		switch {
		case sig.BookSkewRatio >= 1.5:
			sig.BookSkewLabel = "BUY PRESSURE"
		case sig.BookSkewRatio <= 0.67:
			sig.BookSkewLabel = "SELL PRESSURE"
		default:
			sig.BookSkewLabel = "BALANCED"
		}
	} else {
		sig.BookSkewLabel = "NO DATA"
	}

	momentumScore := float64(upCount)/float64(len(changes))*2 - 1
	edgeScore := math.Max(-1, math.Min(1, sig.OracleEdge*4))
	bookScore := 0.0
	if sig.BookSkewRatio > 0 {
		bookScore = math.Max(-1, math.Min(1, math.Log(sig.BookSkewRatio)))
	}
	sig.CompositeScore = momentumScore*0.25 + edgeScore*0.50 + bookScore*0.25

	absScore := math.Abs(sig.CompositeScore)
	switch {
	case absScore >= 0.5:
		sig.CompositeConf = "HIGH"
	case absScore >= 0.25:
		sig.CompositeConf = "MEDIUM"
	default:
		sig.CompositeConf = "LOW"
	}
	switch {
	case sig.CompositeScore >= 0.15:
		sig.CompositeLabel = "BUY UP"
	case sig.CompositeScore <= -0.15:
		sig.CompositeLabel = "BUY DOWN"
	default:
		sig.CompositeLabel = "NEUTRAL"
	}

	return sig
}

func renderSignals(sig *Signals, W int) []string {
	var lines []string

	solidBar := func(score float64, width int) string {
		mid := width / 2
		filled := int(math.Round(math.Abs(score) * float64(mid)))
		if filled > mid {
			filled = mid
		}
		buf := []rune(strings.Repeat("░", width))
		if score >= 0 {
			for i := mid; i < mid+filled && i < width; i++ {
				buf[i] = '█'
			}
		} else {
			for i := mid - filled; i < mid && i >= 0; i++ {
				buf[i] = '█'
			}
		}
		buf[mid] = '┼'
		return string(buf)
	}

	labelColor := func(label string) string {
		switch label {
		case "BULLISH", "BUY UP", "BUY PRESSURE":
			return color(label, bold+green)
		case "BEARISH", "BUY DOWN", "SELL PRESSURE":
			return color(label, bold+red)
		case "HIGH":
			return color(label, bold+green)
		case "MEDIUM":
			return color(label, yellow)
		case "LOW":
			return color(label, dim)
		default:
			return color(label, dim)
		}
	}

	lines = append(lines, pad(color("  ◆ SIGNALS", bold+cyan), W))

	momArrows := ""
	for i := 0; i < sig.MomentumTotal && i < 10; i++ {
		if i < sig.MomentumUpCount {
			momArrows += color("▲", green)
		} else {
			momArrows += color("▼", red)
		}
	}
	lines = append(lines, pad(fmt.Sprintf("  MOMENTUM   %s  %d/%d up  avg %+.3f$/5s  [%s]  %s",
		momArrows,
		sig.MomentumUpCount, sig.MomentumTotal,
		sig.MomentumAvgDelta,
		solidBar(float64(sig.MomentumUpCount)/float64(max(sig.MomentumTotal, 1))*2-1, 20),
		labelColor(sig.MomentumLabel),
	), W))

	deltaCol := green
	if sig.DeltaFromOpen < 0 {
		deltaCol = red
	}
	fvCol := green
	if sig.FairValueUp < 0.5 {
		fvCol = red
	}
	edgeCol := green
	if sig.OracleEdge < 0 {
		edgeCol = red
	}
	lines = append(lines, pad(fmt.Sprintf("  EDGE        Δ open %s  vol ±%.3f$/5s  Z=%+.2f  FV=%s  edge=%s  → %s",
		color(fmt.Sprintf("%+.2f (%+.2f%%)", sig.DeltaFromOpen, sig.DeltaPct), deltaCol),
		sig.VolPerInterval, sig.ZScore,
		color(fmt.Sprintf("%.3f", sig.FairValueUp), fvCol),
		color(fmt.Sprintf("%+.3f", sig.OracleEdge), edgeCol),
		labelColor(sig.OracleEdgeLabel),
	), W))

	skewCol := dim
	if sig.BookSkewLabel == "BUY PRESSURE" {
		skewCol = green
	} else if sig.BookSkewLabel == "SELL PRESSURE" {
		skewCol = red
	}
	lines = append(lines, pad(fmt.Sprintf("  BOOK SKEW   bid $%.0f / ask $%.0f = %.2fx  [%s]  %s",
		sig.BookBidTotal, sig.BookAskTotal, sig.BookSkewRatio,
		color(solidBar(math.Log(math.Max(sig.BookSkewRatio, 0.01)), 20), skewCol),
		labelColor(sig.BookSkewLabel),
	), W))

	compCol := dim
	switch sig.CompositeLabel {
	case "BUY UP":
		compCol = bold + green
	case "BUY DOWN":
		compCol = bold + red
	}
	lines = append(lines, pad(fmt.Sprintf("  COMPOSITE   [%s]  score %+.3f  → %s  confidence: %s",
		color(solidBar(sig.CompositeScore, 30), compCol),
		sig.CompositeScore,
		color(sig.CompositeLabel, compCol),
		labelColor(sig.CompositeConf),
	), W))

	return lines
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── Render ───────────────────────────────────────────────────────────────────

func bar(size, maxSize float64, width int) string {
	if maxSize <= 0 {
		return strings.Repeat("░", width)
	}
	filled := int(math.Round(float64(width) * size / maxSize))
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func fmtPrice(p float64) string {
	s := fmt.Sprintf("%.2f", p)
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	neg := strings.HasPrefix(intPart, "-")
	if neg {
		intPart = intPart[1:]
	}
	var out []byte
	for i, c := range []byte(intPart) {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	result := "$" + string(out)
	if len(parts) > 1 {
		result += "." + parts[1]
	}
	if neg {
		result = "-" + result
	}
	return result
}
func fmtSize(s float64) string {
	if s >= 1000 {
		return fmt.Sprintf("%8.0f", s)
	}
	return fmt.Sprintf("%8.2f", s)
}
func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 {
		return "EXPIRED"
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm %02ds", m, s)
}

func renderOrderHalf(book *OrderBook, side string, width int) []string {
	const barWidth = 10
	labelW := width - 1
	lines := []string{}

	lines = append(lines, pad(color(fmt.Sprintf("  ORDER BOOK (%s)", side), bold+cyan), labelW))

	if book == nil {
		for i := 0; i < orderBookRows*2+3; i++ {
			lines = append(lines, strings.Repeat(" ", labelW))
		}
		return lines
	}

	displayed := append(book.Asks[:min(len(book.Asks), orderBookRows)],
		book.Bids[:min(len(book.Bids), orderBookRows)]...)
	maxSize := 0.0
	for _, l := range displayed {
		if l.Size > maxSize {
			maxSize = l.Size
		}
	}

	asks := book.Asks
	if len(asks) > orderBookRows {
		asks = asks[:orderBookRows]
	}
	for i := len(asks) - 1; i >= 0; i-- {
		l := asks[i]
		b := bar(l.Size, maxSize, barWidth)
		lines = append(lines, pad(fmt.Sprintf("  %s %s  %s %s",
			color(fmt.Sprintf("%.2f", l.Price), red), fmtSize(l.Size), color(b, red), "ASK",
		), labelW))
	}

	spread := ""
	if len(book.Asks) > 0 && len(book.Bids) > 0 {
		sp := book.Asks[0].Price - book.Bids[0].Price
		mid := (book.Asks[0].Price + book.Bids[0].Price) / 2
		spread = fmt.Sprintf("  ─────── mid: %.3f  spread: %.3f ───────", mid, sp)
	} else {
		spread = "  ─────────────────────────────────────"
	}
	lines = append(lines, pad(color(spread, dim), labelW))

	bids := book.Bids
	if len(bids) > orderBookRows {
		bids = bids[:orderBookRows]
	}
	for _, l := range bids {
		b := bar(l.Size, maxSize, barWidth)
		lines = append(lines, pad(fmt.Sprintf("  %s %s  %s %s",
			color(fmt.Sprintf("%.2f", l.Price), green), fmtSize(l.Size), color(b, green), "BID",
		), labelW))
	}

	total := orderBookRows*2 + 3
	for len(lines) < total {
		lines = append(lines, strings.Repeat(" ", labelW))
	}
	return lines
}

func renderTabs(activeIdx int, W int) string {
	var sb strings.Builder
	sb.WriteString("  ")
	for i, cfg := range allConfigs {
		label := fmt.Sprintf("[%d] %s %s", i+1, cfg.Name, cfg.IntervalLabel)
		if i == activeIdx {
			sb.WriteString(color(" "+label+" ", bold+cyan))
		} else {
			sb.WriteString(color(" "+label+" ", dim))
		}
		if i < len(allConfigs)-1 {
			sb.WriteString(color("│", dim))
		}
	}
	sb.WriteString(color("  tab/n=next  p=prev  q=quit", dim))
	vis := stripANSI(sb.String())
	sp := W - 2 - len([]rune(vis))
	if sp < 0 {
		sp = 0
	}
	return sb.String() + strings.Repeat(" ", sp)
}

func renderFrame(s *State) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	W := termWidth
	half := (W - 1) / 2
	var b strings.Builder

	write := func(line string) {
		vis := stripANSI(line)
		if len([]rune(vis)) > W {
			line = line[:W]
		}
		b.WriteString(line + "\n")
	}
	box := func(w int) string { return strings.Repeat("─", w) }

	now := time.Now().UTC()
	cfg := allConfigs[s.activeIdx]

	// ── Title + tabs ─────────────────────────────────────────────────────────
	title := color(fmt.Sprintf("  POLYMARKET %s %s ORACLE MONITOR", cfg.Name, cfg.IntervalLabel), bold+cyan)
	ts := color(now.Format("15:04:05")+" UTC", dim)
	titleLine := pad(title, W-15) + ts
	write("┌" + box(W-2) + "┐")
	write("│" + pad(titleLine, W-2) + "│")
	write("├" + box(W-2) + "┤")
	write("│" + renderTabs(s.activeIdx, W) + "│")
	write("├" + box(half-1) + "┬" + box(W-half-2) + "┤")

	// ── Left: Oracle  │  Right: Market ──────────────────────────────────────
	cl := s.chainlink
	mkt := s.market

	leftLines := make([]string, 0, 8)
	if cl != nil {
		leftLines = append(leftLines, color(fmt.Sprintf("  %s", fmtPrice(cl.Price)), bold+yellow))

		if len(s.priceHistory) >= 2 {
			prev := s.priceHistory[len(s.priceHistory)-2].Price
			diff := cl.Price - prev
			pct := diff / prev * 100
			arrow, col := "▲", green
			if diff < 0 {
				arrow, col = "▼", red
			} else if diff == 0 {
				arrow, col = "━", dim
			}
			leftLines = append(leftLines, color(fmt.Sprintf("  %s %+.4f (%+.2f%%)", arrow, diff, pct), col))
		} else {
			leftLines = append(leftLines, "")
		}
		age := now.Unix() - cl.UpdatedAt
		leftLines = append(leftLines, color(fmt.Sprintf("  Round  %-10s", cl.RoundID[len(cl.RoundID)-8:]), dim))
		leftLines = append(leftLines, color(fmt.Sprintf("  Updated %ds ago", age), dim))
		leftLines = append(leftLines, color(fmt.Sprintf("  Oracle  %s...%s", cfg.OracleAddr[:6], cfg.OracleAddr[len(cfg.OracleAddr)-4:]), dim))
		leftLines = append(leftLines, "")

		if len(s.priceHistory) > 0 {
			sparks := "  "
			pmin, pmax := s.priceHistory[0].Price, s.priceHistory[0].Price
			for _, p := range s.priceHistory {
				if p.Price < pmin {
					pmin = p.Price
				}
				if p.Price > pmax {
					pmax = p.Price
				}
			}
			rng := pmax - pmin
			chars := []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
			for _, p := range s.priceHistory {
				idx := 0
				if rng > 0 {
					idx = int((p.Price-pmin)/rng*7)
				}
				sparks += chars[idx]
			}
			leftLines = append(leftLines, color(sparks, cyan))
		}
	} else {
		leftLines = append(leftLines, color(fmt.Sprintf("  Fetching %s oracle...", cfg.Name), dim))
	}

	rightLines := make([]string, 0, 8)
	if mkt != nil {
		rightLines = append(rightLines, color("  "+mkt.Slug, bold))
		rightLines = append(rightLines, color(fmt.Sprintf("  Window  %s → %s",
			mkt.StartDate.Format("15:04"), mkt.EndDate.Format("15:04 UTC")), dim))
		remaining := mkt.EndDate.Sub(now)
		timeColor := green
		if remaining < 2*time.Minute {
			timeColor = red
		} else if remaining < 5*time.Minute {
			timeColor = yellow
		}
		rightLines = append(rightLines, color(fmt.Sprintf("  ⏱  %s remaining", fmtDuration(remaining)), timeColor))

		if mkt.OpeningPrice > 0 && cl != nil {
			delta := cl.Price - mkt.OpeningPrice
			pct := delta / mkt.OpeningPrice * 100
			dir, col := "UP ↑", green
			if delta < 0 {
				dir, col = "DOWN ↓", red
			}
			rightLines = append(rightLines, fmt.Sprintf("  Open %s  Now %s  %s",
				color(fmtPrice(mkt.OpeningPrice), dim),
				color(fmtPrice(cl.Price), bold),
				color(fmt.Sprintf("%+.2f (%+.2f%%) %s", delta, pct, dir), col)))
		} else {
			rightLines = append(rightLines, color("  Waiting for opening price...", dim))
		}
		rightLines = append(rightLines, "")

		upC, downC := white, white
		if mkt.UpPrice > mkt.DownPrice {
			upC = green
		} else {
			downC = green
		}
		rightLines = append(rightLines, fmt.Sprintf("  Up: %s   Down: %s   Spread: %.3f",
			color(fmt.Sprintf("%.3f", mkt.UpPrice), upC),
			color(fmt.Sprintf("%.3f", mkt.DownPrice), downC),
			mkt.BestAsk-mkt.BestBid,
		))
		rightLines = append(rightLines, color(fmt.Sprintf("  Best Bid: %.2f   Best Ask: %.2f", mkt.BestBid, mkt.BestAsk), dim))
	} else {
		rightLines = append(rightLines, color(fmt.Sprintf("  Fetching %s %s market...", cfg.Name, cfg.IntervalLabel), dim))
	}

	rows := 8
	for i := 0; i < rows; i++ {
		l, r := "", ""
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if i < len(rightLines) {
			r = rightLines[i]
		}
		lv := stripANSI(l)
		rv := stripANSI(r)
		lpad := half - 2 - len([]rune(lv))
		rpad2 := W - half - 2 - len([]rune(rv))
		if lpad < 0 {
			lpad = 0
		}
		if rpad2 < 0 {
			rpad2 = 0
		}
		write("│" + l + strings.Repeat(" ", lpad) + " │ " + r + strings.Repeat(" ", rpad2) + "│")
	}

	// ── Order books ──────────────────────────────────────────────────────────
	write("├" + box(half-1) + "┼" + box(W-half-2) + "┤")
	upLines := renderOrderHalf(s.bookUp, "UP", half-1)
	downLines := renderOrderHalf(s.bookDown, "DOWN", W-half-2)
	maxOB := max(len(upLines), len(downLines))
	for i := 0; i < maxOB; i++ {
		l, r := "", ""
		if i < len(upLines) {
			l = upLines[i]
		}
		if i < len(downLines) {
			r = downLines[i]
		}
		lv, rv := stripANSI(l), stripANSI(r)
		lp := half - 1 - len([]rune(lv))
		rp := W - half - 2 - len([]rune(rv))
		if lp < 0 {
			lp = 0
		}
		if rp < 0 {
			rp = 0
		}
		write("│" + l + strings.Repeat(" ", lp) + "│" + r + strings.Repeat(" ", rp) + "│")
	}

	// ── Signals ──────────────────────────────────────────────────────────────
	write("├" + box(W-2) + "┤")
	for _, sigLine := range renderSignals(computeSignals(s.priceHistory, s.market, s.bookUp), W-2) {
		vis := stripANSI(sigLine)
		sp := W - 2 - len([]rune(vis))
		if sp < 0 {
			sp = 0
		}
		write("│" + sigLine + strings.Repeat(" ", sp) + "│")
	}

	// ── Price history ────────────────────────────────────────────────────────
	write("├" + box(W-2) + "┤")
	histLine := "  "
	if len(s.priceHistory) > 0 {
		prev := s.priceHistory[0].Price
		for i, p := range s.priceHistory {
			diff := p.Price - prev
			col := dim
			arrow := "━"
			if diff > 0.01 {
				col, arrow = green, "▲"
			} else if diff < -0.01 {
				col, arrow = red, "▼"
			}
			if i == 0 {
				col, arrow = cyan, "●"
			}
			histLine += color(fmt.Sprintf("%s%.2f ", arrow, p.Price), col)
			prev = p.Price
		}
	} else {
		histLine += color("Collecting price history...", dim)
	}
	vis := stripANSI(histLine)
	hp := W - 2 - len([]rune(vis))
	if hp < 0 {
		hp = 0
	}
	write("│" + histLine + strings.Repeat(" ", hp) + "│")

	// ── Status bar ───────────────────────────────────────────────────────────
	write("├" + box(W-2) + "┤")
	statusMsg := ""
	if len(s.errors) > 0 {
		statusMsg = "  " + color("⚠ "+s.errors[len(s.errors)-1], red)
	} else {
		statusMsg = "  " + color("● Live", green) + color(fmt.Sprintf("  Oracle: Chainlink %s (Polygon)  CLOB: clob.polymarket.com  %s", cfg.Name, now.Format("15:04:05")), dim)
	}
	vis2 := stripANSI(statusMsg)
	sp2 := W - 2 - len([]rune(vis2))
	if sp2 < 0 {
		sp2 = 0
	}
	write("│" + statusMsg + strings.Repeat(" ", sp2) + "│")
	write("└" + box(W-2) + "┘")

	return b.String()
}

// ── Goroutines ────────────────────────────────────────────────────────────────

func pollChainlink(state *State, client *http.Client, interval time.Duration) {
	lastIdx := -1
	doFetch := func() {
		state.mu.RLock()
		idx := state.activeIdx
		state.mu.RUnlock()

		cfg := allConfigs[idx]
		p, err := fetchChainlink(client, cfg.OracleAddr)
		if err != nil {
			state.addError("oracle: " + err.Error())
			return
		}
		state.mu.Lock()
		if state.activeIdx != idx {
			state.mu.Unlock()
			return
		}
		state.chainlink = p
		state.priceHistory = append(state.priceHistory, *p)
		if len(state.priceHistory) > priceHistoryLen {
			state.priceHistory = state.priceHistory[len(state.priceHistory)-priceHistoryLen:]
		}
		if state.market != nil && state.market.OpeningPrice == 0 {
			if time.Now().UTC().After(state.market.StartDate) {
				state.market.OpeningPrice = p.Price
			}
		}
		lastIdx = idx
		state.mu.Unlock()
	}

	doFetch()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for range tick.C {
		state.mu.RLock()
		idx := state.activeIdx
		state.mu.RUnlock()
		if idx != lastIdx {
			// config changed; fetch immediately on next tick (already ticking)
		}
		doFetch()
	}
}

func pollMarket(state *State, client *http.Client, slugFlag string, interval time.Duration) {
	doFetch := func() {
		state.mu.RLock()
		idx := state.activeIdx
		state.mu.RUnlock()

		cfg := allConfigs[idx]
		slug := ""
		if slugFlag != "" && idx == 0 {
			slug = slugFlag
		}
		m, err := fetchActiveMarket(client, cfg, slug)
		if err != nil {
			state.addError("market: " + err.Error())
			return
		}
		state.mu.Lock()
		if state.activeIdx != idx {
			state.mu.Unlock()
			return
		}
		prevSlug := ""
		if state.market != nil {
			prevSlug = state.market.Slug
			m.OpeningPrice = state.market.OpeningPrice
		}
		if prevSlug != m.Slug {
			m.OpeningPrice = 0
		}
		state.market = m
		state.mu.Unlock()
	}

	doFetch()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for range tick.C {
		doFetch()
	}
}

func pollBooks(state *State, client *http.Client, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for range tick.C {
		state.mu.RLock()
		mkt := state.market
		idx := state.activeIdx
		state.mu.RUnlock()

		if mkt == nil || mkt.TokenIDUp == "" {
			continue
		}

		upBook, err := fetchBook(client, mkt.TokenIDUp)
		if err != nil {
			state.addError("book(up): " + err.Error())
		}
		downBook, err2 := fetchBook(client, mkt.TokenIDDown)
		if err2 != nil {
			state.addError("book(down): " + err2.Error())
		}

		state.mu.Lock()
		if state.activeIdx == idx {
			if upBook != nil {
				state.bookUp = upBook
			}
			if downBook != nil {
				state.bookDown = downBook
			}
		}
		state.mu.Unlock()
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	slugFlag := flag.String("slug", "", "Pin a specific market slug (default: auto-detect; only applies to ETH/USD 15m)")
	flag.Parse()

	client := &http.Client{Timeout: 10 * time.Second}
	state := &State{}

	fmt.Print(hideCursor)
	fmt.Print(clearScr)
	fmt.Println(color("  Initializing...", dim))

	go pollChainlink(state, client, 5*time.Second)
	go pollMarket(state, client, *slugFlag, 10*time.Second)
	go pollBooks(state, client, 5*time.Second)

	renderTick := time.NewTicker(500 * time.Millisecond)
	defer renderTick.Stop()

	osig := make(chan os.Signal, 1)
	signal.Notify(osig, syscall.SIGINT, syscall.SIGTERM)

	quit := make(chan struct{})
	switchCh := make(chan int, 4)

	go func() {
		buf := make([]byte, 1)
		for {
			n, _ := os.Stdin.Read(buf)
			if n == 0 {
				continue
			}
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
				idx := int(ch - '1')
				if idx < len(allConfigs) {
					switchCh <- 100 + idx
				}
			}
		}
	}()

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
				if next < 0 {
					next = len(allConfigs) - 1
				}
			}
			state.switchMarket(next)
		}
	}()

	for {
		select {
		case <-osig:
			fmt.Print(showCursor)
			fmt.Print("\033[2J\033[H")
			fmt.Println("Exiting.")
			os.Exit(0)
		case <-quit:
			fmt.Print(showCursor)
			fmt.Print("\033[2J\033[H")
			os.Exit(0)
		case <-renderTick.C:
			frame := renderFrame(state)
			fmt.Print("\033[H")
			fmt.Print(frame)
		}
	}
}
