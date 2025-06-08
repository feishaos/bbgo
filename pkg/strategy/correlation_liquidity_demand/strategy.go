package correlation_liquidity_demand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/strategy/common"
	"github.com/c9s/bbgo/pkg/types"
	"github.com/sirupsen/logrus"
)

const ID = "correlation-liquidity-demand"

func init() {
	bbgo.RegisterStrategy(ID, &Strategy{})
}

type record struct {
	LiquidityDemand float64    `json:"liquidityDemand"`
	PriceDiff       float64    `json:"priceDiff"`
	Time            types.Time `json:"time"`
}

type Strategy struct {
	*common.Strategy

	Environment *bbgo.Environment
	Market      types.Market

	Symbol   string         `json:"symbol"`
	Interval types.Interval `json:"interval"`
	Window   int            `json:"window"`
	Delay    int            `json:"delay"`

	logger           *logrus.Entry
	records          []record
	kLineCloseBuffer *types.Float64Series

	startTime, endTime *types.Time
}

func (s *Strategy) ID() string {
	return ID
}

func (s *Strategy) InstanceID() string {
	return fmt.Sprintf("%s:%s", ID, s.Symbol)
}

func (s *Strategy) Validate() error {
	if len(s.Symbol) == 0 {
		return errors.New("symbol is required")
	}
	if s.Window < 0 {
		return errors.New("window should not be negative")
	}
	if s.Delay < 0 {
		return errors.New("delay should not be negative")
	}

	return nil
}

func (s *Strategy) Initialize() error {
	if s.Strategy == nil {
		s.Strategy = &common.Strategy{}
	}
	if s.Interval == "" {
		s.Interval = types.Interval1m
	}
	if s.Window == 0 {
		s.Window = 20
	}
	s.logger = logrus.WithField("strategy", s.ID())
	s.kLineCloseBuffer = types.NewFloat64Series()
	return nil
}

func (s *Strategy) Subscribe(session *bbgo.ExchangeSession) {
	session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{Interval: s.Interval})
}

func (s *Strategy) Run(ctx context.Context, _ bbgo.OrderExecutor, session *bbgo.ExchangeSession) error {
	s.Strategy.Initialize(ctx, s.Environment, session, s.Market, ID, s.InstanceID())

	liqDemand := session.Indicators(s.Symbol).LiquidityDemand(
		types.IntervalWindow{
			Interval: s.Interval,
			Window:   s.Window,
		},
	)
	session.MarketDataStream.OnKLineClosed(types.KLineWith(s.Symbol, s.Interval, func(k types.KLine) {
		if s.startTime == nil {
			s.startTime = &k.EndTime
		} else {
			s.endTime = &k.EndTime
		}
		s.kLineCloseBuffer.Push(k.Close.Float64())
		if s.kLineCloseBuffer.Length() > s.Delay {
			ld := liqDemand.Last(s.Delay)
			firstClose := s.kLineCloseBuffer.Last(s.Delay)
			if ld >= 0 {
				s.records = append(s.records, record{
					LiquidityDemand: ld,
					PriceDiff:       s.kLineCloseBuffer.Max(s.Delay) - firstClose,
					Time:            k.EndTime,
				})
			} else {
				s.records = append(s.records, record{
					LiquidityDemand: ld,
					PriceDiff:       s.kLineCloseBuffer.Min(s.Delay) - firstClose,
					Time:            k.EndTime,
				})
			}
		}
	}))

	bbgo.OnShutdown(ctx, func(ctx context.Context, wg *sync.WaitGroup) {
		defer wg.Done()
		// dump records to a file in JSON format
		if len(s.records) > 0 {
			priceDiffs := types.NewFloat64Series()
			liqDemands := types.NewFloat64Series()
			for _, rec := range s.records {
				priceDiffs.Push(rec.PriceDiff)
				liqDemands.Push(rec.LiquidityDemand)
			}
			corr := priceDiffs.Correlation(liqDemands, priceDiffs.Length())
			s.logger.Infof("correlation between price diff and liquidity demand: %f", corr)
			s.logger.Infof("number of records: %d", len(s.records))
			fileName := fmt.Sprintf(
				"%s-%s-%s-%s-%d-records.json",
				s.Symbol,
				s.startTime.Time().Format("2006-01-02"),
				s.endTime.Time().Format("2006-01-02"),
				s.Interval,
				s.Window,
			)
			file, err := os.OpenFile(fileName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				s.logger.WithError(err).Errorf("unable to open file %s for writing", fileName)
				return
			}
			defer file.Close()

			jsonData, err := json.MarshalIndent(s.records, "", "  ")
			if err != nil {
				s.logger.WithError(err).Errorf("unable to marshal records to JSON")
				return
			}
			_, err = file.Write(jsonData)
			if err != nil {
				s.logger.WithError(err).Errorf("unable to write records to file %s", fileName)
				return
			}
		}
	})
	return nil
}
