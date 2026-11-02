// launchrecovery is a prepared test fixture, never a product control hook.
// "keys" selects Manifest chunks for an inode logical disk range after
// validating the carrier geometry; "guest" performs gated real reads.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/buildversion"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func keys(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("keys MANIFEST CARRIER OFFSET LENGTH")
	}
	off, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || off < 0 {
		return fmt.Errorf("invalid offset")
	}
	length, err := strconv.ParseInt(args[3], 10, 64)
	if err != nil || length <= 0 || length > 65536 {
		return fmt.Errorf("invalid length")
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	m, _, err := codec.Unmarshal(b)
	if err != nil {
		return err
	}
	f, err := os.Open(args[1])
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	src, _, err := tarstream.SourceAt(f, st.Size(), "")
	if err != nil {
		return err
	}
	if src.Size() != m.ImageSize {
		return fmt.Errorf("carrier and manifest logical sizes differ")
	}
	if _, err = src.ReadAt(context.Background(), make([]byte, int(length)), uint64(off)); err != nil {
		return err
	}
	var selected []string
	for _, chunk := range m.Entries {
		if chunk.IsZero {
			continue
		}
		// manifest-ctl ingests the decoded sparse source. Its entry offsets
		// address disk bytes, never physical tarstream carrier positions.
		if chunk.Offset < uint64(off+length) && chunk.Offset+uint64(chunk.Size) > uint64(off) {
			selected = append(selected, fmt.Sprintf("%x", chunk.CiphertextHash))
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("logical read selected no stored chunks")
	}
	fmt.Println(strings.Join(selected, ","))
	return nil
}
func guest() error {
	fmt.Println("LAUNCH-APP-STARTED")
	for {
		if _, err := os.Stat("/read.begin"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f, err := os.Open("/data/payload")
	if err != nil {
		return err
	}
	defer f.Close()
	b := make([]byte, 4096)
	if _, err := f.ReadAt(b, 1<<20); err != nil {
		return err
	}
	if !bytes.Equal(b, bytes.Repeat([]byte{5}, len(b))) {
		return fmt.Errorf("recovered read returned incorrect bytes")
	}
	fmt.Println("LAUNCH-READ-RECOVERED")
	for {
		time.Sleep(time.Hour)
	}
}

// pong uses the existing Guest management exchange, including its EOF barrier.
func pong(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("pong VSOCK_SOCKET")
	}
	host := guestlink.HostClient{BasePath: args[0]}
	reply, err := host.RoundTripContext(context.Background(), &proto.Message{Type: proto.TypePing, ID: 1}, 5*time.Second)
	if err != nil {
		return err
	}
	if reply.Type != proto.TypePong {
		return fmt.Errorf("expected Pong: %s", reply.Type)
	}
	return json.NewEncoder(os.Stdout).Encode(reply.InitialLaunchState)
}
func main() {
	buildversion.ExitIfRequested()
	var err error
	if len(os.Args) > 1 && os.Args[1] == "guest" {
		err = guest()
	} else if len(os.Args) > 1 && os.Args[1] == "pong" {
		err = pong(os.Args[2:])
	} else if len(os.Args) > 1 && os.Args[1] == "keys" {
		err = keys(os.Args[2:])
	} else {
		err = fmt.Errorf("expected keys, guest or pong")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
