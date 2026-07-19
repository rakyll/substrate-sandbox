package main

// Default images for `ssbx deploy`, injected at release time with
//
//	go build -ldflags "-X main.defaultGuestImage=... -X main.defaultAteomImage=..."
//
// The release workflow publishes digest-pinned ssbx-guest and
// ateom-gvisor images and bakes their references in here, so released
// binaries can deploy without the user building images. Source builds
// leave them empty, and deploy requires the corresponding flags.
var (
	defaultGuestImage string
	defaultAteomImage    string
	defaultAPIImage      string
)
