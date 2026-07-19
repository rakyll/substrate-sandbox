package main

// Default images for `sbcli system deploy`, injected at release time with
//
//	go build -ldflags "-X main.defaultGuestdImage=... -X main.defaultAteomImage=..."
//
// The release workflow publishes digest-pinned substrate-guestd and
// ateom-gvisor images and bakes their references in here, so released
// binaries can deploy without the user building images. Source builds
// leave them empty, and deploy requires the corresponding flags.
var (
	defaultGuestdImage string
	defaultAteomImage  string
	defaultAPIImage    string
)
