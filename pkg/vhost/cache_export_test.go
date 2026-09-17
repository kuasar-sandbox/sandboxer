package vhost

// NewCOWCacheForTest exposes deterministic worker gates to the external tests
// exercising the real snapshot and runtime owner packages. Not a production API.
func NewCOWCacheForTest(size, dirty uint64, beforeSelect func(), beforeWrite func() error, beforeCleanup ...func() error) (*COWCache, error) {
	hooks := cacheHooks{beforeSelect: beforeSelect}
	if beforeWrite != nil {
		hooks.beforeWrite = func([]*cachePage) error { return beforeWrite() }
	}
	if len(beforeCleanup) != 0 {
		hooks.beforeCleanup = beforeCleanup[0]
	}
	return newCOWCache(size, dirty, hooks)
}
