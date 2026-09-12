package usage

import (
	"bytes"
	"encoding/json"
	"math/big"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func TestCounterCreationRemainderAndIdentity(t *testing.T) {
	var c Counter
	for raw := uint64(1); raw <= 997; raw++ {
		if err := c.Observe("boot/17/42", raw, 997, true); err != nil {
			t.Fatal(err)
		}
	}
	if c.KnownTotal != (Uint128{Lo: 1e9}) || c.Remainder != 0 || !c.Complete {
		t.Fatalf("%+v", c)
	}
	before := c.KnownTotal
	if err := c.Observe("boot/17/42", 1, 997, true); err == nil || c.KnownTotal != before || c.Complete {
		t.Fatalf("regression: %+v", c)
	}
	if err := c.Observe("boot/17/43", 997, 997, true); err != nil {
		t.Fatal(err)
	}
	if c.KnownTotal != (Uint128{Lo: 2e9}) || c.Complete {
		t.Fatalf("replacement: %+v", c)
	}
	var unknown Counter
	if err := unknown.Observe("existing", 1000, 100, false); err != nil {
		t.Fatal(err)
	}
	if err := unknown.Observe("existing", 1005, 100, false); err != nil {
		t.Fatal(err)
	}
	if unknown.KnownTotal != (Uint128{Lo: 50e6}) || unknown.Complete {
		t.Fatalf("unknown baseline: %+v", unknown)
	}
}

func TestGaugeFailuresTimeAndGrouping(t *testing.T) {
	for seed := int64(0); seed < 100; seed++ {
		rng := rand.New(rand.NewSource(seed))
		plain, grouped := Gauge{Name: "ram"}, Gauge{Name: "ram"}
		var windows Window
		var at int64
		for req := uint64(1); req <= 1000; req++ {
			at += int64(rng.Intn(4)+1) * int64(time.Second)
			status := OK
			if rng.Intn(7) == 0 {
				status = Missing
			}
			value := uint64(rng.Intn(1024))
			for _, g := range []*Gauge{&plain, &grouped} {
				if err := g.Observe("ram/1", req, at, value, status, time.Millisecond, time.Second); err != nil {
					t.Fatal(err)
				}
			}
			if rng.Intn(3) == 0 {
				var err error
				windows, err = mergeWindow(windows, grouped.Window)
				if err != nil {
					t.Fatal(err)
				}
				grouped.Window = Window{}
			}
		}
		var err error
		grouped.Window, err = mergeWindow(windows, grouped.Window)
		if err != nil {
			t.Fatal(err)
		}
		// Reassigning an equal peak to an adjacent record may retain a different
		// observed endpoint with the same maximum. Totals and peak must agree.
		grouped.Window.PeakAt = plain.Window.PeakAt
		if !reflect.DeepEqual(plain, grouped) {
			t.Fatalf("seed=%d\nplain=%+v\ngrouped=%+v", seed, plain, grouped)
		}
	}
}

func TestGaugeSaveBetweenSamples(t *testing.T) {
	g := Gauge{Name: "disk"}
	if err := g.Observe("fs/1", 1, 0, 8, OK, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	first := g.Window
	g.Window = Window{} // save at 0.5s does not extrapolate to that time
	if err := g.Observe("fs/1", 2, 1e9, 0, OK, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	if first.Area != (Uint128{}) || g.IntegralTotal != (Uint128{Lo: 8e9}) || g.CoveredTotal != 1e9 || g.Window.Peak != 8 {
		t.Fatalf("%+v first=%+v", g, first)
	}
	g.Window = Window{}
	if err := g.Observe("fs/1", 3, 2e9, 2, OK, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	if g.Window.Peak != 2 {
		t.Fatalf("carried old peak: %+v", g.Window)
	}
	if err := g.Observe("fs/1", 4, 3e9, 0, Missing, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := g.Observe("fs/1", 5, 4e9, 7, OK, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	if g.CoveredTotal != 2e9 || g.SpanTotal != 4e9 {
		t.Fatalf("gap counted twice: %+v", g)
	}
	before := g
	if err := g.Observe("fs/1", 5, 5e9, 100, OK, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	if g != before {
		t.Fatal("duplicate applied")
	}
	if err := g.Observe("fs/1", 6, 4e9, 100, OK, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	if g.Continuous {
		t.Fatal("nonincreasing time retained continuity")
	}
}

func TestGaugeSourceRunAndOverflow(t *testing.T) {
	g := Gauge{Name: "ram"}
	_ = g.Observe("one", 1, 0, 7, OK, 0, time.Second)
	_ = g.Observe("two", 2, 1e9, 8, OK, 0, time.Second)
	if g.SpanTotal != 0 || g.CoveredTotal != 0 {
		t.Fatalf("joined sources: %+v", g)
	}
	g.Break(Paused)
	_ = g.Observe("two", 3, 90e9, 9, OK, 0, time.Second)
	if g.SpanTotal != 0 {
		t.Fatal("counted paused time")
	}
	g.IntegralTotal = Uint128{^uint64(0), ^uint64(0)}
	old := g
	if err := g.Observe("two", 4, 91e9, 9, OK, 0, time.Second); err != ErrOverflow || g != old {
		t.Fatalf("overflow not transactional: %v", err)
	}
}

func TestUint128JSONAndAverage(t *testing.T) {
	x := Uint128{^uint64(0), ^uint64(0)}
	b, err := json.Marshal(x)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, []byte(`"340282366920938463463374607431768211455"`)) {
		t.Fatal(string(b))
	}
	var y Uint128
	if err := json.Unmarshal(b, &y); err != nil || x != y {
		t.Fatalf("%v %+v", err, y)
	}
	for _, bad := range []string{`1`, `"-1"`, `"340282366920938463463374607431768211456"`, `"+1"`} {
		if json.Unmarshal([]byte(bad), &y) == nil {
			t.Fatal(bad)
		}
	}
	if _, ok := Average(x, 0); ok {
		t.Fatal("zero coverage available")
	}
	if n, ok := Average(product(1234, 5678), 5678); !ok || n != 1234 {
		t.Fatalf("%d %v", n, ok)
	}
}

func TestGaugeFullWidthTimeDifferences(t *testing.T) {
	const min, max int64 = -1 << 63, 1<<63 - 1
	pairs := [][2]int64{{min, max}, {min, 0}, {-1, max}, {-3, 4}, {min, min + 1}, {max - 1, max}, {1, 2}, {-2, -1}}
	rng := rand.New(rand.NewSource(211))
	for i := 0; i < 10000; i++ {
		a, b := int64(rng.Uint64()), int64(rng.Uint64())
		if a > b {
			a, b = b, a
		}
		if a != b {
			pairs = append(pairs, [2]int64{a, b})
		}
	}
	for _, pair := range pairs {
		difference := new(big.Int).Sub(big.NewInt(pair[1]), big.NewInt(pair[0]))
		if !difference.IsUint64() {
			t.Fatal("ordered int64 endpoints exceeded the uint64 difference domain")
		}
		want := difference.Uint64()
		g := Gauge{Name: "ram"}
		if err := g.Observe("memory", 1, pair[0], 3, OK, 0, 4); err != nil {
			t.Fatal(err)
		}
		if err := g.Observe("memory", 2, pair[1], 5, OK, 0, 4); err != nil {
			t.Fatal(err)
		}
		covered := uint64(0)
		if want <= 8 {
			covered = want
		}
		if g.SpanTotal != want || g.Window.Span != want || g.CoveredTotal != covered || g.IntegralTotal != product(3, covered) {
			t.Fatalf("%v: mathematical difference %s; gauge=%+v", pair, difference, g)
		}
	}
	// A maximal individual gap is representable. Adding it to an existing
	// nonzero span is the true overflow, rejected without partial mutation.
	g := Gauge{Name: "ram"}
	_ = g.Observe("memory", 1, min, 3, OK, 0, time.Second)
	g.SpanTotal = 1
	before := g
	if err := g.Observe("memory", 2, max, 5, OK, 0, time.Second); err != ErrOverflow || g != before {
		t.Fatalf("cumulative span overflow not transactional: %+v, %v", g, err)
	}
	// Full-width totals remain lossless in both persisted and JSON forms.
	m := testManager(&faultWriter{})
	if err := m.Gauge("ram", "memory", 1, min, 3, OK, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Gauge("ram", "memory", 2, max, 5, OK, 0); err != nil {
		t.Fatal(err)
	}
	m.Save(time.Now())
	awaitSave(t, m)
	r := m.View().Saved
	if r == nil {
		t.Fatal("full-width gap was not saved")
	}
	encoded, err := EncodeRecord(*r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecord(encoded, "test")
	if err != nil || !reflect.DeepEqual(decoded.Snapshot.Gauges, r.Snapshot.Gauges) {
		t.Fatalf("file changed full-width span: %+v, %v", decoded, err)
	}
	encoded, err = json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON Record
	if err := json.Unmarshal(encoded, &fromJSON); err != nil || !reflect.DeepEqual(fromJSON, *r) {
		t.Fatalf("JSON changed full-width span: %+v, %v", fromJSON, err)
	}
}
