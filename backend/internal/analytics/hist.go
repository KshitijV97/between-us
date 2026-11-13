package analytics

import (
	"math"
	"sort"
)

// Mergeable histograms (docs/04-metrics-catalog.md §5). Rollup rows store
// fixed-shape count arrays whose element-wise sum describes the union of the
// rows, so window aggregation stays a plain sum. These helpers are shared by
// the builder (which fills the arrays) and the compute layer (which reads
// percentiles back out of them).

// ResponseBucket maps a response time in seconds to its log-scale bucket
// (§5.1): bucket i covers [2^(i/4), 2^((i+1)/4)) seconds, bucket 0 also holds
// everything below 2^(1/4) (including 0 s), and bucket 63 holds everything
// from 2^(63/4) s (≈ 15.3 h) upwards.
func ResponseBucket(seconds int64) int {
	if seconds < 2 { // 2^(1/4) ≈ 1.19: 0 s and 1 s both fall in bucket 0
		return 0
	}
	i := int(math.Floor(4 * math.Log2(float64(seconds))))
	// Guard against floating-point error right at a bucket edge: the bucket
	// must satisfy lo <= seconds < hi.
	for i > 0 && float64(seconds) < math.Exp2(float64(i)/4) {
		i--
	}
	for i < ResponseHistBuckets-1 && float64(seconds) >= math.Exp2(float64(i+1)/4) {
		i++
	}
	return min(max(i, 0), ResponseHistBuckets-1)
}

// ResponseBucketBounds returns bucket i's [lo, hi) range in seconds. Bucket 0
// starts at 0 so interpolation inside it is linear; the open-ended last
// bucket is capped at 2^16 s for interpolation purposes.
func ResponseBucketBounds(i int) (lo, hi float64) {
	if i <= 0 {
		return 0, math.Exp2(0.25)
	}
	if i >= ResponseHistBuckets-1 {
		return math.Exp2(float64(ResponseHistBuckets-1) / 4), math.Exp2(16)
	}
	return math.Exp2(float64(i) / 4), math.Exp2(float64(i+1) / 4)
}

// gapEdges are the upper bounds (exclusive) of the first 15 gap buckets in
// seconds (§5.2); the 16th bucket is open-ended (≥ 6 months).
var gapEdges = [GapHistBuckets - 1]int64{
	60,          // 0–1 m
	5 * 60,      // 1–5 m
	15 * 60,     // 5–15 m
	3600,        // 15–60 m
	3 * 3600,    // 1–3 h
	6 * 3600,    // 3–6 h
	12 * 3600,   // 6–12 h
	86400,       // 12–24 h
	2 * 86400,   // 1–2 d
	4 * 86400,   // 2–4 d
	7 * 86400,   // 4–7 d
	14 * 86400,  // 1–2 w
	28 * 86400,  // 2–4 w
	90 * 86400,  // 1–3 mo (90 days)
	180 * 86400, // 3–6 mo (180 days)
}

// GapBucket maps an inter-message gap in seconds to its coarse bucket (§5.2).
func GapBucket(seconds int64) int {
	return sort.Search(len(gapEdges), func(i int) bool { return seconds < gapEdges[i] })
}

// GapBucketBounds returns bucket i's [lo, hi) range in seconds; the last
// bucket's hi is math.Inf(1).
func GapBucketBounds(i int) (lo, hi float64) {
	if i > 0 {
		lo = float64(gapEdges[min(i, len(gapEdges))-1])
	}
	if i >= len(gapEdges) {
		return lo, math.Inf(1)
	}
	return lo, float64(gapEdges[i])
}

// SentimentBucket maps a compound score in [-1, 1] to one of 20 buckets of
// width 0.1 (§5.3); the last bucket is closed so +1.0 lands in bucket 19.
func SentimentBucket(score float64) int {
	// The epsilon keeps exact edges such as -0.9 in their own bucket despite
	// binary floating point ((−0.9 + 1) × 10 = 0.99999…).
	i := int(math.Floor((score+1)*10 + 1e-9))
	return min(max(i, 0), SentimentHistBuckets-1)
}

// SentimentBucketBounds returns bucket i's [lo, hi) score range.
func SentimentBucketBounds(i int) (lo, hi float64) {
	return -1 + float64(i)/10, -1 + float64(i+1)/10
}

// PercentileFromHist estimates the p-quantile (0 ≤ p ≤ 1) of a histogram:
// it finds the bucket where the cumulative count crosses p·N and
// interpolates inside it. Interpolation is geometric (lo·(hi/lo)^frac) when
// the bucket's lo is positive — the right model for the log-scale response
// histogram, worst-case relative error ≈ 9 % (§5.1) — and linear otherwise
// (bucket 0 of the response histogram, or any bucket with lo ≤ 0). ok is
// false for an empty histogram. For linear-scale histograms such as
// sentiment use LinearPercentileFromHist.
func PercentileFromHist(hist []int, p float64, bounds func(int) (float64, float64)) (value float64, ok bool) {
	return percentile(hist, p, bounds, true)
}

// LinearPercentileFromHist is PercentileFromHist with linear interpolation
// in every bucket (sentiment, §5.3; error ≤ half a bucket width).
func LinearPercentileFromHist(hist []int, p float64, bounds func(int) (float64, float64)) (value float64, ok bool) {
	return percentile(hist, p, bounds, false)
}

func percentile(hist []int, p float64, bounds func(int) (float64, float64), geometric bool) (float64, bool) {
	total := 0
	for _, c := range hist {
		total += c
	}
	if total == 0 {
		return 0, false
	}
	p = min(max(p, 0), 1)
	target := p * float64(total)
	cum := 0.0
	for i, c := range hist {
		if c == 0 {
			continue
		}
		if cum+float64(c) >= target {
			frac := (target - cum) / float64(c)
			lo, hi := bounds(i)
			if math.IsInf(hi, 1) {
				return lo, true
			}
			if geometric && lo > 0 {
				return lo * math.Pow(hi/lo, frac), true
			}
			return lo + (hi-lo)*frac, true
		}
		cum += float64(c)
	}
	// Unreachable for p ≤ 1 (the loop always crosses target); return the top
	// of the last non-empty bucket defensively.
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i] > 0 {
			_, hi := bounds(i)
			return hi, true
		}
	}
	return 0, false
}
