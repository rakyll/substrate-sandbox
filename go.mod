module github.com/rakyll/substrate-sandbox

go 1.26.3

require (
	github.com/agent-substrate/substrate v0.0.0-20260717234919-a2d55e99e02e
	github.com/spf13/cobra v1.10.2
	google.golang.org/grpc v1.81.0
	google.golang.org/protobuf v1.36.12-0.20260120151049-f2248ac996af
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260406210006-6f92a3bedf2d // indirect
)

replace github.com/agent-substrate/substrate => ../substrate
