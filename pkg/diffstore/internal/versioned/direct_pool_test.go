package versioned

import (
	"testing"
	"time"
)

// Exercise the production buffer acquisition itself: no model or replacement
// semaphore. Stalled callbacks hold exactly the resource a blocked syscall owns.
func TestSharedDirectPoolSaturation(t *testing.T) {
	var gates [4]chan struct{}
	entered := make(chan int, 4)
	done := make(chan error, 5)
	for i := range gates {
		gates[i] = make(chan struct{})
		go func(i int) {
			done <- withDirectBuffer(PageSize, func([]byte) error { entered <- i; <-gates[i]; return nil })
		}(i)
	}
	defer func() {
		for _, g := range gates {
			select {
			case <-g:
			default:
				close(g)
			}
		}
	}()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("four DIO slots unavailable")
		}
	}
	fifth := make(chan struct{})
	go func() { done <- withDirectBuffer(PageSize, func([]byte) error { close(fifth); return nil }) }()
	select {
	case <-fifth:
		t.Fatal("pool allowed more than four simultaneous buffers")
	case <-time.After(50 * time.Millisecond):
	}
	close(gates[0])
	select {
	case <-fifth:
	case <-time.After(time.Second):
		t.Fatal("freed DIO slot did not admit fifth caller")
	}
	for _, g := range gates[1:] {
		close(g)
	}
	for range 5 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	t.Log("actual shared 4-slot DIO pool blocks a fifth independent caller until one held callback completes")
}
