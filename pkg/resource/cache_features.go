package resource

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	FeatureCacheBudgetV1      = "cache_budget_v1"
	cacheMaximumFeaturePrefix = "cache_budget_max_bytes:"
)

// CacheMaximumFeature records the immutable upper bound in the existing
// client_features field. This intentionally preserves the strict legacy lease
// JSON schema: old controllers may ignore unknown features without treating a
// new lease field as an invalid live run.
func CacheMaximumFeature(max uint64) string {
	return cacheMaximumFeaturePrefix + strconv.FormatUint(max, 10)
}

func CacheMaximumFromFeatures(features []string) (uint64, error) {
	hasBudget, found := false, false
	var maximum uint64
	for _, feature := range features {
		if feature == FeatureCacheBudgetV1 {
			hasBudget = true
			continue
		}
		if !strings.HasPrefix(feature, cacheMaximumFeaturePrefix) {
			continue
		}
		if found {
			return 0, fmt.Errorf("duplicate cache budget maximum in lease features")
		}
		found = true
		var err error
		maximum, err = strconv.ParseUint(strings.TrimPrefix(feature, cacheMaximumFeaturePrefix), 10, 64)
		if err != nil || maximum == 0 || maximum%4096 != 0 || maximum > math.MaxInt {
			return 0, fmt.Errorf("invalid cache budget maximum in lease features")
		}
	}
	if hasBudget != found {
		return 0, fmt.Errorf("cache budget feature and immutable maximum must appear together")
	}
	return maximum, nil
}

func HasFeature(features []string, want string) bool {
	for _, feature := range features {
		if feature == want {
			return true
		}
	}
	return false
}
