package dca3

import (
	"context"

	"github.com/c9s/bbgo/pkg/exchange/retry"
	"github.com/c9s/bbgo/pkg/types"
)

func (s *Strategy) SetupPriceTriggerMode(ctx context.Context) {
	s.stateMachine.RegisterTransitionFunc(IdleWaiting, OpenPositionReady, s.openPosition)
	s.stateMachine.RegisterTransitionFunc(OpenPositionReady, OpenPositionOrderFilled, s.readyToFinishOpenPositionStage)
	s.stateMachine.RegisterTransitionFunc(OpenPositionOrderFilled, OpenPositionFinished, s.finishOpenPositionStage)
	s.stateMachine.RegisterTransitionFunc(OpenPositionFinished, TakeProfitReady, s.cancelOpenPositionOrdersAndPlaceTakeProfitOrder)
	s.stateMachine.RegisterTransitionFunc(TakeProfitReady, IdleWaiting, s.finishTakeProfitStage)

	s.OrderExecutor.ActiveMakerOrders().OnFilled(func(o types.Order) {
		s.logger.Infof("FILLED ORDER: %s", o.String())

		switch o.Side {
		case OpenPositionSide:
			s.stateMachine.EmitNextState(OpenPositionOrderFilled)
		case TakeProfitSide:
			s.stateMachine.EmitNextState(IdleWaiting)
		default:
			s.logger.Infof("unsupported side (%s) of order: %s", o.Side, o)
		}

		openOrders, err := retry.QueryOpenOrdersUntilSuccessful(ctx, s.ExchangeSession.Exchange, s.Symbol)
		if err != nil {
			s.logger.WithError(err).Warn("failed to query open orders when order filled")
		}

		// update active orders metrics
		numActiveMakerOrders := s.OrderExecutor.ActiveMakerOrders().NumOfOrders()
		updateNumOfActiveOrdersMetrics(numActiveMakerOrders)

		if len(openOrders) != numActiveMakerOrders {
			s.logger.Warnf("num of open orders (%d) and active orders (%d) is different when order filled, please check it.", len(openOrders), numActiveMakerOrders)
		}

		if err == nil && o.Side == OpenPositionSide && numActiveMakerOrders == 0 && len(openOrders) == 0 {
			s.stateMachine.EmitNextState(OpenPositionFinished)
		}
	})
}
