module github.com/kuasar-sandbox/sandboxer

go 1.24.0

require (
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/moby/sys/user v0.4.0
	google.golang.org/grpc v1.80.0 // indirect
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/golang/snappy v1.0.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260120221211-b8f7ae30c516 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

require (
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/kuasar-sandbox/accelerator v0.0.0
	github.com/kuasar-sandbox/connector v0.0.0
	golang.org/x/crypto v0.47.0
	golang.org/x/net v0.49.0
	golang.org/x/sys v0.40.0
)

replace (
	github.com/kuasar-sandbox/accelerator => ../accelerator
	github.com/kuasar-sandbox/connector => ../connector
)
