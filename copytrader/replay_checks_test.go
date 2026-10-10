package copytrader

import (
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/require"
	"nofx/store"
	"nofx/trader/types"
	"testing"
	"time"
)

func TestReplayEndToEndDoesNotPersistLiveCallsOrOrders(t *testing.T) {
	encoded, err := json.Marshal(replayOpen())
	require.NoError(t, err)
	e, x, llm, msg := fixtureEngine(t, marketReferenceFixture{Name: "historical-ray", Content: "#RAY 做空 进场：市价2.473 止损：2.617 止盈：2.3658", Response: encoded, MarketPrice: 2}, 1)
	item := e.replayOne(msg)
	require.Equal(t, 1, llm.calls)
	require.Equal(t, VerdictSkip, item.Verdict)
	require.Len(t, item.Evaluations, 1)
	require.Equal(t, "passed", item.Evaluations[0].Source.Status)
	require.Equal(t, "passed", item.Evaluations[0].Parameters.Status)
	require.Equal(t, "failed", item.Evaluations[0].Market.Status)
	require.Equal(t, "ai", item.ProcessingPath)
	stats, err := e.st.CopyTrade().RecognitionStats(e.traderID)
	require.NoError(t, err)
	require.Zero(t, stats.RecognitionRuns)
	require.Zero(t, x.marketEntries)
	require.Empty(t, x.limitEntries)
	signals, err := e.st.CopyTrade().GetRecentSignals(e.traderID, time.Time{}, time.Time{}, 10)
	require.NoError(t, err)
	require.Empty(t, signals)
}

type replayCheckExchange struct {
	*mockExchange
	price    float64
	priceErr error
	rules    *types.ManagedMarketRules
	rulesErr error
}

func (x *replayCheckExchange) GetMarketPrice(string) (float64, error) { return x.price, x.priceErr }
func (x *replayCheckExchange) MarketRules(string) (*types.ManagedMarketRules, error) {
	return x.rules, x.rulesErr
}
func replayOpen() *SourceInterpretation {
	return &SourceInterpretation{Classification: ClassificationSignal, Action: ActionOpen, Symbol: "RAY", Direction: DirectionShort,
		EntryOrders:      []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 2.473}}},
		StopLossLevels:   []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 2.617}}},
		TakeProfitLevels: []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 2.3658}}}}
}
func TestReplayCurrentMarketIsNotInterpretationError(t *testing.T) {
	x := &replayCheckExchange{mockExchange: &mockExchange{}, price: 2, rules: &types.ManagedMarketRules{Status: "TRADING"}}
	e := &Engine{exec: &Executor{ex: x}}
	_, v, _ := e.replayChecks(replayOpen())
	require.Equal(t, VerdictSkip, v, "correct author parameters cannot become INVALID solely because today's price crossed TP")
}

func TestReplayLayeredChecksAndPersistence(t *testing.T) {
	x := &replayCheckExchange{mockExchange: &mockExchange{}, price: 2, rules: &types.ManagedMarketRules{Status: "TRADING"}}
	e := &Engine{exec: &Executor{ex: x}}
	ins := replayOpen()
	ev, v, _ := e.replayChecks(ins)
	require.Equal(t, "passed", ev.Parameters.Status)
	require.Equal(t, "failed", ev.Market.Status)
	require.Equal(t, VerdictSkip, v)
	require.Equal(t, PriceMarket, ins.EntryOrders[0].Price.Type, "replay cannot mutate the interpreted plan")
	x.price = 0
	x.priceErr = errors.New("price unavailable")
	x.rules = &types.ManagedMarketRules{Status: "SETTLING"}
	ev, v, _ = e.replayChecks(ins)
	require.Equal(t, "passed", ev.Parameters.Status)
	require.Equal(t, "unavailable", ev.Market.Status)
	require.Equal(t, "failed", ev.Contract.Status)
	require.Equal(t, "SETTLING", ev.Contract.Detail)
	require.Equal(t, VerdictSkip, v)
	x.rulesErr = errors.New("network failure")
	x.rules = nil
	ev, _, _ = e.replayChecks(ins)
	require.Equal(t, "unavailable", ev.Contract.Status)
	ins.EntryOrders[0].Price.Price = 0
	ev, _, _ = e.replayChecks(ins)
	require.Equal(t, "needs_price", ev.Parameters.Status)
	ins.EntryOrders[0].Price.Price = 2.7 // original SL below short entry: truly invalid parameters
	ev, v, _ = e.replayChecks(ins)
	require.Equal(t, "failed", ev.Parameters.Status)
	require.Equal(t, VerdictInvalid, v)
	item := ReplayItem{Evaluations: []ReplayEvaluation{ev}, ProcessingModel: "test-model", ProcessingPath: "ai"}
	st := newTestStore(t)
	require.NoError(t, st.CopyTrade().CreateReplay(&store.CopyTradeReplay{ID: "checks", TraderID: "trader-1"}))
	row := replayItemToStore("checks", 0, item)
	require.NoError(t, st.GormDB().Create(row).Error)
	r, items, err := st.CopyTrade().GetLatestReplay("trader-1")
	require.NoError(t, err)
	report := replayReportFromStore(r, items)
	require.Equal(t, item.Evaluations, report.Items[0].Evaluations)
	require.Equal(t, "test-model", report.Items[0].ProcessingModel)
	old := replayReportFromStore(r, []*store.CopyTradeReplayItem{{}})
	require.Empty(t, old.Items[0].Evaluations, "do not fabricate missing historical checks")
}

func TestReplayManagementWithoutCurrentQuote(t *testing.T) {
	x := &replayCheckExchange{mockExchange: &mockExchange{}, priceErr: errors.New("missing quote"), rulesErr: errors.New("settling")}
	e := &Engine{exec: &Executor{ex: x}}
	for _, ins := range []*SourceInterpretation{
		{Classification: ClassificationSignal, Action: ActionCancel, Symbol: "RAY"},
		{Classification: ClassificationSignal, Action: ActionReduce, Symbol: "RAY"},
		{Classification: ClassificationSignal, Action: ActionUpdateSL, Symbol: "RAY", StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceEntry}}}},
	} {
		ev, v, _ := e.replayChecks(ins)
		require.Equal(t, VerdictExecute, v)
		require.Equal(t, "not_required", ev.Market.Status)
		require.Equal(t, "not_required", ev.Contract.Status)
	}
}
