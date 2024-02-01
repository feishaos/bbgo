package atrpin

import (
	"context"
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/strategy/common"
	"github.com/c9s/bbgo/pkg/strategy/xfixedmaker"
	"github.com/c9s/bbgo/pkg/types"
)

const ID = "xatrpin"

func init() {
	bbgo.RegisterStrategy(ID, &Strategy{})
}

type Strategy struct {
	*common.Strategy

	Environment *bbgo.Environment

	TradingExchange string           `json:"tradingExchange"`
	Symbol          string           `json:"symbol"`
	Interval        types.Interval   `json:"interval"`
	Window          int              `json:"window"`
	Multiplier      float64          `json:"multiplier"`
	MinPriceRange   fixedpoint.Value `json:"minPriceRange"`

	ReferenceExchange       string               `json:"referenceExchange"`
	ReferencePriceEMA       types.IntervalWindow `json:"referencePriceEMA"`
	OrderPriceLossThreshold fixedpoint.Value     `json:"orderPriceLossThreshold"`

	bbgo.QuantityOrAmount

	market                types.Market
	orderPriceRiskControl *xfixedmaker.OrderPriceRiskControl
}

func (s *Strategy) Initialize() error {
	s.Strategy = &common.Strategy{}
	return nil
}

func (s *Strategy) ID() string {
	return ID
}

func (s *Strategy) InstanceID() string {
	return fmt.Sprintf("%s:%s:%s:%d", ID, s.Symbol, s.Interval, s.Window)
}

func (s *Strategy) CrossSubscribe(sessions map[string]*bbgo.ExchangeSession) {
	tradingSession, ok := sessions[s.TradingExchange]
	if !ok {
		log.Errorf("trading session %s is not found", s.TradingExchange)
	}
	tradingSession.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{Interval: s.Interval})

	referenceSession, ok := sessions[s.ReferenceExchange]
	if !ok {
		log.Errorf("reference session %s is not found", s.ReferenceExchange)
	}
	referenceSession.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{Interval: s.ReferencePriceEMA.Interval})
}

func (s *Strategy) Defaults() error {
	if s.Multiplier == 0.0 {
		s.Multiplier = 10.0
	}

	if s.Interval == "" {
		s.Interval = types.Interval5m
	}

	return nil
}

