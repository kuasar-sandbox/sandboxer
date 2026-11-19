package resource

import (
	"bytes"
	"context"
	"net"
	"reflect"
	"testing"
)

const testCacheReclaimRun = "0123456789abcdef0123456789abcdef"

func TestCacheReclaimFeatureContract(t *testing.T) {
	valid := []string{FeatureCacheBudgetV1, CacheMaximumFeature(64 << 20), FeatureCacheReclaimV1, CacheReclaimRunFeature(testCacheReclaimRun)}
	if run, err := CacheReclaimRunFromFeatures(valid); err != nil || run != testCacheReclaimRun {
		t.Fatalf("valid run: %q %v", run, err)
	}
	if run, err := CacheReclaimRunFromFeatures(valid[:2]); err != nil || run != "" {
		t.Fatalf("legacy budget: %q %v", run, err)
	}
	for _, features := range [][]string{
		{FeatureCacheBudgetV1, FeatureCacheReclaimV1},
		{FeatureCacheBudgetV1, CacheReclaimRunFeature(testCacheReclaimRun)},
		{FeatureCacheReclaimV1, CacheReclaimRunFeature(testCacheReclaimRun)},
		{FeatureCacheBudgetV1, FeatureCacheReclaimV1, CacheReclaimRunFeature("bad")},
		{FeatureCacheBudgetV1, FeatureCacheReclaimV1, CacheReclaimRunFeature("0123456789ABCDEF0123456789ABCDEF")},
		append(append([]string(nil), valid...), CacheReclaimRunFeature(testCacheReclaimRun)),
	} {
		if _, err := CacheReclaimRunFromFeatures(features); err == nil {
			t.Fatalf("accepted invalid features %q", features)
		}
	}
}

func TestCacheReclaimWireRoundTrip(t *testing.T) {
	task := CacheReclaimTask{RunID: testCacheReclaimRun, Seq: 7, Source: CacheReclaimAutonomous, IntentID: 3, MaxBytes: 4 << 20, Target: 8 << 20}
	intent := &CacheReclaimIntent{ID: 3, Bytes: 4 << 20, Observation: 2}
	original := &Message{Type: TypeCacheReclaimUpdate, Token: "token", CacheReclaim: &CacheReclaimState{
		RunID: testCacheReclaimRun, Current: 8 << 20, LastSeq: 7, Intent: intent,
		Done: &CacheReclaimDone{Task: task, Current: 8 << 20, Intent: intent},
	}, CacheReclaimAck: 6, CacheReclaimStatus: &CacheReclaimStatus{InFlightTasks: 1, InFlightBytes: 4 << 20, LegacyParticipants: 2}}
	var wire bytes.Buffer
	if err := WriteMessage(&wire, original); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadMessage(&wire)
	if err != nil || !reflect.DeepEqual(original, decoded) {
		t.Fatalf("wire roundtrip: %#v %v", decoded, err)
	}
	copyState := CloneCacheReclaimState(original.CacheReclaim)
	copyState.Intent.Bytes = 4096
	copyState.Done.Intent.ID = 99
	if original.CacheReclaim.Intent.Bytes != 4<<20 || original.CacheReclaim.Done.Intent.ID != 3 {
		t.Fatal("mutable RPC snapshot aliases the live outbox")
	}
}

func TestCacheReclaimClientUpdateAndStateSync(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	client := &Client{conn: clientConn, token: "before"}
	task := CacheReclaimTask{RunID: testCacheReclaimRun, Seq: 1, Source: CacheReclaimAutonomous, IntentID: 1, MaxBytes: 4 << 20, Target: 8 << 20}
	state := CacheReclaimState{RunID: testCacheReclaimRun, Current: 8 << 20, LastSeq: 1, Done: &CacheReclaimDone{Task: task, Current: 8 << 20}}
	errs := make(chan error, 1)
	requests := make(chan Message, 2)
	go func() {
		for i := 0; i < 2; i++ {
			req, err := ReadMessage(serverConn)
			if err != nil {
				errs <- err
				return
			}
			requests <- *req
			resp := &Message{Type: TypeAck, CacheReservation: 8 << 20, CacheReclaimAck: 1}
			if i == 1 {
				resp.Token = "after"
				resp.NewAllocatable = 128 << 20
				resp.ClientFeatures = []string{FeatureCacheBudgetV1, FeatureCacheReclaimV1}
			}
			if err := WriteMessage(serverConn, resp); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	result, err := client.UpdateCacheReclaimContext(context.Background(), state)
	if err != nil || result.Reservation != 8<<20 || result.Ack != 1 {
		t.Fatalf("update: %+v %v", result, err)
	}
	synced, err := client.StateSync(StateSyncParams{SandboxID: "test", AppliedAllocatableMemory: 128 << 20,
		AppliedCacheReservation: 8 << 20, AppliedCacheMaximum: 64 << 20, CacheReclaim: &state})
	if err != nil || synced.CacheReclaimAck != 1 || !HasFeature(synced.ClientFeatures, FeatureCacheReclaimV1) || client.Token() != "after" {
		t.Fatalf("sync: %+v %v", synced, err)
	}
	if req := <-requests; req.Type != TypeCacheReclaimUpdate || req.CacheReclaim.Done.Task.Seq != 1 || req.CacheRequested != 0 {
		t.Fatalf("intent/completion used wrong message: %+v", req)
	}
	if req := <-requests; req.Type != TypeStateSync || !reflect.DeepEqual(req.CacheReclaim, &state) {
		t.Fatalf("state sync lost outbox: %+v", req)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}
