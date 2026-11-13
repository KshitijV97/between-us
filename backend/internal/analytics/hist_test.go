package analytics

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
)

func TestResponseBucketBoundsContainValue(t *testing.T) {
	for _, s := range []int64{0, 1, 2, 3, 4, 59, 60, 61, 840, 900, 1800, 3600, 21600, 55108, 55109, 1 << 20} {
		i := ResponseBucket(s)
		lo, hi := ResponseBucketBounds(i)
		if i < 0 || i >= ResponseHistBuckets {
			t.Fatalf("bucket %d out of range for %d s", i, s)
		}
		if i < ResponseHistBuckets-1 && (float64(s) < lo || float64(s) >= hi) {
			t.Errorf("%d s in bucket %d [%.3f, %.3f)", s, i, lo, hi)
		}
	}
	if ResponseBucket(0) != 0 || ResponseBucket(1) != 0 || ResponseBucket(2) != 4 || ResponseBucket(4) != 8 {
		t.Error("unexpected low buckets")
	}
	if ResponseBucket(6*3600) > 57 { // §5.1: 6 h session gap never exceeds bucket 57
		t.Errorf("6 h in bucket %d", ResponseBucket(6*3600))
	}
	if ResponseBucket(1<<30) != ResponseHistBuckets-1 {
		t.Error("huge value must land in the last bucket")
	}
}

func TestGapBucket(t *testing.T) {
	cases := map[int64]int{0: 0, 59: 0, 60: 1, 299: 1, 300: 2, 3599: 3, 3600: 4, 86399: 7, 86400: 8, 180 * 86400: 15, 1 << 40: 15}
	for s, want := range cases {
		if got := GapBucket(s); got != want {
			t.Errorf("GapBucket(%d) = %d, want %d", s, got, want)
		}
	}
	lo, hi := GapBucketBounds(15)
	if lo != 180*86400 || !math.IsInf(hi, 1) {
		t.Errorf("last gap bucket [%v, %v)", lo, hi)
	}
	if lo, hi := GapBucketBounds(0); lo != 0 || hi != 60 {
		t.Errorf("first gap bucket [%v, %v)", lo, hi)
	}
}

func TestSentimentBucket(t *testing.T) {
	cases := map[float64]int{-1: 0, -0.95: 0, -0.9: 1, 0: 10, 0.049: 10, 0.5: 15, 0.99: 19, 1: 19, 2: 19, -3: 0}
	for s, want := range cases {
		if got := SentimentBucket(s); got != want {
			t.Errorf("SentimentBucket(%v) = %d, want %d", s, got, want)
		}
	}
}

func TestPercentileFromHistEmptyAndSingle(t *testing.T) {
	if _, ok := PercentileFromHist(make([]int, ResponseHistBuckets), 0.5, ResponseBucketBounds); ok {
		t.Error("empty histogram must report !ok")
	}
	h := make([]int, ResponseHistBuckets)
	h[ResponseBucket(840)] = 1
	v, ok := PercentileFromHist(h, 0.5, ResponseBucketBounds)
	lo, hi := ResponseBucketBounds(ResponseBucket(840))
	if !ok || v < lo || v > hi {
		t.Errorf("median %v outside bucket [%v, %v]", v, lo, hi)
	}
	// Bucket 0 interpolates linearly from 0.
	h0 := make([]int, ResponseHistBuckets)
	h0[0] = 2
	if v, _ := PercentileFromHist(h0, 0.5, ResponseBucketBounds); v <= 0 || v >= math.Exp2(0.25) {
		t.Errorf("bucket-0 median %v", v)
	}
}

// The histogram estimate stays within the documented ~9 % of the exact
// percentile for realistic response-time samples (§5.1).
func TestPercentileFromHistAccuracy(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for trial := 0; trial < 50; trial++ {
		n := 50 + r.IntN(2000)
		vals := make([]float64, n)
		h := make([]int, ResponseHistBuckets)
		for i := range vals {
			v := int64(math.Exp(r.Float64()*9)) + 2 // ~2 s .. ~2.3 h, log-uniform
			vals[i] = float64(v)
			h[ResponseBucket(v)]++
		}
		sort.Float64s(vals)
		for _, p := range []float64{0.5, 0.9} {
			exact := vals[int(math.Ceil(p*float64(n)))-1]
			est, _ := PercentileFromHist(h, p, ResponseBucketBounds)
			if rel := math.Abs(est-exact) / exact; rel > 0.19 { // one bucket width either side
				t.Fatalf("trial %d p%.0f: est %.1f exact %.1f (%.1f%%)", trial, p*100, est, exact, rel*100)
			}
		}
	}
}

func TestLinearPercentileSentiment(t *testing.T) {
	h := make([]int, SentimentHistBuckets)
	h[SentimentBucket(0.6)] = 3
	h[SentimentBucket(-0.2)] = 1
	v, ok := LinearPercentileFromHist(h, 0.5, SentimentBucketBounds)
	if !ok || v < 0.6 || v > 0.7 {
		t.Errorf("median sentiment %v", v)
	}
}
