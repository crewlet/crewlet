package growth

import "slices"

// Metric is one figure the growth domain keeps per account and day: closed,
// appended only, one metric per meaning and never one name for two (package
// doc, "Metrics").
type Metric string

// Scale is how a metric's int64 value is counted.
type Scale string

// The scales.
const (
	// ScaleMoneyMicros is money in millionths of the row's currency.
	ScaleMoneyMicros Scale = "money_micros"
	// ScaleCount is a whole count.
	ScaleCount Scale = "count"
	// ScalePPM is millionths: a fractional figure, an average or a ratio
	// times 1e6.
	ScalePPM Scale = "ppm"
)

// Aggregate is how a metric's daily values combine over days or accounts.
type Aggregate string

// The aggregations.
const (
	// AggregateSum adds the values.
	AggregateSum Aggregate = "sum"
	// AggregateWeighted averages the values weighted by another metric's.
	AggregateWeighted Aggregate = "weighted"
	// AggregateLatest takes the newest day's value: a figure that does not
	// add across days.
	AggregateLatest Aggregate = "latest"
)

// MetricSpec is how one metric is counted and combined.
type MetricSpec struct {
	Metric    Metric
	Scale     Scale
	Aggregate Aggregate
	// WeightedBy is the metric a weighted average weighs by.
	WeightedBy Metric
}

// The metrics. An advertiser's figures (ad_*) are never a publisher's
// (ad_earnings, app_ad_*), and a store's written-review average is never its
// overall rating.
const (
	AdSpend           Metric = "ad_spend"
	AdImpressions     Metric = "ad_impressions"
	AdClicks          Metric = "ad_clicks"
	AdConversions     Metric = "ad_conversions"
	AdConversionValue Metric = "ad_conversion_value"
	AdInstalls        Metric = "ad_installs"

	AdEarnings          Metric = "ad_earnings"
	AdPageViews         Metric = "ad_page_views"
	AdImpressionsServed Metric = "ad_impressions_served"

	AppAdEarnings    Metric = "app_ad_earnings"
	AppAdRequests    Metric = "app_ad_requests"
	AppAdImpressions Metric = "app_ad_impressions"

	Sessions        Metric = "sessions"
	ActiveUsers     Metric = "active_users"
	NewUsers        Metric = "new_users"
	KeyEvents       Metric = "key_events"
	PurchaseRevenue Metric = "purchase_revenue"

	SearchClicks      Metric = "search_clicks"
	SearchImpressions Metric = "search_impressions"
	SearchPosition    Metric = "search_position"

	StoreInstalls            Metric = "store_installs"
	StoreRatingAverage       Metric = "store_rating_average"
	StoreRatings             Metric = "store_ratings"
	StoreReviews             Metric = "store_reviews"
	StoreReviewRatingAverage Metric = "store_review_rating_average"
	StoreProceeds            Metric = "store_proceeds"
	StoreImpressions         Metric = "store_impressions"
	StorePageViews           Metric = "store_page_views"
	CrashedUsers             Metric = "crashed_users"
	ANRUsers                 Metric = "anr_users"
	VitalsUsers              Metric = "vitals_users"
)

func sum(m Metric, s Scale) MetricSpec {
	return MetricSpec{Metric: m, Scale: s, Aggregate: AggregateSum}
}

// Metrics is every metric with its spec, in the order a board lists them.
func Metrics() []MetricSpec {
	return []MetricSpec{
		sum(AdSpend, ScaleMoneyMicros), sum(AdImpressions, ScaleCount), sum(AdClicks, ScaleCount),
		// FRACTIONAL: attribution splits one conversion across clicks.
		sum(AdConversions, ScalePPM), sum(AdConversionValue, ScaleMoneyMicros), sum(AdInstalls, ScaleCount),
		sum(AdEarnings, ScaleMoneyMicros), sum(AdPageViews, ScaleCount), sum(AdImpressionsServed, ScaleCount),
		sum(AppAdEarnings, ScaleMoneyMicros), sum(AppAdRequests, ScaleCount), sum(AppAdImpressions, ScaleCount),
		sum(Sessions, ScaleCount),
		{Metric: ActiveUsers, Scale: ScaleCount, Aggregate: AggregateLatest},
		sum(NewUsers, ScaleCount), sum(KeyEvents, ScalePPM), sum(PurchaseRevenue, ScaleMoneyMicros),
		sum(SearchClicks, ScaleCount), sum(SearchImpressions, ScaleCount),
		{Metric: SearchPosition, Scale: ScalePPM, Aggregate: AggregateWeighted, WeightedBy: SearchImpressions},
		sum(StoreInstalls, ScaleCount),
		{Metric: StoreRatingAverage, Scale: ScalePPM, Aggregate: AggregateLatest},
		sum(StoreRatings, ScaleCount), sum(StoreReviews, ScaleCount),
		{Metric: StoreReviewRatingAverage, Scale: ScalePPM, Aggregate: AggregateWeighted, WeightedBy: StoreReviews},
		sum(StoreProceeds, ScaleMoneyMicros), sum(StoreImpressions, ScaleCount), sum(StorePageViews, ScaleCount),
		sum(CrashedUsers, ScaleCount), sum(ANRUsers, ScaleCount), sum(VitalsUsers, ScaleCount),
	}
}

// Valid reports whether m is a metric this build knows.
func (m Metric) Valid() bool {
	return slices.ContainsFunc(Metrics(), func(s MetricSpec) bool { return s.Metric == m })
}

// Spec is m's spec, false for a metric this build does not know.
func (m Metric) Spec() (MetricSpec, bool) {
	i := slices.IndexFunc(Metrics(), func(s MetricSpec) bool { return s.Metric == m })
	if i < 0 {
		return MetricSpec{}, false
	}
	return Metrics()[i], true
}

// Money reports whether m's values are money, which a row carries with its
// currency and nothing sums across currencies.
func (m Metric) Money() bool {
	s, ok := m.Spec()
	return ok && s.Scale == ScaleMoneyMicros
}
