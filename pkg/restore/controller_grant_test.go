package restore

import "testing"

func TestValidateRestoredGrant(t *testing.T) {
	const (
		floor    = uint64(256 << 20)
		capacity = uint64(1024 << 20)
	)
	for name, test := range map[string]struct {
		grant uint64
		valid bool
	}{
		"floor":          {grant: floor, valid: true},
		"within bounds":  {grant: 512 << 20, valid: true},
		"capacity":       {grant: capacity, valid: true},
		"below floor":    {grant: floor - 1},
		"above capacity": {grant: capacity + 1},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateRestoredGrant(test.grant, floor, capacity)
			if (err == nil) != test.valid {
				t.Fatalf("validateRestoredGrant(%d) = %v, valid=%v", test.grant, err, test.valid)
			}
		})
	}
}
