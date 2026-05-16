package main

import (
	"context"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	zlog "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/settings"
)

// startProjectionPublisher subscribes to the eventbus, watches for
// SensorEventObservedEvent kinds we map to UI projections, and republishes
// the corresponding high-level event (WeatherUpdatedEvent / StockUpdatedEvent
// / CalendarTodayChangedEvent) carrying the full projection payload so the
// SSE client can drop it straight into its React Query cache via
// setQueryData.
//
// The subscriber lives in cmd/zeno (not internal/projection or
// internal/eventbus) to avoid a projection ↔ eventbus import cycle. It runs
// for the lifetime of ctx; cancel ctx to stop it.
//
// Drop semantics: the bus.Subscribe channel is bounded; if a snapshot burst
// fills the buffer, sensor events get dropped. That's acceptable here — we
// only care about the *latest* state, so a coalesced view is fine. The
// projection re-reads the durable log every time it runs, so the next
// publish naturally catches up.
func startProjectionPublisher(
	ctx context.Context,
	bus *eventbus.Bus,
	reader zlog.Reader,
	settingsSvc *settings.Service,
	log *logrus.Entry,
) {
	if bus == nil || reader == nil {
		return
	}
	sub := bus.Subscribe()
	go func() {
		defer bus.Unsubscribe(sub)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				obs, ok := ev.(eventbus.SensorEventObservedEvent)
				if !ok {
					continue
				}
				switch obs.Kind_ {
				case zlog.KindWeatherSnapshot:
					publishWeather(ctx, bus, reader, settingsSvc, log)
				case zlog.KindStockSnapshot:
					publishStock(ctx, bus, reader, settingsSvc, log)
				case zlog.KindCalListSnapshot, zlog.KindCalEventChanged, zlog.KindCalEventSeen:
					publishCalendar(ctx, bus, reader, settingsSvc, log)
				}
			}
		}
	}()
}

func publishWeather(
	ctx context.Context,
	bus *eventbus.Bus,
	reader zlog.Reader,
	settingsSvc *settings.Service,
	log *logrus.Entry,
) {
	view, err := (projection.Weather{Cfg: projectionCfg(settingsSvc)}).Compute(ctx, reader)
	if err != nil {
		if log != nil {
			log.WithError(err).Debug("projection-publisher: weather compute failed")
		}
		return
	}
	bus.Publish(eventbus.WeatherUpdatedEvent{Weather: view})
}

func publishStock(
	ctx context.Context,
	bus *eventbus.Bus,
	reader zlog.Reader,
	settingsSvc *settings.Service,
	log *logrus.Entry,
) {
	view, err := (projection.Stock{Cfg: projectionCfg(settingsSvc), Tickers: settingsSvc}).Compute(ctx, reader)
	if err != nil {
		if log != nil {
			log.WithError(err).Debug("projection-publisher: stock compute failed")
		}
		return
	}
	bus.Publish(eventbus.StockUpdatedEvent{Stock: view})
}

func publishCalendar(
	ctx context.Context,
	bus *eventbus.Bus,
	reader zlog.Reader,
	settingsSvc *settings.Service,
	log *logrus.Entry,
) {
	cfg := projectionCfg(settingsSvc)

	today, err := (projection.TodaysCalendar{Cfg: cfg}).Compute(ctx, reader)
	if err != nil {
		if log != nil {
			log.WithError(err).Debug("projection-publisher: calendar compute failed")
		}
		return
	}
	bus.Publish(eventbus.CalendarTodayChangedEvent{Events: today})

	tomorrow, err := (projection.TomorrowsCalendar{Cfg: cfg}).Compute(ctx, reader)
	if err != nil {
		if log != nil {
			log.WithError(err).Debug("projection-publisher: tomorrow calendar compute failed")
		}
	} else {
		bus.Publish(eventbus.CalendarTomorrowChangedEvent{Events: tomorrow})
	}

	week, err := (projection.WeekCalendar{Cfg: cfg}).Compute(ctx, reader)
	if err != nil {
		if log != nil {
			log.WithError(err).Debug("projection-publisher: week calendar compute failed")
		}
	} else {
		bus.Publish(eventbus.CalendarWeekChangedEvent{Events: week})
	}
}

// projectionCfg builds the per-call projection.Config from the live
// settings service so a TZ change in the Settings UI takes effect on the
// next publish without restart.
func projectionCfg(settingsSvc *settings.Service) projection.Config {
	return projection.Config{
		TZ: settingsSvc.TZ(),
	}
}