func (s *Strategy) CrossRun(ctx context.Context, _ bbgo.OrderExecutionRouter, sessions map[string]*bbgo.ExchangeSession) error {
	tradingSession, ok := sessions[s.TradingExchange]
	if !ok {
		return fmt.Errorf("trading session %s is not defined", s.TradingExchange)
	}

	referenceSession, ok := sessions[s.ReferenceExchange]
	if !ok {
		return fmt.Errorf("reference session %s is not defined", s.ReferenceExchange)
	}

	market, ok := tradingSession.Market(s.Symbol)
	if !ok {
		return fmt.Errorf("market %s not found", s.Symbol)
	}
	s.market = market

	s.Strategy.Initialize(ctx, s.Environment, tradingSession, s.market, ID, s.InstanceID())

	s.orderPriceRiskControl = xfixedmaker.NewOrderPriceRiskControl(
		referenceSession.Indicators(s.Symbol).EMA(s.ReferencePriceEMA),
		s.OrderPriceLossThreshold,
	)

	atr := tradingSession.Indicators(s.Symbol).ATR(s.Interval, s.Window)

	tradingSession.MarketDataStream.OnKLineClosed(types.KLineWith(s.Symbol, s.Interval, func(k types.KLine) {
		if err := s.Strategy.OrderExecutor.GracefulCancel(ctx); err != nil {
			log.WithError(err).Error("unable to cancel open orders...")
		}

		account, err := tradingSession.UpdateAccount(ctx)
		if err != nil {
			log.WithError(err).Error("unable to update account")
			return
		}

		baseBalance, _ := account.Balance(s.market.BaseCurrency)
		quoteBalance, _ := account.Balance(s.market.QuoteCurrency)

		lastAtr := atr.Last(0)
		log.Infof("atr: %f", lastAtr)

		// protection
		if lastAtr <= k.High.Sub(k.Low).Float64() {
			lastAtr = k.High.Sub(k.Low).Float64()
		}

		priceRange := fixedpoint.NewFromFloat(lastAtr * s.Multiplier)

		// if the atr is too small, apply the price range protection with 10%
		// priceRange protection 10%
		priceRange = fixedpoint.Max(priceRange, k.Close.Mul(s.MinPriceRange))
		log.Infof("priceRange: %f", priceRange.Float64())

		ticker, err := tradingSession.Exchange.QueryTicker(ctx, s.Symbol)
		if err != nil {
			log.WithError(err).Error("unable to query ticker")
			return
		}

		log.Info(ticker.String())

		bidPrice := fixedpoint.Max(ticker.Buy.Sub(priceRange), s.market.TickSize)
		askPrice := ticker.Sell.Add(priceRange)

		bidQuantity := s.QuantityOrAmount.CalculateQuantity(bidPrice)
		askQuantity := s.QuantityOrAmount.CalculateQuantity(askPrice)

		var orderForms []types.SubmitOrder

		position := s.Strategy.OrderExecutor.Position()
		if !position.IsDust() {
			log.Infof("position: %+v", position)

			side := types.SideTypeSell
			takerPrice := fixedpoint.Zero

			if position.IsShort() {
				side = types.SideTypeBuy
				takerPrice = ticker.Sell
			} else if position.IsLong() {
				side = types.SideTypeSell
				takerPrice = ticker.Buy
			}

			positionQuantity := position.GetQuantity()
			if !s.orderPriceRiskControl.IsSafe(side, takerPrice, positionQuantity) {
				return
			}

			orderForms = append(orderForms, types.SubmitOrder{
				Symbol:      s.Symbol,
				Type:        types.OrderTypeLimit,
				Side:        side,
				Price:       takerPrice,
				Quantity:    positionQuantity,
				Market:      s.market,
				TimeInForce: types.TimeInForceGTC,
				Tag:         "takeProfit",
			})

			log.Infof("SUBMIT TAKER ORDER: %+v", orderForms)

			if _, err := s.Strategy.OrderExecutor.SubmitOrders(ctx, orderForms...); err != nil {
				log.WithError(err).Error("unable to submit orders")
			}

			return
		}

		askQuantity = s.market.AdjustQuantityByMinNotional(askQuantity, askPrice)
		if !s.market.IsDustQuantity(askQuantity, askPrice) && askQuantity.Compare(baseBalance.Available) < 0 {
			orderForms = append(orderForms, types.SubmitOrder{
				Symbol:      s.Symbol,
				Side:        types.SideTypeSell,
				Type:        types.OrderTypeLimitMaker,
				Quantity:    askQuantity,
				Price:       askPrice,
				Market:      s.market,
				TimeInForce: types.TimeInForceGTC,
				Tag:         "pinOrder",
			})
		}

		bidQuantity = s.market.AdjustQuantityByMinNotional(bidQuantity, bidPrice)
		if !s.market.IsDustQuantity(bidQuantity, bidPrice) && bidQuantity.Mul(bidPrice).Compare(quoteBalance.Available) < 0 {
			orderForms = append(orderForms, types.SubmitOrder{
				Symbol:   s.Symbol,
				Side:     types.SideTypeBuy,
				Type:     types.OrderTypeLimitMaker,
				Price:    bidPrice,
				Quantity: bidQuantity,
				Market:   s.market,
				Tag:      "pinOrder",
			})
		}

		if len(orderForms) == 0 {
			log.Infof("no order to place")
			return
		}

		log.Infof("bid/ask: %f/%f", bidPrice.Float64(), askPrice.Float64())

		if _, err := s.Strategy.OrderExecutor.SubmitOrders(ctx, orderForms...); err != nil {
			log.WithError(err).Error("unable to submit orders")
		}
	}))

	bbgo.OnShutdown(ctx, func(ctx context.Context, wg *sync.WaitGroup) {
		defer wg.Done()

		if err := s.Strategy.OrderExecutor.GracefulCancel(ctx); err != nil {
			log.WithError(err).Error("unable to cancel open orders...")
		}
	})

	return nil
}
