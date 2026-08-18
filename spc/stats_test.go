package spc

import (
	"math"
	"testing"
)

const eps = 1e-9

func close(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestMean(t *testing.T) {
	if _, ok := Mean(nil); ok {
		t.Error("Mean(nil) should report false")
	}
	m, ok := Mean([]float64{1, 2, 3, 4})
	if !ok {
		t.Fatal("Mean reported false")
	}
	close(t, m, 2.5, "Mean")
	if _, ok := Mean([]float64{1, math.NaN()}); ok {
		t.Error("Mean with NaN should report false")
	}
}

func TestStdDev(t *testing.T) {
	if _, ok := StdDev([]float64{1}); ok {
		t.Error("StdDev of one point should report false")
	}
	if _, ok := StdDev([]float64{5, 5, 5, 5}); ok {
		t.Error("StdDev of a constant series should report false, not zero")
	}
	// mean 5; deviations -3,-1,1,3; sum of squares 20; /3 = 6.6667.
	sd, ok := StdDev([]float64{2, 4, 6, 8})
	if !ok {
		t.Fatal("StdDev reported false")
	}
	close(t, sd, math.Sqrt(20.0/3.0), "StdDev")
}

func TestMedian(t *testing.T) {
	if _, ok := Median(nil); ok {
		t.Error("Median(nil) should report false")
	}
	m, _ := Median([]float64{3, 1, 2})
	close(t, m, 2, "Median odd")
	m, _ = Median([]float64{4, 1, 3, 2})
	close(t, m, 2.5, "Median even")
}

func TestMedianDoesNotModifyInput(t *testing.T) {
	xs := []float64{3, 1, 2}
	Median(xs)
	MAD(xs)
	if xs[0] != 3 || xs[1] != 1 || xs[2] != 2 {
		t.Errorf("input was reordered: %v", xs)
	}
}

func TestMAD(t *testing.T) {
	// median 3; deviations 2,1,0,1,2; median of those is 1.
	mad, ok := MAD([]float64{1, 2, 3, 4, 5})
	if !ok {
		t.Fatal("MAD reported false")
	}
	close(t, mad, 1, "MAD")
	if _, ok := MAD([]float64{7, 7, 7}); ok {
		t.Error("MAD of a constant series should report false")
	}
}

// MAD·MADScale estimates the same dispersion as StdDev on clean normal-ish
// data, which is the property that makes the two interchangeable as a control
// chart's sigma.
func TestMADScaleAgreesWithStdDev(t *testing.T) {
	// A symmetric series with a known shape; the two estimators should land
	// within 25% of each other.
	xs := []float64{9.4, 9.7, 9.9, 10.0, 10.0, 10.1, 10.2, 10.4, 10.6, 9.8, 10.3, 9.6}
	sd, ok := StdDev(xs)
	if !ok {
		t.Fatal("StdDev reported false")
	}
	mad, ok := MAD(xs)
	if !ok {
		t.Fatal("MAD reported false")
	}
	robust := mad * MADScale
	if ratio := robust / sd; ratio < 0.75 || ratio > 1.25 {
		t.Errorf("MAD*MADScale = %v, StdDev = %v, ratio %v outside [0.75,1.25]", robust, sd, ratio)
	}
}

// One outlier moves StdDev a long way and MAD not at all. This is the whole
// reason TrailingRobust exists.
func TestMADResistsAnOutlier(t *testing.T) {
	clean := []float64{10, 10, 10, 11, 9, 10, 11, 9, 10, 10, 11, 9}
	dirty := append([]float64(nil), clean...)
	dirty[len(dirty)-1] = 500 // one corrupted observation, same count

	sdClean, _ := StdDev(clean)
	sdDirty, _ := StdDev(dirty)
	if sdDirty < sdClean*10 {
		t.Errorf("StdDev barely moved: %v -> %v", sdClean, sdDirty)
	}

	madClean, _ := MAD(clean)
	madDirty, _ := MAD(dirty)
	close(t, madDirty, madClean, "MAD with an outlier")
}
