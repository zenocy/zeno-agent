package projection

import "time"

// CalendarEvent is one row in TodaysCalendar.
//
// Attendees and LastModified are V2.3.0 P3 additions. Attendees holds the
// human-readable names (CN= when available, falling back to the local-part
// of the mailto: address). LastModified is the LAST-MODIFIED property from
// the source VEVENT, in UTC; zero when the calendar omitted it. Both feed
// the inject detector's calendar-move path.
type CalendarEvent struct {
	UID          string    `json:"uid"`
	Title        string    `json:"title,omitempty"`
	Location     string    `json:"location,omitempty"`
	Tag          string    `json:"tag,omitempty"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	Attendees    []string  `json:"attendees,omitempty"`
	LastModified time.Time `json:"last_modified,omitzero"`
}

// Thread is one row in OpenEmailThreads.
type Thread struct {
	Subject      string    `json:"subject"`
	LastSender   string    `json:"last_sender"`
	LastReceived time.Time `json:"last_received"`
	MessageCount int       `json:"message_count"`
	UnreadCount  int       `json:"unread_count"`
}

// Window is the result of RunWindow.
type Window struct {
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Condition string    `json:"condition"`
}

// WeatherView is the result of the Weather projection — a compact, UI-shaped
// view of the latest weather snapshot. Nil from Compute means no fresh data.
type WeatherView struct {
	CapturedAt time.Time          `json:"captured_at"`
	Timezone   string             `json:"timezone"`
	Location   string             `json:"location,omitempty"`
	Current    WeatherCurrent     `json:"current"`
	Hourly     []WeatherHourPoint `json:"hourly"`
	NowIndex   int                `json:"now_index"`
	Daily      []WeatherDayPoint  `json:"daily,omitempty"`
}

// WeatherCurrent is "right now" — temperature, the WMO label and wind in km/h.
type WeatherCurrent struct {
	Time     time.Time `json:"time"`
	TempC    float64   `json:"temp_c"`
	Label    string    `json:"label"`
	WindKmh  float64   `json:"wind_kmh"`
	PrecipMM float64   `json:"precip_mm"`
}

// WeatherHourPoint is one hour of the day's temperature curve.
type WeatherHourPoint struct {
	Time  time.Time `json:"time"`
	TempC float64   `json:"temp_c"`
}

// WeatherDayPoint is one day in the multi-day forecast surfaced beneath
// the hourly graph in the right-rail widget.
type WeatherDayPoint struct {
	Date     time.Time `json:"date"`
	TempMaxC float64   `json:"temp_max_c"`
	TempMinC float64   `json:"temp_min_c"`
	Label    string    `json:"label,omitempty"`
	Code     int       `json:"code"`
}

// StockView is the result of the Stock projection — the list of latest
// quotes for every configured ticker, in the order the user listed
// them. Nil from Compute means no tickers are configured (the widget
// renders an empty / disabled state).
type StockView struct {
	AsOf   time.Time    `json:"as_of"`
	Quotes []StockQuote `json:"quotes"`
}

// StockQuote is one row in StockView — the latest snapshot for one
// ticker, with the day-change percent precomputed for display.
//
// Phase 4 added Open / DayHigh / DayLow / Volume / Post* + Series.
// The widget renders Series as an intraday sparkline; older snapshots
// missing those fields just stay at zero (the omitempty tags keep the
// JSON tight).
type StockQuote struct {
	Ticker      string      `json:"ticker"`
	Price       float64     `json:"price"`
	PrevClose   float64     `json:"prev_close"`
	Currency    string      `json:"currency,omitempty"`
	ChangePct   float64     `json:"change_pct"`
	AsOf        time.Time   `json:"as_of"`
	Stale       bool        `json:"stale,omitempty"` // true when AsOf is older than the staleness window
	Open        float64     `json:"open,omitempty"`
	DayHigh     float64     `json:"day_high,omitempty"`
	DayLow      float64     `json:"day_low,omitempty"`
	Volume      int64       `json:"volume,omitempty"`
	PostPrice   float64     `json:"post_price,omitempty"`
	PostChange  float64     `json:"post_change_pct,omitempty"`
	MarketState string      `json:"market_state,omitempty"`
	Series      []StockTick `json:"series,omitempty"`
}

// StockTick is one point on the intraday sparkline. The widget reads
// at most ~60 of these per ticker per render.
type StockTick struct {
	AsOf  time.Time `json:"as_of"`
	Price float64   `json:"price"`
}

// StockBreach is one stock.alert event surfaced through the
// RecentStockBreaches projection — the inject detector's stock_breach
// path consumes a list of these to decide whether to fire a reactive
// card.
type StockBreach struct {
	Ticker       string    `json:"ticker"`
	Price        float64   `json:"price"`
	PrevClose    float64   `json:"prev_close"`
	ChangePct    float64   `json:"change_pct"`
	ThresholdPct float64   `json:"threshold_pct"`
	Currency     string    `json:"currency,omitempty"`
	AsOf         time.Time `json:"as_of"`
	EvidenceID   string    `json:"evidence_id"` // event-log ID; the inject signal carries this as the durable handle
}
