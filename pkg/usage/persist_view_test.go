package usage

import "testing"

func TestViewPreservesEachMetricsRequestBoundary(t *testing.T) {
	m := testManager(&faultWriter{})
	for _, name := range []string{"guest.memory", "filesystem.root"} {
		if err := m.Gauge(name, "source", 1, 0, 10, OK, 0); err != nil {
			t.Fatal(err)
		}
		if err := m.Gauge(name, "source", 2, 1e9, 0, Missing, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Gauge("guest.memory", "source", 3, 2e9, 20, OK, 0); err != nil {
		t.Fatal(err)
	}
	// A query between these individual merges must retain each request's
	// actual identity and status, not present a fabricated whole-round value.
	v := m.View()
	if v.Live.Gauges[0].LastRequest != 3 || v.Live.Gauges[0].Status != OK ||
		v.Live.Gauges[1].LastRequest != 2 || v.Live.Gauges[1].Status != Missing || v.Live.Gauges[1].LastValue != 10 {
		t.Fatalf("mixed publication lost metric identity: %+v", v.Live.Gauges)
	}
	if err := m.Gauge("filesystem.root", "source", 3, 2e9, 30, OK, 0); err != nil {
		t.Fatal(err)
	}
	if v.Live.Gauges[1].LastRequest != 2 || m.View().Live.Gauges[1].LastRequest != 3 {
		t.Fatal("query did not preserve its copied state")
	}
}
