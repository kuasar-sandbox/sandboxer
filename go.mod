module github.com/kuasar-sandbox/sandboxer

go 1.26.1

require (
	github.com/moby/sys/user v0.4.0
	google.golang.org/grpc v1.83.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/golang/snappy v1.0.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

require (
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/kuasar-sandbox/accelerator v0.1.3
	github.com/kuasar-sandbox/connector v0.1.2
	golang.org/x/crypto v0.56.0
	golang.org/x/net v0.58.0
	golang.org/x/sys v0.47.0
)

replace (
	github.com/kuasar-sandbox/accelerator => ../accelerator
	github.com/kuasar-sandbox/connector => ../connector
)
